package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/duynhlab/order-service/config"
	migrations "github.com/duynhlab/order-service/db/migrations"
	seed "github.com/duynhlab/order-service/db/seed"
	"github.com/duynhlab/order-service/internal/cancellation"
	database "github.com/duynhlab/order-service/internal/core"
	"github.com/duynhlab/order-service/internal/core/domain"
	"github.com/duynhlab/order-service/internal/core/repository"
	"github.com/duynhlab/order-service/internal/fulfillment"
	grpcv1 "github.com/duynhlab/order-service/internal/grpc/v1"
	logicv1 "github.com/duynhlab/order-service/internal/logic/v1"
	"github.com/duynhlab/order-service/internal/reconcile"
	"github.com/duynhlab/order-service/internal/saga"
	v1 "github.com/duynhlab/order-service/internal/web/v1"
	"github.com/duynhlab/order-service/middleware"
	"github.com/duynhlab/pkg/authmw"
	"github.com/duynhlab/pkg/grpcx"
	"github.com/duynhlab/pkg/logger/zapx"
	"github.com/duynhlab/pkg/migratex"
	"github.com/duynhlab/pkg/obsx"
	inventoryv1 "github.com/duynhlab/pkg/proto/inventory/v1"
	notificationv1 "github.com/duynhlab/pkg/proto/notification/v1"
	orderv1 "github.com/duynhlab/pkg/proto/order/v1"
	paymentv1 "github.com/duynhlab/pkg/proto/payment/v1"
	shippingv1 "github.com/duynhlab/pkg/proto/shipping/v1"
	"github.com/duynhlab/pkg/temporalx"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
	"google.golang.org/grpc"
)

func main() {
	cfg := config.Load()

	logger, err := zapx.New(os.Getenv("LOG_LEVEL"))
	if err != nil {
		panic("Failed to initialize logger: " + err.Error())
	}
	defer func() { _ = logger.Sync() }()

	// `<binary> migrate` runs embedded schema migrations, `<binary> seed` applies
	// DEV-ONLY demo data; both run their SQL and exit. No args serves the app.
	if maybeRunSubcommand(cfg, logger) {
		return
	}

	if err := cfg.Validate(); err != nil {
		panic("Configuration validation failed: " + err.Error())
	}

	logger.Info("Service starting",
		zap.String("service", cfg.Service.Name),
		zap.String("version", cfg.Service.Version),
		zap.String("env", cfg.Service.Env),
		zap.String("port", cfg.Service.Port),
	)

	// RFC-0014 OTel wiring — runs before the `worker` branch below, so the
	// Temporal worker gets the same telemetry wiring as the serve path
	// (including the OTLP-teed logger returned here).
	tp, logger := initObservability(logger)

	profilingShutdown := initProfiling(cfg, logger)
	defer profilingShutdown()

	pool, err := database.Connect(context.Background(), cfg)
	if err != nil {
		logger.Error("Failed to connect to database", zap.Error(err))
		return
	}
	defer pool.Close()
	logger.Info("Database connection pool established")

	orderRepo := repository.NewPostgresOrderRepository(pool)
	txManager := repository.NewPostgresTransactionManager(pool)
	startRequests := repository.NewPostgresStartRequestRepository(pool)
	cancellations := repository.NewPostgresCancellationRepository(pool)
	orderService := logicv1.NewOrderService(orderRepo, txManager, startRequests, startRequests, orderRepo, orderRepo, cancellations)

	registerTableGauges(startRequests, cancellations, logger)

	// `<binary> worker` runs the Temporal worker for the order-fulfillment saga
	// and serves no HTTP; it returns (and the deferred cleanups run) on shutdown.
	if maybeRunWorker(cfg, logger, orderRepo, startRequests, cancellations) {
		return
	}

	// Local RS256 JWT verification against the Keycloak realm JWKS (cached) is
	// the only credential — no gRPC fallback. NewVerifier does not block on an
	// unreachable JWKS — it refreshes in the background, so a verifier is safe
	// to build at startup.
	verifier, err := authmw.NewVerifier(authmw.Config{
		Issuer:   cfg.OIDCIssuer,
		Audience: cfg.OIDCAudience,
		JWKSURL:  cfg.OIDCJWKSURL,
	})
	if err != nil {
		logger.Error("JWKS verifier init failed", zap.Error(err))
		return
	}
	// Second verifier for the protected Backoffice group (ADR-050): the
	// workforce realm. Customer tokens never pass it and vice versa.
	staffVerifier, err := authmw.NewVerifier(authmw.Config{
		Issuer:   cfg.OIDCStaffIssuer,
		Audience: cfg.OIDCAudience,
		JWKSURL:  cfg.OIDCStaffJWKSURL,
	})
	if err != nil {
		logger.Error("staff JWKS verifier init failed", zap.Error(err))
		return
	}

	shippingClient, shippingCleanup, ok := configureShippingClient(cfg, logger)
	if !ok {
		return
	}
	defer shippingCleanup()

	// Temporal client starts the cancellation workflow from CancelOrder (order
	// creation and its saga start live on the gRPC path since RFC-0015 P4). If
	// Temporal is unavailable the cancel is still accepted — the outbox row
	// stays PENDING and the dispatcher retries the start.
	temporalClient, temporalCleanup := configureTemporalClient(cfg, logger)
	defer temporalCleanup()

	// Details-enrichment gRPC clients (payment + inventory). Dialed lazily;
	// every enrichment soft-fails, so an unreachable service only omits its
	// block. Nil interfaces (not typed-nils) on dial failure so the
	// aggregation's nil checks work.
	paymentFetch, inventoryFetch, enrichCleanup := dialEnrichmentClients(cfg, logger)
	defer enrichCleanup()

	// orderRepo appears three times on purpose: the handler asks for it once
	// per capability it needs (the processing projection, the status write, the
	// history read) rather than as one wide repository. Same value, three
	// narrow doors — a customer path holding this handler still cannot reach a
	// generic status write.
	orderHandler := v1.NewOrderHandler(orderService, shippingClient, temporalClient,
		cfg.Temporal.TaskQueue, paymentFetch, cancellations,
		orderRepo, inventoryFetch, orderRepo, orderRepo)

	grpcSrv := startGRPC(cfg, logger, orderService, temporalClient)

	var isShuttingDown atomic.Bool
	srv := setupServer(cfg, logger, verifier, staffVerifier, orderHandler, &isShuttingDown)
	runGracefulShutdown(cfg, srv, grpcSrv, tp, pool, logger, &isShuttingDown)
}

