// Package saga implements the Temporal order-fulfillment workflow and its
// activities. The workflow is started by the web layer right after the order row
// commits (status "pending") and drives fulfillment durably:
//
//	AuthorizePayment -> ReserveStock -> CreateShipment -> CapturePayment ->
//	ConfirmOrder (pivot) -> SendNotification -> ClearCart
//
// Steps before the pivot compensate in reverse on failure (ReleaseStock /
// CancelShipment, and VoidPayment before capture / RefundPayment after) and the
// order is marked "failed". Once ConfirmOrder succeeds the order is "confirmed";
// the remaining steps are best-effort and never roll the order back. See
// homelab/docs/api/temporal-order-fulfillment.md.
package saga

import (
	"fmt"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// TaskQueue is the Temporal task queue the order worker polls.
const TaskQueue = "order-fulfillment"

// WorkflowID returns the dedup workflow ID for an order's fulfillment.
func WorkflowID(orderID string) string { return "order-fulfillment-" + orderID }

// ReserveItem is a product/quantity pair for the ReserveStock step.
type ReserveItem struct {
	ProductID string
	Quantity  int
}

// NotifyInput is the payload for the SendNotification step.
type NotifyInput struct {
	OrderID string
	UserID  string
	Total   int64 // minor units
}

// OrderFulfillmentInput is the workflow input. The best-effort cart-clear step
// uses UserID against cart's internal (NetworkPolicy-fenced) endpoint, so no
// bearer token is carried in the workflow input/history.
type OrderFulfillmentInput struct {
	OrderID string
	UserID  string
	Total   int64 // minor units
	Items   []ReserveItem

	// PaymentMethod is the checkout's opaque payment token. Empty = the
	// authorize activity falls back to its demo token (API-created orders,
	// older clients).
	PaymentMethod string
}

// activityOptions applies a bounded retry to every activity. Business
// rejections (e.g. insufficient stock) are returned as non-retryable
// application errors by the activity, so they fail fast instead of retrying.
func activityOptions() workflow.ActivityOptions {
	return workflow.ActivityOptions{
		StartToCloseTimeout: 30 * time.Second,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    time.Second,
			BackoffCoefficient: 2.0,
			MaximumInterval:    30 * time.Second,
			MaximumAttempts:    5,
		},
	}
}

// OrderFulfillmentWorkflow orchestrates the order-fulfillment saga.
func OrderFulfillmentWorkflow(ctx workflow.Context, in OrderFulfillmentInput) error {
	ctx = workflow.WithActivityOptions(ctx, activityOptions())
	log := workflow.GetLogger(ctx)

	// Nil receiver: ExecuteActivity only needs the method's identity; Temporal
	// invokes the registered *Activities instance at execution time.
	var a *Activities

	// Step 0 — authorize the payment hold (pre-pivot). Nothing to compensate yet.
	if err := workflow.ExecuteActivity(ctx, a.AuthorizePayment, in.OrderID, in.UserID, in.Total, in.PaymentMethod).Get(ctx, nil); err != nil {
		log.Error("AuthorizePayment failed; marking order failed", "order_id", in.OrderID, "error", err)
		failOrder(ctx, in.OrderID, outcomeFailed)
		return fmt.Errorf("authorize payment: %w", err)
	}

	// Step 1 — reserve stock. Compensate: void the hold.
	if err := workflow.ExecuteActivity(ctx, a.ReserveStock, in.OrderID, in.Items).Get(ctx, nil); err != nil {
		log.Error("ReserveStock failed; compensating", "order_id", in.OrderID, "error", err)
		voidPayment(ctx, in.OrderID)
		failOrder(ctx, in.OrderID, outcomeFailed)
		return fmt.Errorf("reserve stock: %w", err)
	}

	// Step 2 — create shipment. Compensate: release stock + void the hold.
	if err := workflow.ExecuteActivity(ctx, a.CreateShipment, in.OrderID).Get(ctx, nil); err != nil {
		log.Error("CreateShipment failed; compensating", "order_id", in.OrderID, "error", err)
		releaseStock(ctx, in)
		voidPayment(ctx, in.OrderID)
		failOrder(ctx, in.OrderID, outcomeFailed)
		return fmt.Errorf("create shipment: %w", err)
	}

	// Step 3 — capture the payment (immediately before the pivot). Still pre-pivot,
	// so compensate: cancel shipment + release stock + void the hold.
	if err := workflow.ExecuteActivity(ctx, a.CapturePayment, in.OrderID).Get(ctx, nil); err != nil {
		log.Error("CapturePayment failed; compensating", "order_id", in.OrderID, "error", err)
		cancelShipment(ctx, in.OrderID)
		releaseStock(ctx, in)
		voidPayment(ctx, in.OrderID)
		failOrder(ctx, in.OrderID, outcomeFailed)
		return fmt.Errorf("capture payment: %w", err)
	}

	// Step 4 (pivot) — confirm the order. Payment is already captured, so the
	// compensation is a refund (not a void): cancel shipment + release stock.
	if err := workflow.ExecuteActivity(ctx, a.ConfirmOrder, in.OrderID).Get(ctx, nil); err != nil {
		log.Error("ConfirmOrder failed; compensating", "order_id", in.OrderID, "error", err)
		refundPayment(ctx, in)
		cancelShipment(ctx, in.OrderID)
		releaseStock(ctx, in)
		failOrder(ctx, in.OrderID, outcomeCompensated)
		return fmt.Errorf("confirm order: %w", err)
	}
	recordSagaOutcome(ctx, outcomeConfirmed)

	// Past the pivot: the order is confirmed. Remaining steps are best-effort —
	// their failure is logged but does not fail the order.
	if err := workflow.ExecuteActivity(ctx, a.SendNotification,
		NotifyInput{OrderID: in.OrderID, UserID: in.UserID, Total: in.Total}).Get(ctx, nil); err != nil {
		log.Warn("SendNotification failed (non-fatal)", "order_id", in.OrderID, "error", err)
	}

	// Payment receipt (best-effort) — money was captured before the pivot.
	if err := workflow.ExecuteActivity(ctx, a.SendReceipt,
		NotifyInput{OrderID: in.OrderID, UserID: in.UserID, Total: in.Total}).Get(ctx, nil); err != nil {
		log.Warn("SendReceipt failed (non-fatal)", "order_id", in.OrderID, "error", err)
	}

	if in.UserID != "" {
		if err := workflow.ExecuteActivity(ctx, a.ClearCart, in.UserID).Get(ctx, nil); err != nil {
			log.Warn("ClearCart failed (non-fatal)", "order_id", in.OrderID, "error", err)
		}
	}

	return nil
}

