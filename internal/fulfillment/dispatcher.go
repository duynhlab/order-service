package fulfillment

import (
	"context"
	"errors"
	"time"

	"go.temporal.io/api/workflowservice/v1"

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
// Describer reports the state of an existing workflow. Separate from Starter
// because only the dispatcher needs it: when a start is refused as a duplicate,
// what the existing run is DOING decides whether the outbox row may be closed.
// *client.Client satisfies it.
type Describer interface {
	DescribeWorkflowExecution(ctx context.Context, workflowID, runID string) (*workflowservice.DescribeWorkflowExecutionResponse, error)
}

type Dispatcher struct {
	outbox      domain.StartRequestRepository
	orders      domain.OrderLoader
	starter     Starter
	describer   Describer
	taskQueue   string
	participant saga.Participant
	log         *zap.Logger

	pollInterval time.Duration
	batchSize    int
	lease        time.Duration
	maxAttempts  int
	// perRowBudget bounds every database call and the start for one row, so the
	// lease arithmetic below is founded on something rather than assumed.
	perRowBudget time.Duration
	// maxRowAge refuses rows older than the workflow-id dedup window.
	maxRowAge time.Duration

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

	// DefaultPerRowBudget bounds one row's whole dispatch — every database call
	// plus the workflow start. Without it nothing bounds the sweep: only Start
	// had a deadline, so a stalled Postgres (a failover, a lock) blocked the
	// single sweep goroutine indefinitely and no lease value could be justified.
	DefaultPerRowBudget = 15 * time.Second

	// DefaultLease covers the whole CLAIMED BATCH, not one row: a sweep leases
	// every row at once and dispatches them sequentially, so the last row starts
	// long after its lease began. It is derived, not guessed —
	// batchSize × DefaultPerRowBudget = 150s — with margin on top, because an
	// overrunning batch has its tail reclaimed mid-flight, which double-counts
	// attempts and walks rows toward FAILED early.
	//
	// The cost is that a dispatcher which dies holding a lease leaves those rows
	// idle for up to this long; acceptable for a path that only runs when
	// something is already wrong.
	DefaultLease = 5 * time.Minute

	// DefaultMaxRowAge refuses to dispatch a row older than the Temporal
	// namespace retention (7 days on this platform), with margin.
	//
	// This is a money guarantee, not tidiness. REJECT_DUPLICATE can only reject a
	// duplicate while the previous run still EXISTS: once it ages out of
	// retention the server has nothing left to reject, so a stale PENDING row
	// whose saga already ran (and closed abnormally without updating the order)
	// would start a brand new saga — a second authorize and a second capture.
	// The outbox deliberately stores no run id, so age is the only evidence
	// available.
	DefaultMaxRowAge = 5 * 24 * time.Hour

	// DefaultMaxAttempts is where automatic retry stops and a human takes over.
	// At the backoff below that is roughly two hours of trying.
	//
	// COUPLED to the worker's fail-fast Temporal dial, and the coupling is not
	// obvious. Attempts burn on CLAIM, and the cap does not distinguish "this row
	// is poison" from "the dependency is down" — so a dispatcher sweeping through
	// a multi-hour Temporal outage would walk EVERY stranded row to the cap and
	// mark them FAILED, i.e. the platform would give up on every order because of
	// an outage that then ended. Today that cannot happen: the worker exits when
	// Temporal is unreachable, so no sweeps occur while the dependency is down.
	//
	// If the worker is ever made to survive an unreachable Temporal (a reasonable
	// change on its own), this cap MUST first learn the difference — either by
	// not counting UNAVAILABLE/DEADLINE_EXCEEDED toward it, or by tracking
	// consecutive-failure-with-a-live-dependency separately. Changing one without
	// the other converts an outage into mass order failure.
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
	codeTokenCleared      = "TOKEN_CLEARED"
	codeTooOld            = "TOO_OLD"
	codeAbandonedRun      = "ABANDONED_RUN"
	codeDescribeFailed    = "DESCRIBE_FAILED"
)

// Dispatch results — bounded metric labels.
const (
	ResultDispatched     = "dispatched"      // the start succeeded here
	ResultAlreadyStarted = "already_started" // the inline path had started it after all
	ResultRetry          = "retry"           // transient; scheduled for another attempt
	ResultFailed         = "failed"          // attempt cap reached, now a human's problem
	ResultSkipped        = "skipped"         // the order no longer needs a saga
)

// participantFor resolves the branch this row's saga belongs on, and says so
// loudly when the row held something this build could not use — the one outcome
// that means an operator has to look at the row.
func (d *Dispatcher) participantFor(ctx context.Context, req domain.FulfillmentStartRequest) saga.Participant {
	participant, source := ParticipantFor(ctx, req.Participant, d.participant)
	if source == SourceUnrecognised {
		d.log.Error("outbox row records an unknown stock participant; falling back to this process's flag",
			zap.String("order_id", req.OrderID), zap.String("row_participant", req.Participant),
			zap.String("fallback", string(d.participant)))
	}
	return participant
}

// NewDispatcher builds a dispatcher with the package defaults.
func NewDispatcher(outbox domain.StartRequestRepository, orders domain.OrderLoader, starter Starter,
	describer Describer, taskQueue string, participant saga.Participant, log *zap.Logger) *Dispatcher {
	return &Dispatcher{
		outbox:       outbox,
		orders:       orders,
		starter:      starter,
		describer:    describer,
		taskQueue:    taskQueue,
		participant:  participant,
		log:          log,
		pollInterval: DefaultPollInterval,
		batchSize:    DefaultBatchSize,
		lease:        DefaultLease,
		maxAttempts:  DefaultMaxAttempts,
		perRowBudget: DefaultPerRowBudget,
		maxRowAge:    DefaultMaxRowAge,
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

	// Sweep before waiting for the first tick. This process just started, and the
	// most likely reason a row is waiting is the crash that restarted it — making
	// recovery wait a full poll interval for no reason.
	if err := d.Sweep(ctx); err != nil && !errors.Is(err, context.Canceled) {
		d.log.Error("initial outbox sweep failed", zap.Error(err))
	}

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
		rowCtx, cancel := context.WithTimeout(ctx, d.perRowBudget)
		result := d.dispatch(rowCtx, req)
		cancel()
		recordStartDispatch(ctx, result)
	}
	return nil
}

// dispatch handles one claimed row and returns its bounded result label.
func (d *Dispatcher) dispatch(ctx context.Context, req domain.FulfillmentStartRequest) string {
	// A row whose token was cleared must never be dispatched: the saga would fall
	// back to its DEMO payment token, so a hand-requeued FAILED row would
	// authorize and capture against something that is not the customer's
	// instrument. The operator's remedy is to fail the order and let the customer
	// retry, and this makes that the only option rather than a documented hope.
	if req.PaymentMethodCleared && req.PaymentMethod == "" {
		d.log.Error("refusing to dispatch a start whose payment token was cleared; fail the order instead of charging the demo token",
			zap.String("order_id", req.OrderID))
		if err := d.finish(ctx, req, codeTokenCleared); err != nil {
			return ResultRetry
		}
		return ResultFailed
	}

	// Past the dedup window the server has nothing left to reject, so starting
	// could duplicate a saga that already ran. See DefaultMaxRowAge.
	if age := d.timeNow().Sub(req.CreatedAt); age > d.maxRowAge {
		d.log.Error("refusing to dispatch a start older than the workflow-id dedup window; a duplicate saga could not be prevented",
			zap.String("order_id", req.OrderID), zap.Duration("age", age))
		if err := d.finish(ctx, req, codeTooOld); err != nil {
			return ResultRetry
		}
		return ResultFailed
	}

	order, err := d.orders.LoadForFulfillment(ctx, req.OrderID)
	switch {
	case errors.Is(err, domain.ErrNotFound):
		// The FK cascades, so this should be unreachable; if it happens the row
		// is garbage and retrying it forever would be worse than closing it.
		d.log.Error("start outbox row has no order; marking failed",
			zap.String("order_id", req.OrderID))
		if err := d.finish(ctx, req, codeOrderNotFound); err != nil {
			return ResultRetry
		}
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
		ReusePolicy: enumspb.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE,
		// From the ROW, not from this process's flag. The API stamped the row in
		// the order's own transaction; the dispatcher is the deferred half of that
		// same start, so it must honour the decision already recorded rather than
		// re-deciding with a copy of the flag that may have rolled out at a
		// different time. A skew either way corrupts the reconciler's product-path
		// vs lost-reserve judgement. See ParticipantFor for what an absent or
		// unusable value resolves to.
		StockParticipant: d.participantFor(ctx, req),
	})
	switch {
	case err == nil:
		d.markStarted(ctx, req.OrderID)
		d.log.Info("recovered a fulfillment start from the outbox",
			zap.String("order_id", req.OrderID), zap.Int("attempts", req.Attempts))
		return ResultDispatched
	case errors.Is(err, ErrAlreadyStarted):
		return d.reconcileExistingRun(ctx, req)
	default:
		return d.retryOrFail(ctx, req, startErrCode(err), err)
	}
}

