package fulfillment

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	workflowpb "go.temporal.io/api/workflow/v1"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"
	"go.uber.org/zap"

	"github.com/duynhlab/order-service/internal/core/domain"
	"github.com/duynhlab/order-service/internal/saga"
)

// fakeOutbox is written by the dispatcher goroutine and read by the test, so
// every field is guarded. The Run tests found this the hard way: -race flagged
// the unguarded slice immediately.
type fakeOutbox struct {
	mu              sync.Mutex
	claim           []domain.FulfillmentStartRequest
	claimErr        error
	markFailedErr   error
	dispatched      []string
	failed          map[string]string
	rescheduled     map[string]time.Time
	rescheduleCodes map[string]string
}

func newFakeOutbox(reqs ...domain.FulfillmentStartRequest) *fakeOutbox {
	return &fakeOutbox{
		claim:           reqs,
		failed:          map[string]string{},
		rescheduled:     map[string]time.Time{},
		rescheduleCodes: map[string]string{},
	}
}

func (f *fakeOutbox) EnqueueWithTx(context.Context, domain.Transaction, string, string, string) error {
	return nil
}

func (f *fakeOutbox) MarkDispatched(_ context.Context, orderID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dispatched = append(f.dispatched, orderID)
	return nil
}

func (f *fakeOutbox) ClaimDue(context.Context, int, time.Duration) ([]domain.FulfillmentStartRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.claimErr != nil {
		return nil, f.claimErr
	}
	out := f.claim
	f.claim = nil // one sweep only, like a real lease
	return out, nil
}

func (f *fakeOutbox) Reschedule(_ context.Context, orderID string, next time.Time, code string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rescheduled[orderID] = next
	f.rescheduleCodes[orderID] = code
	return nil
}

func (f *fakeOutbox) MarkFailed(_ context.Context, orderID, code string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.markFailedErr != nil {
		return f.markFailedErr
	}
	f.failed[orderID] = code
	return nil
}

// Accessors — the tests read through these so nothing touches a guarded field
// directly.
func (f *fakeOutbox) dispatchedIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.dispatched...)
}

func (f *fakeOutbox) failedCode(orderID string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	code, ok := f.failed[orderID]
	return code, ok
}

func (f *fakeOutbox) failedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.failed)
}

func (f *fakeOutbox) rescheduledAt(orderID string) (time.Time, string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	at, ok := f.rescheduled[orderID]
	return at, f.rescheduleCodes[orderID], ok
}

func (f *fakeOutbox) rescheduledCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.rescheduled)
}

func (f *fakeOutbox) Stats(context.Context) (domain.StartRequestStats, error) {
	return domain.StartRequestStats{}, nil
}

// fakeDescriber answers the "what is the existing run doing?" question the
// dispatcher must ask before closing a row on a duplicate collision.
type fakeDescriber struct {
	status enumspb.WorkflowExecutionStatus
	err    error
	calls  int
}

func (f *fakeDescriber) DescribeWorkflowExecution(context.Context, string, string) (*workflowservice.DescribeWorkflowExecutionResponse, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return &workflowservice.DescribeWorkflowExecutionResponse{
		WorkflowExecutionInfo: &workflowpb.WorkflowExecutionInfo{Status: f.status},
	}, nil
}

type fakeLoader struct {
	order *domain.Order
	err   error
}

func (f *fakeLoader) LoadForFulfillment(context.Context, string) (*domain.Order, error) {
	return f.order, f.err
}

type recordingStarter struct {
	calls int
	opts  client.StartWorkflowOptions
	args  []any
	err   error
}

func (r *recordingStarter) ExecuteWorkflow(_ context.Context, opts client.StartWorkflowOptions, _ any, args ...any) (client.WorkflowRun, error) {
	r.calls++
	r.opts = opts
	r.args = args
	return nil, r.err
}

// startedParticipant reports the participant the started workflow was given.
func (r *recordingStarter) startedParticipant(t *testing.T) saga.Participant {
	t.Helper()
	if len(r.args) == 0 {
		t.Fatal("no workflow input recorded")
	}
	in, ok := r.args[0].(saga.OrderFulfillmentInput)
	if !ok {
		t.Fatalf("workflow arg is %T, want saga.OrderFulfillmentInput", r.args[0])
	}
	return in.StockParticipant
}

