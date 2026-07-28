package reconcile

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	workflowpb "go.temporal.io/api/workflow/v1"
	"go.temporal.io/api/workflowservice/v1"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/duynhlab/order-service/internal/core/domain"
	inventoryv1 "github.com/duynhlab/pkg/proto/inventory/v1"
)

var testReader *sdkmetric.ManualReader

func TestMain(m *testing.M) {
	testReader = sdkmetric.NewManualReader()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(testReader)))
	os.Exit(m.Run())
}

type fakeLister struct {
	candidates []domain.ReconcileCandidate
	err        error
	gotFrom    time.Time
	gotTo      time.Time
	gotLimit   int
}

func (f *fakeLister) ListForReconcile(_ context.Context, from, to time.Time, limit int) ([]domain.ReconcileCandidate, error) {
	f.gotFrom, f.gotTo, f.gotLimit = from, to, limit
	return f.candidates, f.err
}

// fakeInventory is the inventory API as the reconciler sees it: a status to
// report, and a record of the repairs issued.
type fakeInventory struct {
	inventoryv1.InventoryServiceClient // everything the reconciler does not call

	mu             sync.Mutex
	status         inventoryv1.ReservationStatus
	orderID        string
	getErr         error
	commitErr      error
	releaseErr     error
	committed      []string
	released       []string
	releaseReasons []string
}

func (f *fakeInventory) GetReservation(_ context.Context, in *inventoryv1.GetReservationRequest, _ ...grpc.CallOption) (*inventoryv1.GetReservationResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return nil, f.getErr
	}
	return &inventoryv1.GetReservationResponse{
		Reservation: &inventoryv1.Reservation{
			Id:      in.GetReservationId(),
			OrderId: f.orderIDOr(in.GetReservationId()),
			Status:  f.status,
		},
	}, nil
}

// orderIDOr defaults the owning order to the reservation id, which is the
// scheme the saga uses; a test overrides it to exercise a mismatch.
func (f *fakeInventory) orderIDOr(def string) string {
	if f.orderID != "" {
		return f.orderID
	}
	return def
}

func (f *fakeInventory) Commit(_ context.Context, in *inventoryv1.CommitRequest, _ ...grpc.CallOption) (*inventoryv1.CommitResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.commitErr != nil {
		return nil, f.commitErr
	}
	f.committed = append(f.committed, in.GetReservationId())
	return &inventoryv1.CommitResponse{}, nil
}

func (f *fakeInventory) Release(_ context.Context, in *inventoryv1.ReleaseRequest, _ ...grpc.CallOption) (*inventoryv1.ReleaseResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.releaseErr != nil {
		return nil, f.releaseErr
	}
	f.released = append(f.released, in.GetReservationId())
	f.releaseReasons = append(f.releaseReasons, in.GetReason())
	return &inventoryv1.ReleaseResponse{}, nil
}

func (f *fakeInventory) committedIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.committed...)
}

func (f *fakeInventory) releasedIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.released...)
}

func (f *fakeInventory) reasons() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.releaseReasons...)
}

// fakeWorkflows answers the only question the reconciler asks Temporal: is
// anyone still working on this order? Tests default it to a CLOSED workflow,
// because that is the state in which the order row is the final word.
type fakeWorkflows struct {
	status enumspb.WorkflowExecutionStatus
	err    error
	calls  int
}

func (f *fakeWorkflows) DescribeWorkflowExecution(context.Context, string, string) (*workflowservice.DescribeWorkflowExecutionResponse, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return &workflowservice.DescribeWorkflowExecutionResponse{
		WorkflowExecutionInfo: &workflowpb.WorkflowExecutionInfo{Status: f.status},
	}, nil
}

func closedWorkflow() *fakeWorkflows {
	return &fakeWorkflows{status: enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED}
}

var testNow = time.Unix(1_700_000_000, 0).UTC()

func newReconciler(t *testing.T, lister *fakeLister, inv *fakeInventory) *Reconciler {
	t.Helper()
	return newReconcilerWith(t, lister, inv, closedWorkflow())
}