// reconcileExistingRun decides what a duplicate-start collision means.
//
// Closing the row on ErrAlreadyStarted alone is wrong: the collision says a run
// EXISTS, not that it did its job. If that run was terminated by an operator, or
// timed out, or failed before it could mark the order, then nothing is driving
// the order and closing the row deletes the only record that it needs a saga —
// the order sits `pending` forever with pending == 0 on the dashboard.
func (d *Dispatcher) reconcileExistingRun(ctx context.Context, req domain.FulfillmentStartRequest) string {
	if d.describer == nil {
		// Cannot verify, so cannot safely close. Keep the row.
		return d.retryOrFail(ctx, req, codeDescribeFailed, errors.New("no describer configured"))
	}

	resp, err := d.describer.DescribeWorkflowExecution(ctx, saga.WorkflowID(req.OrderID), "")
	if err != nil {
		return d.retryOrFail(ctx, req, codeDescribeFailed, err)
	}

	// Every status is listed on purpose. The linter insisting on it caught two
	// real mistakes: CONTINUED_AS_NEW and PAUSED are LIVE sagas, and a default
	// branch would have marked their orders abandoned and failed them.
	switch status := resp.GetWorkflowExecutionInfo().GetStatus(); status {
	case enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING,
		enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED,
		enumspb.WORKFLOW_EXECUTION_STATUS_CONTINUED_AS_NEW,
		enumspb.WORKFLOW_EXECUTION_STATUS_PAUSED:
		// Progressing, finished, carried into a new run, or resumable. Either way
		// a saga exists for this order and nothing is owed.
		d.markStarted(ctx, req.OrderID)
		return ResultAlreadyStarted

	case enumspb.WORKFLOW_EXECUTION_STATUS_TERMINATED,
		enumspb.WORKFLOW_EXECUTION_STATUS_TIMED_OUT,
		enumspb.WORKFLOW_EXECUTION_STATUS_FAILED,
		enumspb.WORKFLOW_EXECUTION_STATUS_CANCELED:
		// The saga stopped without finishing and no retry will resume it, because
		// the workflow id is taken. A human has to decide.
		d.log.Error("a run for this order exists but did not complete; the order needs a decision",
			zap.String("order_id", req.OrderID), zap.String("run_status", status.String()))
		if err := d.finish(ctx, req, codeAbandonedRun); err != nil {
			return ResultRetry
		}
		return ResultFailed

	case enumspb.WORKFLOW_EXECUTION_STATUS_UNSPECIFIED:
		// A server or SDK this build does not understand. Failing the order on a
		// status we cannot interpret is too harsh; keep the row and retry.
		d.log.Error("unrecognised run status for an existing saga; keeping the start request",
			zap.String("order_id", req.OrderID), zap.String("run_status", status.String()))
		return d.retryOrFail(ctx, req, codeDescribeFailed, errors.New("unspecified run status"))
	}

	// Unreachable while the switch is exhaustive; the linter enforces that.
	return d.retryOrFail(ctx, req, codeDescribeFailed, errors.New("unhandled run status"))
}