// dialEnrichmentClients dials the /details enrichment backends (soft-fail).
func dialEnrichmentClients(cfg *config.Config, logger *zap.Logger) (v1.PaymentFetcher, v1.ReservationFetcher, func()) {
	var cleanups []func()
	var paymentFetch v1.PaymentFetcher
	if conn, err := grpcx.Dial(cfg.PaymentGRPCAddr); err != nil {
		logger.Error("Failed to dial payment gRPC (enrichment unavailable)", zap.String("addr", cfg.PaymentGRPCAddr), zap.Error(err))
	} else {
		cleanups = append(cleanups, func() { _ = conn.Close() })
		paymentFetch = v1.NewPaymentGRPCClient(conn)
	}
	var inventoryFetch v1.ReservationFetcher
	if conn, err := grpcx.Dial(cfg.InventoryGRPCAddr); err != nil {
		logger.Error("Failed to dial inventory gRPC (details enrichment unavailable)", zap.String("addr", cfg.InventoryGRPCAddr), zap.Error(err))
	} else {
		cleanups = append(cleanups, func() { _ = conn.Close() })
		inventoryFetch = v1.NewInventoryGRPCClient(conn)
	}
	return paymentFetch, inventoryFetch, func() {
		for _, f := range cleanups {
			f()
		}
	}
}

// startGRPC starts the internal gRPC server on cfg.GRPC.Port, serving
// order.v1.OrderService (checkout's confirm handoff — RFC-0015 P2) alongside
// the HTTP listener (dual-port, pattern shipping). gRPC is the official
// east-west transport, so it always runs; it returns nil only if the listener
// can't bind. temporalClient may be nil (Temporal down at startup) — the
// adapter then answers Unavailable on the saga kickoff so the caller retries.
func startGRPC(cfg *config.Config, logger *zap.Logger, svc *logicv1.OrderService, temporalClient fulfillment.Starter) *grpc.Server {
	lc := net.ListenConfig{}
	lis, err := lc.Listen(context.Background(), "tcp", ":"+cfg.GRPC.Port)
	if err != nil {
		logger.Error("Failed to listen for gRPC", zap.String("port", cfg.GRPC.Port), zap.Error(err))
		return nil
	}

	grpcSrv, _ := grpcx.NewServer(logger)
	orderv1.RegisterOrderServiceServer(grpcSrv, grpcv1.NewServer(svc, temporalClient, cfg.Temporal.TaskQueue))

	go func() {
		logger.Info("Starting gRPC server", zap.String("port", cfg.GRPC.Port))
		if err := grpcSrv.Serve(lis); err != nil {
			logger.Error("gRPC server error", zap.Error(err))
		}
	}()

	return grpcSrv
}