func newReconcilerWith(t *testing.T, lister *fakeLister, inv *fakeInventory, wf Describer) *Reconciler {
	t.Helper()
	r := New(lister, inv, wf, zap.NewNop())
	r.now = func() time.Time { return testNow }
	return r
}

func candidate(status string) []domain.ReconcileCandidate {
	return []domain.ReconcileCandidate{{OrderID: "42", Status: status}}
}

// The repair the reconciler exists for: a confirmed order whose stock is still
// only RESERVED. v1 reservations never expire, so nothing else would ever
// release or commit it.
func TestReconciler_CommitsConfirmedButReserved(t *testing.T) {
	inv := &fakeInventory{status: inventoryv1.ReservationStatus_RESERVATION_STATUS_RESERVED}
	r := newReconciler(t, &fakeLister{candidates: candidate(statusConfirmed)}, inv)

	if err := r.Pass(context.Background()); err != nil {
		t.Fatalf("Pass() = %v", err)
	}

	if got := inv.committedIDs(); len(got) != 1 || got[0] != "42" {
		t.Errorf("committed = %v, want [42]", got)
	}
	if len(inv.releasedIDs()) != 0 {
		t.Errorf("released = %v for a confirmed order, want none", inv.releasedIDs())
	}
	if r.backlog.Load() != 0 {
		t.Errorf("backlog = %d after a successful repair, want 0", r.backlog.Load())
	}
}

// A failed order holding stock is the mirror case: nothing ships, so the units
// have to go back.
func TestReconciler_ReleasesFailedButReserved(t *testing.T) {
	inv := &fakeInventory{status: inventoryv1.ReservationStatus_RESERVATION_STATUS_RESERVED}
	r := newReconciler(t, &fakeLister{candidates: candidate(statusFailed)}, inv)

	if err := r.Pass(context.Background()); err != nil {
		t.Fatalf("Pass() = %v", err)
	}

	if got := inv.releasedIDs(); len(got) != 1 || got[0] != "42" {
		t.Errorf("released = %v, want [42]", got)
	}
	if got := inv.reasons(); len(got) != 1 || got[0] != reasonOrderFailed {
		t.Errorf("reason = %v, want [%s] — the movement ledger records WHY", got, reasonOrderFailed)
	}
	if len(inv.committedIDs()) != 0 {
		t.Errorf("committed = %v for a failed order, want none", inv.committedIDs())
	}
}

// NOT_FOUND is the normal answer for every product-path order. Treating it as an
// inconsistency would make the entire pre-cutover backlog look broken.
func TestReconciler_ProductPathOrdersAreSkipped(t *testing.T) {
	inv := &fakeInventory{getErr: status.Error(codes.NotFound, "no reservation")}
	r := newReconciler(t, &fakeLister{candidates: candidate(statusConfirmed)}, inv)

	if err := r.Pass(context.Background()); err != nil {
		t.Fatalf("Pass() = %v", err)
	}

	if len(inv.committedIDs()) != 0 || len(inv.releasedIDs()) != 0 {
		t.Error("a product-path order was repaired; it has no inventory reservation to repair")
	}
	if r.backlog.Load() != 0 {
		t.Errorf("backlog = %d, want 0 — a product-path order is not an inconsistency", r.backlog.Load())
	}
}

// Already-consistent pairs must be silent: no repair, no backlog. Otherwise
// every healthy order would look like work.
func TestReconciler_ConsistentPairsAreLeftAlone(t *testing.T) {
	for _, tc := range []struct {
		name        string
		orderStatus string
		reservation inventoryv1.ReservationStatus
	}{
		{"confirmed + committed", statusConfirmed, inventoryv1.ReservationStatus_RESERVATION_STATUS_COMMITTED},
		{"failed + released", statusFailed, inventoryv1.ReservationStatus_RESERVATION_STATUS_RELEASED},
		{"failed + expired", statusFailed, inventoryv1.ReservationStatus_RESERVATION_STATUS_EXPIRED},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inv := &fakeInventory{status: tc.reservation}
			r := newReconciler(t, &fakeLister{candidates: candidate(tc.orderStatus)}, inv)

			if err := r.Pass(context.Background()); err != nil {
				t.Fatalf("Pass() = %v", err)
			}
			if len(inv.committedIDs()) != 0 || len(inv.releasedIDs()) != 0 {
				t.Error("a consistent pair was repaired")
			}
			if r.backlog.Load() != 0 {
				t.Errorf("backlog = %d for a consistent pair, want 0", r.backlog.Load())
			}
		})
	}
}

