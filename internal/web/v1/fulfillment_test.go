package v1

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/duynhlab/order-service/internal/core/domain"
	logicv1 "github.com/duynhlab/order-service/internal/logic/v1"
	"github.com/duynhlab/order-service/internal/saga"
	"go.temporal.io/sdk/client"
	"go.uber.org/zap"
)

// stubOutbox is the start-outbox seam for handler tests: startFulfillment
// closes the row after a successful start, so the handler needs a real service
// wired to something rather than a nil one.
type stubOutbox struct {
	marked              []string
	err                 error
	enqueuedParticipant string
}

func (s *stubOutbox) EnqueueWithTx(_ context.Context, _ domain.Transaction, _, _, participant string) error {
	s.enqueuedParticipant = participant
	return nil
}

func (s *stubOutbox) MarkDispatchedForUser(_ context.Context, _, orderID string) error {
	return s.MarkDispatched(context.Background(), orderID)
}

func (s *stubOutbox) MarkDispatched(_ context.Context, orderID string) error {
	s.marked = append(s.marked, orderID)
	return s.err
}

func (s *stubOutbox) ClaimDue(context.Context, int, time.Duration) ([]domain.FulfillmentStartRequest, error) {
	return nil, nil
}

func (s *stubOutbox) Reschedule(context.Context, string, time.Time, string) error { return nil }
func (s *stubOutbox) MarkFailed(context.Context, string, string) error            { return nil }

func (s *stubOutbox) Stats(context.Context) (domain.StartRequestStats, error) {
	return domain.StartRequestStats{}, nil
}

// newOutboxService builds the minimal OrderService startFulfillment needs: only
// the outbox is reachable from that path.
func newOutboxService(outbox *stubOutbox) *logicv1.OrderService {
	return logicv1.NewOrderService(nil, nil, outbox, outbox, noopProjection{})
}

type stubStarter struct {
	called   bool
	gotID    string
	gotInput saga.OrderFulfillmentInput
	err      error
}

func (s *stubStarter) ExecuteWorkflow(_ context.Context, opts client.StartWorkflowOptions, _ any, args ...any) (client.WorkflowRun, error) {
	s.called = true
	s.gotID = opts.ID
	if len(args) > 0 {
		if in, ok := args[0].(saga.OrderFulfillmentInput); ok {
			s.gotInput = in
		}
	}
	return nil, s.err
}

func TestStartFulfillment_StartsWorkflow(t *testing.T) {
	starter := &stubStarter{}
	h := NewOrderHandler(newOutboxService(&stubOutbox{}), nil, nil, starter, "order-fulfillment", nil, "")
	order := &domain.Order{ID: "42", UserID: "7", Total: 25, Items: []domain.OrderItem{{ProductID: "1", Quantity: 2}}}
	c, _ := ctxWithBody(http.MethodPost, "/order/v1/private/orders", "7", "{}", map[string]string{"Authorization": "Bearer tok"})

	h.startFulfillment(c, zap.NewNop(), order, "")

	if !starter.called {
		t.Fatal("expected ExecuteWorkflow to be called")
	}
	if starter.gotID != "order-fulfillment-42" {
		t.Errorf("workflow id = %q, want order-fulfillment-42", starter.gotID)
	}
	if starter.gotInput.OrderID != "42" || starter.gotInput.UserID != "7" {
		t.Errorf("unexpected workflow input %+v", starter.gotInput)
	}
	if len(starter.gotInput.Items) != 1 || starter.gotInput.Items[0].ProductID != "1" || starter.gotInput.Items[0].Quantity != 2 {
		t.Errorf("items not mapped correctly: %+v", starter.gotInput.Items)
	}
}

func TestStartFulfillment_NilTemporalIsNoop(t *testing.T) {
	h := NewOrderHandler(newOutboxService(&stubOutbox{}), nil, nil, nil, "order-fulfillment", nil, "")
	order := &domain.Order{ID: "42"}
	c, _ := ctxWithBody(http.MethodPost, "/order/v1/private/orders", "7", "{}", nil)

	// Must not panic; the order is simply left pending (logged).
	h.startFulfillment(c, zap.NewNop(), order, "")
}

