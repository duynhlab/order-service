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
	database "github.com/duynhlab/order-service/internal/core"
	"github.com/duynhlab/order-service/internal/core/repository"
	"github.com/duynhlab/order-service/internal/fulfillment"
	grpcv1 "github.com/duynhlab/order-service/internal/grpc/v1"
	logicv1 "github.com/duynhlab/order-service/internal/logic/v1"
	"github.com/duynhlab/order-service/internal/saga"
	v1 "github.com/duynhlab/order-service/internal/web/v1"
	"github.com/duynhlab/order-service/middleware"
	"github.com/duynhlab/pkg/authmw"
	"github.com/duynhlab/pkg/grpcx"
	"github.com/duynhlab/pkg/logger/zapx"
	"github.com/duynhlab/pkg/migratex"
	"github.com/duynhlab/pkg/obsx"
	notificationv1 "github.com/duynhlab/pkg/proto/notification/v1"
	orderv1 "github.com/duynhlab/pkg/proto/order/v1"
	paymentv1 "github.com/duynhlab/pkg/proto/payment/v1"
	productv1 "github.com/duynhlab/pkg/proto/product/v1"
	shippingv1 "github.com/duynhlab/pkg/proto/shipping/v1"
	"github.com/duynhlab/pkg/temporalx"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
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
	orderService := logicv1.NewOrderService(orderRepo, txManager)

	// `<binary> worker` runs the Temporal worker for the order-fulfillment saga
	// and serves no HTTP; it returns (and the deferred cleanups run) on shutdown.
	if maybeRunWorker(cfg, logger, orderRepo) {
		return
	}

	// Local RS256 JWT verification (cached JWKS) is the only credential — no
	// gRPC fallback. NewVerifier does not block on an unreachable JWKS — it
	// refreshes in the background, so a verifier is safe to build at startup.
	verifier, err := authmw.NewVerifier(cfg.JWKSURL, cfg.JWTIssuer, cfg.JWTAudience)
	if err != nil {
		logger.Error("JWKS verifier init failed", zap.Error(err))
		return
	}

	shippingClient, shippingCleanup, ok := configureShippingClient(cfg, logger)
	if !ok {
		return
	}
	defer shippingCleanup()

	cartClient := v1.NewCartClient(cfg.CartServiceURL)

	// Temporal client starts the order-fulfillment saga from CreateOrder. If
	// Temporal is unavailable the handler still creates orders (left "pending");
	// the saga (notification + cart-clear + fulfillment) just isn't kicked off.
	temporalClient, temporalCleanup := configureTemporalClient(cfg, logger)
	defer temporalCleanup()

	// Payment gRPC client for the order-details enrichment. Dialed lazily; the
	// enrichment soft-fails, so an unreachable payment service only omits the
	// field. Kept as a nil interface (not a typed-nil) on dial failure so the
	// aggregation's nil check works (typed-nil footgun).
	var paymentFetch v1.PaymentFetcher
	paymentConn, perr := grpcx.Dial(cfg.PaymentGRPCAddr)
	if perr != nil {
		logger.Error("Failed to dial payment gRPC (enrichment unavailable)", zap.String("addr", cfg.PaymentGRPCAddr), zap.Error(perr))
	} else {
		defer func() { _ = paymentConn.Close() }()
		paymentFetch = v1.NewPaymentGRPCClient(paymentConn)
	}

	orderHandler := v1.NewOrderHandler(orderService, cartClient, shippingClient, temporalClient, cfg.Temporal.TaskQueue, paymentFetch)

	grpcSrv := startGRPC(cfg, logger, orderService, temporalClient)

	var isShuttingDown atomic.Bool
	srv := setupServer(cfg, logger, verifier, orderHandler, &isShuttingDown)
	runGracefulShutdown(cfg, srv, grpcSrv, tp, pool, logger, &isShuttingDown)
}