// orderStatusPending is the only status whose saga is still owed.
const orderStatusPending = "pending"

// retryOrFail schedules the next attempt, or gives up at the cap.
func (d *Dispatcher) retryOrFail(ctx context.Context, req domain.FulfillmentStartRequest, code string, cause error) string {
	if req.Attempts >= d.maxAttempts {
		d.log.Error("fulfillment start gave up after the attempt cap; requeue by hand after fixing the cause",
			zap.String("order_id", req.OrderID), zap.Int("attempts", req.Attempts),
			zap.String("code", code), zap.Error(cause))
		if err := d.finish(ctx, req, code); err != nil {
			// Report what actually happened. If MarkFailed did not persist the row
			// is still PENDING and WILL be reclaimed, so calling this "failed"
			// would tell the dashboard the dispatcher stopped when it has not —
			// and a persistently failing MarkFailed would otherwise show up as a
			// climbing failed count while the row retried forever.
			return ResultRetry
		}
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

// finish makes the row terminal. It returns the error so the caller can report
// the truth rather than the intent.
func (d *Dispatcher) finish(ctx context.Context, req domain.FulfillmentStartRequest, code string) error {
	if err := d.outbox.MarkFailed(ctx, req.OrderID, code); err != nil {
		d.log.Error("marking a start request failed did not stick; the row stays pending and will be retried",
			zap.String("order_id", req.OrderID), zap.Error(err))
		return err
	}
	return nil
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
