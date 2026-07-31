package saga

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/duynhlab/order-service/internal/core/domain"
	notificationv1 "github.com/duynhlab/pkg/proto/notification/v1"
	productv1 "github.com/duynhlab/pkg/proto/product/v1"
	shippingv1 "github.com/duynhlab/pkg/proto/shipping/v1"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
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
	mu   sync.Mutex
	cmds []domain.StatusCommand
	err  error
}

func (s *stubOrders) ApplyStatusCommand(_ context.Context, cmd domain.StatusCommand) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return false, s.err
	}
	s.cmds = append(s.cmds, cmd)
	return false, nil
}

func (s *stubOrders) commands() []domain.StatusCommand {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]domain.StatusCommand(nil), s.cmds...)
}

// runActivity executes one activity in a real activity environment (so
// activity.GetInfo works) and returns its error.
func runActivity(t *testing.T, fn any, args ...any) error {
	t.Helper()
	ts := &testsuite.WorkflowTestSuite{}
	env := ts.NewTestActivityEnvironment()
	env.RegisterActivity(fn)
	_, err := env.ExecuteActivity(fn, args...)
	return err
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

// The order-transition activities run inside a real activity environment so
// applyOrderCommand can read the workflow identity off the context.
func TestOrderTransitionActivities(t *testing.T) {
	run := func(fn any, args ...any) error {
		t.Helper()
		ts := &testsuite.WorkflowTestSuite{}
		env := ts.NewTestActivityEnvironment()
		env.RegisterActivity(fn)
		_, err := env.ExecuteActivity(fn, args...)
		return err
	}

	t.Run("each activity issues its command with workflow identity", func(t *testing.T) {
		orders := &stubOrders{}
		a := &Activities{Orders: orders}
		if err := run(a.ConfirmOrder, "42"); err != nil {
			t.Fatalf("ConfirmOrder = %v", err)
		}
		if err := run(a.FailOrder, "42", domain.ReasonPaymentDeclined); err != nil {
			t.Fatalf("FailOrder = %v", err)
		}
		if err := run(a.MarkManualReview, "42", domain.ReasonCompensationIncomplete); err != nil {
			t.Fatalf("MarkManualReview = %v", err)
		}
		if err := run(a.Complete, "42"); err != nil {
			t.Fatalf("Complete = %v", err)
		}

		cmds := orders.commands()
		if len(cmds) != 4 {
			t.Fatalf("commands = %d, want 4", len(cmds))
		}
		wantIDs := []string{"confirm:42", "fail:42", "manual-review:42", "complete:42"}
		wantTo := []domain.OrderStatus{domain.OrderStatusConfirmed, domain.OrderStatusFailed,
			domain.OrderStatusManualReview, domain.OrderStatusCompleted}
		for i, cmd := range cmds {
			if cmd.CommandID != wantIDs[i] || cmd.To != wantTo[i] {
				t.Errorf("command %d = %q → %s, want %q → %s", i, cmd.CommandID, cmd.To, wantIDs[i], wantTo[i])
			}
			if cmd.ActorType != domain.ActorWorkflow {
				t.Errorf("command %d actor = %s, want WORKFLOW", i, cmd.ActorType)
			}
			if cmd.WorkflowID == "" || cmd.RunID == "" {
				t.Errorf("command %d missing workflow identity: %+v", i, cmd)
			}
		}
		if cmds[1].Reason != domain.ReasonPaymentDeclined {
			t.Errorf("fail reason = %q", cmds[1].Reason)
		}
	})

	t.Run("domain refusals are non-retryable, transport errors retry", func(t *testing.T) {
		refused := &Activities{Orders: &stubOrders{err: domain.ErrInvalidTransition}}
		err := run(refused.ConfirmOrder, "42")
		if err == nil || !isNonRetryable(err) {
			t.Fatalf("invalid transition: %v, want non-retryable", err)
		}

		conflicted := &Activities{Orders: &stubOrders{err: domain.ErrIdempotencyConflict}}
		if err := run(conflicted.ConfirmOrder, "42"); err == nil || !isNonRetryable(err) {
			t.Fatalf("idempotency conflict: %v, want non-retryable", err)
		}

		transient := &Activities{Orders: &stubOrders{err: errors.New("db down")}}
		if err := run(transient.ConfirmOrder, "42"); err == nil || isNonRetryable(err) {
			t.Fatalf("transient: %v, want retryable", err)
		}

		racing := &Activities{Orders: &stubOrders{err: domain.ErrConcurrencyConflict}}
		if err := run(racing.ConfirmOrder, "42"); err == nil || isNonRetryable(err) {
			t.Fatalf("concurrency conflict: %v, want retryable", err)
		}
	})

	t.Run("an unknown reason is refused before touching the store", func(t *testing.T) {
		orders := &stubOrders{}
		a := &Activities{Orders: orders}
		err := run(a.FailOrder, "42", domain.ReasonCode("pq: boom"))
		if err == nil || !isNonRetryable(err) {
			t.Fatalf("unknown reason: %v, want non-retryable", err)
		}
		if len(orders.commands()) != 0 {
			t.Error("a refused command must not reach the store")
		}
	})
}

// The three customer-email activities share one body (sendCustomerEmail), so one
// table exercises all of them: happy path, invalid user id (non-retryable), and a
// surfaced send error.
func TestCustomerEmailActivities(t *testing.T) {
	send := map[string]func(*Activities, context.Context, NotifyInput) error{
		"SendNotification": func(a *Activities, ctx context.Context, in NotifyInput) error { return a.SendNotification(ctx, in) },
		"SendReceipt":      func(a *Activities, ctx context.Context, in NotifyInput) error { return a.SendReceipt(ctx, in) },
		"SendRefundNotification": func(a *Activities, ctx context.Context, in NotifyInput) error {
			return a.SendRefundNotification(ctx, in)
		},
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