// startGRPC starts the internal gRPC server on cfg.GRPC.Port, serving
// order.v1.OrderService (checkout's confirm handoff — RFC-0015 P2) alongside
// the HTTP listener (dual-port, pattern shipping). gRPC is the official
// east-west transport, so it always runs; it returns nil only if the listener
// can't bind. temporalClient may be nil (Temporal down at startup) — the
// adapter then answers Unavailable on the saga kickoff so the caller retries.
func startGRPC(cfg *config.Config, logger *zap.Logger, svc *logicv1.OrderService, temporalClient client.Client) *grpc.Server {
	lc := net.ListenConfig{}
	lis, err := lc.Listen(context.Background(), "tcp", ":"+cfg.GRPC.Port)
	if err != nil {
		logger.Error("Failed to listen for gRPC", zap.String("port", cfg.GRPC.Port), zap.Error(err))
		return nil
	}

	// A nil client.Client must reach the adapter as a nil Starter interface,
	// not a typed non-nil one, so its nil check keeps working.
	var starter fulfillment.Starter
	if temporalClient != nil {
		starter = temporalClient
	}

	grpcSrv, _ := grpcx.NewServer(logger)
	orderv1.RegisterOrderServiceServer(grpcSrv, grpcv1.NewServer(svc, starter, cfg.Temporal.TaskQueue))

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
// or a downstream being unreachable at startup is fatal (the worker can do
// nothing without them) — distinct from the serve path, which degrades.
func maybeRunWorker(cfg *config.Config, logger *zap.Logger, orderRepo *repository.PostgresOrderRepository) bool {
	if len(os.Args) <= 1 || os.Args[1] != "worker" {
		return false
	}

	tc, err := temporalx.Dial(temporalx.Config{HostPort: cfg.Temporal.HostPort, Namespace: cfg.Temporal.Namespace})
	if err != nil {
		logger.Fatal("Failed to connect to Temporal", zap.String("hostport", cfg.Temporal.HostPort), zap.Error(err))
	}
	defer tc.Close()

	productConn, err := grpcx.Dial(cfg.ProductGRPCAddr)
	if err != nil {
		logger.Fatal("Failed to dial product gRPC", zap.String("addr", cfg.ProductGRPCAddr), zap.Error(err))
	}
	defer func() { _ = productConn.Close() }()

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

	cartClient := v1.NewCartClient(cfg.CartServiceURL)

	acts := &saga.Activities{
		Product:      productv1.NewProductServiceClient(productConn),
		Shipping:     shippingv1.NewShippingServiceClient(shippingConn),
		Notification: notificationv1.NewNotificationServiceClient(notifyConn),
		Payment:      paymentv1.NewPaymentServiceClient(paymentConn),
		Orders:       orderRepo,
		ClearCartFn:  cartClient.ClearCart,
	}

	w := temporalx.NewWorker(tc, cfg.Temporal.TaskQueue)
	w.RegisterWorkflow(saga.OrderFulfillmentWorkflow)
	w.RegisterActivity(acts)

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
	ready.Store(true)
	if err := w.Run(worker.InterruptCh()); err != nil {
		logger.Fatal("Temporal worker stopped with error", zap.Error(err))
	}
	return true
}

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

// configureTemporalClient dials Temporal for the serve path. A failure is NOT
// fatal: it returns a nil client so the handler still creates orders (left
// "pending") and just doesn't start the saga. The returned cleanup closes the
// client (a no-op when nil).
func configureTemporalClient(cfg *config.Config, logger *zap.Logger) (client.Client, func()) {
	tc, err := temporalx.Dial(temporalx.Config{HostPort: cfg.Temporal.HostPort, Namespace: cfg.Temporal.Namespace})
	if err != nil {
		logger.Warn("Temporal unavailable; orders will be created but the fulfillment saga won't start",
			zap.String("hostport", cfg.Temporal.HostPort), zap.Error(err))
		return nil, func() {}
	}
	logger.Info("Temporal client initialized",
		zap.String("hostport", cfg.Temporal.HostPort),
		zap.String("namespace", cfg.Temporal.Namespace),
	)
	return tc, func() { tc.Close() }
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

func setupServer(cfg *config.Config, logger *zap.Logger, verifier *authmw.Verifier, orderHandler *v1.OrderHandler, isShuttingDown *atomic.Bool) *http.Server {
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
	privateOrders := r.Group("/order/v1/private")
	privateOrders.Use(authmw.MiddlewareJWT(verifier))
	{
		privateOrders.GET("/orders", orderHandler.ListOrders)
		privateOrders.GET("/orders/:id", orderHandler.GetOrder)
		privateOrders.GET("/orders/:id/details", orderHandler.GetOrderDetails)
		privateOrders.POST("/orders", orderHandler.CreateOrder)
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