// The compensation helpers below each run one compensation activity and record
// its outcome on order.saga.compensation.total. Extracting them keeps the
// workflow's terminal branches flat and single-purpose. Payment compensations
// (void/refund) log a terminal failure at Error — money may be held or not
// returned, an alertable reconcile-worthy event; stock/shipment/fail
// compensations stay silent (best-effort, unchanged from before instrumentation).

// The helpers take a nil *Activities receiver (the method-identity sentinel,
// exactly as the workflow does) rather than accepting it as a parameter, since
// it is always the same nil value.

// voidPayment releases an authorized-but-uncaptured hold.
func voidPayment(ctx workflow.Context, orderID string) {
	var a *Activities
	err := workflow.ExecuteActivity(ctx, a.VoidPayment, orderID).Get(ctx, nil)
	recordCompensation(ctx, compVoidPayment, compResult(err))
	if err != nil {
		workflow.GetLogger(ctx).Error("VoidPayment compensation failed; authorized hold may remain",
			"order_id", orderID, "error", err)
	}
}

// refundPayment returns already-captured money and, on success, emails the
// customer (best-effort; the notification never blocks the compensation).
func refundPayment(ctx workflow.Context, in OrderFulfillmentInput) {
	var a *Activities
	log := workflow.GetLogger(ctx)
	err := workflow.ExecuteActivity(ctx, a.RefundPayment, in.OrderID, in.Total).Get(ctx, nil)
	recordCompensation(ctx, compRefundPayment, compResult(err))
	if err != nil {
		log.Error("RefundPayment compensation failed; captured money may not be returned",
			"order_id", in.OrderID, "error", err)
		return
	}
	if err := workflow.ExecuteActivity(ctx, a.SendRefundNotification,
		NotifyInput{OrderID: in.OrderID, UserID: in.UserID, Total: in.Total}).Get(ctx, nil); err != nil {
		log.Warn("SendRefundNotification failed (non-fatal)", "order_id", in.OrderID, "error", err)
	}
}

// releaseStock returns reserved inventory (compensation for ReserveStock).
func releaseStock(ctx workflow.Context, in OrderFulfillmentInput) {
	var a *Activities
	err := workflow.ExecuteActivity(ctx, a.ReleaseStock, in.OrderID, in.Items).Get(ctx, nil)
	recordCompensation(ctx, compReleaseStock, compResult(err))
}

// cancelShipment cancels the order's shipment (compensation for CreateShipment).
func cancelShipment(ctx workflow.Context, orderID string) {
	var a *Activities
	err := workflow.ExecuteActivity(ctx, a.CancelShipment, orderID).Get(ctx, nil)
	recordCompensation(ctx, compCancelShipment, compResult(err))
}

// failOrder marks the order failed (terminal compensation) and records both the
// fail_order compensation step and the saga's terminal outcome (failed when the
// money was voided pre-capture, compensated when it was refunded post-capture).
func failOrder(ctx workflow.Context, orderID, outcome string) {
	var a *Activities
	err := workflow.ExecuteActivity(ctx, a.FailOrder, orderID).Get(ctx, nil)
	recordCompensation(ctx, compFailOrder, compResult(err))
	recordSagaOutcome(ctx, outcome)
}
