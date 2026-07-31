package reconcile

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
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
	"github.com/duynhlab/pkg/grpcx"
	inventoryv1 "github.com/duynhlab/pkg/proto/inventory/v1"
)

var testReader *sdkmetric.ManualReader

func TestMain(m *testing.M) {
	testReader = sdkmetric.NewManualReader()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(testReader)))
	os.Exit(m.Run())
}

// fakeStore stands in for the reconcile persistence: it hands out candidates and
// records how each was settled, which is what the assertions read instead of an
// in-memory backlog.
type fakeStore struct {
	mu            sync.Mutex
	candidates    []domain.ReconcileCandidate
	err           error
	gotSettle     time.Duration
	gotWindow     time.Duration
	gotLimit      int
	reconciled    []string
	statusCounts  map[string]int
	breaches      map[string]string
	markErr       error
	markCtxErr    error
	markedLive    bool
	breachErr     error
	breachCtxLive bool
}

func newFakeStore(cs ...domain.ReconcileCandidate) *fakeStore {
	return &fakeStore{candidates: cs, breaches: map[string]string{}}
}

func (f *fakeStore) ListForReconcile(_ context.Context, settleDelay, window time.Duration, limit int) ([]domain.ReconcileCandidate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gotSettle, f.gotWindow, f.gotLimit = settleDelay, window, limit
	return f.candidates, f.err
}

func (f *fakeStore) CountUnreconciled(_ context.Context, _ time.Duration) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return 0, f.err
	}
	// Whatever has not been settled is still outstanding.
	return len(f.candidates) - len(f.reconciled), nil
}

func (f *fakeStore) CountOrdersInStatus(_ context.Context, status string, _ time.Duration) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return 0, f.err
	}
	return f.statusCounts[status], nil
}

func (f *fakeStore) MarkReconciled(ctx context.Context, orderID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.markedLive = ctx.Err() == nil
	f.markCtxErr = ctx.Err()
	if f.markErr != nil {
		return f.markErr
	}
	f.reconciled = append(f.reconciled, orderID)
	return nil
}

func (f *fakeStore) MarkReconcileBreach(ctx context.Context, orderID, code string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.breachCtxLive = ctx.Err() == nil
	if f.breachErr != nil {
		return f.breachErr
	}
	// Mirrors the repository, which routes the code through truncateErrCode and so
	// stores NULL for "". Without this the fake would accept an empty code and hide
	// that an empty one reads back as "no breach recorded" — silently disabling
	// once-per-order reporting.
	if code == "" {
		f.breaches[orderID] = ""
		return nil
	}
	f.breaches[orderID] = code
	return nil
}

func (f *fakeStore) settledIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.reconciled...)
}

func (f *fakeStore) breachCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.breaches)
}

// breachCodeOf returns the code recorded for an order. Distinct from breachCount
// so a test can assert the REASON, not merely that something was written — an
// empty code would still count.
func (f *fakeStore) breachCodeOf(orderID string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.breaches[orderID]
}

func (f *fakeStore) scanArgs() (time.Duration, time.Duration, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gotSettle, f.gotWindow, f.gotLimit
}

// fakeInventory is the inventory API as the reconciler sees it: a status to
// report, and a record of the repairs issued.
type fakeInventory struct {
	inventoryv1.InventoryServiceClient // everything the reconciler does not call

	mu      sync.Mutex
	status  inventoryv1.ReservationStatus
	orderID string
	delay   time.Duration
	getErr  error
	// emptyResp returns a SUCCESS with no reservation in it — a server that answers
	// without the field rather than with NOT_FOUND.
	emptyResp      bool
	commitErr      error
	releaseErr     error
	committed      []string
	released       []string
	releaseReasons []string
}