// Terminal-but-wrong pairs cannot be repaired — COMMITTED and RELEASED are
// end states. They must be reported as a breach and counted in the backlog, not
// silently accepted and not pointlessly retried.
func TestReconciler_UnfixableBreachesAreReported(t *testing.T) {
	for _, tc := range []struct {
		name        string
		orderStatus string
		reservation inventoryv1.ReservationStatus
	}{
		{"failed order with committed stock", statusFailed, inventoryv1.ReservationStatus_RESERVATION_STATUS_COMMITTED},
		{"confirmed order with released stock", statusConfirmed, inventoryv1.ReservationStatus_RESERVATION_STATUS_RELEASED},
		{"unrecognised reservation status", statusConfirmed, inventoryv1.ReservationStatus_RESERVATION_STATUS_UNSPECIFIED},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inv := &fakeInventory{status: tc.reservation}
			r := newReconciler(t, &fakeLister{candidates: candidate(tc.orderStatus)}, inv)

			if err := r.Pass(context.Background()); err != nil {
				t.Fatalf("Pass() = %v", err)
			}
			if len(inv.committedIDs()) != 0 || len(inv.releasedIDs()) != 0 {
				t.Error("a terminal-state breach was 'repaired'; that transition is invalid")
			}
			if r.backlog.Load() != 1 {
				t.Errorf("backlog = %d, want 1 — an unfixable breach must be visible", r.backlog.Load())
			}
		})
	}
}

// A failed repair stays in the backlog so the next pass tries again and the
// gauge keeps showing it.
func TestReconciler_FailedRepairStaysInTheBacklog(t *testing.T) {
	inv := &fakeInventory{
		status:    inventoryv1.ReservationStatus_RESERVATION_STATUS_RESERVED,
		commitErr: errors.New("inventory unavailable"),
	}
	r := newReconciler(t, &fakeLister{candidates: candidate(statusConfirmed)}, inv)

	if err := r.Pass(context.Background()); err != nil {
		t.Fatalf("Pass() = %v", err)
	}
	if r.backlog.Load() != 1 {
		t.Errorf("backlog = %d after a failed repair, want 1", r.backlog.Load())
	}
}

// The backlog is SET each pass, not accumulated: once repaired it must return to
// zero rather than decaying or sticking.
func TestReconciler_BacklogResetsWhenRepaired(t *testing.T) {
	inv := &fakeInventory{
		status:    inventoryv1.ReservationStatus_RESERVATION_STATUS_RESERVED,
		commitErr: errors.New("inventory unavailable"),
	}
	lister := &fakeLister{candidates: candidate(statusConfirmed)}
	r := newReconciler(t, lister, inv)

	if err := r.Pass(context.Background()); err != nil {
		t.Fatalf("first Pass: %v", err)
	}
	if r.backlog.Load() != 1 {
		t.Fatalf("backlog = %d, want 1", r.backlog.Load())
	}

	inv.mu.Lock()
	inv.commitErr = nil
	inv.mu.Unlock()
	if err := r.Pass(context.Background()); err != nil {
		t.Fatalf("second Pass: %v", err)
	}
	if r.backlog.Load() != 0 {
		t.Errorf("backlog = %d after the repair succeeded, want 0", r.backlog.Load())
	}
}