func pendingOrder() *domain.Order {
	return &domain.Order{
		ID: "42", UserID: "7", Total: 1300, Status: "pending",
		Items: []domain.OrderItem{{ProductID: "1", Quantity: 2}},
	}
}

func newDispatcher(t *testing.T, outbox *fakeOutbox, loader *fakeLoader, starter Starter) *Dispatcher {
	t.Helper()
	return newDispatcherWith(t, outbox, loader, starter,
		&fakeDescriber{status: enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING})
}

func newDispatcherWith(t *testing.T, outbox *fakeOutbox, loader *fakeLoader, starter Starter, describer Describer) *Dispatcher {
	t.Helper()
	d := NewDispatcher(outbox, loader, starter, describer, "order-fulfillment", zap.NewNop())
	d.now = func() time.Time { return testNow }
	return d
}

// testNow is the dispatcher's clock in tests; rows are created just before it so
// the age guard does not fire unless a test wants it to.
var testNow = time.Unix(1_700_000_000, 0).UTC()

// req is a row as the API stamps one today: inventory is the only participant a
// new order can be created with since RFC-0021 P4.
func req(attempts int) domain.FulfillmentStartRequest {
	return domain.FulfillmentStartRequest{OrderID: "42", Status: domain.StartRequestPending,
		PaymentMethod: "tok_visa_ok", Participant: string(saga.ParticipantInventory),
		Attempts: attempts, CreatedAt: testNow.Add(-time.Minute)}
}

// The recovery path: a row the inline start left behind gets started, and the
// row is closed so it never comes back.
func TestDispatcher_StartsAndClosesTheRow(t *testing.T) {
	outbox := newFakeOutbox(req(1))
	starter := &recordingStarter{}
	d := newDispatcher(t, outbox, &fakeLoader{order: pendingOrder()}, starter)

	if err := d.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep() = %v", err)
	}

	if starter.calls != 1 {
		t.Fatalf("ExecuteWorkflow called %d times, want 1", starter.calls)
	}
	if got := outbox.dispatchedIDs(); len(got) != 1 || got[0] != "42" {
		t.Errorf("dispatched = %v, want [42]", got)
	}
}

// RejectDuplicate is what stops a row whose saga already ran AND CLOSED from
// starting a second one — a second authorize and a second capture.
func TestDispatcher_RejectsDuplicateStarts(t *testing.T) {
	outbox := newFakeOutbox(req(1))
	starter := &recordingStarter{}
	d := newDispatcher(t, outbox, &fakeLoader{order: pendingOrder()}, starter)

	if err := d.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep() = %v", err)
	}

	const want = 3 // enumspb.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE
	if int(starter.opts.WorkflowIDReusePolicy) != want {
		t.Errorf("reuse policy = %v, want REJECT_DUPLICATE — the default would let a completed saga run twice",
			starter.opts.WorkflowIDReusePolicy)
	}
}

// Temporal answering AlreadyStarted means the inline path won the race. That is
// a success, not an error: the row must close rather than retry forever.
func TestDispatcher_AlreadyStartedClosesTheRow(t *testing.T) {
	outbox := newFakeOutbox(req(1))
	starter := &recordingStarter{err: &serviceerror.WorkflowExecutionAlreadyStarted{}}
	d := newDispatcherWith(t, outbox, &fakeLoader{order: pendingOrder()}, starter,
		&fakeDescriber{status: enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING})

	if err := d.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep() = %v", err)
	}

	if got := outbox.dispatchedIDs(); len(got) != 1 {
		t.Errorf("dispatched = %v, want the row closed on AlreadyStarted", got)
	}
	if outbox.rescheduledCount() != 0 {
		t.Error("row was rescheduled on AlreadyStarted, want none")
	}
}

