package domain

import (
	"context"
	"time"
)

// CancellationRequest is one cancellation-outbox row: a durable record that
// an order flipped to `cancelling` and its cancellation workflow still has
// to be started. Same shape as FulfillmentStartRequest minus the payment
// token — the cancellation workflow reads every external state server-side.
type CancellationRequest struct {
	OrderID string
	Status  string
	// Epoch is the orders.version observed when the episode was requested;
	// it namespaces the workflow id and the episode's command ids.
	Epoch         int64
	CreatedAt     time.Time
	Attempts      int
	NextAttemptAt time.Time
	LastErrorCode string
}

// CancellationRequestStore is the cancellation outbox's persistence surface.
// The lease semantics mirror StartRequestRepository: ClaimDue pushes
// next_attempt_at forward in the claiming statement, so no transaction stays
// open across a Temporal call.
type CancellationRequestStore interface {
	// ArmWithTx writes (or re-arms) the PENDING row inside the caller's
	// transaction — the same one that applies the confirmed→cancelling CAS.
	// Re-arming an existing row resets it for the new epoch: a second legal
	// episode reuses the PK slot.
	ArmWithTx(ctx context.Context, tx Transaction, orderID string, epoch int64) error

	// MarkDispatched closes the row once a workflow with THIS EPISODE's id
	// exists. Epoch-qualified: a dispatcher holding a stale lease for a
	// previous epoch must not close a row that was re-armed for a new one.
	MarkDispatched(ctx context.Context, orderID string, epoch int64) error

	// ClaimDue atomically leases up to limit due PENDING rows.
	ClaimDue(ctx context.Context, limit int, lease time.Duration) ([]CancellationRequest, error)

	// Reschedule keeps a row PENDING and due at next, recording the failure.
	// Epoch-qualified like MarkDispatched.
	Reschedule(ctx context.Context, orderID string, epoch int64, next time.Time, errCode string) error

	// MarkFailed makes the row terminal — a human owns it from here (the
	// order sits in `cancelling` and the stuck-cancelling alert fires).
	// Epoch- and status-qualified: a stale claim reaching its cap must not
	// flip a healthy DISPATCHED or re-armed row to FAILED.
	MarkFailed(ctx context.Context, orderID string, epoch int64, errCode string) error

	// Stats backs the cancellation-outbox gauges.
	Stats(ctx context.Context) (CancellationRequestStats, error)
}

// CancellationRequestStats is the outbox's observable state.
type CancellationRequestStats struct {
	Pending          int
	Failed           int
	OldestPendingAge time.Duration
}

// CancellationCloser is the ONLY cancellation-outbox operation the request
// path gets, and it is scoped by user — the same IDOR reasoning as
// StartRequestCloser: the handler that inline-starts the workflow must not
// hold an unscoped close.
type CancellationCloser interface {
	// CloseDispatchedForUser closes the row only if the order belongs to
	// userID AND the row still carries this episode's epoch. (Named
	// differently from StartRequestCloser's method on purpose — identical
	// method sets would make the two interfaces satisfy each other, and a
	// wiring transposition of the two outboxes must not compile.)
	CloseDispatchedForUser(ctx context.Context, userID, orderID string, epoch int64) error
}

// OrderStatusTxWriter applies a status command inside the caller's
// transaction. Separate from OrderStatusWriter by name so only code that
// genuinely composes the CAS with another write (the cancel path's outbox
// arm) reaches for it.
type OrderStatusTxWriter interface {
	ApplyStatusCommandWithTx(ctx context.Context, tx Transaction, cmd StatusCommand) (replayed bool, err error)
}