// The settle delay keeps the pass away from orders that just reached a terminal
// status, so it does not race the saga's own commit and report repairs for work
// the saga was already finishing.
func TestReconciler_WindowExcludesJustSettledOrders(t *testing.T) {
	lister := &fakeLister{}
	r := newReconciler(t, lister, &fakeInventory{})

	if err := r.Pass(context.Background()); err != nil {
		t.Fatalf("Pass() = %v", err)
	}

	wantTo := testNow.Add(-DefaultSettleDelay)
	if !lister.gotTo.Equal(wantTo) {
		t.Errorf("to = %v, want %v (now minus the settle delay)", lister.gotTo, wantTo)
	}
	if want := wantTo.Add(-DefaultWindow); !lister.gotFrom.Equal(want) {
		t.Errorf("from = %v, want %v", lister.gotFrom, want)
	}
	if lister.gotLimit != DefaultBatch {
		t.Errorf("limit = %d, want %d", lister.gotLimit, DefaultBatch)
	}
}

func TestReconciler_ListErrorIsReturned(t *testing.T) {
	r := newReconciler(t, &fakeLister{err: errors.New("db down")}, &fakeInventory{})

	if err := r.Pass(context.Background()); err == nil {
		t.Fatal("Pass() = nil, want the list error surfaced so Run can log it")
	}
}

// Run must pass on its tick and stop on cancellation — a loop that ignores
// cancellation keeps the worker from shutting down.
func TestReconciler_RunPassesThenStopsOnCancel(t *testing.T) {
	inv := &fakeInventory{status: inventoryv1.ReservationStatus_RESERVATION_STATUS_RESERVED}
	r := newReconciler(t, &fakeLister{candidates: candidate(statusConfirmed)}, inv)
	r.interval = time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()

	deadline := time.After(2 * time.Second)
	for len(inv.committedIDs()) == 0 {
		select {
		case <-deadline:
			t.Fatal("Run did not pass within 2s")
		default:
			time.Sleep(time.Millisecond)
		}
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
}

func TestRegisterGauge_ReportsTheLastPassBacklog(t *testing.T) {
	inv := &fakeInventory{status: inventoryv1.ReservationStatus_RESERVATION_STATUS_COMMITTED}
	r := newReconciler(t, &fakeLister{candidates: candidate(statusFailed)}, inv)

	reg, err := r.RegisterGauge()
	if err != nil {
		t.Fatalf("RegisterGauge() = %v", err)
	}
	t.Cleanup(func() { _ = reg.Unregister() })

	if err := r.Pass(context.Background()); err != nil {
		t.Fatalf("Pass() = %v", err)
	}

	var rm metricdata.ResourceMetrics
	if err := testReader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	var got int64 = -1
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "order.reconciler.backlog" {
				continue
			}
			if g, ok := m.Data.(metricdata.Gauge[int64]); ok {
				for _, dp := range g.DataPoints {
					got = dp.Value
				}
			}
		}
	}
	if got != 1 {
		t.Errorf("backlog gauge = %d, want 1 (a failed order with committed stock)", got)
	}
}

// The release side needs the same treatment as the commit side: a failure keeps
// the order in the backlog so the next pass retries and the gauge keeps showing
// it, rather than reporting a repair that did not happen.
func TestReconciler_FailedReleaseStaysInTheBacklog(t *testing.T) {
	inv := &fakeInventory{
		status:     inventoryv1.ReservationStatus_RESERVATION_STATUS_RESERVED,
		releaseErr: errors.New("inventory unavailable"),
	}
	r := newReconciler(t, &fakeLister{candidates: candidate(statusFailed)}, inv)

	if err := r.Pass(context.Background()); err != nil {
		t.Fatalf("Pass() = %v", err)
	}
	if len(inv.releasedIDs()) != 0 {
		t.Errorf("released = %v despite the error", inv.releasedIDs())
	}
	if r.backlog.Load() != 1 {
		t.Errorf("backlog = %d after a failed release, want 1", r.backlog.Load())
	}
}

