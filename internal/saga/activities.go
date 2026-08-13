package saga

import (
	"context"
	"errors"
	"fmt"

	"github.com/duynhlab/order-service/internal/core/domain"
	inventoryv1 "github.com/duynhlab/pkg/proto/inventory/v1"
	notificationv1 "github.com/duynhlab/pkg/proto/notification/v1"
	paymentv1 "github.com/duynhlab/pkg/proto/payment/v1"
	shippingv1 "github.com/duynhlab/pkg/proto/shipping/v1"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	"time"
)

// OrderTransitioner is the subset of the order repository the activities
// need: the aggregate's one status-write surface (RFC-0021 P5).
// *repository.PostgresOrderRepository satisfies it.
type OrderTransitioner interface {
	ApplyStatusCommand(ctx context.Context, cmd domain.StatusCommand) (replayed bool, err error)
}

// reasonOrderTransitionRefused is the bounded ApplicationError type for a
// status command the domain refused. Non-retryable by construction: retrying
// cannot make an illegal transition legal or un-collide a command id.
const reasonOrderTransitionRefused = "OrderTransitionRefused"

// applyOrderCommand stamps the executing workflow's identity onto cmd and
// applies it. Domain refusals (illegal transition, idempotency conflict,
// malformed command) come back non-retryable; everything else — including
// ErrConcurrencyConflict, which a retry resolves by replaying — stays
// retryable for the activity's retry policy.
func applyOrderCommand(ctx context.Context, orders OrderTransitioner, cmd domain.StatusCommand) error {
	info := activity.GetInfo(ctx)
	cmd = cmd.WithWorkflowIdentity(info.WorkflowExecution.ID, info.WorkflowExecution.RunID)
	_, err := orders.ApplyStatusCommand(ctx, cmd)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, domain.ErrInvalidTransition),
		errors.Is(err, domain.ErrIdempotencyConflict),
		errors.Is(err, domain.ErrInvalidInput):
		return temporal.NewNonRetryableApplicationError(
			fmt.Sprintf("order %s refused %s", cmd.OrderID, cmd.CommandID),
			reasonOrderTransitionRefused, err)
	default:
		return fmt.Errorf("apply %s: %w", cmd.CommandID, err)
	}
}

// Activities holds the dependencies for the order-fulfillment activities. The
// gRPC fields are the generated client interfaces (easy to stub in tests);
// ClearCartFn is injected so this package doesn't depend on the web layer.
type Activities struct {
	Shipping     shippingv1.ShippingServiceClient
	Notification notificationv1.NotificationServiceClient
	Payment      paymentv1.PaymentServiceClient
	// Inventory is the sole stock participant since RFC-0021 P4 removed the
	// product branch. The product gRPC client went with it: stock reservation was
	// the only thing order ever called product for, so the dial, the config and
	// the NetworkPolicy allowing order → product:9090 are all gone too.
	Inventory inventoryv1.InventoryServiceClient
	Orders    OrderTransitioner
	// Projection is the processing-stage read model's write surface
	// (RFC-0021 P5); the stage activities are best-effort by contract.
	Projection  domain.ProcessingProjector
	ClearCartFn func(ctx context.Context, userID string) error
	// CommitPause is the GameDay fault hook (ORDER_FAULT_COMMIT_PAUSE): a
	// non-zero value holds CommitInventory between the server-side commit and
	// the activity result, so an operator can kill the worker inside the one
	// window hand timing cannot hit (RFC-0021 G2b). Zero in steady state.
	CommitPause time.Duration
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
// A retry or a reset replays the recorded command instead of re-applying.
func (a *Activities) ConfirmOrder(ctx context.Context, orderID string) error {
	cmd, err := domain.NewConfirmCommand(orderID)
	if err != nil {
		return temporal.NewNonRetryableApplicationError(
			"confirm order "+orderID, reasonOrderTransitionRefused, err)
	}
	return applyOrderCommand(ctx, a.Orders, cmd)
}

// FailOrder transitions the order to failed (terminal compensation). The
// bounded reason lands on orders.failure_code and in the history row; it is
// NOT part of the command id, so a reset that fails for a different reason
// replays the first outcome instead of colliding.
func (a *Activities) FailOrder(ctx context.Context, orderID string, reason domain.ReasonCode) error {
	cmd, err := domain.NewFailCommand(orderID, reason)
	if err != nil {
		return temporal.NewNonRetryableApplicationError(
			"fail order "+orderID, reasonOrderTransitionRefused, err)
	}
	return applyOrderCommand(ctx, a.Orders, cmd)
}

// MarkManualReview parks a pending order whose side effects are unaccounted
// for — the terminal branch when a compensation (or FailOrder itself) has
// exhausted its retries. Only an operator command moves the order out.
func (a *Activities) MarkManualReview(ctx context.Context, orderID string, reason domain.ReasonCode) error {
	cmd, err := domain.NewMarkManualReviewCommand(orderID, reason)
	if err != nil {
		return temporal.NewNonRetryableApplicationError(
			"park order "+orderID, reasonOrderTransitionRefused, err)
	}
	return applyOrderCommand(ctx, a.Orders, cmd)
}

// Complete records that the fulfillment tail finished (confirmed ->
// completed). Completion policy v1: "workflow done" — it upgrades when
// shipping grows real dispatch states.
func (a *Activities) Complete(ctx context.Context, orderID string) error {
	cmd, err := domain.NewCompleteCommand(orderID)
	if err != nil {
		return temporal.NewNonRetryableApplicationError(
			"complete order "+orderID, reasonOrderTransitionRefused, err)
	}
	return applyOrderCommand(ctx, a.Orders, cmd)
}

// sendCustomerEmail is the shared body of the order-lifecycle notification
// activities: send the caller-rendered subject/body via the notification
// service (a dumb sink) and wrap the error. The user id is the OIDC token
// subject — an opaque string (ADR-041/042) passed through verbatim. kind names
// the message in the error; deliveryType is the bounded token in the
// idempotency key "order:<id>:type:<t>:version:1" — deterministic per (order,
// message type), so a Temporal retry of the activity replays the original
// inbox row instead of duplicating it. Recipient is a placeholder — routing is
// by user id (a real customer-email lookup is a separate follow-up across all
// three call sites).
func (a *Activities) sendCustomerEmail(ctx context.Context, in NotifyInput, kind, deliveryType, subject, body string) error {
	if _, err := a.Notification.SendEmail(ctx, &notificationv1.SendEmailRequest{
		UserId:      in.UserID,
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