func (f *fakeInventory) GetReservation(_ context.Context, in *inventoryv1.GetReservationRequest, _ ...grpc.CallOption) (*inventoryv1.GetReservationResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	if f.getErr != nil {
		return nil, f.getErr
	}
	if f.emptyResp {
		return &inventoryv1.GetReservationResponse{}, nil
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
}

func (f *fakeWorkflows) DescribeWorkflowExecution(context.Context, string, string) (*workflowservice.DescribeWorkflowExecutionResponse, error) {
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

func newReconciler(t *testing.T, store *fakeStore, inv *fakeInventory) *Reconciler {
	t.Helper()
	return newReconcilerWith(t, store, inv, closedWorkflow())
}

func newReconcilerWith(t *testing.T, store *fakeStore, inv *fakeInventory, wf Describer) *Reconciler {
	t.Helper()
	return New(store, inv, wf, zap.NewNop())
}

func candidate(status string) []domain.ReconcileCandidate {
	return []domain.ReconcileCandidate{{OrderID: "42", Status: status}}
}

// The repair the reconciler exists for: a confirmed order whose stock is still
// only RESERVED. v1 reservations never expire, so nothing else would ever
// release or commit it.
func TestReconciler_CommitsConfirmedButReserved(t *testing.T) {
	inv := &fakeInventory{status: inventoryv1.ReservationStatus_RESERVATION_STATUS_RESERVED}
	store := newFakeStore(candidate(statusConfirmed)...)
	r := newReconciler(t, store, inv)

	if err := r.Pass(context.Background()); err != nil {
		t.Fatalf("Pass() = %v", err)
	}

	if got := inv.committedIDs(); len(got) != 1 || got[0] != "42" {
		t.Errorf("committed = %v, want [42]", got)
	}
	if len(inv.releasedIDs()) != 0 {
		t.Errorf("released = %v for a confirmed order, want none", inv.releasedIDs())
	}
	// The durable half of the repair: a settled row LEAVES the scan, so the next
	// pass does not re-examine an order that already agrees.
	if got := store.settledIDs(); len(got) != 1 || got[0] != "42" {
		t.Errorf("settled = %v, want [42] after a successful repair", got)
	}
}

// completed is confirmed-plus-bookkeeping: a completed order whose stock is
// still RESERVED gets the same commit repair, not a drift error.
func TestReconciler_CompletedClassifiesLikeConfirmed(t *testing.T) {
	inv := &fakeInventory{status: inventoryv1.ReservationStatus_RESERVATION_STATUS_RESERVED}
	store := newFakeStore(candidate(statusCompleted)...)
	r := newReconciler(t, store, inv)

	if err := r.Pass(context.Background()); err != nil {
		t.Fatalf("Pass() = %v", err)
	}

	if got := inv.committedIDs(); len(got) != 1 || got[0] != "42" {
		t.Errorf("committed = %v, want [42]", got)
	}
	if len(inv.releasedIDs()) != 0 {
		t.Errorf("released = %v for a completed order, want none", inv.releasedIDs())
	}
	if got := store.settledIDs(); len(got) != 1 || got[0] != "42" {
		t.Errorf("settled = %v, want [42]", got)
	}
}

// The breach side of the same rule: a completed order whose stock went back
// is the confirmed-but-released disagreement, not a consistent pair.
func TestReconciler_CompletedWithReleasedStockIsABreach(t *testing.T) {
	inv := &fakeInventory{status: inventoryv1.ReservationStatus_RESERVATION_STATUS_RELEASED}
	store := newFakeStore(candidate(statusCompleted)...)
	r := newReconciler(t, store, inv)

	if err := r.Pass(context.Background()); err != nil {
		t.Fatalf("Pass() = %v", err)
	}

	if got := store.breaches["42"]; got != BreachStockReturned {
		t.Errorf("breach = %q, want %q", got, BreachStockReturned)
	}
	if got := store.settledIDs(); len(got) != 0 {
		t.Errorf("settled = %v, want none — the pair still disagrees", got)
	}
}

// A failed order holding stock is the mirror case: nothing ships, so the units
// have to go back.
func TestReconciler_ReleasesFailedButReserved(t *testing.T) {
	inv := &fakeInventory{status: inventoryv1.ReservationStatus_RESERVATION_STATUS_RESERVED}
	r := newReconciler(t, newFakeStore(candidate(statusFailed)...), inv)

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
	store := newFakeStore(domain.ReconcileCandidate{
		OrderID: "42", Status: statusConfirmed, Participant: "product",
	})
	r := newReconciler(t, store, inv)

	if err := r.Pass(context.Background()); err != nil {
		t.Fatalf("Pass() = %v", err)
	}

	if len(inv.committedIDs()) != 0 || len(inv.releasedIDs()) != 0 {
		t.Error("a product-path order was repaired; it has no inventory reservation to repair")
	}
	if got := store.settledIDs(); len(got) != 1 || got[0] != "42" {
		t.Errorf("settled = %v, want [42] — a product-path order is consistent, not work", got)
	}
	if store.breachCount() != 0 {
		t.Error("a product-path order was recorded as a breach")
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
			store := newFakeStore(candidate(tc.orderStatus)...)
			r := newReconciler(t, store, inv)

			if err := r.Pass(context.Background()); err != nil {
				t.Fatalf("Pass() = %v", err)
			}
			if len(inv.committedIDs()) != 0 || len(inv.releasedIDs()) != 0 {
				t.Error("a consistent pair was repaired")
			}
			if got := store.settledIDs(); len(got) != 1 {
				t.Errorf("settled = %v, want the consistent pair settled so it leaves the scan", got)
			}
			if store.breachCount() != 0 {
				t.Error("a consistent pair was recorded as a breach")
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
			store := newFakeStore(candidate(tc.orderStatus)...)
			r := newReconciler(t, store, inv)

			if err := r.Pass(context.Background()); err != nil {
				t.Fatalf("Pass() = %v", err)
			}
			if len(inv.committedIDs()) != 0 || len(inv.releasedIDs()) != 0 {
				t.Error("a terminal-state breach was 'repaired'; that transition is invalid")
			}
			if len(store.settledIDs()) != 0 {
				t.Error("an unfixable breach was settled; it would vanish from the backlog unresolved")
			}
			if store.breachCount() != 1 {
				t.Errorf("breaches = %d, want 1 — an unfixable breach must be recorded", store.breachCount())
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
	store := newFakeStore(candidate(statusConfirmed)...)
	r := newReconciler(t, store, inv)

	if err := r.Pass(context.Background()); err != nil {
		t.Fatalf("Pass() = %v", err)
	}
	if len(store.settledIDs()) != 0 {
		t.Error("a failed repair settled the row; the order would drop out of the scan still broken")
	}
	// Not a breach either: the inconsistency is repairable, the repair just did
	// not land. Recording a breach code would push it behind fresh work forever.
	if store.breachCount() != 0 {
		t.Error("a transient repair failure was recorded as a permanent breach")
	}
}

// A repairable inconsistency stays in the scan until the repair actually lands.
// This is the property that replaced an in-memory counter: the state that decides
// whether an order is still work lives in the row, so it survives a restart and
// cannot drift from what the gauge reports.
func TestReconciler_RowSettlesOnlyOnceTheRepairLands(t *testing.T) {
	inv := &fakeInventory{
		status:    inventoryv1.ReservationStatus_RESERVATION_STATUS_RESERVED,
		commitErr: errors.New("inventory unavailable"),
	}
	store := newFakeStore(candidate(statusConfirmed)...)
	r := newReconciler(t, store, inv)

	if err := r.Pass(context.Background()); err != nil {
		t.Fatalf("first Pass: %v", err)
	}
	if len(store.settledIDs()) != 0 {
		t.Fatal("the row settled while the commit was still failing")
	}

	inv.mu.Lock()
	inv.commitErr = nil
	inv.mu.Unlock()
	if err := r.Pass(context.Background()); err != nil {
		t.Fatalf("second Pass: %v", err)
	}
	if got := store.settledIDs(); len(got) != 1 || got[0] != "42" {
		t.Errorf("settled = %v, want [42] once the commit succeeded", got)
	}
}

// The scan is parameterised by DURATIONS, not by instants the process computed.
// That is deliberate: orders.updated_at is written by the database, and a
// timestamp this process derives from its own clock would be compared against it
// across whatever skew (and, for a timestamp-without-zone column, whatever
// timezone) separates the two. Passing the window as a duration makes the
// database evaluate it with the same clock that wrote the rows.
func TestReconciler_ScanIsParameterisedByDurationsNotInstants(t *testing.T) {
	store := newFakeStore()
	r := newReconciler(t, store, &fakeInventory{})

	if err := r.Pass(context.Background()); err != nil {
		t.Fatalf("Pass() = %v", err)
	}

	settle, window, limit := store.scanArgs()
	if settle != DefaultSettleDelay {
		t.Errorf("settleDelay = %v, want %v", settle, DefaultSettleDelay)
	}
	if window != DefaultWindow {
		t.Errorf("window = %v, want %v", window, DefaultWindow)
	}
	if limit != DefaultBatch {
		t.Errorf("limit = %d, want %d", limit, DefaultBatch)
	}
}

func TestReconciler_ListErrorIsReturned(t *testing.T) {
	r := newReconciler(t, func() *fakeStore { st := newFakeStore(); st.err = errors.New("db down"); return st }(), &fakeInventory{})

	if err := r.Pass(context.Background()); err == nil {
		t.Fatal("Pass() = nil, want the list error surfaced so Run can log it")
	}
}

// Run must pass on its tick and stop on cancellation — a loop that ignores
// cancellation keeps the worker from shutting down.
func TestReconciler_RunPassesThenStopsOnCancel(t *testing.T) {
	inv := &fakeInventory{status: inventoryv1.ReservationStatus_RESERVATION_STATUS_RESERVED}
	r := newReconciler(t, newFakeStore(candidate(statusConfirmed)...), inv)
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

// The gauge is a QUERY, not a memory of the last pass. Collected here WITHOUT
// running a pass at all: a value carried in memory would read zero until the
// first pass completed, stick at its last reading when a pass failed, and reset
// on every restart — all of which report "everything agrees" during exactly the
// conditions where it does not.
func TestRegisterBacklogGauge_ReadsTheBacklogFromTheStoreNotFromMemory(t *testing.T) {
	store := newFakeStore(candidate(statusFailed)...)

	reg, err := RegisterBacklogGauge(store, zap.NewNop())
	if err != nil {
		t.Fatalf("RegisterBacklogGauge() = %v", err)
	}
	t.Cleanup(func() {
		if err := reg.Unregister(); err != nil {
			t.Errorf("Unregister() = %v; a leaked callback makes later tests order-dependent", err)
		}
	})

	if got := collectBacklog(t); got != 1 {
		t.Errorf("backlog gauge = %d, want 1 read straight from the store, with no pass having run", got)
	}
}

// A backlog read that fails must publish NOTHING — not zero (which reads as "all
// agrees" during a database problem) and not an error to the SDK (which discards
// the whole export cycle, taking every other metric down with it).
func TestRegisterBacklogGauge_AFailedReadPublishesNothingAndDoesNotFailCollection(t *testing.T) {
	store := newFakeStore(candidate(statusFailed)...)
	store.err = errors.New("db down")

	reg, err := RegisterBacklogGauge(store, zap.NewNop())
	if err != nil {
		t.Fatalf("RegisterBacklogGauge() = %v", err)
	}
	t.Cleanup(func() {
		if err := reg.Unregister(); err != nil {
			t.Errorf("Unregister() = %v", err)
		}
	})

	var rm metricdata.ResourceMetrics
	// The load-bearing half: Collect must SUCCEED. PeriodicReader exports only
	// when Collect returns nil, so a callback that surfaces its error would blank
	// every series in the process, not just this one.
	if err := testReader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect() = %v; one failing gauge must not fail the whole cycle", err)
	}

	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != backlogMetric {
				continue
			}
			if g, ok := m.Data.(metricdata.Gauge[int64]); ok && len(g.DataPoints) > 0 {
				t.Errorf("published %d datapoint(s) despite a failed read; a zero here reads as healthy", len(g.DataPoints))
			}
		}
	}
}

const backlogMetric = "order.reconciler.backlog"

// repairCount reads order_reconciler_repairs_total for one action label.
//
// Counters accumulate across the whole test binary, so every assertion is a
// DELTA taken around the call under test — an absolute value would be
// order-dependent from the first test that shares a label.
func repairCount(t *testing.T, action string) int64 {
	t.Helper()
	return repairCountOf(t, "order.reconciler.repairs.total", action)
}

// repairCountOf sums the datapoints of an Int64 counter, optionally filtered to
// one action label ("" means every datapoint).
func repairCountOf(t *testing.T, name, action string) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := testReader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	var total int64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				continue
			}
			for _, dp := range sum.DataPoints {
				if action == "" {
					total += dp.Value
					continue
				}
				if v, found := dp.Attributes.Value(attribute.Key("action")); found && v.AsString() == action {
					total += dp.Value
				}
			}
		}
	}
	return total
}

