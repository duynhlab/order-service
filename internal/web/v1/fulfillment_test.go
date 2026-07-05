package v1

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/duynhlab/order-service/internal/core/domain"
	"github.com/duynhlab/order-service/internal/saga"
	"go.temporal.io/sdk/client"
	"go.uber.org/zap"
)

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
	h := NewOrderHandler(nil, nil, nil, starter, "order-fulfillment", false, nil)
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
	if starter.gotInput.PaymentEnabled {
		t.Error("PaymentEnabled must default to false when the handler flag is off")
	}
}

func TestStartFulfillment_PropagatesPaymentEnabled(t *testing.T) {
	starter := &stubStarter{}
	h := NewOrderHandler(nil, nil, nil, starter, "order-fulfillment", true, nil)
	order := &domain.Order{ID: "42", UserID: "7", Total: 25, Items: []domain.OrderItem{{ProductID: "1", Quantity: 2}}}
	c, _ := ctxWithBody(http.MethodPost, "/order/v1/private/orders", "7", "{}", map[string]string{"Authorization": "Bearer tok"})

	h.startFulfillment(c, zap.NewNop(), order, "")

	if !starter.gotInput.PaymentEnabled {
		t.Error("handler paymentEnabled=true must propagate into the workflow input")
	}
}

func TestStartFulfillment_NilTemporalIsNoop(t *testing.T) {
	h := NewOrderHandler(nil, nil, nil, nil, "order-fulfillment", false, nil)
	order := &domain.Order{ID: "42"}
	c, _ := ctxWithBody(http.MethodPost, "/order/v1/private/orders", "7", "{}", nil)

	// Must not panic; the order is simply left pending (logged).
	h.startFulfillment(c, zap.NewNop(), order, "")
}

func TestStartFulfillment_CarriesPaymentMethod(t *testing.T) {
	starter := &stubStarter{}
	h := NewOrderHandler(nil, nil, nil, starter, "order-fulfillment", true, nil)
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
	h := NewOrderHandler(nil, nil, nil, nil, "", true, nil)
	c, rec := ctxWithBody(http.MethodPost, "/order/v1/private/orders", "7",
		`{"payment_method":"4111111111111111"}`, map[string]string{"Idempotency-Key": "k1"})
	h.CreateOrder(c)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("PAN-shaped payment_method must 400 before anything persists, got %d (%s)", rec.Code, rec.Body)
	}
}
