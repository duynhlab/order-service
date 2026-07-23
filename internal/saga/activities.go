package saga

import (
	"context"
	"fmt"
	"strconv"

	"github.com/duynhlab/order-service/internal/core/domain"
	notificationv1 "github.com/duynhlab/pkg/proto/notification/v1"
	paymentv1 "github.com/duynhlab/pkg/proto/payment/v1"
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
	Payment      paymentv1.PaymentServiceClient
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
			recordStockReservation(ctx, resultInsufficient)
			return temporal.NewNonRetryableApplicationError("insufficient stock", "InsufficientStock", err)
		}
		recordStockReservation(ctx, resultError)
		return fmt.Errorf("reserve stock for order %s: %w", orderID, err)
	}
	recordStockReservation(ctx, resultReserved)
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

// sendCustomerEmail is the shared body of the order-lifecycle notification
// activities: validate the user id, send the caller-rendered subject/body via the
// notification service (a dumb sink), and wrap the error. kind names the message
// in the error; deliveryType is the bounded token in the idempotency key
// "order:<id>:type:<t>:version:1" — deterministic per (order, message type), so
// a Temporal retry of the activity replays the original inbox row instead of
// duplicating it. Recipient is a placeholder — routing is by user id (a real
// customer-email lookup is a separate follow-up across all three call sites).
func (a *Activities) sendCustomerEmail(ctx context.Context, in NotifyInput, kind, deliveryType, subject, body string) error {
	uid, err := strconv.Atoi(in.UserID)
	if err != nil || uid < 0 {
		return temporal.NewNonRetryableApplicationError(msgInvalidUserID, reasonInvalidUserID, fmt.Errorf("user id %q", in.UserID))
	}
	if _, err := a.Notification.SendEmail(ctx, &notificationv1.SendEmailRequest{
		UserId:      int32(uid), //nolint:gosec // DB-issued user id, guarded non-negative above
		To:          "noreply@orders.local",
		Subject:     subject,
		Body:        body,
		DeliveryKey: "order:" + in.OrderID + ":type:" + deliveryType + ":version:1",
	}); err != nil {
		return fmt.Errorf("send %s for order %s: %w", kind, in.OrderID, err)
	}
	return nil
}

// SendNotification emails the customer that the order is placed (best-effort).
func (a *Activities) SendNotification(ctx context.Context, in NotifyInput) error {
	return a.sendCustomerEmail(ctx, in, "notification", "order_confirmed",
		"Order #"+in.OrderID+" placed",
		fmt.Sprintf("Your order #%s for $%.2f has been confirmed.", in.OrderID, domain.Dollars(in.Total)))
}

// SendReceipt emails the customer a payment receipt after the money is captured
// (best-effort). Rendered saga-side — notification-service stores subject/body
// verbatim, and the saga holds the order id + captured total.
func (a *Activities) SendReceipt(ctx context.Context, in NotifyInput) error {
	return a.sendCustomerEmail(ctx, in, "receipt", "receipt",
		"Payment receipt for order #"+in.OrderID,
		fmt.Sprintf("We received your payment of $%.2f for order #%s. Thank you!", domain.Dollars(in.Total), in.OrderID))
}

// SendRefundNotification emails the customer that a refund was issued
// (best-effort). Triggered from the refund compensation after the money is
// actually returned.
func (a *Activities) SendRefundNotification(ctx context.Context, in NotifyInput) error {
	return a.sendCustomerEmail(ctx, in, "refund notification", "refund",
		"Refund issued for order #"+in.OrderID,
		fmt.Sprintf("We've refunded $%.2f for order #%s.", domain.Dollars(in.Total), in.OrderID))
}

// ClearCart empties the customer's cart after a confirmed order (best-effort).
// Identified by userID against cart's internal endpoint — no bearer token.
func (a *Activities) ClearCart(ctx context.Context, userID string) error {
	if a.ClearCartFn == nil {
		return nil
	}
	return a.ClearCartFn(ctx, userID)
}
