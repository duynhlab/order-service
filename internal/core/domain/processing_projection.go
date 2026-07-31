package domain

import "context"

// ProcessingStage is the UX vocabulary for "where is processing right now".
// It is a read projection over the workflows' progress — never a correctness
// gate, and never a source for orders.status. The SQL CHECK on the
// projection table mirrors this set; adding a stage is a code change plus a
// migration, deliberately.
type ProcessingStage string

const (
	StageOrderCreated       ProcessingStage = "ORDER_CREATED"
	StagePaymentAuthorized  ProcessingStage = "PAYMENT_AUTHORIZED"
	StageInventoryReserved  ProcessingStage = "INVENTORY_RESERVED"
	StageShipmentPrepared   ProcessingStage = "SHIPMENT_PREPARED"
	StagePaymentCaptured    ProcessingStage = "PAYMENT_CAPTURED"
	StageOrderConfirmed     ProcessingStage = "ORDER_CONFIRMED"
	StagePostProcessing     ProcessingStage = "POST_PROCESSING"
	StageInventoryCommitted ProcessingStage = "INVENTORY_COMMITTED"
	StageCompensating       ProcessingStage = "COMPENSATING"
	StageCancelling         ProcessingStage = "CANCELLING"
	StageManualReview       ProcessingStage = "MANUAL_REVIEW"
	StageDone               ProcessingStage = "DONE"
)

// ProcessingUpdate is one projection write: the stage the order just
// reached, optionally the step that got it there and a bounded error token.
type ProcessingUpdate struct {
	OrderID string
	Stage   ProcessingStage
	// LastStep is the bounded activity-name token that completed, empty when
	// the stage isn't tied to one (ORDER_CREATED, POST_PROCESSING,
	// COMPENSATING, and the terminal stages, which preserve the last
	// business step via COALESCE).
	LastStep string
	// LastErrorCode is a bounded reason token, never a message.
	LastErrorCode string
}

// ProcessingProjector is the projection's write surface. Best-effort by
// contract: callers swallow its errors (counted, logged) — a projection
// write must never fail or stall a money-bearing path.
type ProcessingProjector interface {
	// UpsertProcessingStage records the order's current stage.
	UpsertProcessingStage(ctx context.Context, u ProcessingUpdate) error
	// UpsertProcessingStageWithTx is the one transactional exception:
	// ORDER_CREATED is seeded in the same transaction as the order row, so
	// the projection exists from the order's first moment.
	UpsertProcessingStageWithTx(ctx context.Context, tx Transaction, u ProcessingUpdate) error
}

// ProcessingState is the projection as read back for /details.
type ProcessingState struct {
	Stage         ProcessingStage
	LastStep      string
	LastErrorCode string
	UpdatedAt     string
}
