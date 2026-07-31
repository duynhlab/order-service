package saga

import (
	"context"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/duynhlab/order-service/internal/core/domain"
)

// Bounded last_successful_step tokens — the activity names as the projection
// renders them. Only steps a stage boundary reports appear here.
const (
	stepAuthorizePayment = "AUTHORIZE_PAYMENT"
	stepReserveStock     = "RESERVE_STOCK"
	stepReserveInventory = "RESERVE_INVENTORY"
	stepCreateShipment   = "CREATE_SHIPMENT"
	stepCapturePayment   = "CAPTURE_PAYMENT"
	stepConfirmOrder     = "CONFIRM_ORDER"
	stepCommitInventory  = "COMMIT_INVENTORY"
	stepComplete         = "COMPLETE"
)

// RecordProcessingStage upserts the order's processing-stage projection.
// Best-effort by contract: the workflow swallows its error after a short
// retry budget — a UX table must never stall or fail a money-bearing path.
func (a *Activities) RecordProcessingStage(ctx context.Context, u domain.ProcessingUpdate) error {
	return a.Projection.UpsertProcessingStage(ctx, u)
}

// projectionActivityOptions is deliberately the smallest budget in the saga:
// three quick attempts, then the workflow moves on and the next boundary
// self-heals the row.
func projectionActivityOptions() workflow.ActivityOptions {
	return workflow.ActivityOptions{
		// The tightest budget in the saga (worst case ~7s): recordStage runs
		// synchronously at boundaries that sit AHEAD of money-bearing
		// compensations, so a dark projection may delay them by at most this.
		StartToCloseTimeout: 3 * time.Second,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    time.Second,
			BackoffCoefficient: 2.0,
			MaximumInterval:    2 * time.Second,
			MaximumAttempts:    2,
		},
	}
}

// recordStage writes one projection boundary. Never fails the caller: a lost
// write is counted (alertable) and superseded by the next boundary.
func recordStage(ctx workflow.Context, u domain.ProcessingUpdate) {
	var a *Activities
	c := workflow.WithActivityOptions(ctx, projectionActivityOptions())
	if err := workflow.ExecuteActivity(c, a.RecordProcessingStage, u).Get(c, nil); err != nil {
		recordProjectionFailure(ctx)
		workflow.GetLogger(ctx).Warn("projection write failed (non-fatal); the next boundary self-heals it",
			"order_id", u.OrderID, "stage", u.Stage, "error", err)
	}
}