// maybeRunSubcommand handles the `migrate` and `seed` subcommands, reporting
// whether it handled one (caller then exits). It needs only DB config, so it
// runs before cfg.Validate().
//
// `migrate` applies the versioned schema migrations and runs in every
// environment (init container, direct DB host). `seed` applies DEV-ONLY demo
// data and is invoked explicitly — never by `migrate` or the serve path — and it
// refuses to run against a production database, so prod is never seeded.
func maybeRunSubcommand(cfg *config.Config, logger *zap.Logger) bool {
	if len(os.Args) <= 1 {
		return false
	}
	switch os.Args[1] {
	case "migrate":
		if err := migratex.Run(migrations.FS, "sql", cfg.Database.BuildDSN()); err != nil {
			logger.Fatal("Schema migration failed", zap.Error(err))
		}
		logger.Info("Schema migrations applied")
		return true
	case "seed":
		if cfg.IsProduction() {
			logger.Fatal("seed refused in production — demo data is dev-only")
		}
		if err := applySeed(context.Background(), cfg); err != nil {
			logger.Fatal("Demo seed failed", zap.Error(err))
		}
		logger.Info("Demo seed data applied")
		return true
	default:
		return false
	}
}

// applySeed executes the embedded dev-only seed SQL directly against the database.
// It does NOT use golang-migrate: seeds are idempotent (ON CONFLICT) and must not
// share the schema_migrations version table with the schema migrations. Simple
// query protocol lets each multi-statement seed file run in one Exec.
func applySeed(ctx context.Context, cfg *config.Config) error {
	poolCfg, err := pgxpool.ParseConfig(cfg.Database.BuildDSN())
	if err != nil {
		return fmt.Errorf("parse seed DSN: %w", err)
	}
	poolCfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return fmt.Errorf("connect for seed: %w", err)
	}
	defer pool.Close()

	entries, err := fs.ReadDir(seed.FS, "sql")
	if err != nil {
		return fmt.Errorf("read seed dir: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".up.sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		b, readErr := fs.ReadFile(seed.FS, "sql/"+name)
		if readErr != nil {
			return fmt.Errorf("read seed %s: %w", name, readErr)
		}
		if _, execErr := pool.Exec(ctx, string(b)); execErr != nil {
			return fmt.Errorf("apply seed %s: %w", name, execErr)
		}
	}
	return nil
}

