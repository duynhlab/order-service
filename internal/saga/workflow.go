// Package saga implements the Temporal order-fulfillment workflow and its
// activities. The workflow is started by the web layer right after the order row
// commits (status "pending") and drives fulfillment durably:
//
//	[AuthorizePayment] -> ReserveStock -> CreateShipment -> [CapturePayment] ->
//	ConfirmOrder (pivot) -> SendNotification -> ClearCart
//
// The bracketed payment steps run only when PaymentEnabled (default off = the
// saga is unchanged). Steps before the pivot compensate in reverse on failure
// (ReleaseStock / CancelShipment, and VoidPayment before capture / RefundPayment
// after) and the order is marked "failed". Once ConfirmOrder succeeds the order
// is "confirmed"; the remaining steps are best-effort and never roll the order
// back. See homelab/docs/api/temporal-order-fulfillment.md.
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

	// PaymentEnabled gates the payment steps (authorize-early / capture-late plus
	// void/refund compensations). Read once from config at workflow start so a
	// mid-flight config flip can't break Temporal determinism. False = the saga
	// runs exactly as before payment integration.
	PaymentEnabled bool
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

	// voidPayment / refundPayment are the payment compensations, no-ops when
	// payment is disabled. Void releases an authorized-but-uncaptured hold;
	// refund returns already-captured money. Both retry (Temporal policy); a
	// terminal failure is logged at Error since it means money may be held or
	// not returned — an alertable, reconcile-worthy event.
	voidPayment := func() {
		if in.PaymentEnabled {
			if err := workflow.ExecuteActivity(ctx, a.VoidPayment, in.OrderID).Get(ctx, nil); err != nil {
				log.Error("VoidPayment compensation failed; authorized hold may remain", "order_id", in.OrderID, "error", err)
			}
		}
	}
	refundPayment := func() {
		if in.PaymentEnabled {
			if err := workflow.ExecuteActivity(ctx, a.RefundPayment, in.OrderID, in.Total).Get(ctx, nil); err != nil {
				log.Error("RefundPayment compensation failed; captured money may not be returned", "order_id", in.OrderID, "error", err)
			}
		}
	}

	// Step 0 — authorize the payment hold (pre-pivot). Nothing to compensate yet.
	if in.PaymentEnabled {
		if err := workflow.ExecuteActivity(ctx, a.AuthorizePayment, in.OrderID, in.UserID, in.Total).Get(ctx, nil); err != nil {
			log.Error("AuthorizePayment failed; marking order failed", "order_id", in.OrderID, "error", err)
			_ = workflow.ExecuteActivity(ctx, a.FailOrder, in.OrderID).Get(ctx, nil)
			return fmt.Errorf("authorize payment: %w", err)
		}
	}

	// Step 1 — reserve stock. Compensate: void the hold.
	if err := workflow.ExecuteActivity(ctx, a.ReserveStock, in.OrderID, in.Items).Get(ctx, nil); err != nil {
		log.Error("ReserveStock failed; compensating", "order_id", in.OrderID, "error", err)
		voidPayment()
		_ = workflow.ExecuteActivity(ctx, a.FailOrder, in.OrderID).Get(ctx, nil)
		return fmt.Errorf("reserve stock: %w", err)
	}

	// Step 2 — create shipment. Compensate: release stock + void the hold.
	if err := workflow.ExecuteActivity(ctx, a.CreateShipment, in.OrderID).Get(ctx, nil); err != nil {
		log.Error("CreateShipment failed; compensating", "order_id", in.OrderID, "error", err)
		_ = workflow.ExecuteActivity(ctx, a.ReleaseStock, in.OrderID, in.Items).Get(ctx, nil)
		voidPayment()
		_ = workflow.ExecuteActivity(ctx, a.FailOrder, in.OrderID).Get(ctx, nil)
		return fmt.Errorf("create shipment: %w", err)
	}

	// Step 3 — capture the payment (immediately before the pivot). Still pre-pivot,
	// so compensate: cancel shipment + release stock + void the hold.
	if in.PaymentEnabled {
		if err := workflow.ExecuteActivity(ctx, a.CapturePayment, in.OrderID).Get(ctx, nil); err != nil {
			log.Error("CapturePayment failed; compensating", "order_id", in.OrderID, "error", err)
			_ = workflow.ExecuteActivity(ctx, a.CancelShipment, in.OrderID).Get(ctx, nil)
			_ = workflow.ExecuteActivity(ctx, a.ReleaseStock, in.OrderID, in.Items).Get(ctx, nil)
			voidPayment()
			_ = workflow.ExecuteActivity(ctx, a.FailOrder, in.OrderID).Get(ctx, nil)
			return fmt.Errorf("capture payment: %w", err)
		}
	}

	// Step 4 (pivot) — confirm the order. Payment is already captured, so the
	// compensation is a refund (not a void): cancel shipment + release stock.
	if err := workflow.ExecuteActivity(ctx, a.ConfirmOrder, in.OrderID).Get(ctx, nil); err != nil {
		log.Error("ConfirmOrder failed; compensating", "order_id", in.OrderID, "error", err)
		refundPayment()
		_ = workflow.ExecuteActivity(ctx, a.CancelShipment, in.OrderID).Get(ctx, nil)
		_ = workflow.ExecuteActivity(ctx, a.ReleaseStock, in.OrderID, in.Items).Get(ctx, nil)
		_ = workflow.ExecuteActivity(ctx, a.FailOrder, in.OrderID).Get(ctx, nil)
		return fmt.Errorf("confirm order: %w", err)
	}

	// Past the pivot: the order is confirmed. Remaining steps are best-effort —
	// their failure is logged but does not fail the order.
	if err := workflow.ExecuteActivity(ctx, a.SendNotification,
		NotifyInput{OrderID: in.OrderID, UserID: in.UserID, Total: in.Total}).Get(ctx, nil); err != nil {
		log.Warn("SendNotification failed (non-fatal)", "order_id", in.OrderID, "error", err)
	}

	if in.UserID != "" {
		if err := workflow.ExecuteActivity(ctx, a.ClearCart, in.UserID).Get(ctx, nil); err != nil {
			log.Warn("ClearCart failed (non-fatal)", "order_id", in.OrderID, "error", err)
		}
	}

	return nil
}
