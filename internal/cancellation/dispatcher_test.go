package cancellation

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
	"go.uber.org/zap"

	"github.com/duynhlab/order-service/internal/core/domain"
)

// alreadyStartedErr is the concrete service error the Start seam maps to
// ErrAlreadyStarted.
func alreadyStartedErr() error {
	return serviceerror.NewWorkflowExecutionAlreadyStarted("exists", "", "")
}

type fakeOutbox struct {
	due        []domain.CancellationRequest
	dispatched []struct {
		id    string
		epoch int64
	}
	rescheduled []struct {
		id    string
		epoch int64
	}
	failed []struct {
		id    string
		epoch int64
	}
}

func (f *fakeOutbox) ArmWithTx(context.Context, domain.Transaction, string, int64) error { return nil }
func (f *fakeOutbox) ClaimDue(context.Context, int, time.Duration) ([]domain.CancellationRequest, error) {
	due := f.due
	f.due = nil
	return due, nil
}
func (f *fakeOutbox) MarkDispatched(_ context.Context, id string, epoch int64) error {
	f.dispatched = append(f.dispatched, struct {
		id    string
		epoch int64
	}{id, epoch})
	return nil
}
func (f *fakeOutbox) Reschedule(_ context.Context, id string, epoch int64, _ time.Time, _ string) error {
	f.rescheduled = append(f.rescheduled, struct {
		id    string
		epoch int64
	}{id, epoch})
	return nil
}
func (f *fakeOutbox) MarkFailed(_ context.Context, id string, epoch int64, _ string) error {
	f.failed = append(f.failed, struct {
		id    string
		epoch int64
	}{id, epoch})
	return nil
}
func (f *fakeOutbox) Stats(context.Context) (domain.CancellationRequestStats, error) {
	return domain.CancellationRequestStats{}, nil
}

type fakeLoader struct{ err error }

func (f *fakeLoader) LoadForFulfillment(_ context.Context, orderID string) (*domain.Order, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &domain.Order{ID: orderID, UserID: "7", Total: 2500}, nil
}

type fakeStarter struct {
	err   error
	calls []client.StartWorkflowOptions
}

func (f *fakeStarter) ExecuteWorkflow(_ context.Context, opts client.StartWorkflowOptions,
	_ interface{}, _ ...interface{}) (client.WorkflowRun, error) {
	f.calls = append(f.calls, opts)
	return nil, f.err
}

func newTestDispatcher(outbox *fakeOutbox, orders *fakeLoader, starter *fakeStarter) *Dispatcher {
	d := NewDispatcher(outbox, orders, starter, "order-fulfillment", zap.NewNop())
	d.timeNow = func() time.Time { return time.Unix(0, 0) }
	return d
}

func req(id string, epoch int64, attempts int) domain.CancellationRequest {
	return domain.CancellationRequest{OrderID: id, Epoch: epoch, Attempts: attempts}
}

func TestDispatcher_StartsAndClosesWithTheEpoch(t *testing.T) {
	outbox := &fakeOutbox{due: []domain.CancellationRequest{req("42", 5, 1)}}
	starter := &fakeStarter{}
	d := newTestDispatcher(outbox, &fakeLoader{}, starter)

	if err := d.Sweep(context.Background()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(starter.calls) != 1 || starter.calls[0].ID != "order-cancellation-42-v5" {
		t.Fatalf("starts = %+v, want the episode's workflow id", starter.calls)
	}
	if len(outbox.dispatched) != 1 || outbox.dispatched[0].epoch != 5 {
		t.Fatalf("dispatched = %+v, want the claimed epoch", outbox.dispatched)
	}
}

func TestDispatcher_AlreadyStartedClosesTheRow(t *testing.T) {
	outbox := &fakeOutbox{due: []domain.CancellationRequest{req("42", 5, 1)}}
	// The seam wraps the service error; the dispatcher must treat it as
	// "the workflow exists".
	starter := &fakeStarter{err: errors.New("wrapped")}
	d := newTestDispatcher(outbox, &fakeLoader{}, starter)
	// Simulate the Start seam's classification by injecting the sentinel
	// through a starter error the seam maps — easiest is calling dispatch
	// directly with a starter that yields ErrAlreadyStarted via Start().
	// Start() only maps *serviceerror.WorkflowExecutionAlreadyStarted, so
	// use that concrete type.
	starter.err = alreadyStartedErr()

	if err := d.Sweep(context.Background()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(outbox.dispatched) != 1 {
		t.Fatalf("dispatched = %+v, want closed on AlreadyStarted", outbox.dispatched)
	}
	if len(outbox.rescheduled) != 0 || len(outbox.failed) != 0 {
		t.Fatalf("no retry bookkeeping expected: %+v %+v", outbox.rescheduled, outbox.failed)
	}
}

func TestDispatcher_TransientStartReschedules(t *testing.T) {
	outbox := &fakeOutbox{due: []domain.CancellationRequest{req("42", 5, 3)}}
	starter := &fakeStarter{err: errors.New("temporal down")}
	d := newTestDispatcher(outbox, &fakeLoader{}, starter)

	if err := d.Sweep(context.Background()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(outbox.rescheduled) != 1 || outbox.rescheduled[0].epoch != 5 {
		t.Fatalf("rescheduled = %+v", outbox.rescheduled)
	}
	if len(outbox.failed) != 0 || len(outbox.dispatched) != 0 {
		t.Fatalf("unexpected terminal bookkeeping: %+v %+v", outbox.failed, outbox.dispatched)
	}
}

func TestDispatcher_CapFailsTheRowWithTheEpoch(t *testing.T) {
	outbox := &fakeOutbox{due: []domain.CancellationRequest{req("42", 5, DefaultMaxAttempts)}}
	starter := &fakeStarter{err: errors.New("still down")}
	d := newTestDispatcher(outbox, &fakeLoader{}, starter)

	if err := d.Sweep(context.Background()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(outbox.failed) != 1 || outbox.failed[0].epoch != 5 {
		t.Fatalf("failed = %+v, want the claimed epoch", outbox.failed)
	}
}

func TestDispatcher_OrderReadFailureRetries(t *testing.T) {
	outbox := &fakeOutbox{due: []domain.CancellationRequest{req("42", 5, 1)}}
	starter := &fakeStarter{}
	d := newTestDispatcher(outbox, &fakeLoader{err: errors.New("db down")}, starter)

	if err := d.Sweep(context.Background()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(starter.calls) != 0 {
		t.Fatal("must not start a workflow without the order's input")
	}
	if len(outbox.rescheduled) != 1 {
		t.Fatalf("rescheduled = %+v", outbox.rescheduled)
	}
}

// backoffFor's shape is pinned: flat 15s for the first two attempts, then
// doubling, capped at 5 minutes.
func TestBackoffFor_Shape(t *testing.T) {
	want := map[int]time.Duration{
		0:  15 * time.Second,
		1:  15 * time.Second,
		2:  30 * time.Second,
		3:  time.Minute,
		6:  5 * time.Minute, // capped (would be 8m)
		60: 5 * time.Minute,
	}
	for attempts, wantD := range want {
		if got := backoffFor(attempts); got != wantD {
			t.Errorf("backoffFor(%d) = %v, want %v", attempts, got, wantD)
		}
	}
}