// counterWithLabel sums an Int64 counter filtered to one label key/value. Same
// delta discipline as repairCount: counters live for the whole test binary.
func counterWithLabel(t *testing.T, name, key, value string) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := testReader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	var total int64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				continue
			}
			for _, dp := range sum.DataPoints {
				if v, found := dp.Attributes.Value(attribute.Key(key)); found && v.AsString() == value {
					total += dp.Value
				}
			}
		}
	}
	return total
}

const disagreementMetric = "order.reconciler.participant_disagreements.total"

// collectBacklog reads the current backlog datapoint, or -1 when the gauge
// published nothing.
func collectBacklog(t *testing.T) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := testReader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	var got int64 = -1
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != backlogMetric {
				continue
			}
			if g, ok := m.Data.(metricdata.Gauge[int64]); ok {
				for _, dp := range g.DataPoints {
					got = dp.Value
				}
			}
		}
	}
	return got
}

// The release side needs the same treatment as the commit side: a failure keeps
// the order in the backlog so the next pass retries and the gauge keeps showing
// it, rather than reporting a repair that did not happen.
func TestReconciler_FailedReleaseStaysInTheBacklog(t *testing.T) {
	inv := &fakeInventory{
		status:     inventoryv1.ReservationStatus_RESERVATION_STATUS_RESERVED,
		releaseErr: errors.New("inventory unavailable"),
	}
	store := newFakeStore(candidate(statusFailed)...)
	r := newReconciler(t, store, inv)

	if err := r.Pass(context.Background()); err != nil {
		t.Fatalf("Pass() = %v", err)
	}
	if len(inv.releasedIDs()) != 0 {
		t.Errorf("released = %v despite the error", inv.releasedIDs())
	}
	if len(store.settledIDs()) != 0 {
		t.Error("a failed release settled the row; the held stock would stop being tracked")
	}
}

// The candidate query only returns terminal orders, so a non-terminal one
// reaching the repair means the query and this logic have drifted apart. It must
// refuse rather than guess — guessing here would either commit stock for an order
// still deciding, or release stock out from under a running saga.
func TestReconciler_NonTerminalOrderIsRefused(t *testing.T) {
	inv := &fakeInventory{status: inventoryv1.ReservationStatus_RESERVATION_STATUS_RESERVED}
	store := newFakeStore(domain.ReconcileCandidate{OrderID: "42", Status: "pending"})
	r := newReconciler(t, store, inv)

	if err := r.Pass(context.Background()); err != nil {
		t.Fatalf("Pass() = %v", err)
	}

	if len(inv.committedIDs()) != 0 || len(inv.releasedIDs()) != 0 {
		t.Error("a non-terminal order was repaired; the saga may still be deciding its outcome")
	}
	if store.breachCount() != 1 {
		t.Errorf("breaches = %d, want 1 — the drift must be visible", store.breachCount())
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
	store := newFakeStore(candidate(statusFailed)...)
	r := newReconciler(t, store, inv)

	if err := r.Pass(context.Background()); err != nil {
		t.Fatalf("Pass() = %v", err)
	}

	if got := inv.releasedIDs(); len(got) != 1 || got[0] != "42" {
		t.Fatalf("released = %v, want the orphaned hold returned", got)
	}
	if got := inv.reasons(); got[0] != reasonOrderFailed {
		t.Errorf("reason = %q, want %q", got[0], reasonOrderFailed)
	}
	if got := store.settledIDs(); len(got) != 1 || got[0] != "42" {
		t.Errorf("settled = %v, want [42] after releasing the orphan", got)
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
	r := New(newFakeStore(candidates...), inv, closedWorkflow(), zap.New(core))
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

	r := New(newFakeStore(candidate(statusFailed)...), inv, closedWorkflow(), zap.New(core))
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
			store := newFakeStore(candidate(statusConfirmed)...)
			r := newReconcilerWith(t, store, inv, &fakeWorkflows{status: st})

			if err := r.Pass(context.Background()); err != nil {
				t.Fatalf("Pass() = %v", err)
			}

			if len(inv.committedIDs()) != 0 || len(inv.releasedIDs()) != 0 {
				t.Errorf("stock was moved while the saga was %s — it may be compensating", st)
			}
			if len(store.settledIDs()) != 0 {
				t.Error("a deferred order was settled; it would leave the scan while still unresolved")
			}
			// Deferring is not a breach — the saga may well finish the job itself.
			if store.breachCount() != 0 {
				t.Error("a still-running saga was recorded as a breach")
			}
		})
	}
}