// maybeRunWorker runs the Temporal worker for the order-fulfillment saga when
// invoked as `<binary> worker`, and reports whether it handled the command. It
// dials Temporal + the downstream services (product/shipping/notification/cart),
// registers the workflow and activities, and blocks until interrupted. Temporal
// or a downstream being unreachable at startup (after the shared dial-retry
// budget) is fatal — the worker can do nothing without them, and the platform
// restart policy re-runs it. Distinct from the serve path, which degrades.
func maybeRunWorker(cfg *config.Config, logger *zap.Logger, orderRepo *repository.PostgresOrderRepository,
	startRequests *repository.PostgresStartRequestRepository,
	cancelStore *repository.PostgresCancellationRepository) bool {
	if len(os.Args) <= 1 || os.Args[1] != "worker" {
		return false
	}

	tc, err := dialTemporalRetry(cfg, logger, temporalDialAttempts, temporalDialBackoff)
	if err != nil {
		logger.Fatal("Failed to connect to Temporal", zap.String("hostport", cfg.Temporal.HostPort), zap.Error(err))
	}
	defer tc.Close()

	shippingConn, err := grpcx.Dial(cfg.ShippingGRPCAddr)
	if err != nil {
		logger.Fatal("Failed to dial shipping gRPC", zap.String("addr", cfg.ShippingGRPCAddr), zap.Error(err))
	}
	defer func() { _ = shippingConn.Close() }()

	notifyConn, err := grpcx.Dial(cfg.NotificationGRPCAddr)
	if err != nil {
		logger.Fatal("Failed to dial notification gRPC", zap.String("addr", cfg.NotificationGRPCAddr), zap.Error(err))
	}
	defer func() { _ = notifyConn.Close() }()

	// grpcx.Dial is lazy (grpc.NewClient — no connect, no error if payment is
	// down), so the worker always holds a payment client and the saga's payment
	// activities never deref a nil client.
	paymentConn, err := grpcx.Dial(cfg.PaymentGRPCAddr)
	if err != nil {
		logger.Fatal("Failed to dial payment gRPC", zap.String("addr", cfg.PaymentGRPCAddr), zap.Error(err))
	}
	defer func() { _ = paymentConn.Close() }()

	// Inventory is dialed unconditionally (same lazy semantics): the worker
	// must be able to execute v1-branch inventory activities regardless of the
	// upcoming stock-participant flag (ADR-030) — in-flight
	// inventory-participant histories keep draining on the inventory path even
	// after a flag revert.
	inventoryConn, err := grpcx.Dial(cfg.InventoryGRPCAddr)
	if err != nil {
		logger.Fatal("Failed to dial inventory gRPC", zap.String("addr", cfg.InventoryGRPCAddr), zap.Error(err))
	}
	defer func() { _ = inventoryConn.Close() }()

	cartClient := v1.NewCartClient(cfg.CartServiceURL)

	acts := &saga.Activities{
		Shipping:     shippingv1.NewShippingServiceClient(shippingConn),
		Notification: notificationv1.NewNotificationServiceClient(notifyConn),
		Payment:      paymentv1.NewPaymentServiceClient(paymentConn),
		Inventory:    inventoryv1.NewInventoryServiceClient(inventoryConn),
		Orders:       orderRepo,
		Projection:   orderRepo,
		ClearCartFn:  cartClient.ClearCart,
		CommitPause:  cfg.FaultCommitPause,
	}

	// Worker Deployment Versioning (RFC-0021 P3, homelab ADR-030). Off unless the
	// manifests set TEMPORAL_WORKER_DEPLOYMENT_NAME + TEMPORAL_WORKER_BUILD_ID, so
	// merging this changes nothing at runtime. Once on, the server pins each
	// workflow to the version that started it and in-flight sagas keep running
	// the build they began on — which is what lets the stock-write migration ship
	// without version markers in the workflow.
	//
	// NOTE for whoever flips it: setting the env only makes this worker POLL as
	// versioned. The deployment's current version must be set server-side in the
	// same operation, or new workflows target unversioned workers and stall
	// silently. See temporalx's package docs and RUNBOOK-007.
	w := temporalx.NewWorker(tc, cfg.Temporal.TaskQueue, temporalx.MustVersioningFromEnv())
	registerWorkflows(w, acts)

	// The worker has no HTTP server of its own, but it still runs under
	// Kubernetes liveness/readiness probes (and the local-stack healthcheck),
	// which hit /health and /ready on cfg.Service.Port. Serve them here so the
	// worker can report healthy; /ready flips to OK once w.Run is polling.
	ready := &atomic.Bool{}
	healthSrv := startWorkerHealthServer(cfg.Service.Port, logger, ready)
	defer func() { _ = healthSrv.Close() }()

	logger.Info("Starting Temporal worker",
		zap.String("hostport", cfg.Temporal.HostPort),
		zap.String("namespace", cfg.Temporal.Namespace),
		zap.String("task_queue", cfg.Temporal.TaskQueue),
	)
	stopDispatcher := startOutboxDispatcher(cfg, logger, orderRepo, startRequests, tc)
	defer stopDispatcher()

	stopCancelDispatcher := startCancellationDispatcher(cfg, logger, orderRepo, cancelStore, tc)
	defer stopCancelDispatcher()

	stopReconciler := startInventoryReconciler(cfg, logger, startRequests, acts.Inventory, tc)
	defer stopReconciler()

	ready.Store(true)
	if err := w.Run(worker.InterruptCh()); err != nil {
		logger.Fatal("Temporal worker stopped with error", zap.Error(err))
	}
	return true
}

// startOutboxDispatcher starts the fulfillment start-outbox dispatcher
// (RFC-0021 P3) and returns its stop function.
//
// It runs in the WORKER rather than the API on purpose: the API scales with
// traffic and would run N dispatchers competing for the same rows, while the
// worker is the process that already owns saga execution. Claiming is
// lease-based with SKIP LOCKED, so extra instances stay correct — just
// unnecessary.
//
// It stamps the participant the same way the API does. That used to come from
// ORDER_STOCK_PARTICIPANT; since RFC-0021 P4 removed the product branch there is
// one servable participant left, so the flag is gone and the value is a constant.
// Nothing about an ALREADY started saga is re-read here either way — the record on
// the order is what pins its branch.
func startOutboxDispatcher(cfg *config.Config, logger *zap.Logger,
	orderRepo *repository.PostgresOrderRepository,
	startRequests *repository.PostgresStartRequestRepository,
	tc client.Client) func() {
	if !cfg.StartDispatchersEnabled {
		// Warn, not Info: with this off, an order whose inline start failed has
		// nothing to recover it on THIS process. That is correct on a draining
		// build (the Current one sweeps the same table) and wrong anywhere else,
		// so it must be visible in the log either way.
		logger.Warn("Fulfillment outbox dispatcher is DISABLED by ORDER_START_DISPATCHERS_ENABLED; this build starts no sagas")
		return func() {}
	}

	ctx, cancel := context.WithCancel(context.Background())
	dispatcher := fulfillment.NewDispatcher(startRequests, orderRepo, tc, tc,
		cfg.Temporal.TaskQueue, logger)
	go dispatcher.Run(ctx)

	return cancel
}

