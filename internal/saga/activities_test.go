package saga

import (
	"context"
	"errors"
	"testing"

	notificationv1 "github.com/duynhlab/pkg/proto/notification/v1"
	productv1 "github.com/duynhlab/pkg/proto/product/v1"
	shippingv1 "github.com/duynhlab/pkg/proto/shipping/v1"
	"go.temporal.io/sdk/temporal"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Stubs embed the generated client interface (nil) so only the methods exercised
// here need bodies; any other call would panic (and none are made).

type stubProductClient struct {
	productv1.ProductServiceClient
	reserveErr error
	releaseErr error
}

func (s *stubProductClient) ReserveStock(_ context.Context, _ *productv1.ReserveStockRequest, _ ...grpc.CallOption) (*productv1.ReserveStockResponse, error) {
	return &productv1.ReserveStockResponse{}, s.reserveErr
}

func (s *stubProductClient) ReleaseStock(_ context.Context, _ *productv1.ReleaseStockRequest, _ ...grpc.CallOption) (*productv1.ReleaseStockResponse, error) {
	return &productv1.ReleaseStockResponse{}, s.releaseErr
}

type stubShippingClient struct {
	shippingv1.ShippingServiceClient
	createErr error
	cancelErr error
}

func (s *stubShippingClient) CreateShipment(_ context.Context, _ *shippingv1.CreateShipmentRequest, _ ...grpc.CallOption) (*shippingv1.CreateShipmentResponse, error) {
	return &shippingv1.CreateShipmentResponse{}, s.createErr
}

func (s *stubShippingClient) CancelShipment(_ context.Context, _ *shippingv1.CancelShipmentRequest, _ ...grpc.CallOption) (*shippingv1.CancelShipmentResponse, error) {
	return &shippingv1.CancelShipmentResponse{}, s.cancelErr
}

type stubNotificationClient struct {
	notificationv1.NotificationServiceClient
	err     error
	lastReq *notificationv1.SendEmailRequest
}

func (s *stubNotificationClient) SendEmail(_ context.Context, req *notificationv1.SendEmailRequest, _ ...grpc.CallOption) (*notificationv1.SendEmailResponse, error) {
	s.lastReq = req
	return &notificationv1.SendEmailResponse{}, s.err
}

type stubOrders struct {
	gotID, gotStatus string
	err              error
}

func (s *stubOrders) UpdateStatus(_ context.Context, id, status string) error {
	s.gotID, s.gotStatus = id, status
	return s.err
}

func isNonRetryable(err error) bool {
	var appErr *temporal.ApplicationError
	return errors.As(err, &appErr) && appErr.NonRetryable()
}

func TestReserveStock(t *testing.T) {
	items := []ReserveItem{{ProductID: "1", Quantity: 2}}

	t.Run("success", func(t *testing.T) {
		a := &Activities{Product: &stubProductClient{}}
		if err := a.ReserveStock(context.Background(), "42", items); err != nil {
			t.Fatalf("ReserveStock = %v, want nil", err)
		}
	})

	t.Run("insufficient stock is non-retryable", func(t *testing.T) {
		a := &Activities{Product: &stubProductClient{reserveErr: status.Error(codes.FailedPrecondition, "insufficient")}}
		err := a.ReserveStock(context.Background(), "42", items)
		if err == nil || !isNonRetryable(err) {
			t.Fatalf("ReserveStock = %v, want a non-retryable error", err)
		}
	})

	t.Run("other errors are retryable", func(t *testing.T) {
		a := &Activities{Product: &stubProductClient{reserveErr: status.Error(codes.Unavailable, "down")}}
		err := a.ReserveStock(context.Background(), "42", items)
		if err == nil || isNonRetryable(err) {
			t.Fatalf("ReserveStock = %v, want a retryable error", err)
		}
	})
}

func TestReleaseStock(t *testing.T) {
	a := &Activities{Product: &stubProductClient{}}
	if err := a.ReleaseStock(context.Background(), "42", []ReserveItem{{ProductID: "1", Quantity: 2}}); err != nil {
		t.Fatalf("ReleaseStock = %v, want nil", err)
	}
	a = &Activities{Product: &stubProductClient{releaseErr: errors.New("boom")}}
	if err := a.ReleaseStock(context.Background(), "42", nil); err == nil {
		t.Fatal("ReleaseStock = nil, want error")
	}
}

func TestShipmentActivities(t *testing.T) {
	ok := &Activities{Shipping: &stubShippingClient{}}
	if err := ok.CreateShipment(context.Background(), "42"); err != nil {
		t.Fatalf("CreateShipment = %v, want nil", err)
	}
	if err := ok.CancelShipment(context.Background(), "42"); err != nil {
		t.Fatalf("CancelShipment = %v, want nil", err)
	}

	bad := &Activities{Shipping: &stubShippingClient{createErr: errors.New("x"), cancelErr: errors.New("y")}}
	if err := bad.CreateShipment(context.Background(), "42"); err == nil {
		t.Fatal("CreateShipment = nil, want error")
	}
	if err := bad.CancelShipment(context.Background(), "42"); err == nil {
		t.Fatal("CancelShipment = nil, want error")
	}
}

func TestConfirmAndFailOrder(t *testing.T) {
	orders := &stubOrders{}
	a := &Activities{Orders: orders}

	if err := a.ConfirmOrder(context.Background(), "42"); err != nil {
		t.Fatalf("ConfirmOrder = %v, want nil", err)
	}
	if orders.gotStatus != orderStatusConfirmed || orders.gotID != "42" {
		t.Errorf("UpdateStatus got (%q,%q), want (42,confirmed)", orders.gotID, orders.gotStatus)
	}

	if err := a.FailOrder(context.Background(), "42"); err != nil {
		t.Fatalf("FailOrder = %v, want nil", err)
	}
	if orders.gotStatus != orderStatusFailed {
		t.Errorf("UpdateStatus status = %q, want failed", orders.gotStatus)
	}

	failing := &Activities{Orders: &stubOrders{err: errors.New("db")}}
	if err := failing.ConfirmOrder(context.Background(), "42"); err == nil {
		t.Fatal("ConfirmOrder = nil, want error")
	}
}

// The three customer-email activities share one body (sendCustomerEmail), so one
// table exercises all of them: happy path, invalid user id (non-retryable), and a
// surfaced send error.
func TestCustomerEmailActivities(t *testing.T) {
	send := map[string]func(*Activities, context.Context, NotifyInput) error{
		"SendNotification":       func(a *Activities, ctx context.Context, in NotifyInput) error { return a.SendNotification(ctx, in) },
		"SendReceipt":            func(a *Activities, ctx context.Context, in NotifyInput) error { return a.SendReceipt(ctx, in) },
		"SendRefundNotification": func(a *Activities, ctx context.Context, in NotifyInput) error { return a.SendRefundNotification(ctx, in) },
	}
	// Deterministic idempotency keys per message type: a Temporal retry
	// replays the original inbox row notification-side.
	deliveryKeys := map[string]string{
		"SendNotification":       "order:42:type:order_confirmed:version:1",
		"SendReceipt":            "order:42:type:receipt:version:1",
		"SendRefundNotification": "order:42:type:refund:version:1",
	}

	for name, fn := range send {
		t.Run(name, func(t *testing.T) {
			stub := &stubNotificationClient{}
			a := &Activities{Notification: stub}
			if err := fn(a, context.Background(), NotifyInput{OrderID: "42", UserID: "7", Total: 25}); err != nil {
				t.Fatalf("%s = %v, want nil", name, err)
			}
			if got := stub.lastReq.GetDeliveryKey(); got != deliveryKeys[name] {
				t.Fatalf("%s delivery key = %q, want %q", name, got, deliveryKeys[name])
			}
			for _, bad := range []string{"abc", "-1"} { // non-numeric and negative both non-retryable
				if err := fn(a, context.Background(), NotifyInput{OrderID: "42", UserID: bad}); err == nil || !isNonRetryable(err) {
					t.Fatalf("%s(%q) = %v, want non-retryable", name, bad, err)
				}
			}
			bad := &Activities{Notification: &stubNotificationClient{err: errors.New("smtp down")}}
			if err := fn(bad, context.Background(), NotifyInput{OrderID: "42", UserID: "7"}); err == nil {
				t.Fatalf("%s must surface a send error", name)
			}
		})
	}
}

func TestClearCart(t *testing.T) {
	// No clear function configured -> no-op success.
	a := &Activities{}
	if err := a.ClearCart(context.Background(), "user-7"); err != nil {
		t.Fatalf("ClearCart (nil fn) = %v, want nil", err)
	}

	var gotUserID string
	a = &Activities{ClearCartFn: func(_ context.Context, userID string) error {
		gotUserID = userID
		return nil
	}}
	if err := a.ClearCart(context.Background(), "user-7"); err != nil {
		t.Fatalf("ClearCart = %v, want nil", err)
	}
	if gotUserID != "user-7" {
		t.Errorf("clear fn got userID %q, want 'user-7'", gotUserID)
	}
}