// A workflow that no longer exists counts as closed: past retention nobody is
// left to compensate, so the order row is the only evidence available.
func TestReconciler_RepairsWhenTheWorkflowIsGone(t *testing.T) {
	inv := &fakeInventory{status: inventoryv1.ReservationStatus_RESERVATION_STATUS_RESERVED}
	r := newReconcilerWith(t, newFakeStore(candidate(statusConfirmed)...), inv,
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
	store := newFakeStore(candidate(statusConfirmed)...)
	r := newReconcilerWith(t, store, inv, &fakeWorkflows{err: errors.New("frontend down")})

	if err := r.Pass(context.Background()); err != nil {
		t.Fatalf("Pass() = %v", err)
	}
	if len(inv.committedIDs()) != 0 {
		t.Error("stock was moved without knowing whether the saga is still running")
	}
	if len(store.settledIDs()) != 0 {
		t.Error("the row was settled without knowing whether the saga is still running")
	}
}

// Moving stock on a reservation that belongs to another order would be the worst
// possible failure, and the owning order id is already on the wire.
func TestReconciler_RefusesAReservationOwnedByAnotherOrder(t *testing.T) {
	inv := &fakeInventory{
		status:  inventoryv1.ReservationStatus_RESERVATION_STATUS_RESERVED,
		orderID: "999",
	}
	store := newFakeStore(candidate(statusConfirmed)...)
	r := newReconciler(t, store, inv)

	if err := r.Pass(context.Background()); err != nil {
		t.Fatalf("Pass() = %v", err)
	}
	if len(inv.committedIDs()) != 0 || len(inv.releasedIDs()) != 0 {
		t.Error("moved stock on a reservation owned by a different order")
	}
	if store.breachCount() != 1 {
		t.Errorf("breaches = %d, want 1 — a stranger's reservation is not something to retry quietly", store.breachCount())
	}
}

// A settle that runs on a dead context is worse than no settle at all: every pass
// would repair the same order and none would ever record that it agreed. The
// per-candidate budget is deliberately cancelled before the outcome is known, so
// the bookkeeping write must get a fresh one from the parent.
func TestReconciler_SettleWriteGetsALiveContext(t *testing.T) {
	inv := &fakeInventory{status: inventoryv1.ReservationStatus_RESERVATION_STATUS_RESERVED}
	store := newFakeStore(candidate(statusConfirmed)...)
	r := newReconciler(t, store, inv)

	if err := r.Pass(context.Background()); err != nil {
		t.Fatalf("Pass() = %v", err)
	}

	store.mu.Lock()
	live, ctxErr := store.markedLive, store.markCtxErr
	store.mu.Unlock()
	if !live {
		t.Errorf("the settle write ran on a cancelled context (%v); the order would be repaired forever", ctxErr)
	}
}

// A missing reservation is NORMAL for a product-path order and a BREACH for an
// inventory-path one. Reading both as "fine" — which a participant-blind
// reconciler must do, because it cannot tell them apart — silently swallows a
// lost Reserve, which is precisely the failure CommitInventory delegates here.
func TestReconciler_MissingReservationIsABreachOnlyForInventoryPathOrders(t *testing.T) {
	for _, tc := range []struct {
		name        string
		participant string
		wantBreach  bool
		wantSettled bool
	}{
		{"product path", "product", false, true},
		{"unstamped legacy order", "", false, true},
		{"inventory path", "inventory", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inv := &fakeInventory{getErr: status.Error(codes.NotFound, "no reservation")}
			store := newFakeStore(domain.ReconcileCandidate{
				OrderID: "42", Status: statusConfirmed, Participant: tc.participant,
			})
			r := newReconciler(t, store, inv)

			if err := r.Pass(context.Background()); err != nil {
				t.Fatalf("Pass() = %v", err)
			}

			if got := store.breachCount() == 1; got != tc.wantBreach {
				t.Errorf("breach recorded = %v, want %v", got, tc.wantBreach)
			}
			if got := len(store.settledIDs()) == 1; got != tc.wantSettled {
				t.Errorf("settled = %v, want %v", got, tc.wantSettled)
			}
		})
	}
}

// A breach is reported ONCE per order, not once per pass. A single permanently
// stuck order otherwise emits 1,440 counter increments and 1,440 error lines a
// day, which makes one unresolved incident indistinguishable from a stream of
// fresh saga failures.
func TestReconciler_AnAlreadyRecordedBreachIsNotReportedAgain(t *testing.T) {
	inv := &fakeInventory{status: inventoryv1.ReservationStatus_RESERVATION_STATUS_COMMITTED}
	core, logs := observer.New(zap.ErrorLevel)

	store := newFakeStore(domain.ReconcileCandidate{
		OrderID: "42", Status: statusFailed, BreachCode: ActionBreach,
	})
	r := New(store, inv, closedWorkflow(), zap.New(core))

	if err := r.Pass(context.Background()); err != nil {
		t.Fatalf("Pass() = %v", err)
	}

	if logs.Len() != 0 {
		t.Errorf("re-logged a known breach %d times; on-call would read it as new", logs.Len())
	}
	// Still unsettled — it is genuinely still inconsistent, so it must keep
	// showing in the backlog until a human resolves it.
	if len(store.settledIDs()) != 0 {
		t.Error("a known breach was settled; it would disappear from the backlog unresolved")
	}
	// And not re-stamped: the code is already there.
	if store.breachCount() != 0 {
		t.Error("re-wrote a breach code that was already recorded")
	}
}

