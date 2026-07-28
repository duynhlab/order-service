package fulfillment

import (
	"context"
	"errors"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.uber.org/zap"

	"github.com/duynhlab/order-service/internal/core/domain"
	"github.com/duynhlab/order-service/internal/saga"
)

// Dispatcher retries fulfillment starts the inline path failed to complete
// (RFC-0021 P3).
//
// It is the second half of the outbox: CreateOrder writes a PENDING row in the
// order's transaction, the inline start closes it on success, and whatever is
// left is an order that committed while Temporal was unreachable — or a process
// that died between the commit and the start. Without this loop those orders sit
// `pending` forever with nothing driving them.
//
// It runs in the WORKER, not the API: the API scales with traffic and would run
// N dispatchers competing for the same rows, while the worker is the process
// that already owns saga execution. Claiming is lease-based and uses SKIP
// LOCKED, so more than one instance is still correct — just unnecessary.
type Dispatcher struct {
	outbox      domain.StartRequestRepository
	orders      domain.OrderLoader
	starter     Starter
	taskQueue   string
	participant saga.Participant
	log         *zap.Logger

	pollInterval time.Duration
	batchSize    int
	lease        time.Duration
	maxAttempts  int

	// now is injectable so the backoff schedule can be asserted without
	// sleeping. Production passes nil and gets time.Now.
	now func() time.Time
}

// Dispatcher defaults.
const (
	// DefaultPollInterval is how often the outbox is swept. Seconds, not
	// milliseconds: on a healthy platform there is nothing to find, and the
	// inline start already covers the latency-sensitive path.
	DefaultPollInterval = 5 * time.Second

	// DefaultBatchSize caps one sweep. Small on purpose — a Temporal outage that
	// stranded thousands of orders should recover steadily rather than open
	// thousands of concurrent starts the moment it heals.
	DefaultBatchSize = 10

	// DefaultLease must comfortably exceed one dispatch attempt (a load + a
	// workflow start), because it is what stops the next sweep from re-claiming
	// a row that is still being worked.
	DefaultLease = time.Minute

	// DefaultMaxAttempts is where automatic retry stops and a human takes over.
	// At the backoff below that is roughly two hours of trying.
	DefaultMaxAttempts = 20

	baseBackoff = 5 * time.Second
	maxBackoff  = 10 * time.Minute
)

// Bounded last_error_code tokens. A closed vocabulary on purpose: the column
// groups causes and the values reach a database, so they must never be derived
// from an error message.
const (
	codeUnavailable       = "UNAVAILABLE"
	codeDeadlineExceeded  = "DEADLINE_EXCEEDED"
	codeCanceled          = "CANCELED"
	codeInternal          = "INTERNAL"
	codeResourceExhausted = "RESOURCE_EXHAUSTED"
	codeNamespaceNotFound = "NAMESPACE_NOT_FOUND"
	codeInvalidArgument   = "INVALID_ARGUMENT"
	codeOrderNotFound     = "ORDER_NOT_FOUND"
	codeOrderLoadFailed   = "ORDER_LOAD_FAILED"
	codeUnknown           = "UNKNOWN"
)

// Dispatch results — bounded metric labels.
const (
	ResultDispatched     = "dispatched"      // the start succeeded here
	ResultAlreadyStarted = "already_started" // the inline path had started it after all
	ResultRetry          = "retry"           // transient; scheduled for another attempt
	ResultFailed         = "failed"          // attempt cap reached, now a human's problem
	ResultSkipped        = "skipped"         // the order no longer needs a saga
)

// NewDispatcher builds a dispatcher with the package defaults.
func NewDispatcher(outbox domain.StartRequestRepository, orders domain.OrderLoader, starter Starter,
	taskQueue string, participant saga.Participant, log *zap.Logger) *Dispatcher {
	return &Dispatcher{
		outbox:       outbox,
		orders:       orders,
		starter:      starter,
		taskQueue:    taskQueue,
		participant:  participant,
		log:          log,
		pollInterval: DefaultPollInterval,
		batchSize:    DefaultBatchSize,
		lease:        DefaultLease,
		maxAttempts:  DefaultMaxAttempts,
	}
}

// Run sweeps until ctx is cancelled. It never returns an error: a failing sweep
// is logged and retried on the next tick, because the alternative — stopping the
// only thing that recovers stranded orders — is strictly worse than a noisy log.
func (d *Dispatcher) Run(ctx context.Context) {
	ticker := time.NewTicker(d.pollInterval)
	defer ticker.Stop()

	d.log.Info("fulfillment start dispatcher running",
		zap.Duration("poll_interval", d.pollInterval), zap.Int("batch_size", d.batchSize))

	for {
		select {
		case <-ctx.Done():
			d.log.Info("fulfillment start dispatcher stopped")
			return
		case <-ticker.C:
			if err := d.Sweep(ctx); err != nil && !errors.Is(err, context.Canceled) {
				d.log.Error("outbox sweep failed", zap.Error(err))
			}
		}
	}
}

// Sweep claims one batch of due rows and dispatches each. Exported so a test —
// and the e2e — can drive exactly one pass deterministically.
func (d *Dispatcher) Sweep(ctx context.Context) error {
	claimed, err := d.outbox.ClaimDue(ctx, d.batchSize, d.lease)
	if err != nil {
		return err
	}
	for _, req := range claimed {
		result := d.dispatch(ctx, req)
		recordStartDispatch(ctx, result)
	}
	return nil
}