func TestStartFulfillment_CarriesPaymentMethod(t *testing.T) {
	starter := &stubStarter{}
	h := NewOrderHandler(newOutboxService(&stubOutbox{}), nil, nil, starter, "order-fulfillment", nil, "")
	order := &domain.Order{ID: "42", UserID: "7", Total: 25}
	c, _ := ctxWithBody(http.MethodPost, "/order/v1/private/orders", "7", "{}", map[string]string{"Authorization": "Bearer tok"})

	h.startFulfillment(c, zap.NewNop(), order, "tok_mastercard")
	if starter.gotInput.PaymentMethod != "tok_mastercard" {
		t.Fatalf("workflow input payment method = %q, want tok_mastercard", starter.gotInput.PaymentMethod)
	}
}

func TestIsTestToken(t *testing.T) {
	for _, ok := range []string{"tok_visa", "tok_mastercard", "tok_ABC_123"} {
		if !isTestToken(ok) {
			t.Errorf("isTestToken(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{
		"visa",                           // no tok_ prefix
		"4111111111111111",               // bare PAN
		"tok_4111111111111111",           // PAN behind the prefix
		"tok_4111_1111_1111_1111",        // grouped PAN
		"tok_" + strings.Repeat("a", 61), // over 64 chars
		"tok_with-dash",                  // disallowed char
		"tok",                            // too short
	} {
		if isTestToken(bad) {
			t.Errorf("isTestToken(%q) = true, want false", bad)
		}
	}
}

func TestCreateOrder_RejectsBadPaymentMethod(t *testing.T) {
	h := NewOrderHandler(newOutboxService(&stubOutbox{}), nil, nil, nil, "", nil, "")
	c, rec := ctxWithBody(http.MethodPost, "/order/v1/private/orders", "7",
		`{"payment_method":"4111111111111111"}`, map[string]string{"Idempotency-Key": "k1"})
	h.CreateOrder(c)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("PAN-shaped payment_method must 400 before anything persists, got %d (%s)", rec.Code, rec.Body)
	}
}

// A started saga must release its outbox row — otherwise the dispatcher keeps
// re-attempting a start that already happened, and the payment token stays in
// the table.
func TestStartFulfillment_ClosesTheOutboxRowOnSuccess(t *testing.T) {
	starter := &stubStarter{}
	outbox := &stubOutbox{}
	h := NewOrderHandler(newOutboxService(outbox), nil, nil, starter, "order-fulfillment", nil, "")
	order := &domain.Order{ID: "42", UserID: "7", Total: 1300}
	c, _ := ctxWithBody(http.MethodPost, "/order/v1/private/orders", "7", "{}", nil)

	h.startFulfillment(c, zap.NewNop(), order, "tok_visa_ok")

	if len(outbox.marked) != 1 || outbox.marked[0] != "42" {
		t.Errorf("marked = %v, want [42]", outbox.marked)
	}
}

// A failed start must NOT close the row: that row is the only durable record
// that this order still needs a saga.
func TestStartFulfillment_LeavesTheRowPendingOnFailure(t *testing.T) {
	starter := &stubStarter{err: errors.New("temporal down")}
	outbox := &stubOutbox{}
	h := NewOrderHandler(newOutboxService(outbox), nil, nil, starter, "order-fulfillment", nil, "")
	order := &domain.Order{ID: "42", UserID: "7", Total: 1300}
	c, _ := ctxWithBody(http.MethodPost, "/order/v1/private/orders", "7", "{}", nil)

	h.startFulfillment(c, zap.NewNop(), order, "tok_visa_ok")

	if len(outbox.marked) != 0 {
		t.Errorf("marked = %v after a failed start; the dispatcher would never retry it", outbox.marked)
	}
}

// stubTx / stubTxManager give the REST create path a transaction to commit, so
// the participant stamp can be observed where it actually lands: the outbox row.
type stubTx struct{}

func (stubTx) Commit(context.Context) error   { return nil }
func (stubTx) Rollback(context.Context) error { return nil }

type stubTxManager struct{}

func (stubTxManager) Begin(context.Context) (domain.Transaction, error) { return stubTx{}, nil }

// createRepo is the minimal OrderRepository the create path touches.
type createRepo struct{ mockOrderRepo }

func (r *createRepo) CreateWithTx(_ context.Context, _ domain.Transaction, order *domain.Order) error {
	order.ID = "77"
	return nil
}

// The REST transport must stamp the configured participant into the PERSISTED
// request, not only into the workflow input.
//
// This is a separate assignment from the saga input one, and it is the column the
// reconciler judges a missing reservation by — a product-path order legitimately
// has no reservation, a confirmed inventory-path order that has none lost its
// Reserve. If the stamp is dropped here, every REST-created order persists NULL,
// the reconciler reads them all as product-path, and the breach detection this
// whole change exists for never fires for a single real order.
func TestCreateOrder_PersistsTheConfiguredStockParticipant(t *testing.T) {
	cart := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"items":[{"product_id":"p1","quantity":1,"price":1000}]}`))
	}))
	defer cart.Close()

	outbox := &stubOutbox{}
	svc := logicv1.NewOrderService(&createRepo{}, stubTxManager{}, outbox, outbox, noopProjection{})
	h := NewOrderHandler(svc, NewCartClient(cart.URL), nil, &stubStarter{}, "order-fulfillment", nil,
		saga.ParticipantInventory)

	c, rec := ctxWithBody(http.MethodPost, "/order/v1/private/orders", "7",
		`{"payment_method":"tok_visa_ok"}`, map[string]string{"Idempotency-Key": "k1"})
	h.CreateOrder(c)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body)
	}
	if outbox.enqueuedParticipant != string(saga.ParticipantInventory) {
		t.Errorf("persisted participant = %q, want %q — the reconciler judges a missing reservation by this column",
			outbox.enqueuedParticipant, saga.ParticipantInventory)
	}
}

// conflictReplayRepo reproduces the racing double-submit: the handler's
// pre-check misses, the insert then trips the (user, idempotency key) unique
// index, and CreateOrder replays the order the winner committed.
type conflictReplayRepo struct {
	mockOrderRepo
	lookups int
	replay  *domain.Order
}

func (r *conflictReplayRepo) CreateWithTx(context.Context, domain.Transaction, *domain.Order) error {
	return domain.ErrConflict
}

func (r *conflictReplayRepo) FindByIdempotencyKey(context.Context, string, string) (*domain.Order, error) {
	r.lookups++
	if r.lookups == 1 {
		return nil, domain.ErrNotFound // the pre-check, before the winner committed
	}
	return r.replay, nil
}

// The REST transport must start the replayed order on the branch its ROW records,
// not on this handler's flag. The row was stamped by the request that won the race
// — during the cutover's rolling restart that can be a replica still running the
// other side of the flag, and the reconciler later judges the order by that column.
func TestCreateOrder_ReplayStartsOnTheRecordedParticipantNotTheFlag(t *testing.T) {
	cart := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"items":[{"product_id":"p1","quantity":1,"price":1000}]}`))
	}))
	defer cart.Close()

	repo := &conflictReplayRepo{replay: &domain.Order{ID: "77", Status: "pending",
		StockParticipant: string(saga.ParticipantInventory)}}
	outbox := &stubOutbox{}
	svc := logicv1.NewOrderService(repo, stubTxManager{}, outbox, outbox, noopProjection{})
	starter := &stubStarter{}
	// Flag deliberately DISAGREES with the row.
	h := NewOrderHandler(svc, NewCartClient(cart.URL), nil, starter, "order-fulfillment", nil,
		saga.ParticipantProduct)

	c, rec := ctxWithBody(http.MethodPost, "/order/v1/private/orders", "7",
		`{"payment_method":"tok_visa_ok"}`, map[string]string{"Idempotency-Key": "k1"})
	h.CreateOrder(c)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body)
	}
	if !starter.called {
		t.Fatal("no workflow started for the replayed order")
	}
	if starter.gotInput.StockParticipant != saga.ParticipantInventory {
		t.Errorf("StockParticipant = %q, want %q — the order's row decides, not the handler's flag",
			starter.gotInput.StockParticipant, saga.ParticipantInventory)
	}
}

// noopProjection satisfies domain.ProcessingProjector for wiring tests.
type noopProjection struct{}

func (noopProjection) UpsertProcessingStage(context.Context, domain.ProcessingUpdate) error {
	return nil
}
func (noopProjection) UpsertProcessingStageWithTx(context.Context, domain.Transaction, domain.ProcessingUpdate) error {
	return nil
}