// A TRANSIENT read failure must not settle the row. This is the mutation that
// matters most: `return ActionUnreadable, "", false` → `..., true` means that
// during any inventory outage every candidate is marked reconciled, leaves the
// scan permanently, and the backlog reads 0 — the exact failure the
// read-from-the-table redesign exists to prevent.
func TestReconciler_ATransientReadFailureNeitherSettlesNorBreaches(t *testing.T) {
	inv := &fakeInventory{getErr: status.Error(codes.Unavailable, "inventory down")}
	store := newFakeStore(candidate(statusConfirmed)...)
	r := newReconciler(t, store, inv)
	before := repairCount(t, ActionUnreadable)

	if err := r.Pass(context.Background()); err != nil {
		t.Fatalf("Pass() = %v", err)
	}

	if len(store.settledIDs()) != 0 {
		t.Error("an unreadable order was settled; an inventory outage would silently empty the backlog")
	}
	if store.breachCount() != 0 {
		t.Error("an unreadable order was recorded as a permanent breach; it is retryable")
	}
	if got := repairCount(t, ActionUnreadable); got != before+1 {
		t.Errorf("repairs_total{action=unreadable} = %d, want %d — an unreadable order needs a metric, not just a log", got, before+1)
	}
}

// A Temporal that cannot be asked is the same class: defer, do not settle, and be
// countable.
func TestReconciler_AnUnaskableTemporalIsCountedNotSettled(t *testing.T) {
	inv := &fakeInventory{status: inventoryv1.ReservationStatus_RESERVATION_STATUS_RESERVED}
	store := newFakeStore(candidate(statusConfirmed)...)
	r := newReconcilerWith(t, store, inv, &fakeWorkflows{err: errors.New("frontend down")})
	before := repairCount(t, ActionUnreadable)

	if err := r.Pass(context.Background()); err != nil {
		t.Fatalf("Pass() = %v", err)
	}
	if len(store.settledIDs()) != 0 || store.breachCount() != 0 {
		t.Error("an order whose saga state is unknown was settled or breached")
	}
	if got := repairCount(t, ActionUnreadable); got != before+1 {
		t.Errorf("repairs_total{action=unreadable} = %d, want %d", got, before+1)
	}
}

// The workflow-open guard must cover EVERY branch, not just the repairable one.
//
// Mid-compensation, a confirmed order whose stock reads RELEASED is NORMAL: the
// release already landed and failOrder has not. Calling that a terminal breach
// files a hard error against a saga that fixes itself seconds later — and settling
// a pair while its saga runs is worse still, because nothing ever resets
// reconciled_at, so the order becomes invisible forever.
func TestReconciler_DoesNotJudgeAnyPairWhileTheSagaRuns(t *testing.T) {
	for _, tc := range []struct {
		name        string
		orderStatus string
		reservation inventoryv1.ReservationStatus
		getErr      error
		participant string
	}{
		{name: "confirmed + released (mid-compensation)", orderStatus: statusConfirmed,
			reservation: inventoryv1.ReservationStatus_RESERVATION_STATUS_RELEASED},
		{name: "failed + committed", orderStatus: statusFailed,
			reservation: inventoryv1.ReservationStatus_RESERVATION_STATUS_COMMITTED},
		{name: "consistent pair must not settle early", orderStatus: statusConfirmed,
			reservation: inventoryv1.ReservationStatus_RESERVATION_STATUS_COMMITTED},
		{name: "confirmed inventory-path with no reservation yet", orderStatus: statusConfirmed,
			getErr: status.Error(codes.NotFound, "no reservation"), participant: "inventory"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inv := &fakeInventory{status: tc.reservation, getErr: tc.getErr}
			store := newFakeStore(domain.ReconcileCandidate{
				OrderID: "42", Status: tc.orderStatus, Participant: tc.participant,
			})
			r := newReconcilerWith(t, store, inv,
				&fakeWorkflows{status: enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING})

			if err := r.Pass(context.Background()); err != nil {
				t.Fatalf("Pass() = %v", err)
			}
			if store.breachCount() != 0 {
				t.Errorf("breach filed against a RUNNING saga (%s); it may still make the pair consistent",
					store.breachCodeOf("42"))
			}
			if len(store.settledIDs()) != 0 {
				t.Error("settled while the saga was running; nothing ever resets reconciled_at, so the order would be invisible forever")
			}
			if len(inv.committedIDs()) != 0 || len(inv.releasedIDs()) != 0 {
				t.Error("moved stock while the saga was running")
			}
		})
	}
}

// Every breach must persist a SPECIFIC reason. The column exists so the table
// answers "what is wrong with this order" after the log line has aged out; a
// single shared "breach" token, or an empty one, makes it decoration. An empty
// code is worse than useless: the repository stores NULL, so the row reads back as
// "no breach recorded" and once-per-order reporting silently turns off.
func TestReconciler_EachBreachPersistsItsOwnReason(t *testing.T) {
	for _, tc := range []struct {
		name        string
		orderStatus string
		inv         *fakeInventory
		participant string
		want        string
	}{
		{"failed order with committed stock", statusFailed,
			&fakeInventory{status: inventoryv1.ReservationStatus_RESERVATION_STATUS_COMMITTED}, "", BreachStockConsumed},
		{"confirmed order with released stock", statusConfirmed,
			&fakeInventory{status: inventoryv1.ReservationStatus_RESERVATION_STATUS_RELEASED}, "", BreachStockReturned},
		{"unrecognised reservation status", statusConfirmed,
			&fakeInventory{status: inventoryv1.ReservationStatus_RESERVATION_STATUS_UNSPECIFIED}, "", BreachUnknownStatus},
		{"status outside every known value", statusConfirmed,
			&fakeInventory{status: inventoryv1.ReservationStatus(99)}, "", BreachUnknownStatus},
		{"reservation owned by another order", statusConfirmed,
			&fakeInventory{status: inventoryv1.ReservationStatus_RESERVATION_STATUS_RESERVED, orderID: "999"}, "", BreachForeignReservation},
		{"confirmed inventory-path order with no reservation", statusConfirmed,
			&fakeInventory{getErr: status.Error(codes.NotFound, "no reservation")}, "inventory", BreachReservationMissing},
		{"non-terminal order reaching the repair", "pending",
			&fakeInventory{status: inventoryv1.ReservationStatus_RESERVATION_STATUS_RESERVED}, "", BreachNonTerminalOrder},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeStore(domain.ReconcileCandidate{
				OrderID: "42", Status: tc.orderStatus, Participant: tc.participant,
			})
			r := newReconciler(t, store, tc.inv)

			if err := r.Pass(context.Background()); err != nil {
				t.Fatalf("Pass() = %v", err)
			}
			if got := store.breachCodeOf("42"); got != tc.want {
				t.Errorf("reconcile_breach_code = %q, want %q", got, tc.want)
			}
			if len(store.settledIDs()) != 0 {
				t.Error("a breach was settled")
			}
		})
	}
}