// The candidate query only returns terminal orders, so a non-terminal one
// reaching the repair means the query and this logic have drifted apart. It must
// refuse rather than guess — guessing here would either commit stock for an order
// still deciding, or release stock out from under a running saga.
func TestReconciler_NonTerminalOrderIsRefused(t *testing.T) {
	inv := &fakeInventory{status: inventoryv1.ReservationStatus_RESERVATION_STATUS_RESERVED}
	r := newReconciler(t, &fakeLister{
		candidates: []domain.ReconcileCandidate{{OrderID: "42", Status: "pending"}},
	}, inv)

	if err := r.Pass(context.Background()); err != nil {
		t.Fatalf("Pass() = %v", err)
	}

	if len(inv.committedIDs()) != 0 || len(inv.releasedIDs()) != 0 {
		t.Error("a non-terminal order was repaired; the saga may still be deciding its outcome")
	}
	if r.backlog.Load() != 1 {
		t.Errorf("backlog = %d, want 1 — the drift must be visible", r.backlog.Load())
	}
}

// The seam inventory-service explicitly delegates to this reconciler: a
// compensation that ran BEFORE its Reserve landed leaves an orphaned hold.
// Release found no row and returned success, then the Reserve created a
// reservation nothing was watching — so the order is failed and stock is held by
// a reservation no saga will ever touch again.
//
// Named separately from the ordinary failed-but-reserved case even though it
// takes the same branch, so the cross-repo requirement stays traceable from
// here.
func TestReconciler_ReleasesAnOrphanedHoldFromReleaseBeforeReserve(t *testing.T) {
	inv := &fakeInventory{status: inventoryv1.ReservationStatus_RESERVATION_STATUS_RESERVED}
	r := newReconciler(t, &fakeLister{candidates: candidate(statusFailed)}, inv)

	if err := r.Pass(context.Background()); err != nil {
		t.Fatalf("Pass() = %v", err)
	}

	if got := inv.releasedIDs(); len(got) != 1 || got[0] != "42" {
		t.Fatalf("released = %v, want the orphaned hold returned", got)
	}
	if got := inv.reasons(); got[0] != reasonOrderFailed {
		t.Errorf("reason = %q, want %q", got[0], reasonOrderFailed)
	}
	if r.backlog.Load() != 0 {
		t.Errorf("backlog = %d after releasing the orphan, want 0", r.backlog.Load())
	}
}

// A pass that fills its batch has NOT seen the whole window. It must not look
// like a complete count — that is how enough permanent breaches at the head of an
// oldest-first scan quietly starve newer, repairable inconsistencies.
func TestReconciler_FullBatchIsReportedAsTruncated(t *testing.T) {
	inv := &fakeInventory{status: inventoryv1.ReservationStatus_RESERVATION_STATUS_COMMITTED}
	core, logs := observer.New(zap.WarnLevel)

	var candidates []domain.ReconcileCandidate
	for i := 0; i < 3; i++ {
		candidates = append(candidates, domain.ReconcileCandidate{OrderID: "42", Status: statusFailed})
	}
	r := New(&fakeLister{candidates: candidates}, inv, closedWorkflow(), zap.New(core))
	r.now = func() time.Time { return testNow }
	r.batch = 3 // the lister returned exactly the cap

	if err := r.Pass(context.Background()); err != nil {
		t.Fatalf("Pass() = %v", err)
	}

	if logs.FilterMessageSnippet("batch cap").Len() == 0 {
		t.Error("a truncated pass did not say so; its backlog would read as a complete count")
	}
}

// A pass below the cap has seen the whole window, so it must NOT cry truncation —
// otherwise the signal is noise.
func TestReconciler_PartialBatchIsNotReportedAsTruncated(t *testing.T) {
	inv := &fakeInventory{status: inventoryv1.ReservationStatus_RESERVATION_STATUS_COMMITTED}
	core, logs := observer.New(zap.WarnLevel)

	r := New(&fakeLister{candidates: candidate(statusFailed)}, inv, closedWorkflow(), zap.New(core))
	r.now = func() time.Time { return testNow }
	r.batch = 10

	if err := r.Pass(context.Background()); err != nil {
		t.Fatalf("Pass() = %v", err)
	}

	if logs.FilterMessageSnippet("batch cap").Len() != 0 {
		t.Error("a complete pass reported truncation")
	}
}

