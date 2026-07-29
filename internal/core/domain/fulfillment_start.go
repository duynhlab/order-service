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

	// PaymentMethodCleared records that this row once had a token which has been
	// cleared, so an empty PaymentMethod can be told apart from an order that
	// never had one. A cleared row must not be dispatched: the saga would fall
	// back to the demo payment token.
	PaymentMethodCleared bool

	// CreatedAt is when the order committed. The dispatcher refuses rows older
	// than the workflow-id dedup window — see the dispatcher for why that is a
	// money guarantee, not tidiness.
	CreatedAt time.Time

	// Attempts counts CLAIMS, not failures: it is incremented when a row is
	// claimed, so a dispatcher that dies mid-dispatch still burns an attempt
	// and a poison row cannot be retried forever.
	Attempts int

	NextAttemptAt time.Time

	// LastErrorCode is a bounded token (a grpcx reason or a Temporal error
	// type), never an error message — the column is for grouping, not prose.
	LastErrorCode string

	// Participant is the stock owner the API decided when the order committed.
	// The dispatcher starts the saga with THIS rather than with its own copy of
	// the flag: API and worker are separate Deployments, so a cutover rolls them
	// at different times, and the process that stamps the row must be the process
	// that decides. Otherwise the row and the saga disagree — and the reconciler,
	// which trusts the row to tell a product-path order from a lost reserve, files
	// a false breach in one direction and silently swallows a real one in the
	// other. Empty for rows written before the column existed (all product-path).
	Participant string
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
	EnqueueWithTx(ctx context.Context, tx Transaction, orderID, paymentMethod, participant string) error

	// MarkDispatched closes the row out and clears the payment token. Unscoped —
	// only the dispatcher uses it, and the dispatcher has no user context.
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

// StartRequestCloser is the ONLY outbox operation the request path gets, and it
// is scoped by user.
//
// The logic layer previously held the whole StartRequestRepository — ClaimDue,
// MarkFailed, Reschedule, Stats, all unscoped and all keyed by a bare order id —
// and exposed an unscoped close on a type the web handler holds concretely. That
// is the same hazard OrderLoader's doc warns about, applied to WRITES. The
// obvious next feature is a "retry my fulfillment" endpoint (FAILED rows need a
// human, and no tooling ships with them); handed a path parameter, an unscoped
// close would let any authenticated user flip a victim's row to DISPATCHED, NULL
// its payment token, and drop it from both the worklist and the failed gauge —
// leaving a saga that can never be started, because the token was the one input
// that cannot be rebuilt.
//
// Both call sites already have the user id, so scoping costs nothing.
type StartRequestCloser interface {
	// MarkDispatchedForUser closes the row only if the order belongs to userID.
	MarkDispatchedForUser(ctx context.Context, userID, orderID string) error
}

// ReconcileCandidate is a terminal order whose stock has not been confirmed to
// agree with its outcome (RFC-0021 P3).
type ReconcileCandidate struct {
	OrderID string
	// Status is the order's terminal status — confirmed or failed. It decides
	// which repair is correct: a confirmed order's reservation must end
	// COMMITTED, a failed order's must be RELEASED.
	Status string
	// Participant is which service owns this order's stock. Without it a missing
	// reservation is ambiguous: normal for the product path, an invariant breach
	// for a confirmed inventory-path order. Empty for rows written before the
	// column existed, which are all product-path by definition.
	Participant string
	// BreachCode is set if a previous pass already reported this row as
	// unrepairable, so the reconciler can report it once rather than once per
	// pass.
	BreachCode string
}

// ReconcileStore is the reconciler's whole persistence surface.
//
// Separate from OrderRepository for the same reason as OrderLoader: it reads and
// writes across ALL users, which is exactly the capability that must not sit on
// the interface the request path holds.
//
// The window is expressed as durations rather than instants on purpose. The
// bounds are then evaluated by the DATABASE against its own clock, so the scan
// does not depend on the pod's clock agreeing with the database's, nor on
// orders.updated_at (a tz-naive column) having been written in the same timezone
// the reconciler happens to run in.
type ReconcileStore interface {
	// ListForReconcile returns unsettled terminal orders whose status last
	// changed at least settleDelay ago and at most window ago. Rows with no
	// recorded breach come first, so known-unrepairable ones cannot starve fresh
	// work, then oldest-first within each group.
	ListForReconcile(ctx context.Context, settleDelay, window time.Duration, limit int) ([]ReconcileCandidate, error)

	// CountUnreconciled returns how many terminal orders are still unsettled. This
	// is the backlog: read from the table, so it cannot drift, stick after a failed
	// pass, or reset when a process restarts.
	//
	// Deliberately NOT bounded by the scan's window. An unrepairable breach stays
	// unsettled by design, so a windowed count would return to zero 24h later while
	// stock was still consumed against an order that never happened — the same
	// "backlog reads zero while something is wrong" failure the settle column was
	// added to remove, on a 24h fuse. It would also make the kill switch destructive
	// rather than passive: with the reconciler off for longer than the window, every
	// affected order would vanish from the gauge as well as from the scan.
	//
	// settleDelay is still applied: an order that reached a terminal status seconds
	// ago is not a backlog item, it is a saga in progress.
	CountUnreconciled(ctx context.Context, settleDelay time.Duration) (int, error)

	// MarkReconciled records that this order's stock agrees with its outcome —
	// whether it already did, or a repair made it so. It also clears any recorded
	// breach, since the disagreement is gone.
	MarkReconciled(ctx context.Context, orderID string) error

	// MarkReconcileBreach records a disagreement no valid transition can repair.
	// The row stays unsettled deliberately: it is still inconsistent.
	MarkReconcileBreach(ctx context.Context, orderID, code string) error
}