// startInventoryReconciler starts the RFC-0021 P3 reconciler and returns its
// stop function.
//
// It repairs the outcomes the saga cannot: a confirmed order whose CommitInventory
// gave up, a run terminated between the pivot and the commit, a compensation that
// exhausted its retries. It lives in the worker because it is the same class of
// background work as the dispatcher, and it reaches inventory through the client
// the activities already hold — no second connection, no cross-database read.
//
// Unlike the dispatcher it needs nothing from Temporal to make progress on
// bookkeeping, but it does describe the workflow before touching stock, so a
// Temporal outage makes it defer rather than repair. The backlog gauge is
// registered separately in main (both processes), so switching this off or losing
// the worker does not hide the number.
//
// MULTI-REPLICA: unlike the dispatcher there is no lease, so two worker replicas
// would both scan the same rows. That is SAFE but noisy — inventory serializes
// transitions on the reservation row and short-circuits on the current status, so
// the second repair is a no-op RPC; the cost is a duplicated repair counter
// increment per order. The worker runs a single replica (the same posture as
// payment's), so this does not arise today. If it ever scales out, the scan needs
// the same FOR UPDATE SKIP LOCKED claim the dispatcher uses.
//
// The returned stop function is a TWO-PHASE drain, and the phases are not
// cosmetic. Phase one asks Run to stop starting passes and waits for the current
// one; phase two cancels. Skipping straight to cancel would abort an in-flight
// Commit/Release, which surfaces as an Error log and a
// repairs_total{action="failed"} increment on EVERY worker restart — poisoning the
// signal a deploy should leave untouched.
// registerTableGauges wires every table-backed gauge in BOTH processes,
// deliberately: they read their tables on each collection cycle, so two
// reporters is the same number observed twice — and the dispatcher/
// reconciler live in the worker, which exits when Temporal is unreachable.
// If the gauges only lived there, the situations where backlogs build
// (Temporal down while the API keeps committing orders or cancels) would be
// exactly the situations with no signal. Registered for the lifetime of the
// process, so the Registrations are deliberately dropped; failures are
// logged, never fatal — the process is more useful blind than dead.
func registerTableGauges(startRequests *repository.PostgresStartRequestRepository,
	cancellations *repository.PostgresCancellationRepository, logger *zap.Logger) {
	if _, err := fulfillment.RegisterOutboxGauges(startRequests); err != nil {
		logger.Error("Failed to register start-outbox gauges; the outbox runs unobserved", zap.Error(err))
	}
	if _, err := cancellation.RegisterOutboxGauges(cancellations, logger); err != nil {
		logger.Error("Failed to register cancellation-outbox gauges", zap.Error(err))
	}
	registerReconcileGauges(startRequests, logger)
}

// registerReconcileGauges wires the table-backed backlog gauges (reconciler
// backlog, manual_review, stuck-cancelling). Failures are logged, never
// fatal: the process is more useful blind than dead.
func registerReconcileGauges(store domain.ReconcileStore, logger *zap.Logger) {
	if _, err := reconcile.RegisterBacklogGauge(store, logger); err != nil {
		logger.Error("Failed to register the reconciler backlog gauge; inconsistencies would be invisible", zap.Error(err))
	}
	if _, err := reconcile.RegisterOrderStateGauges(store, logger); err != nil {
		logger.Error("Failed to register the order-state backlog gauges; parked orders would be invisible", zap.Error(err))
	}
}

// registerWorkflows pins both workflows and the activity set on the worker.
//
// Pinned explicitly rather than relying on temporalx's default: this saga
// holds money and stock, so a workflow must never be moved onto a new build
// mid-flight. RegisterWorkflow is exactly RegisterWorkflowWithOptions with
// zero options in the SDK, so the workflow TYPE NAMES are unchanged and
// existing histories still resolve. The cancellation workflow (RFC-0021 P5)
// rides the same task queue and the same pinned deployment — a NEW workflow
// type on a pinned deployment is safe by construction: no prior histories
// exist for it, and its first execution pins to whatever version is Current
// when it starts.
func registerWorkflows(w worker.Worker, acts *saga.Activities) {
	w.RegisterWorkflowWithOptions(saga.OrderFulfillmentWorkflow, workflow.RegisterOptions{
		VersioningBehavior: workflow.VersioningBehaviorPinned,
	})
	w.RegisterWorkflowWithOptions(saga.CancellationWorkflow, workflow.RegisterOptions{
		VersioningBehavior: workflow.VersioningBehaviorPinned,
	})
	w.RegisterActivity(acts)
}