// The guard that stops the reconciler from causing the breach it reports.
//
// The order row records the saga's last durable write, not its intent:
// ConfirmOrder can commit status=confirmed and lose its ack, so the workflow
// takes the compensation branch and starts releasing while the database says
// confirmed. Committing into that consumes units for an order being refunded.
// A running saga owns its own reservation, full stop.
func TestReconciler_DoesNotTouchAReservationWhileTheSagaRuns(t *testing.T) {
	for _, st := range []enumspb.WorkflowExecutionStatus{
		enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING,
		enumspb.WORKFLOW_EXECUTION_STATUS_PAUSED,
		enumspb.WORKFLOW_EXECUTION_STATUS_CONTINUED_AS_NEW,
		// Unknown must count as running: treating it as closed would let the
		// reconciler write into a live saga.
		enumspb.WORKFLOW_EXECUTION_STATUS_UNSPECIFIED,
	} {
		t.Run(st.String(), func(t *testing.T) {
			inv := &fakeInventory{status: inventoryv1.ReservationStatus_RESERVATION_STATUS_RESERVED}
			r := newReconcilerWith(t, &fakeLister{candidates: candidate(statusConfirmed)}, inv,
				&fakeWorkflows{status: st})

			if err := r.Pass(context.Background()); err != nil {
				t.Fatalf("Pass() = %v", err)
			}

			if len(inv.committedIDs()) != 0 || len(inv.releasedIDs()) != 0 {
				t.Errorf("stock was moved while the saga was %s — it may be compensating", st)
			}
			if r.backlog.Load() != 1 {
				t.Errorf("backlog = %d, want 1 — deferred is not resolved", r.backlog.Load())
			}
		})
	}
}

// A workflow that no longer exists counts as closed: past retention nobody is
// left to compensate, so the order row is the only evidence available.
func TestReconciler_RepairsWhenTheWorkflowIsGone(t *testing.T) {
	inv := &fakeInventory{status: inventoryv1.ReservationStatus_RESERVATION_STATUS_RESERVED}
	r := newReconcilerWith(t, &fakeLister{candidates: candidate(statusConfirmed)}, inv,
		&fakeWorkflows{err: &serviceerror.NotFound{}})

	if err := r.Pass(context.Background()); err != nil {
		t.Fatalf("Pass() = %v", err)
	}
	if got := inv.committedIDs(); len(got) != 1 {
		t.Errorf("committed = %v, want the repair to proceed", got)
	}
}

// If Temporal cannot be asked, the reconciler must defer rather than guess:
// repairing blind is exactly how it would write into a compensating saga.
func TestReconciler_DefersWhenTemporalCannotBeAsked(t *testing.T) {
	inv := &fakeInventory{status: inventoryv1.ReservationStatus_RESERVATION_STATUS_RESERVED}
	r := newReconcilerWith(t, &fakeLister{candidates: candidate(statusConfirmed)}, inv,
		&fakeWorkflows{err: errors.New("frontend down")})

	if err := r.Pass(context.Background()); err != nil {
		t.Fatalf("Pass() = %v", err)
	}
	if len(inv.committedIDs()) != 0 {
		t.Error("stock was moved without knowing whether the saga is still running")
	}
	if r.backlog.Load() != 1 {
		t.Errorf("backlog = %d, want 1", r.backlog.Load())
	}
}

// Moving stock on a reservation that belongs to another order would be the worst
// possible failure, and the owning order id is already on the wire.
func TestReconciler_RefusesAReservationOwnedByAnotherOrder(t *testing.T) {
	inv := &fakeInventory{
		status:  inventoryv1.ReservationStatus_RESERVATION_STATUS_RESERVED,
		orderID: "999",
	}
	r := newReconciler(t, &fakeLister{candidates: candidate(statusConfirmed)}, inv)

	if err := r.Pass(context.Background()); err != nil {
		t.Fatalf("Pass() = %v", err)
	}
	if len(inv.committedIDs()) != 0 || len(inv.releasedIDs()) != 0 {
		t.Error("moved stock on a reservation owned by a different order")
	}
	if r.backlog.Load() != 1 {
		t.Errorf("backlog = %d, want 1", r.backlog.Load())
	}
}