// An order that moved past pending had its saga run already; the row is stale.
func TestDispatcher_SkipsOrdersThatAreNoLongerPending(t *testing.T) {
	outbox := newFakeOutbox(req(1))
	starter := &recordingStarter{}
	order := pendingOrder()
	order.Status = "confirmed"
	d := newDispatcher(t, outbox, &fakeLoader{order: order}, starter)

	if err := d.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep() = %v", err)
	}

	if starter.calls != 0 {
		t.Errorf("ExecuteWorkflow called %d times for a confirmed order, want 0", starter.calls)
	}
	if got := outbox.dispatchedIDs(); len(got) != 1 {
		t.Errorf("dispatched = %v, want the stale row closed", got)
	}
}

// A transient failure reschedules with a BOUNDED code and the exponential delay.
func TestDispatcher_TransientFailureReschedules(t *testing.T) {
	outbox := newFakeOutbox(req(2))
	starter := &recordingStarter{err: &serviceerror.Unavailable{Message: "frontend down"}}
	d := newDispatcher(t, outbox, &fakeLoader{order: pendingOrder()}, starter)

	if err := d.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep() = %v", err)
	}

	next, code, ok := outbox.rescheduledAt("42")
	if !ok {
		t.Fatal("row was not rescheduled")
	}
	if want := d.now().Add(10 * time.Second); !next.Equal(want) {
		t.Errorf("next attempt = %v, want %v (5s << (2-1))", next, want)
	}
	if code != codeUnavailable {
		t.Errorf("code = %q, want UNAVAILABLE — never an error message", code)
	}
	if outbox.failedCount() != 0 {
		t.Error("row was marked failed below the attempt cap, want none")
	}
}

// At the cap the dispatcher stops on its own. A row that retries forever hides a
// real problem behind a log line nobody reads.
func TestDispatcher_GivesUpAtTheAttemptCap(t *testing.T) {
	outbox := newFakeOutbox(req(DefaultMaxAttempts))
	starter := &recordingStarter{err: &serviceerror.Unavailable{}}
	d := newDispatcher(t, outbox, &fakeLoader{order: pendingOrder()}, starter)

	if err := d.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep() = %v", err)
	}

	if got, ok := outbox.failedCode("42"); !ok || got != codeUnavailable {
		t.Errorf("failed code = %q (present=%v), want UNAVAILABLE", got, ok)
	}
	if outbox.rescheduledCount() != 0 {
		t.Error("row was rescheduled at the attempt cap, want none")
	}
}

// A missing order cannot be recovered by retrying, and the FK makes it
// unreachable in practice — so close the row instead of sweeping it forever.
func TestDispatcher_MissingOrderIsTerminal(t *testing.T) {
	outbox := newFakeOutbox(req(1))
	d := newDispatcher(t, outbox, &fakeLoader{err: domain.ErrNotFound}, &recordingStarter{})

	if err := d.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep() = %v", err)
	}

	if got, _ := outbox.failedCode("42"); got != codeOrderNotFound {
		t.Errorf("failed code = %q, want ORDER_NOT_FOUND", got)
	}
}

// A failing claim must not kill the loop: Sweep returns the error and Run logs
// it, because stopping the only thing that recovers stranded orders is worse.
func TestDispatcher_ClaimErrorIsReturned(t *testing.T) {
	outbox := newFakeOutbox()
	outbox.claimErr = errors.New("db down")
	d := newDispatcher(t, outbox, &fakeLoader{order: pendingOrder()}, &recordingStarter{})

	if err := d.Sweep(context.Background()); err == nil {
		t.Fatal("Sweep() = nil, want the claim error surfaced")
	}
}

func TestBackoffFor(t *testing.T) {
	tests := []struct {
		attempts int
		want     time.Duration
	}{
		{0, baseBackoff}, // defensive: never less than the base
		{1, 5 * time.Second},
		{2, 10 * time.Second},
		{3, 20 * time.Second},
		{7, 320 * time.Second},
		{8, maxBackoff}, // 640s would exceed the cap
		{20, maxBackoff},
		{64, maxBackoff}, // must not shift past the word size
	}
	for _, tt := range tests {
		if got := backoffFor(tt.attempts); got != tt.want {
			t.Errorf("backoffFor(%d) = %v, want %v", tt.attempts, got, tt.want)
		}
	}
}

