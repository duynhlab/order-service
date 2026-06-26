package saga

import (
	"context"
	"fmt"
	"strconv"

	notificationv1 "github.com/duynhlab/pkg/proto/notification/v1"
	productv1 "github.com/duynhlab/pkg/proto/product/v1"
	shippingv1 "github.com/duynhlab/pkg/proto/shipping/v1"
	"go.temporal.io/sdk/temporal"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// orderStatusConfirmed / orderStatusFailed are the saga's terminal order states.
const (
	orderStatusConfirmed = "confirmed"
	orderStatusFailed    = "failed"
)

// OrderStatusUpdater is the subset of the order repository the activities need.
// *repository.PostgresOrderRepository satisfies it.
type OrderStatusUpdater interface {
	UpdateStatus(ctx context.Context, id, status string) error
}

// Activities holds the dependencies for the order-fulfillment activities. The
// gRPC fields are the generated client interfaces (easy to stub in tests);
// ClearCartFn is injected so this package doesn't depend on the web layer.
type Activities struct {
	Product      productv1.ProductServiceClient
	Shipping     shippingv1.ShippingServiceClient
	Notification notificationv1.NotificationServiceClient
	Orders       OrderStatusUpdater
	ClearCartFn  func(ctx context.Context, userID string) error
}

func toStockItems(items []ReserveItem) []*productv1.StockItem {
	out := make([]*productv1.StockItem, 0, len(items))
	for _, it := range items {
		out = append(out, &productv1.StockItem{
			ProductId: it.ProductID,
			Quantity:  int32(it.Quantity), //nolint:gosec // order quantities are small, validated > 0 upstream
		})
	}
	return out
}

// ReserveStock reserves inventory for the order (idempotent by order ID).
// Insufficient stock is a business rejection, returned as a non-retryable error
// so the saga fails fast and compensates instead of retrying forever.
func (a *Activities) ReserveStock(ctx context.Context, orderID string, items []ReserveItem) error {
	_, err := a.Product.ReserveStock(ctx, &productv1.ReserveStockRequest{
		ReservationId: orderID,
		Items:         toStockItems(items),
	})
	if err != nil {
		if status.Code(err) == codes.FailedPrecondition {
			return temporal.NewNonRetryableApplicationError("insufficient stock", "InsufficientStock", err)
		}
		return fmt.Errorf("reserve stock for order %s: %w", orderID, err)
	}
	return nil
}

// ReleaseStock returns reserved inventory (compensation for ReserveStock).
func (a *Activities) ReleaseStock(ctx context.Context, orderID string, items []ReserveItem) error {
	if _, err := a.Product.ReleaseStock(ctx, &productv1.ReleaseStockRequest{
		ReservationId: orderID,
		Items:         toStockItems(items),
	}); err != nil {
		return fmt.Errorf("release stock for order %s: %w", orderID, err)
	}
	return nil
}

// CreateShipment creates a shipment for the order (idempotent by order ID).
func (a *Activities) CreateShipment(ctx context.Context, orderID string) error {
	if _, err := a.Shipping.CreateShipment(ctx, &shippingv1.CreateShipmentRequest{
		OrderId: orderID,
	}); err != nil {
		return fmt.Errorf("create shipment for order %s: %w", orderID, err)
	}
	return nil
}

// CancelShipment cancels the order's shipment (compensation for CreateShipment).
func (a *Activities) CancelShipment(ctx context.Context, orderID string) error {
	if _, err := a.Shipping.CancelShipment(ctx, &shippingv1.CancelShipmentRequest{
		OrderId: orderID,
	}); err != nil {
		return fmt.Errorf("cancel shipment for order %s: %w", orderID, err)
	}
	return nil
}

// ConfirmOrder transitions the order pending -> confirmed (the saga pivot).
func (a *Activities) ConfirmOrder(ctx context.Context, orderID string) error {
	if err := a.Orders.UpdateStatus(ctx, orderID, orderStatusConfirmed); err != nil {
		return fmt.Errorf("confirm order %s: %w", orderID, err)
	}
	return nil
}

// FailOrder transitions the order to failed (terminal compensation).
func (a *Activities) FailOrder(ctx context.Context, orderID string) error {
	if err := a.Orders.UpdateStatus(ctx, orderID, orderStatusFailed); err != nil {
		return fmt.Errorf("fail order %s: %w", orderID, err)
	}
	return nil
}

// SendNotification emails the customer that the order is placed (best-effort).
func (a *Activities) SendNotification(ctx context.Context, in NotifyInput) error {
	uid, err := strconv.Atoi(in.UserID)
	if err != nil || uid < 0 {
		return temporal.NewNonRetryableApplicationError("invalid user id", "InvalidUserID", fmt.Errorf("user id %q", in.UserID))
	}
	_, err = a.Notification.SendEmail(ctx, &notificationv1.SendEmailRequest{
		UserId:  int32(uid), //nolint:gosec // DB-issued user id, guarded non-negative above
		To:      "noreply@orders.local",
		Subject: fmt.Sprintf("Order #%s placed", in.OrderID),
		Body:    fmt.Sprintf("Your order #%s for $%.2f has been confirmed.", in.OrderID, in.Total),
	})
	if err != nil {
		return fmt.Errorf("send notification for order %s: %w", in.OrderID, err)
	}
	return nil
}

// ClearCart empties the customer's cart after a confirmed order (best-effort).
// Identified by userID against cart's internal endpoint — no bearer token.
func (a *Activities) ClearCart(ctx context.Context, userID string) error {
	if a.ClearCartFn == nil {
		return nil
	}
	return a.ClearCartFn(ctx, userID)
}
