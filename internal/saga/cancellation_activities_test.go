package saga

import (
	"context"
	"testing"

	inventoryv1 "github.com/duynhlab/pkg/proto/inventory/v1"
	paymentv1 "github.com/duynhlab/pkg/proto/payment/v1"
	shippingv1 "github.com/duynhlab/pkg/proto/shipping/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/duynhlab/order-service/internal/core/domain"
)

// Cancellation-activity stubs: embed the generated interfaces so only the
// exercised methods need bodies.

type cancelShippingStub struct {
	shippingv1.ShippingServiceClient
	shipmentStatus string
	err            error
}

func (s *cancelShippingStub) GetShipmentByOrder(context.Context, *shippingv1.GetShipmentByOrderRequest, ...grpc.CallOption) (*shippingv1.GetShipmentByOrderResponse, error) {
	if s.err != nil {
		return nil, s.err
	}
	return &shippingv1.GetShipmentByOrderResponse{
		Shipment: &shippingv1.Shipment{Status: s.shipmentStatus},
	}, nil
}

type cancelPaymentStub struct {
	paymentv1.PaymentServiceClient
	payment *paymentv1.Payment
	err     error
}

func (s *cancelPaymentStub) GetPayment(context.Context, *paymentv1.GetPaymentRequest, ...grpc.CallOption) (*paymentv1.GetPaymentResponse, error) {
	if s.err != nil {
		return nil, s.err
	}
	return &paymentv1.GetPaymentResponse{Payment: s.payment}, nil
}

type cancelInventoryStub struct {
	inventoryv1.InventoryServiceClient
	resStatus inventoryv1.ReservationStatus
	err       error
}

func (s *cancelInventoryStub) GetReservation(context.Context, *inventoryv1.GetReservationRequest, ...grpc.CallOption) (*inventoryv1.GetReservationResponse, error) {
	if s.err != nil {
		return nil, s.err
	}
	return &inventoryv1.GetReservationResponse{
		Reservation: &inventoryv1.Reservation{Status: s.resStatus},
	}, nil
}

func TestCheckCancellationPolicy(t *testing.T) {
	cases := []struct {
		name        string
		stub        *cancelShippingStub
		wantErr     bool
		wantRefusal bool
	}{
		{"pending shipment passes", &cancelShippingStub{shipmentStatus: "pending"}, false, false},
		{"cancelled shipment passes", &cancelShippingStub{shipmentStatus: "cancelled"}, false, false},
		{"no shipment passes", &cancelShippingStub{err: status.Error(codes.NotFound, "none")}, false, false},
		{"dispatched refuses non-retryably", &cancelShippingStub{shipmentStatus: "in_transit"}, true, true},
		{"unreadable shipping errors retryably", &cancelShippingStub{err: status.Error(codes.Unavailable, "down")}, true, false},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			a := &Activities{Shipping: tt.stub}
			err := a.CheckCancellationPolicy(context.Background(), "42")
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr && tt.wantRefusal != isNonRetryable(err) {
				t.Fatalf("refusal = %v, want %v (err %v)", isNonRetryable(err), tt.wantRefusal, err)
			}
		})
	}
}

func TestGetPaymentState(t *testing.T) {
	t.Run("returns status and amounts", func(t *testing.T) {
		a := &Activities{Payment: &cancelPaymentStub{payment: &paymentv1.Payment{
			Status: "captured", AmountMinor: 2500, RefundedMinor: 1000,
		}}}
		got, err := a.GetPaymentState(context.Background(), "42")
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if got.Status != "captured" || got.AmountMinor != 2500 || got.RefundedMinor != 1000 {
			t.Errorf("state = %+v", got)
		}
	})
	t.Run("no payment is the zero state, not an error", func(t *testing.T) {
		a := &Activities{Payment: &cancelPaymentStub{err: status.Error(codes.NotFound, "none")}}
		got, err := a.GetPaymentState(context.Background(), "42")
		if err != nil || got.Status != "" {
			t.Fatalf("got %+v err=%v", got, err)
		}
	})
	t.Run("bad order id is non-retryable", func(t *testing.T) {
		a := &Activities{Payment: &cancelPaymentStub{}}
		if _, err := a.GetPaymentState(context.Background(), "not-a-number"); !isNonRetryable(err) {
			t.Fatalf("got %v, want non-retryable", err)
		}
	})
	t.Run("transport errors stay retryable", func(t *testing.T) {
		a := &Activities{Payment: &cancelPaymentStub{err: status.Error(codes.Unavailable, "down")}}
		if _, err := a.GetPaymentState(context.Background(), "42"); err == nil || isNonRetryable(err) {
			t.Fatalf("got %v, want retryable error", err)
		}
	})
}

func TestGetReservationState(t *testing.T) {
	t.Run("returns the enum token", func(t *testing.T) {
		a := &Activities{Inventory: &cancelInventoryStub{resStatus: inventoryv1.ReservationStatus_RESERVATION_STATUS_COMMITTED}}
		got, err := a.GetReservationState(context.Background(), "42")
		if err != nil || got != "RESERVATION_STATUS_COMMITTED" {
			t.Fatalf("got %q err=%v", got, err)
		}
	})
	t.Run("no reservation is the empty state", func(t *testing.T) {
		a := &Activities{Inventory: &cancelInventoryStub{err: status.Error(codes.NotFound, "none")}}
		got, err := a.GetReservationState(context.Background(), "42")
		if err != nil || got != "" {
			t.Fatalf("got %q err=%v", got, err)
		}
	})
}

func TestCancellationTerminalActivities(t *testing.T) {
	t.Run("CompleteCancellation issues the epoch command", func(t *testing.T) {
		orders := &stubOrders{}
		a := &Activities{Orders: orders}
		if err := runActivity(t, a.CompleteCancellation, "42", int64(5)); err != nil {
			t.Fatalf("err = %v", err)
		}
		cmds := orders.commands()
		if len(cmds) != 1 || cmds[0].CommandID != "cancel-complete:42:v5" || cmds[0].To != domain.OrderStatusCancelled {
			t.Errorf("commands = %+v", cmds)
		}
	})
	t.Run("CancelManualReview issues the epoch-scoped park", func(t *testing.T) {
		orders := &stubOrders{}
		a := &Activities{Orders: orders}
		if err := runActivity(t, a.CancelManualReview, "42", domain.ReasonCompensationIncomplete, int64(5)); err != nil {
			t.Fatalf("err = %v", err)
		}
		cmds := orders.commands()
		if len(cmds) != 1 || cmds[0].CommandID != "manual-review:42:v5" {
			t.Errorf("commands = %+v", cmds)
		}
	})
	t.Run("domain refusals are non-retryable", func(t *testing.T) {
		a := &Activities{Orders: &stubOrders{err: domain.ErrInvalidTransition}}
		if err := runActivity(t, a.CompleteCancellation, "42", int64(5)); !isNonRetryable(err) {
			t.Fatalf("got %v, want non-retryable", err)
		}
	})
	t.Run("negative epoch is refused before the store", func(t *testing.T) {
		orders := &stubOrders{}
		a := &Activities{Orders: orders}
		if err := runActivity(t, a.CompleteCancellation, "42", int64(-1)); !isNonRetryable(err) {
			t.Fatalf("got %v, want non-retryable", err)
		}
		if len(orders.commands()) != 0 {
			t.Error("refused command must not reach the store")
		}
	})
}