func TestStartErrCode_IsBounded(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ""},
		{"unavailable", &serviceerror.Unavailable{Message: "down"}, codeUnavailable},
		{"internal", &serviceerror.Internal{Message: "boom"}, codeInternal},
		{"namespace", &serviceerror.NamespaceNotFound{}, codeNamespaceNotFound},
		{"deadline", context.DeadlineExceeded, codeDeadlineExceeded},
		{"canceled", context.Canceled, codeCanceled},
		// The point of the fallback: an unrecognised error must NOT leak its
		// message into a column that groups causes.
		{"unknown", errors.New("some upstream prose with details"), codeUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := startErrCode(tt.err)
			if got != tt.want {
				t.Errorf("startErrCode(%v) = %q, want %q", tt.err, got, tt.want)
			}
			if len(got) > errCodeBoundForTest {
				t.Errorf("code %q is longer than the column allows", got)
			}
		})
	}
}

// errCodeBoundForTest mirrors the repository's column width, so a future code
// that would be silently truncated in the database fails here first.
const errCodeBoundForTest = 64

// Run must sweep on its tick and stop when its context is cancelled — the loop
// is what turns the outbox from a table into a recovery mechanism, and a loop
// that ignores cancellation keeps a worker from shutting down.
func TestDispatcher_RunSweepsThenStopsOnCancel(t *testing.T) {
	outbox := newFakeOutbox(req(1))
	starter := &recordingStarter{}
	d := newDispatcher(t, outbox, &fakeLoader{order: pendingOrder()}, starter)
	d.pollInterval = time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		d.Run(ctx)
		close(done)
	}()

	// Wait for the sweep to do its work rather than sleeping a fixed time.
	deadline := time.After(2 * time.Second)
	for len(outbox.dispatchedIDs()) == 0 {
		select {
		case <-deadline:
			t.Fatal("Run did not sweep within 2s")
		default:
			time.Sleep(time.Millisecond)
		}
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}
}

// A sweep that fails must not stop the loop: the alternative is that a blip in
// the database permanently disables the only thing that recovers stranded
// orders.
func TestDispatcher_RunSurvivesASweepError(t *testing.T) {
	outbox := newFakeOutbox()
	outbox.claimErr = errors.New("db down")
	d := newDispatcher(t, outbox, &fakeLoader{order: pendingOrder()}, &recordingStarter{})
	d.pollInterval = time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() {
		d.Run(ctx)
		close(done)
	}()

	select {
	case <-done: // returned on context expiry, not on the sweep error
	case <-time.After(2 * time.Second):
		t.Fatal("Run exited on a sweep error instead of continuing until cancellation")
	}
}

// The gauges are read from the table, so registration only has to succeed — but
// it must not be silently skipped, which is what an unchecked error would do.
func TestRegisterOutboxGauges(t *testing.T) {
	if _, err := RegisterOutboxGauges(newFakeOutbox()); err != nil {
		t.Fatalf("RegisterOutboxGauges() = %v, want nil", err)
	}
}

// If MarkFailed does not persist, the row is still PENDING and WILL be
// reclaimed — so the result must say "retry", not "failed". Reporting "failed"
// would tell the dashboard the dispatcher stopped when it has not, and a
// persistently failing MarkFailed would show a climbing failed count while the
// row retried forever.
func TestDispatcher_ReportsRetryWhenGivingUpDoesNotPersist(t *testing.T) {
	outbox := newFakeOutbox(req(DefaultMaxAttempts))
	outbox.markFailedErr = errors.New("db down")
	starter := &recordingStarter{err: &serviceerror.Unavailable{}}
	d := newDispatcher(t, outbox, &fakeLoader{order: pendingOrder()}, starter)

	if got := d.dispatch(context.Background(), req(DefaultMaxAttempts)); got != ResultRetry {
		t.Errorf("result = %q, want %q — the row is still pending", got, ResultRetry)
	}
	_ = starter
}

// The lease has to cover a whole sequential batch, not one row: a sweep leases
// every row at once and dispatches them one by one.
func TestDefaultLeaseCoversAFullBatch(t *testing.T) {
	// Worst case per row is a load plus a start bounded by the seam's timeout.
	worstCase := time.Duration(DefaultBatchSize) * startTimeout
	if DefaultLease <= worstCase {
		t.Fatalf("DefaultLease = %v, but a full batch can take %v — the tail of a batch would be reclaimed mid-flight",
			DefaultLease, worstCase)
	}
}