// startCancellationDispatcher runs the cancellation outbox's sweeper —
// worker-side for the same reason as the fulfillment dispatcher: one
// replica, no HTTP traffic to compete with.
func startCancellationDispatcher(cfg *config.Config, logger *zap.Logger,
	orderRepo *repository.PostgresOrderRepository,
	cancelStore *repository.PostgresCancellationRepository, tc client.Client) func() {
	if !cfg.StartDispatchersEnabled {
		logger.Warn("Cancellation outbox dispatcher is DISABLED by ORDER_START_DISPATCHERS_ENABLED; this build starts no cancellation episodes")
		return func() {}
	}

	ctx, stop := context.WithCancel(context.Background())
	go cancellation.NewDispatcher(cancelStore, orderRepo, tc, cfg.Temporal.TaskQueue, logger).Run(ctx)
	return stop
}

func startInventoryReconciler(cfg *config.Config, logger *zap.Logger, store domain.ReconcileStore,
	inventory inventoryv1.InventoryServiceClient, workflows reconcile.Describer) func() {
	if !cfg.ReconcilerEnabled {
		// Logged at Warn, not Info: running without it means stranded stock is
		// nobody's job. The backlog gauge keeps reporting regardless, which is what
		// makes this knob passive rather than a way to lose sight of the problem.
		logger.Warn("Inventory reconciler is DISABLED by ORDER_RECONCILER_ENABLED; stranded reservations will not be repaired")
		return func() {}
	}

	ctx, cancel := context.WithCancel(context.Background())
	r := reconcile.New(store, inventory, workflows, logger)

	done := make(chan struct{})
	go func() { defer close(done); r.Run(ctx) }()

	return func() {
		r.Stop() // phase one: no new passes
		select {
		case <-done:
			cancel()
			return
		case <-time.After(reconcilerDrainGrace):
			logger.Warn("Inventory reconciler did not finish its pass within the drain grace; cancelling it")
		}
		cancel() // phase two: abort whatever is in flight
		select {
		case <-done:
		case <-time.After(reconcilerStopTimeout):
			logger.Warn("Inventory reconciler did not stop after cancellation; leaving it")
		}
	}
}

// reconcilerDrainGrace is how long shutdown waits for an in-flight pass to finish
// on its own. Kept well under a pod's termination grace period: a pass that needs
// longer is one working through a backlog, and its unsettled rows are picked up by
// the next process — losing that is cheaper than being SIGKILLed mid-RPC.
const reconcilerDrainGrace = 5 * time.Second

// reconcilerStopTimeout bounds the wait AFTER cancellation. Cancellation
// propagates into the in-flight RPC's context, so Run normally returns in
// milliseconds; this is the backstop for a store call that ignores it.
const reconcilerStopTimeout = 5 * time.Second

// startWorkerHealthServer serves /health and /ready for the worker process
// (which otherwise has no HTTP listener) so probes have an endpoint to hit.
// It listens on the same port as the serve path. Runs in a goroutine.
func startWorkerHealthServer(port string, logger *zap.Logger, ready *atomic.Bool) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("/ready", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !ready.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"status":"starting"}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("worker health server failed", zap.Error(err))
		}
	}()
	return srv
}

// temporalDialAttempts/temporalDialBackoff bound the startup dial retry. A
// single eager dial loses the bring-up race (Temporal reported healthy moments
// after we start, or briefly unreachable under bring-up load) and would leave
// the serve path degraded for the process's whole lifetime; ~20s of linear
// backoff rides that out without hiding a genuinely down Temporal.
const (
	temporalDialAttempts = 5
	temporalDialBackoff  = 2 * time.Second
	// temporalRedialInterval paces the lazy background redial after the
	// startup budget is spent.
	temporalRedialInterval = 15 * time.Second
)