// A FAILED inventory-path order with no reservation is NORMAL, not a breach: the
// saga authorizes payment before it reserves, so a declined card fails the order
// with no reservation ever existing, and an out-of-stock rejection rolls back
// inventory's transaction so the header row never commits. Reading those as
// breaches would poison the backlog with every decline and every sold-out item.
func TestReconciler_MissingReservationIsNormalForAFailedOrder(t *testing.T) {
	inv := &fakeInventory{getErr: status.Error(codes.NotFound, "no reservation")}
	store := newFakeStore(domain.ReconcileCandidate{
		OrderID: "42", Status: statusFailed, Participant: "inventory",
	})
	r := newReconciler(t, store, inv)

	if err := r.Pass(context.Background()); err != nil {
		t.Fatalf("Pass() = %v", err)
	}
	if store.breachCount() != 0 {
		t.Errorf("declined-payment order recorded as %q; every decline would file a breach",
			store.breachCodeOf("42"))
	}
	if got := store.settledIDs(); len(got) != 1 {
		t.Errorf("settled = %v, want the order settled — it is consistent, not work", got)
	}
}

// The reason token must be honoured even when the STATUS CODE is not NotFound.
// grpcx carries the platform reason in an ErrorInfo detail, so a service is free
// to return a different code alongside NOT_FOUND — and this disjunction is
// load-bearing for the product-path-vs-breach decision, so the reason arm has to
// be exercised on its own rather than shadowed by the code arm.
func TestReconciler_RecognisesAGrpcxNotFoundReasonUnderAnotherCode(t *testing.T) {
	inv := &fakeInventory{getErr: grpcx.ErrorWithReason(
		codes.FailedPrecondition, grpcx.ReasonNotFound, "no reservation", nil)}
	store := newFakeStore(domain.ReconcileCandidate{
		OrderID: "42", Status: statusConfirmed, Participant: "product",
	})
	r := newReconciler(t, store, inv)

	if err := r.Pass(context.Background()); err != nil {
		t.Fatalf("Pass() = %v", err)
	}
	if got := store.settledIDs(); len(got) != 1 {
		t.Errorf("settled = %v, want the product-path order settled", got)
	}
}

// The bookkeeping writes must survive a failure without settling, and the failure
// must be visible. markErr/breachErr exist for this; if no test sets them the
// error branches are dead scaffolding.
func TestReconciler_BookkeepingFailuresAreLoggedAndLeaveTheRowUnsettled(t *testing.T) {
	t.Run("settle write fails", func(t *testing.T) {
		core, logs := observer.New(zap.WarnLevel)
		inv := &fakeInventory{status: inventoryv1.ReservationStatus_RESERVATION_STATUS_RESERVED}
		store := newFakeStore(candidate(statusConfirmed)...)
		store.markErr = errors.New("db down")
		r := New(store, inv, closedWorkflow(), zap.New(core))

		if err := r.Pass(context.Background()); err != nil {
			t.Fatalf("Pass() = %v", err)
		}
		if len(store.settledIDs()) != 0 {
			t.Error("the row recorded a settle despite the write failing")
		}
		if logs.FilterMessageSnippet("mark an order reconciled").Len() == 0 {
			t.Error("a failed settle was silent; the order is re-repaired every pass with no signal")
		}
	})

	t.Run("breach write fails", func(t *testing.T) {
		core, logs := observer.New(zap.WarnLevel)
		inv := &fakeInventory{status: inventoryv1.ReservationStatus_RESERVATION_STATUS_COMMITTED}
		store := newFakeStore(candidate(statusFailed)...)
		store.breachErr = errors.New("db down")
		r := New(store, inv, closedWorkflow(), zap.New(core))

		if err := r.Pass(context.Background()); err != nil {
			t.Fatalf("Pass() = %v", err)
		}
		if logs.FilterMessageSnippet("record a reconcile breach").Len() == 0 {
			t.Error("a failed breach write was silent; the breach would be re-logged forever with no explanation")
		}
	})
}

// The breach write needs a live context for the same reason the settle write does:
// on a dead one the code is never recorded, so every breach is re-logged and
// re-counted on every pass — the behaviour once-per-order reporting exists to stop.
func TestReconciler_BreachWriteGetsALiveContext(t *testing.T) {
	inv := &fakeInventory{status: inventoryv1.ReservationStatus_RESERVATION_STATUS_COMMITTED}
	store := newFakeStore(candidate(statusFailed)...)
	r := newReconciler(t, store, inv)

	if err := r.Pass(context.Background()); err != nil {
		t.Fatalf("Pass() = %v", err)
	}
	store.mu.Lock()
	live := store.breachCtxLive
	store.mu.Unlock()
	if !live {
		t.Error("the breach write ran on a cancelled context; the code would never be recorded")
	}
}

// The bookkeeping budget must NOT be derived from the pass budget. With a pass
// budget already expired, a repair that lands must still be settleable — otherwise
// the deadline turns into a repeating-work generator.
func TestReconciler_SettleSurvivesAnExhaustedPassBudget(t *testing.T) {
	inv := &fakeInventory{status: inventoryv1.ReservationStatus_RESERVATION_STATUS_RESERVED}
	store := newFakeStore(candidate(statusConfirmed)...)
	r := newReconciler(t, store, inv)
	// Long enough for the first candidate to be examined, far too short to still
	// be alive when its settle is written.
	r.passBudget = 30 * time.Millisecond
	inv.delay = 60 * time.Millisecond

	if err := r.Pass(context.Background()); err != nil {
		t.Fatalf("Pass() = %v", err)
	}
	if got := store.settledIDs(); len(got) != 1 {
		t.Errorf("settled = %v, want [42] — bookkeeping must not inherit the pass deadline", got)
	}
}

// A pass that runs out of budget stops cleanly: it must not return an error (Run
// would log it as a failure) and must leave the unexamined rows unsettled for the
// next tick.
func TestReconciler_PassStopsCleanlyWhenItsBudgetRunsOut(t *testing.T) {
	core, logs := observer.New(zap.WarnLevel)
	inv := &fakeInventory{status: inventoryv1.ReservationStatus_RESERVATION_STATUS_COMMITTED, delay: 40 * time.Millisecond}
	store := newFakeStore(
		domain.ReconcileCandidate{OrderID: "1", Status: statusConfirmed},
		domain.ReconcileCandidate{OrderID: "2", Status: statusConfirmed},
		domain.ReconcileCandidate{OrderID: "3", Status: statusConfirmed},
	)
	r := New(store, inv, closedWorkflow(), zap.New(core))
	r.passBudget = 50 * time.Millisecond

	if err := r.Pass(context.Background()); err != nil {
		t.Fatalf("Pass() = %v, want nil — an exhausted budget is not a failed pass", err)
	}
	if n := len(store.settledIDs()); n == 3 {
		t.Error("every candidate was examined despite the budget; the guard did nothing")
	}
	if logs.FilterMessageSnippet("ran out of budget").Len() == 0 {
		t.Error("a truncated-by-budget pass was silent")
	}
}