// A duplicate collision only means a run EXISTS. If that run was terminated,
// timed out or failed, nothing is driving the order — closing the row would
// delete the only record that it still needs a saga, and the dashboard would
// read pending == 0.
func TestDispatcher_AbandonedRunIsNotTreatedAsStarted(t *testing.T) {
	for _, status := range []enumspb.WorkflowExecutionStatus{
		enumspb.WORKFLOW_EXECUTION_STATUS_TERMINATED,
		enumspb.WORKFLOW_EXECUTION_STATUS_TIMED_OUT,
		enumspb.WORKFLOW_EXECUTION_STATUS_FAILED,
		enumspb.WORKFLOW_EXECUTION_STATUS_CANCELED,
	} {
		t.Run(status.String(), func(t *testing.T) {
			outbox := newFakeOutbox(req(1))
			starter := &recordingStarter{err: &serviceerror.WorkflowExecutionAlreadyStarted{}}
			d := newDispatcherWith(t, outbox, &fakeLoader{order: pendingOrder()}, starter,
				&fakeDescriber{status: status})

			if err := d.Sweep(context.Background()); err != nil {
				t.Fatalf("Sweep() = %v", err)
			}

			if got := outbox.dispatchedIDs(); len(got) != 0 {
				t.Errorf("dispatched = %v for an abandoned run; the order would be stranded silently", got)
			}
			if code, ok := outbox.failedCode("42"); !ok || code != codeAbandonedRun {
				t.Errorf("failed code = %q (present=%v), want %q", code, ok, codeAbandonedRun)
			}
		})
	}
}

// If the run cannot be described, the row must stay: closing it on a guess is
// how an order gets stranded.
func TestDispatcher_DescribeFailureKeepsTheRow(t *testing.T) {
	outbox := newFakeOutbox(req(1))
	starter := &recordingStarter{err: &serviceerror.WorkflowExecutionAlreadyStarted{}}
	d := newDispatcherWith(t, outbox, &fakeLoader{order: pendingOrder()}, starter,
		&fakeDescriber{err: errors.New("frontend down")})

	if err := d.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep() = %v", err)
	}

	if got := outbox.dispatchedIDs(); len(got) != 0 {
		t.Errorf("dispatched = %v without verifying the run", got)
	}
	if _, code, ok := outbox.rescheduledAt("42"); !ok || code != codeDescribeFailed {
		t.Errorf("reschedule code = %q (present=%v), want %q", code, ok, codeDescribeFailed)
	}
}

// A cleared token must never be dispatched: the saga would fall back to the DEMO
// payment token and charge something that is not the customer's instrument.
func TestDispatcher_RefusesARowWhoseTokenWasCleared(t *testing.T) {
	r := req(1)
	r.PaymentMethod = ""
	r.PaymentMethodCleared = true
	outbox := newFakeOutbox(r)
	starter := &recordingStarter{}
	d := newDispatcher(t, outbox, &fakeLoader{order: pendingOrder()}, starter)

	if err := d.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep() = %v", err)
	}

	if starter.calls != 0 {
		t.Errorf("ExecuteWorkflow called %d times with a cleared token, want 0", starter.calls)
	}
	if code, ok := outbox.failedCode("42"); !ok || code != codeTokenCleared {
		t.Errorf("failed code = %q (present=%v), want %q", code, ok, codeTokenCleared)
	}
}

// A REST order legitimately has no token and must still dispatch — the guard is
// about a token that was CLEARED, not one that never existed.
func TestDispatcher_DispatchesARowThatNeverHadAToken(t *testing.T) {
	r := req(1)
	r.PaymentMethod = ""
	r.PaymentMethodCleared = false
	outbox := newFakeOutbox(r)
	starter := &recordingStarter{}
	d := newDispatcher(t, outbox, &fakeLoader{order: pendingOrder()}, starter)

	if err := d.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep() = %v", err)
	}

	if starter.calls != 1 {
		t.Errorf("ExecuteWorkflow called %d times, want 1", starter.calls)
	}
}