// dialTemporalRetry dials Temporal, retrying a transient startup failure with
// linear backoff (backoff, 2*backoff, …) between attempts. It returns the last
// dial error once the attempt budget is spent.
func dialTemporalRetry(cfg *config.Config, logger *zap.Logger, attempts int, backoff time.Duration) (client.Client, error) {
	var lastErr error
	for i := 1; i <= attempts; i++ {
		tc, err := temporalx.Dial(temporalx.Config{HostPort: cfg.Temporal.HostPort, Namespace: cfg.Temporal.Namespace})
		if err == nil {
			return tc, nil
		}
		lastErr = err
		if i < attempts {
			logger.Warn("Temporal dial failed; retrying",
				zap.Int("attempt", i), zap.Int("attempts", attempts),
				zap.String("hostport", cfg.Temporal.HostPort), zap.Error(err))
			time.Sleep(time.Duration(i) * backoff)
		}
	}
	return nil, lastErr
}

// configureTemporalClient dials Temporal for the serve path (with the startup
// retry budget above). A startup failure is NOT fatal and no longer
// permanent either: it hands back a lazy client whose background loop keeps
// dialing until Temporal appears, so an order pod that raced Temporal at
// bring-up heals itself instead of answering Unavailable until someone
// restarts it. The returned cleanup stops the loop and closes the client.
func configureTemporalClient(cfg *config.Config, logger *zap.Logger) (*fulfillment.Lazy, func()) {
	dial := func() (client.Client, error) {
		return temporalx.Dial(temporalx.Config{HostPort: cfg.Temporal.HostPort, Namespace: cfg.Temporal.Namespace})
	}
	tc, err := dialTemporalRetry(cfg, logger, temporalDialAttempts, temporalDialBackoff)
	if err != nil {
		logger.Warn("Temporal unavailable at startup; background redial engaged — orders create as pending, sagas start once connected",
			zap.String("hostport", cfg.Temporal.HostPort),
			zap.Duration("redial_interval", temporalRedialInterval), zap.Error(err))
		lz := fulfillment.NewLazy(dial, temporalRedialInterval, logger)
		return lz, lz.Close
	}
	logger.Info("Temporal client initialized",
		zap.String("hostport", cfg.Temporal.HostPort),
		zap.String("namespace", cfg.Temporal.Namespace),
	)
	lz := fulfillment.NewLazySeeded(tc, logger)
	return lz, lz.Close
}

// configureShippingClient wires the order→shipping gRPC client and returns it
// alongside a cleanup that closes the connection. order→shipping is gRPC-only;
// ok=false if the dial fails (caller should abort startup).
func configureShippingClient(cfg *config.Config, logger *zap.Logger) (*v1.ShippingGRPCClient, func(), bool) {
	conn, err := grpcx.Dial(cfg.ShippingGRPCAddr)
	if err != nil {
		logger.Error("Failed to dial shipping gRPC", zap.String("addr", cfg.ShippingGRPCAddr), zap.Error(err))
		return nil, nil, false
	}
	client := v1.NewShippingGRPCClient(conn)
	logger.Info("Shipping client: gRPC", zap.String("addr", cfg.ShippingGRPCAddr))
	return client, func() { _ = conn.Close() }, true
}

// initObservability is the RFC-0014 single OTel wiring point — traces per
// TRACING_ENABLED, OTLP metrics (the only pipeline since the P3 cutover;
// OTEL_METRICS_ENABLED defaults on, =false is a kill switch), logs behind
// OTEL_LOGS_ENABLED. The config is built once so the tracer
// scope name and the startup log reflect the values obsx actually uses. The
// returned handle shuts down the whole OTel SDK (nil when setup failed); the
// returned logger tees into the OTLP log pipeline and must replace the
// caller's logger (unchanged when setup failed).
func initObservability(logger *zap.Logger) (interface{ Shutdown(context.Context) error }, *zap.Logger) {
	otelCfg := obsx.ConfigFromEnv()
	middleware.SetServiceName(otelCfg.ServiceName)
	obs, err := obsx.SetupObservability(context.Background(), otelCfg)
	if err != nil {
		logger.Warn("Failed to initialize OpenTelemetry", zap.Error(err))
		return nil, logger
	}
	// RFC-0014 P4: tee application logs into the OTLP pipeline. ZapCore
	// returns a NopCore when OTEL_LOGS_ENABLED is off, so the tee is
	// unconditional; the min level mirrors the stdout core so debug
	// lines never leave the pod on an info-level service.
	minLevel, err := zapcore.ParseLevel(os.Getenv("LOG_LEVEL"))
	if err != nil {
		minLevel = zapcore.InfoLevel
	}
	logger = logger.WithOptions(zap.WrapCore(func(c zapcore.Core) zapcore.Core {
		return zapcore.NewTee(c, obs.ZapCore(otelCfg.ServiceName, minLevel))
	}))
	logger.Info("OpenTelemetry initialized",
		zap.Bool("traces", obs.TracerProvider != nil),
		zap.Bool("otlp_metrics", obs.MeterProvider != nil),
		zap.Bool("otlp_logs", obs.LoggerProvider != nil),
		zap.String("endpoint", otelCfg.Endpoint),
		zap.Float64("sample_rate", otelCfg.SampleRate),
	)
	return obs, logger
}