// Truncation needs a metric, not only a log: it is the one condition under which
// the backlog gauge is a floor rather than a count, and a log line cannot be
// alerted on.
func TestReconciler_AFullBatchIsCounted(t *testing.T) {
	before := repairCountOf(t, "order.reconciler.passes.truncated.total", "")
	inv := &fakeInventory{status: inventoryv1.ReservationStatus_RESERVATION_STATUS_COMMITTED}
	store := newFakeStore(
		domain.ReconcileCandidate{OrderID: "1", Status: statusConfirmed},
		domain.ReconcileCandidate{OrderID: "2", Status: statusConfirmed},
	)
	r := newReconciler(t, store, inv)
	r.batch = 2

	if err := r.Pass(context.Background()); err != nil {
		t.Fatalf("Pass() = %v", err)
	}
	if got := repairCountOf(t, "order.reconciler.passes.truncated.total", ""); got != before+1 {
		t.Errorf("truncated counter = %d, want %d", got, before+1)
	}
}

// Stop must end the loop WITHOUT cancelling the in-flight pass, or every worker
// restart aborts a repair mid-RPC and reports a failed one.
func TestReconciler_StopEndsTheLoopWithoutCancellingWork(t *testing.T) {
	inv := &fakeInventory{status: inventoryv1.ReservationStatus_RESERVATION_STATUS_RESERVED}
	store := newFakeStore(candidate(statusConfirmed)...)
	r := newReconciler(t, store, inv)
	r.interval = time.Millisecond

	done := make(chan struct{})
	go func() { r.Run(context.Background()); close(done) }()

	deadline := time.After(2 * time.Second)
	for len(inv.committedIDs()) == 0 {
		select {
		case <-deadline:
			t.Fatal("Run did not pass within 2s")
		default:
			time.Sleep(time.Millisecond)
		}
	}

	r.Stop()
	r.Stop() // idempotent: a double stop must not panic on a closed channel
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after Stop")
	}
	if len(store.settledIDs()) == 0 {
		t.Error("the repair was not settled, so Stop cancelled work instead of draining it")
	}
}

// A reservation belonging to an order that is NOT recorded as inventory-path is
// repaired — the hold is real, and inventory's v1 reservations never expire, so
// leaving it stranded is worse than acting on a row whose participant disagrees —
// but it must never be repaired SILENTLY.
//
// It is the fingerprint of a saga start that resolved its branch from a process
// flag instead of from the order, which is the one thing that can put these two
// records out of step. Repairing without saying so destroys the only evidence it
// happened, and the cutover is steered by whether it is still happening.
func TestReconciler_ReservationOnANonInventoryOrderIsRepairedAndReported(t *testing.T) {
	for _, tc := range []struct {
		name      string
		row       string
		wantLabel string
	}{
		{name: "recorded product path", row: "product", wantLabel: "product"},
		{name: "unstamped legacy order", row: "", wantLabel: "absent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inv := &fakeInventory{status: inventoryv1.ReservationStatus_RESERVATION_STATUS_RESERVED}
			core, logs := observer.New(zap.ErrorLevel)
			store := newFakeStore(domain.ReconcileCandidate{
				OrderID: "42", Status: statusConfirmed, Participant: tc.row,
			})
			r := New(store, inv, closedWorkflow(), zap.New(core))
			before := counterWithLabel(t, disagreementMetric, "row_participant", tc.wantLabel)

			if err := r.Pass(context.Background()); err != nil {
				t.Fatalf("Pass() = %v", err)
			}

			if got := inv.committedIDs(); len(got) != 1 || got[0] != "42" {
				t.Errorf("committed = %v, want [42] — a confirmed order's hold was earned, whatever the row says", got)
			}
			if got := store.settledIDs(); len(got) != 1 || got[0] != "42" {
				t.Errorf("settled = %v, want [42] — it was repaired, so it is consistent now", got)
			}

			// The log has to carry WHICH row said what, or on-call cannot tell a
			// product-path skew from an unstamped legacy row.
			entries := logs.FilterMessageSnippet("does not account for").All()
			if len(entries) != 1 {
				t.Fatalf("disagreement log entries = %d, want 1", len(entries))
			}
			if got := entries[0].ContextMap()["row_participant"]; got != tc.row {
				t.Errorf("row_participant field = %v, want %q", got, tc.row)
			}

			if delta := counterWithLabel(t, disagreementMetric, "row_participant", tc.wantLabel) - before; delta != 1 {
				t.Errorf("counter delta = %d, want 1 with label %q — a log line cannot be alerted on",
					delta, tc.wantLabel)
			}
		})
	}
}

// Reported for every status, not only the repairable one. A confirmed order whose
// hold is already COMMITTED needs no repair at all, and its record is just as wrong
// — narrowing this to RESERVED would hide most of the skew, because a saga that ran
// the inventory branch to completion leaves exactly this shape behind.
func TestReconciler_ADisagreementIsReportedEvenWhenNothingNeedsRepairing(t *testing.T) {
	inv := &fakeInventory{status: inventoryv1.ReservationStatus_RESERVATION_STATUS_COMMITTED}
	core, logs := observer.New(zap.ErrorLevel)
	store := newFakeStore(domain.ReconcileCandidate{
		OrderID: "42", Status: statusConfirmed, Participant: "product",
	})
	r := New(store, inv, closedWorkflow(), zap.New(core))
	before := counterWithLabel(t, disagreementMetric, "row_participant", "product")

	if err := r.Pass(context.Background()); err != nil {
		t.Fatalf("Pass() = %v", err)
	}

	if got := len(inv.committedIDs()) + len(inv.releasedIDs()); got != 0 {
		t.Errorf("issued %d repair(s) for an already-COMMITTED hold", got)
	}
	if got := logs.FilterMessageSnippet("does not account for").Len(); got != 1 {
		t.Errorf("disagreement reports = %d, want 1 — a completed inventory-branch saga is the common shape", got)
	}
	if delta := counterWithLabel(t, disagreementMetric, "row_participant", "product") - before; delta != 1 {
		t.Errorf("counter delta = %d, want 1", delta)
	}
}