// Past the dedup window REJECT_DUPLICATE has nothing left to reject, so starting
// could duplicate a saga that already ran — a second authorize and capture.
func TestDispatcher_RefusesARowOlderThanTheDedupWindow(t *testing.T) {
	r := req(1)
	r.CreatedAt = testNow.Add(-DefaultMaxRowAge - time.Hour)
	outbox := newFakeOutbox(r)
	starter := &recordingStarter{}
	d := newDispatcher(t, outbox, &fakeLoader{order: pendingOrder()}, starter)

	if err := d.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep() = %v", err)
	}

	if starter.calls != 0 {
		t.Errorf("ExecuteWorkflow called %d times for a stale row, want 0", starter.calls)
	}
	if code, ok := outbox.failedCode("42"); !ok || code != codeTooOld {
		t.Errorf("failed code = %q (present=%v), want %q", code, ok, codeTooOld)
	}
}

// The lease has to cover a batch of BOUNDED dispatches; the bound is what makes
// the arithmetic meaningful.
func TestDefaultLeaseIsDerivedFromTheRowBudget(t *testing.T) {
	worstCase := time.Duration(DefaultBatchSize) * DefaultPerRowBudget
	if DefaultLease <= worstCase {
		t.Fatalf("DefaultLease = %v but a full batch is bounded at %v — the tail would be reclaimed mid-flight",
			DefaultLease, worstCase)
	}
	if DefaultPerRowBudget <= startTimeout {
		t.Fatalf("DefaultPerRowBudget = %v must exceed startTimeout %v, or the database calls have no budget",
			DefaultPerRowBudget, startTimeout)
	}
}

// CONTINUED_AS_NEW and PAUSED are LIVE sagas. Treating them as abandoned would
// fail an order whose fulfillment is still progressing — the mistake the
// exhaustive switch exists to prevent.
func TestDispatcher_LiveRunStatusesCloseTheRow(t *testing.T) {
	for _, status := range []enumspb.WorkflowExecutionStatus{
		enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING,
		enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED,
		enumspb.WORKFLOW_EXECUTION_STATUS_CONTINUED_AS_NEW,
		enumspb.WORKFLOW_EXECUTION_STATUS_PAUSED,
	} {
		t.Run(status.String(), func(t *testing.T) {
			outbox := newFakeOutbox(req(1))
			starter := &recordingStarter{err: &serviceerror.WorkflowExecutionAlreadyStarted{}}
			d := newDispatcherWith(t, outbox, &fakeLoader{order: pendingOrder()}, starter,
				&fakeDescriber{status: status})

			if err := d.Sweep(context.Background()); err != nil {
				t.Fatalf("Sweep() = %v", err)
			}

			if got := outbox.dispatchedIDs(); len(got) != 1 {
				t.Errorf("dispatched = %v for a live run, want the row closed", got)
			}
			if outbox.failedCount() != 0 {
				t.Error("a live run was marked failed; its order would be failed while its saga runs")
			}
		})
	}
}

func TestDispatcher_StartsWithTheParticipantTheRowRecorded(t *testing.T) {
	row := req(1)
	row.Participant = string(saga.ParticipantInventory)
	outbox := newFakeOutbox(row)
	starter := &recordingStarter{}
	// The dispatcher has no flag of its own to fall back to — by construction it
	// can only forward what the row recorded.
	d := newDispatcher(t, outbox, &fakeLoader{order: pendingOrder()}, starter)

	if err := d.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep() = %v", err)
	}
	if got := starter.startedParticipant(t); got != saga.ParticipantInventory {
		t.Errorf("started participant = %q, want %q", got, saga.ParticipantInventory)
	}
}