// initProfiling starts Pyroscope continuous profiling via the shared obsx helper
// and returns a cleanup func (a no-op when profiling is disabled or setup fails).
// It runs on both the serve and worker paths, so the returned stop is deferred in
// main rather than in the serve-only graceful shutdown.
func initProfiling(cfg *config.Config, logger *zap.Logger) func() {
	if !cfg.Profiling.Enabled {
		logger.Info("Profiling disabled (PROFILING_ENABLED=false)")
		return func() { /* profiling disabled: nothing to stop */ }
	}
	stopProfiling, err := obsx.SetupProfiling()
	if err != nil {
		logger.Warn("Failed to initialize profiling", zap.Error(err))
		return func() { /* setup failed: nothing to stop */ }
	}
	logger.Info("Profiling initialized", zap.String("endpoint", cfg.Profiling.Endpoint))
	return func() {
		if err := stopProfiling(context.Background()); err != nil {
			logger.Error("Profiling shutdown error", zap.Error(err))
		}
	}
}

func setupServer(cfg *config.Config, logger *zap.Logger, verifier *authmw.Verifier, staffVerifier *authmw.Verifier, orderHandler *v1.OrderHandler, isShuttingDown *atomic.Bool) *http.Server {
	r := gin.Default()

	r.Use(middleware.TracingMiddleware())
	r.Use(middleware.LoggingMiddleware(logger))

	r.GET("/health", func(c *gin.Context) {
		c.JSON(200, gin.H{"status": "ok"})
	})
	r.GET("/ready", func(c *gin.Context) {
		if isShuttingDown.Load() {
			c.JSON(http.StatusServiceUnavailable, gin.H{"status": "shutting_down"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	// Order v1 routes — all private (JWT required). Variant A edge naming.
	// Protected: the Backoffice's cross-customer reads (RFC-0023), staff-realm
	// verified + role-gated; the private customer group below is untouched.
	v1.RegisterProtectedRoutes(r, orderHandler, staffVerifier)

	privateOrders := r.Group("/order/v1/private")
	privateOrders.Use(authmw.MiddlewareJWT(verifier))
	{
		privateOrders.GET("/orders", orderHandler.ListOrders)
		privateOrders.GET("/orders/:id", orderHandler.GetOrder)
		privateOrders.GET("/orders/:id/details", orderHandler.GetOrderDetails)
		privateOrders.POST("/orders/:id/cancel", orderHandler.CancelOrder)
	}

	return &http.Server{
		Addr:              ":" + cfg.Service.Port,
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
	}
}

func runGracefulShutdown(
	cfg *config.Config,
	srv *http.Server,
	grpcSrv *grpc.Server,
	tp interface{ Shutdown(context.Context) error },
	pool interface{ Close() },
	logger *zap.Logger,
	isShuttingDown *atomic.Bool,
) {
	go func() {
		logger.Info("Starting order service", zap.String("port", cfg.Service.Port))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("Failed to start server", zap.Error(err))
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	<-ctx.Done()
	logger.Info("Shutdown signal received")

	isShuttingDown.Store(true)
	drainDelay := cfg.GetReadinessDrainDelayDuration()
	if drainDelay > 0 {
		logger.Info("Readiness drain delay started", zap.Duration("delay", drainDelay))
		time.Sleep(drainDelay)
	}

	shutdownTimeout := cfg.GetShutdownTimeoutDuration()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	logger.Info("Shutting down server...", zap.Duration("timeout", shutdownTimeout))

	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("HTTP server shutdown error", zap.Error(err))
	} else {
		logger.Info("HTTP server shutdown complete")
	}

	if grpcSrv != nil {
		grpcSrv.GracefulStop()
		logger.Info("gRPC server shutdown complete")
	}

	pool.Close()
	logger.Info("Database pool closed")

	// Shutdown the OTel SDK — flushes pending spans plus any OTLP
	// metrics/logs providers built behind the RFC-0014 flags.
	if tp != nil {
		if err := tp.Shutdown(shutdownCtx); err != nil {
			logger.Error("OpenTelemetry shutdown error", zap.Error(err))
		} else {
			logger.Info("OpenTelemetry shutdown complete")
		}
	}

	logger.Info("Graceful shutdown complete")
}