// The label must stay a closed set even here — especially here, since this fires
// precisely when the column holds something unexpected. A raw value would mint a
// new time series per bad row.
func TestReconciler_AnUnrecognisedRowParticipantIsCountedUnderOther(t *testing.T) {
	inv := &fakeInventory{status: inventoryv1.ReservationStatus_RESERVATION_STATUS_RESERVED}
	store := newFakeStore(domain.ReconcileCandidate{
		OrderID: "42", Status: statusConfirmed, Participant: "warehouse-9",
	})
	r := New(store, inv, closedWorkflow(), zap.NewNop())
	beforeOther := counterWithLabel(t, disagreementMetric, "row_participant", "other")
	beforeRaw := counterWithLabel(t, disagreementMetric, "row_participant", "warehouse-9")

	if err := r.Pass(context.Background()); err != nil {
		t.Fatalf("Pass() = %v", err)
	}

	if delta := counterWithLabel(t, disagreementMetric, "row_participant", "other") - beforeOther; delta != 1 {
		t.Errorf("other delta = %d, want 1", delta)
	}
	if delta := counterWithLabel(t, disagreementMetric, "row_participant", "warehouse-9") - beforeRaw; delta != 0 {
		t.Errorf("raw column value became a label (%d datapoints); the set must stay closed", delta)
	}
}

// The ordinary case stays quiet: an inventory-path order with a reservation is
// exactly what the loop expects, and reporting it would train on-call to ignore the
// one above.
func TestReconciler_ReservationOnAnInventoryOrderIsNotReportedAsADisagreement(t *testing.T) {
	inv := &fakeInventory{status: inventoryv1.ReservationStatus_RESERVATION_STATUS_RESERVED}
	core, logs := observer.New(zap.ErrorLevel)
	store := newFakeStore(domain.ReconcileCandidate{
		OrderID: "42", Status: statusConfirmed, Participant: "inventory",
	})
	r := New(store, inv, closedWorkflow(), zap.New(core))
	before := counterWithLabel(t, disagreementMetric, "row_participant", "product")

	if err := r.Pass(context.Background()); err != nil {
		t.Fatalf("Pass() = %v", err)
	}

	if logs.Len() != 0 {
		t.Errorf("reported %d disagreements for a normal inventory-path repair", logs.Len())
	}
	if delta := counterWithLabel(t, disagreementMetric, "row_participant", "product") - before; delta != 0 {
		t.Errorf("counter delta = %d, want 0", delta)
	}
}

// Once per ORDER, not once per pass — the rule the whole reporting design rests on.
// A row that already carries a breach code has been reported before, so a
// permanent disagreement must not keep incrementing: at one pass a minute it would
// contribute 1,440 a day and drown out a cutover producing fresh ones.
func TestReconciler_AnAlreadyReportedDisagreementIsNotCountedAgain(t *testing.T) {
	inv := &fakeInventory{status: inventoryv1.ReservationStatus_RESERVATION_STATUS_COMMITTED}
	core, logs := observer.New(zap.ErrorLevel)
	store := newFakeStore(domain.ReconcileCandidate{
		OrderID: "42", Status: statusFailed, Participant: "product", BreachCode: BreachStockConsumed,
	})
	r := New(store, inv, closedWorkflow(), zap.New(core))
	before := counterWithLabel(t, disagreementMetric, "row_participant", "product")

	if err := r.Pass(context.Background()); err != nil {
		t.Fatalf("Pass() = %v", err)
	}

	if logs.Len() != 0 {
		t.Errorf("re-logged a known disagreement %d times", logs.Len())
	}
	if delta := counterWithLabel(t, disagreementMetric, "row_participant", "product") - before; delta != 0 {
		t.Errorf("counter delta = %d, want 0 — this order was already reported", delta)
	}
}

// "Holds a reservation" must be something this code can actually support. A
// success with no reservation in it is already reported as an unknown-status
// breach; calling it a participant disagreement as well would send on-call looking
// for a hold that was never described.
func TestReconciler_AnEmptyReservationResponseIsNotCalledADisagreement(t *testing.T) {
	inv := &fakeInventory{emptyResp: true}
	core, logs := observer.New(zap.ErrorLevel)
	store := newFakeStore(domain.ReconcileCandidate{
		OrderID: "42", Status: statusConfirmed, Participant: "product",
	})
	r := New(store, inv, closedWorkflow(), zap.New(core))
	before := counterWithLabel(t, disagreementMetric, "row_participant", "product")

	if err := r.Pass(context.Background()); err != nil {
		t.Fatalf("Pass() = %v", err)
	}

	if got := logs.FilterMessageSnippet("does not account for").Len(); got != 0 {
		t.Errorf("claimed a reservation is held %d times for a response that described none", got)
	}
	if delta := counterWithLabel(t, disagreementMetric, "row_participant", "product") - before; delta != 0 {
		t.Errorf("counter delta = %d, want 0", delta)
	}
}

// collectGauge reads one int64 gauge by name; -1 means no datapoint published.
func collectGauge(t *testing.T, name string) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := testReader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	var got int64 = -1
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			if g, ok := m.Data.(metricdata.Gauge[int64]); ok {
				for _, dp := range g.DataPoints {
					got = dp.Value
				}
			}
		}
	}
	return got
}

// The order-state gauges (RFC-0021 P5) read the table on every collection
// cycle, exactly like the reconciler backlog: manual_review un-aged,
// cancelling behind the stuck threshold.
func TestRegisterOrderStateGauges_ReadFromTheStore(t *testing.T) {
	store := newFakeStore()
	store.statusCounts = map[string]int{
		"manual_review": 3,
		"cancelling":    2,
	}

	reg, err := RegisterOrderStateGauges(store, zap.NewNop())
	if err != nil {
		t.Fatalf("RegisterOrderStateGauges() = %v", err)
	}
	t.Cleanup(func() {
		if err := reg.Unregister(); err != nil {
			t.Errorf("Unregister() = %v", err)
		}
	})

	if got := collectGauge(t, "order.manual_review.backlog"); got != 3 {
		t.Errorf("manual_review backlog = %d, want 3", got)
	}
	if got := collectGauge(t, "order.cancelling.backlog"); got != 2 {
		t.Errorf("cancelling backlog = %d, want 2", got)
	}
}

// Same publish-nothing-on-error rule as the reconciler backlog: a failed read
// must neither report zero nor fail the collection cycle.
func TestRegisterOrderStateGauges_FailedReadPublishesNothing(t *testing.T) {
	store := newFakeStore()
	store.err = errors.New("db down")

	reg, err := RegisterOrderStateGauges(store, zap.NewNop())
	if err != nil {
		t.Fatalf("RegisterOrderStateGauges() = %v", err)
	}
	t.Cleanup(func() {
		if err := reg.Unregister(); err != nil {
			t.Errorf("Unregister() = %v", err)
		}
	})

	var rm metricdata.ResourceMetrics
	if err := testReader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect() = %v; one failing gauge must not fail the whole cycle", err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "order.manual_review.backlog" && m.Name != "order.cancelling.backlog" {
				continue
			}
			if g, ok := m.Data.(metricdata.Gauge[int64]); ok && len(g.DataPoints) > 0 {
				t.Errorf("%s published %d datapoint(s) despite a failed read", m.Name, len(g.DataPoints))
			}
		}
	}
}
