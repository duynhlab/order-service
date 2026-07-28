package v1

import (
	"context"
	"errors"
	"net/http"
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
	marked []string
	err    error
}

func (s *stubOutbox) EnqueueWithTx(context.Context, domain.Transaction, string, string) error {
	return nil
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
func newOutboxService(outbox domain.StartRequestRepository) *logicv1.OrderService {
	return logicv1.NewOrderService(nil, nil, outbox)
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
