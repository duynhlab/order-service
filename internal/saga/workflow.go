// Package saga implements the Temporal order-fulfillment workflow and its
// activities. The workflow is started by the web layer right after the order row
// commits (status "pending") and drives fulfillment durably:
//
//	ReserveStock -> CreateShipment -> ConfirmOrder (pivot) -> SendNotification -> ClearCart
//
// Steps before the pivot compensate in reverse on failure (ReleaseStock /
// CancelShipment) and the order is marked "failed". Once ConfirmOrder succeeds
// the order is "confirmed"; the remaining steps are best-effort and never roll
// the order back. See homelab/docs/api/temporal-order-fulfillment.md.
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
	Total   float64
}

// OrderFulfillmentInput is the workflow input. AuthToken is the caller's bearer
// token, used only by the best-effort cart-clear step (cart's private REST
// endpoint validates it); the saga runs within seconds of checkout.
type OrderFulfillmentInput struct {
	OrderID   string
	UserID    string
	Total     float64
	Items     []ReserveItem
	AuthToken string
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

	// Step 1 — reserve stock. Nothing to compensate yet on failure.
	if err := workflow.ExecuteActivity(ctx, a.ReserveStock, in.OrderID, in.Items).Get(ctx, nil); err != nil {
		log.Error("ReserveStock failed; marking order failed", "order_id", in.OrderID, "error", err)
		_ = workflow.ExecuteActivity(ctx, a.FailOrder, in.OrderID).Get(ctx, nil)
		return fmt.Errorf("reserve stock: %w", err)
	}

	// Step 2 — create shipment. Compensate: release stock.
	if err := workflow.ExecuteActivity(ctx, a.CreateShipment, in.OrderID).Get(ctx, nil); err != nil {
		log.Error("CreateShipment failed; compensating", "order_id", in.OrderID, "error", err)
		_ = workflow.ExecuteActivity(ctx, a.ReleaseStock, in.OrderID, in.Items).Get(ctx, nil)
		_ = workflow.ExecuteActivity(ctx, a.FailOrder, in.OrderID).Get(ctx, nil)
		return fmt.Errorf("create shipment: %w", err)
	}

	// Step 3 (pivot) — confirm the order. Compensate: cancel shipment + release stock.
	if err := workflow.ExecuteActivity(ctx, a.ConfirmOrder, in.OrderID).Get(ctx, nil); err != nil {
		log.Error("ConfirmOrder failed; compensating", "order_id", in.OrderID, "error", err)
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

	if in.AuthToken != "" {
		if err := workflow.ExecuteActivity(ctx, a.ClearCart, in.AuthToken).Get(ctx, nil); err != nil {
			log.Warn("ClearCart failed (non-fatal)", "order_id", in.OrderID, "error", err)
		}
	}

	return nil
}
