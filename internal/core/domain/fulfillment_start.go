package domain

import (
	"context"
	"time"
)

// Outbox statuses for a fulfillment start request (RFC-0021 P3).
const (
	// StartRequestPending is written in the order's own transaction: the order
	// exists and its saga has not been confirmed started yet.
	StartRequestPending = "PENDING"
	// StartRequestDispatched means a workflow with this order's id exists —
	// either the inline start succeeded or the dispatcher started it.
	StartRequestDispatched = "DISPATCHED"
	// StartRequestFailed is terminal and human-owned: the dispatcher gave up
	// after the attempt cap, so the row is a worklist item rather than something
	// the platform keeps retrying forever. Its payment token is cleared, so a
	// requeue is a decision to start a saga without one (the demo fallback) —
	// which is usually the wrong call two hours after the fact. Failing the
	// order is normally the right operator action.
	StartRequestFailed = "FAILED"
)

// FulfillmentStartRequest is one outbox row: a durable record that an order
// committed and its fulfillment saga still has to be started.
//
// It exists because the order row and the workflow start cannot commit
// together — Temporal is not in the database transaction. Writing this row
// inside the order's transaction moves the atomicity to something we do
// control, and turns "the process died between COMMIT and ExecuteWorkflow"
// from a permanently `pending` order into a retry.
type FulfillmentStartRequest struct {
	OrderID string
	Status  string

	// PaymentMethod is the checkout's opaque token, carried only until the row
	// reaches a terminal state, because it is deliberately not a column on
	// orders and the dispatcher cannot rebuild it. Empty on both DISPATCHED and
	// FAILED rows — a terminal row must not hold a payment token indefinitely.
	PaymentMethod string

	// Attempts counts CLAIMS, not failures: it is incremented when a row is
	// claimed, so a dispatcher that dies mid-dispatch still burns an attempt
	// and a poison row cannot be retried forever.
	Attempts int

	NextAttemptAt time.Time

	// LastErrorCode is a bounded token (a grpcx reason or a Temporal error
	// type), never an error message — the column is for grouping, not prose.
	LastErrorCode string
}

// StartRequestRepository is the outbox's persistence surface.
//
// Claiming uses a LEASE rather than a held row lock: ClaimDue pushes
// next_attempt_at into the future in the same statement that selects the row,
// so no transaction stays open across a Temporal call, and a dispatcher that
// crashes simply lets the lease expire.
type StartRequestRepository interface {
	// EnqueueWithTx writes the PENDING row inside the caller's transaction —
	// the same one that inserts the order. Enqueuing twice for one order is a
	// no-op rather than an error: the order id is the primary key, and a
	// retried create must not fail because its outbox row already exists.
	EnqueueWithTx(ctx context.Context, tx Transaction, orderID, paymentMethod string) error

	// MarkDispatched closes the row out and clears the payment token.
	MarkDispatched(ctx context.Context, orderID string) error

	// ClaimDue atomically leases up to limit due PENDING rows: it increments
	// attempts and pushes next_attempt_at out by lease, returning what it
	// claimed. Concurrent dispatchers never claim the same row (SKIP LOCKED).
	ClaimDue(ctx context.Context, limit int, lease time.Duration) ([]FulfillmentStartRequest, error)

	// Reschedule keeps a row PENDING and due at next, recording why the last
	// attempt failed.
	Reschedule(ctx context.Context, orderID string, next time.Time, errCode string) error

	// MarkFailed makes the row terminal — nothing retries it automatically.
	MarkFailed(ctx context.Context, orderID, errCode string) error

	// Stats backs the outbox gauges: how many starts are outstanding, how old
	// the oldest one is, and how many need a human.
	Stats(ctx context.Context) (StartRequestStats, error)
}

// StartRequestStats is the outbox's observable state.
type StartRequestStats struct {
	Pending int
	Failed  int
	// OldestPendingAge is zero when nothing is pending, which is the normal
	// state: the inline start handles almost every order.
	OldestPendingAge time.Duration
}

// OrderLoader reads an order WITHOUT a user scope.
//
// It is deliberately a separate interface from OrderRepository rather than
// another method on it. The dispatcher acts on an order id it read from the
// outbox and has no user context, so it genuinely needs an unscoped read — but
// every request-path handler holds OrderRepository, and an unscoped read sitting
// on that interface is an IDOR waiting for someone to reach for the convenient
// method. Keeping it here means the only code that can perform the read is code
// that asked for this interface by name.
type OrderLoader interface {
	// LoadForFulfillment returns the order with its items, or ErrNotFound.
	LoadForFulfillment(ctx context.Context, orderID string) (*Order, error)
}