// dispatch handles one claimed row and returns its bounded result label.
func (d *Dispatcher) dispatch(ctx context.Context, req domain.FulfillmentStartRequest) string {
	order, err := d.orders.LoadForFulfillment(ctx, req.OrderID)
	switch {
	case errors.Is(err, domain.ErrNotFound):
		// The FK cascades, so this should be unreachable; if it happens the row
		// is garbage and retrying it forever would be worse than closing it.
		d.log.Error("start outbox row has no order; marking failed",
			zap.String("order_id", req.OrderID))
		d.finish(ctx, req, codeOrderNotFound)
		return ResultFailed
	case err != nil:
		return d.retryOrFail(ctx, req, codeOrderLoadFailed, err)
	}

	// The saga already ran: something moved this order past pending, so the row
	// is stale rather than owed. Closing it is correct and keeps a
	// long-recovered order out of every future sweep.
	if order.Status != orderStatusPending {
		d.markStarted(ctx, req.OrderID)
		return ResultSkipped
	}

	// RejectDuplicate is load-bearing here, not a preference. The workflow id is
	// deterministic, so with the server default (AllowDuplicate) a row whose
	// saga already ran and CLOSED would start a second one — a second authorize
	// and a second capture. Rejecting duplicates turns that into
	// ErrAlreadyStarted, which is a success.
	err = Start(ctx, d.starter, d.taskQueue, order, req.PaymentMethod, Options{
		ReusePolicy:      enumspb.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE,
		StockParticipant: d.participant,
	})
	switch {
	case err == nil:
		d.markStarted(ctx, req.OrderID)
		d.log.Info("recovered a fulfillment start from the outbox",
			zap.String("order_id", req.OrderID), zap.Int("attempts", req.Attempts))
		return ResultDispatched
	case errors.Is(err, ErrAlreadyStarted):
		d.markStarted(ctx, req.OrderID)
		return ResultAlreadyStarted
	default:
		return d.retryOrFail(ctx, req, startErrCode(err), err)
	}
}

// orderStatusPending is the only status whose saga is still owed.
const orderStatusPending = "pending"

// retryOrFail schedules the next attempt, or gives up at the cap.
func (d *Dispatcher) retryOrFail(ctx context.Context, req domain.FulfillmentStartRequest, code string, cause error) string {
	if req.Attempts >= d.maxAttempts {
		d.log.Error("fulfillment start gave up after the attempt cap; requeue by hand after fixing the cause",
			zap.String("order_id", req.OrderID), zap.Int("attempts", req.Attempts),
			zap.String("code", code), zap.Error(cause))
		d.finish(ctx, req, code)
		return ResultFailed
	}

	next := d.timeNow().Add(backoffFor(req.Attempts))
	if err := d.outbox.Reschedule(ctx, req.OrderID, next, code); err != nil {
		// The lease already pushed the row out, so a failed reschedule only
		// means the next attempt comes at the lease boundary instead.
		d.log.Warn("rescheduling a start request failed; the lease will re-drive it",
			zap.String("order_id", req.OrderID), zap.Error(err))
	}
	return ResultRetry
}

func (d *Dispatcher) finish(ctx context.Context, req domain.FulfillmentStartRequest, code string) {
	if err := d.outbox.MarkFailed(ctx, req.OrderID, code); err != nil {
		d.log.Error("marking a start request failed did not stick; it will be retried",
			zap.String("order_id", req.OrderID), zap.Error(err))
	}
}

func (d *Dispatcher) markStarted(ctx context.Context, orderID string) {
	if err := d.outbox.MarkDispatched(ctx, orderID); err != nil {
		d.log.Warn("closing a start request failed; the next sweep gets AlreadyStarted",
			zap.String("order_id", orderID), zap.Error(err))
	}
}

func (d *Dispatcher) timeNow() time.Time {
	if d.now != nil {
		return d.now()
	}
	return time.Now()
}

// backoffFor returns the delay before the next attempt: exponential from
// baseBackoff, capped at maxBackoff. attempts is the count AFTER the claim that
// just failed, so the first failure waits baseBackoff.
func backoffFor(attempts int) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	// Shifting past 63 is undefined; the cap is reached long before that, so
	// clamp the exponent rather than relying on the multiplication.
	if attempts > 32 {
		return maxBackoff
	}
	d := baseBackoff << (attempts - 1)
	if d > maxBackoff || d <= 0 {
		return maxBackoff
	}
	return d
}

// startErrCode maps a start failure to a BOUNDED token for last_error_code and
// for the log. Never the error message: the column groups causes, and a message
// would put upstream prose into the database.
func startErrCode(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.DeadlineExceeded):
		return codeDeadlineExceeded
	case errors.Is(err, context.Canceled):
		return codeCanceled
	}

	var unavailable *serviceerror.Unavailable
	var deadline *serviceerror.DeadlineExceeded
	var internal *serviceerror.Internal
	var resourceExhausted *serviceerror.ResourceExhausted
	var namespaceNotFound *serviceerror.NamespaceNotFound
	var invalidArgument *serviceerror.InvalidArgument
	switch {
	case errors.As(err, &unavailable):
		return codeUnavailable
	case errors.As(err, &deadline):
		return codeDeadlineExceeded
	case errors.As(err, &internal):
		return codeInternal
	case errors.As(err, &resourceExhausted):
		return codeResourceExhausted
	case errors.As(err, &namespaceNotFound):
		return codeNamespaceNotFound
	case errors.As(err, &invalidArgument):
		return codeInvalidArgument
	default:
		return codeUnknown
	}
}