// A row this build cannot serve, with NO saga behind it, must not be started and
// must not spend the attempt cap pretending the answer might change. It goes
// terminal at once with a code that names the cause — PARTICIPANT_UNSERVABLE
// rather than the generic give-up an operator cannot tell from a Temporal outage.
func TestDispatcher_RefusesAnUnservableParticipantWithNoRun(t *testing.T) {
	for _, recorded := range []string{"product", "", "warehouse-9"} {
		t.Run("row="+recorded, func(t *testing.T) {
			row := req(1)
			row.Participant = recorded
			outbox := newFakeOutbox(row)
			starter := &recordingStarter{}
			d := newDispatcherWith(t, outbox, &fakeLoader{order: pendingOrder()}, starter,
				&fakeDescriber{err: serviceerror.NewNotFound("no such workflow")})

			if err := d.Sweep(context.Background()); err != nil {
				t.Fatalf("Sweep() = %v", err)
			}
			if starter.calls != 0 {
				t.Errorf("ExecuteWorkflow called %d times, want 0", starter.calls)
			}
			code, ok := outbox.failedCode("42")
			if !ok || code != codeUnservable {
				t.Errorf("failed code = %q (present=%v), want %q", code, ok, codeUnservable)
			}
		})
	}
}

// An unservable row whose run status CANNOT be established keeps the row. The
// refusal is only safe once Temporal has said there is no execution: condemning
// on a describe failure would fail orders during exactly the outage that made the
// describe fail, and those are the orders most likely to have a live saga.
func TestDispatcher_UnservableParticipantKeepsTheRowWhenTheRunIsUnknowable(t *testing.T) {
	for _, tc := range []struct {
		name      string
		describer Describer
	}{
		{"describe fails", &fakeDescriber{err: errors.New("temporal down")}},
		{"no describer configured", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := req(1)
			row.Participant = "product"
			outbox := newFakeOutbox(row)
			d := newDispatcherWith(t, outbox, &fakeLoader{order: pendingOrder()},
				&recordingStarter{}, tc.describer)

			if err := d.Sweep(context.Background()); err != nil {
				t.Fatalf("Sweep() = %v", err)
			}
			if code, ok := outbox.failedCode("42"); ok {
				t.Errorf("row was failed with %q; an unknowable run must not condemn it", code)
			}
			if _, code, ok := outbox.rescheduledAt("42"); !ok || code != codeDescribeFailed {
				t.Errorf("reschedule code = %q (present=%v), want %q", code, ok, codeDescribeFailed)
			}
		})
	}
}

// A refusal that could not be PERSISTED is reported as a retry, not as a failure.
// The row is still PENDING and will be reclaimed, so calling it failed would tell
// the dashboard the dispatcher stopped when it has not.
func TestDispatcher_UnservableRefusalThatCannotPersistIsARetry(t *testing.T) {
	row := req(1)
	row.Participant = "product"
	outbox := newFakeOutbox(row)
	outbox.markFailedErr = errors.New("db down")
	d := newDispatcherWith(t, outbox, &fakeLoader{order: pendingOrder()},
		&recordingStarter{}, &fakeDescriber{err: serviceerror.NewNotFound("no such workflow")})

	if err := d.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep() = %v", err)
	}
	if _, ok := outbox.failedCode("42"); ok {
		t.Error("the row reads as FAILED even though the write did not land")
	}
}

// The same row with a LIVE saga behind it must be honoured, not condemned. A
// pending row does not mean no saga exists: the inline start marks the row on a
// best-effort call that is allowed to fail, so this is the ordinary
// crash-recovery shape — and for an unservable participant it is the shape pinned
// worker versioning is meant to produce, the saga still running on the build that
// started it.
//
// Failing it here would report a healthy order as broken on a CRITICAL alert
// whose runbook mitigation is a raw UPDATE to `failed` — on an order whose saga
// is still going to capture.
func TestDispatcher_HonoursALiveRunForAnUnservableParticipant(t *testing.T) {
	row := req(1)
	row.Participant = "product"
	outbox := newFakeOutbox(row)
	starter := &recordingStarter{}
	d := newDispatcherWith(t, outbox, &fakeLoader{order: pendingOrder()}, starter,
		&fakeDescriber{status: enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING})

	if err := d.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep() = %v", err)
	}
	if starter.calls != 0 {
		t.Errorf("ExecuteWorkflow called %d times, want 0 — this build cannot start that branch", starter.calls)
	}
	if code, ok := outbox.failedCode("42"); ok {
		t.Errorf("row was failed with %q; a live saga must close the row, not condemn it", code)
	}
	if ids := outbox.dispatchedIDs(); len(ids) != 1 || ids[0] != "42" {
		t.Errorf("dispatched = %v; a live saga means the row is closed, not left owed", ids)
	}
}
