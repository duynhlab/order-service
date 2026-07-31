package repository

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/duynhlab/order-service/internal/core/domain"
)

// PostgresStartRequestRepository implements the fulfillment start outbox
// (RFC-0021 P3) on PostgreSQL.
type PostgresStartRequestRepository struct {
	pool *pgxpool.Pool
}

// NewPostgresStartRequestRepository wires the outbox to the shared pool.
func NewPostgresStartRequestRepository(pool *pgxpool.Pool) *PostgresStartRequestRepository {
	return &PostgresStartRequestRepository{pool: pool}
}

// EnqueueWithTx writes the PENDING row in the order's own transaction.
//
// ON CONFLICT DO NOTHING rather than an error: the order id is the primary key,
// and a create that is retried after a partial failure must not be turned into
// a 500 by an outbox row that is already correct. It also means enqueuing is
// idempotent for free.
func (r *PostgresStartRequestRepository) EnqueueWithTx(ctx context.Context, tx domain.Transaction, orderID, paymentMethod, participant string) error {
	pgxTx, ok := tx.(*PostgresTransaction)
	if !ok {
		return errors.New("invalid transaction type")
	}

	// NULL rather than "" for an absent token, so "no token" is distinguishable
	// from "token cleared" in the column itself.
	var token *string
	if paymentMethod != "" {
		token = &paymentMethod
	}

	return pgxTx.Exec(ctx, `
		INSERT INTO fulfillment_start_requests (order_id, status, payment_method, participant)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (order_id) DO NOTHING
	`, orderID, domain.StartRequestPending, token, participant)
}

// MarkDispatched closes the row and drops the payment token. It is idempotent
// and deliberately does not care whether a row was updated: the inline start
// calls it best-effort, and a row already DISPATCHED is a success, not a
// conflict.
//
// It only ever moves a row out of PENDING. Matching status <> 'DISPATCHED'
// would also match FAILED, and a FAILED row's order is by definition still
// `pending` — so any CreateOrder replay for that order (the documented purpose
// of the idempotency key) would reach here and silently erase the worklist item:
// the failed gauge drops to zero, the row looks dispatched, and no workflow was
// ever started.
func (r *PostgresStartRequestRepository) MarkDispatched(ctx context.Context, orderID string) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE fulfillment_start_requests
		SET status = $2,
		    payment_method_cleared = payment_method_cleared OR payment_method IS NOT NULL,
		    payment_method = NULL,
		    last_error_code = NULL,
		    updated_at = now()
		WHERE order_id = $1 AND status = $3
	`, orderID, domain.StartRequestDispatched, domain.StartRequestPending)
	return err
}

// MarkDispatchedForUser is MarkDispatched restricted to an order the user owns.
// The request path gets this one and nothing else, so a future endpoint that
// takes an order id from the URL cannot close a stranger's row.
func (r *PostgresStartRequestRepository) MarkDispatchedForUser(ctx context.Context, userID, orderID string) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE fulfillment_start_requests f
		SET status = $2,
		    payment_method_cleared = f.payment_method_cleared OR f.payment_method IS NOT NULL,
		    payment_method = NULL,
		    last_error_code = NULL,
		    updated_at = now()
		WHERE f.order_id = $1
		  AND f.status = $3
		  AND EXISTS (SELECT 1 FROM orders o WHERE o.id = f.order_id AND o.user_id = $4)
	`, orderID, domain.StartRequestDispatched, domain.StartRequestPending, userID)
	return err
}

// ClaimDue leases due PENDING rows in a single statement.
//
// The inner SELECT ... FOR UPDATE SKIP LOCKED picks the rows; the UPDATE
// increments attempts and pushes next_attempt_at out by lease. Two properties
// follow: a second dispatcher cannot claim the same row, and a dispatcher that
// dies after claiming still burned an attempt — so a row that poisons the
// dispatcher walks its way to the attempt cap instead of retrying forever.
func (r *PostgresStartRequestRepository) ClaimDue(ctx context.Context, limit int, lease time.Duration) ([]domain.FulfillmentStartRequest, error) {
	rows, err := r.pool.Query(ctx, `
		UPDATE fulfillment_start_requests
		SET attempts = attempts + 1,
		    next_attempt_at = now() + make_interval(secs => $3::float8),
		    updated_at = now()
		WHERE order_id IN (
			SELECT order_id
			FROM fulfillment_start_requests
			WHERE status = $1 AND next_attempt_at <= now()
			ORDER BY next_attempt_at
			LIMIT $2
			FOR UPDATE SKIP LOCKED
		)
		-- Repeated deliberately. The locked id set already excludes rows another
		-- dispatcher took, so this is redundant under EvalPlanQual semantics —
		-- but it makes the guarantee readable instead of resting on a subtlety.
		AND status = $1 AND next_attempt_at <= now()
		RETURNING order_id, status, COALESCE(payment_method, ''), payment_method_cleared,
		          attempts, next_attempt_at, created_at, COALESCE(last_error_code, ''),
		          COALESCE(participant, '')
	`, domain.StartRequestPending, limit, lease.Seconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var claimed []domain.FulfillmentStartRequest
	for rows.Next() {
		var req domain.FulfillmentStartRequest
		if err := rows.Scan(&req.OrderID, &req.Status, &req.PaymentMethod, &req.PaymentMethodCleared,
			&req.Attempts, &req.NextAttemptAt, &req.CreatedAt, &req.LastErrorCode,
			&req.Participant); err != nil {
			return nil, err
		}
		claimed = append(claimed, req)
	}
	return claimed, rows.Err()
}

// Reschedule keeps the row PENDING and due at next.
func (r *PostgresStartRequestRepository) Reschedule(ctx context.Context, orderID string, next time.Time, errCode string) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE fulfillment_start_requests
		SET next_attempt_at = $2, last_error_code = $3, updated_at = now()
		WHERE order_id = $1 AND status = $4
	`, orderID, next, truncateErrCode(errCode), domain.StartRequestPending)
	return err
}

// MarkFailed makes the row terminal and clears the payment token.
//
// Nothing retries a FAILED row: it is a worklist item, and the runbook's requeue
// is a deliberate flip back to PENDING by a human who has read
// last_error_code. The token is dropped anyway, because a FAILED row can sit
// indefinitely and a payment token is not something to keep indefinitely. By the
// time a row reaches the attempt cap — roughly two hours of retries — the
// authorization window has almost certainly passed, so the honest operator
// action is to fail the order and let the customer retry, not to start a saga
// hours late against a stale token.
func (r *PostgresStartRequestRepository) MarkFailed(ctx context.Context, orderID, errCode string) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE fulfillment_start_requests
		SET status = $2,
		    payment_method_cleared = payment_method_cleared OR payment_method IS NOT NULL,
		    payment_method = NULL,
		    last_error_code = $3,
		    updated_at = now()
		WHERE order_id = $1 AND status = $4
	`, orderID, domain.StartRequestFailed, truncateErrCode(errCode), domain.StartRequestPending)
	return err
}

// Stats reads the outbox's observable state in one round trip.
func (r *PostgresStartRequestRepository) Stats(ctx context.Context) (domain.StartRequestStats, error) {
	var (
		stats     domain.StartRequestStats
		oldestSec *float64
	)
	// Restricted to the OPEN statuses so the partial index serves it. Without the
	// filter this is a sequential scan of every row ever written — one per order
	// for the lifetime of the platform, since nothing deletes DISPATCHED rows —
	// on every metrics collection cycle, sharing the worker's pool.
	err := r.pool.QueryRow(ctx, `
		SELECT
			count(*) FILTER (WHERE status = $1),
			count(*) FILTER (WHERE status = $2),
			max(EXTRACT(EPOCH FROM (now() - created_at))) FILTER (WHERE status = $1)
		FROM fulfillment_start_requests
		WHERE status <> $3
	`, domain.StartRequestPending, domain.StartRequestFailed, domain.StartRequestDispatched).
		Scan(&stats.Pending, &stats.Failed, &oldestSec)
	if err != nil {
		return domain.StartRequestStats{}, err
	}
	if oldestSec != nil {
		stats.OldestPendingAge = time.Duration(*oldestSec * float64(time.Second))
	}
	return stats, nil
}

// errCodeMax matches the column width. The callers pass bounded tokens, so this
// is a backstop against a future caller passing something longer, not the
// primary guard — a silently truncated token still groups usefully, whereas a
// 22001 from the driver would fail the reschedule and re-drive the row.
const errCodeMax = 64

func truncateErrCode(code string) *string {
	if code == "" {
		return nil
	}
	// Truncate by RUNES, not bytes: slicing bytes can split a multi-byte rune and
	// hand Postgres invalid UTF-8, which it rejects with 22021 — turning the
	// backstop into the failure it exists to prevent.
	if r := []rune(code); len(r) > errCodeMax {
		code = string(r[:errCodeMax])
	}
	return &code
}

// --- reconciler surface (RFC-0021 P3) ---
//
// The window is evaluated by the DATABASE, from durations, so it does not depend
// on the pod's clock matching the database's — which an app-computed `from`/`to`
// pair would.
//
// One residual dependency is worth naming, because it is easy to trip over:
// orders.updated_at is `TIMESTAMP` WITHOUT time zone, and production writes it
// with SQL `NOW()`, so the stored value is the writer SESSION's wall clock.
// Comparing it against a timestamptz expression casts it back with the READER
// session's TimeZone. Those agree here — both are this service's pool, with no
// per-session TimeZone override — so the window is correct. It would silently
// shift if a role-level `SET TimeZone` were ever introduced on one side, and any
// test that ages rows from Go's clock instead of the database's measures the
// offset rather than the query.
//
// Both queries drive from the PARTIAL index on unreconciled rows and join orders
// by primary key. On a healthy platform that set is nearly empty, so their cost
// tracks unsettled work rather than lifetime order volume — and no index has to
// be added to the hot orders table.

// ListForReconcile returns unsettled terminal orders in the window.
//
// orders.updated_at is nullable (000001), and a NULL would be excluded by both
// bounds — silently and permanently. It cannot happen for the rows this scans:
// reaching a terminal status means UpdateStatus ran, and that sets updated_at.
// A pending order has no such guarantee, and is not a candidate.
//
// o.id breaks ties after updated_at so a truncated pass returns the SAME slice
// twice — several orders can reach a terminal status in one statement, and a
// non-deterministic LIMIT makes a repeated pass unreproducible when debugging.
//
// Rows with no recorded breach come FIRST. A breach cannot be repaired, so it
// stays unreconciled forever until a human clears it; without this ordering
// enough of them would sit at the head of every scan and starve the fresh,
// repairable work behind them.
//
// KNOWN LIMIT: that demotion only covers breaches. Two other outcomes also leave
// a row unsettled with no breach code — a still-open saga (deferred) and an order
// whose state could not be READ — so a batch's worth of permanently-open sagas, or
// a long inventory/Temporal outage, would occupy the head of every pass until they
// age out of the window. Not fixed here because both are transient by nature and
// the fix needs a new column (a last-attempt timestamp) to order by; instead they
// are made VISIBLE: repairs_total{action="deferred"|"unreadable"} counts them and
// passes_truncated_total says the window was not fully examined.
func (r *PostgresStartRequestRepository) ListForReconcile(ctx context.Context,
	settleDelay, window time.Duration, limit int) ([]domain.ReconcileCandidate, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT o.id, o.status, COALESCE(f.participant, ''), COALESCE(f.reconcile_breach_code, '')
		FROM fulfillment_start_requests f
		JOIN orders o ON o.id = f.order_id
		WHERE f.reconciled_at IS NULL
		  AND o.status = ANY($1)
		  AND o.updated_at <  now() - make_interval(secs => $2::float8)
		  AND o.updated_at >= now() - make_interval(secs => $3::float8)
		ORDER BY (f.reconcile_breach_code IS NOT NULL), o.updated_at, o.id
		LIMIT $4
	`, []string{orderStatusConfirmed, orderStatusFailed, orderStatusCompleted}, settleDelay.Seconds(), window.Seconds(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.ReconcileCandidate
	for rows.Next() {
		var id int
		var c domain.ReconcileCandidate
		if err := rows.Scan(&id, &c.Status, &c.Participant, &c.BreachCode); err != nil {
			return nil, err
		}
		c.OrderID = strconv.Itoa(id)
		out = append(out, c)
	}
	return out, rows.Err()
}

// CountUnreconciled is the backlog. Read from the table rather than from the last
// pass's result, so it cannot stick when a pass fails, cannot read zero after a
// restart, and cannot publish a false high if a pass is interrupted mid-flight.
//
// UNWINDOWED, unlike the scan — see the interface doc. The scan's window bounds
// how long to keep RE-ATTEMPTING; it must not bound what gets REPORTED, or an
// unresolved breach would quietly age out of the gauge after 24h.
func (r *PostgresStartRequestRepository) CountUnreconciled(ctx context.Context,
	settleDelay time.Duration) (int, error) {
	var n int
	err := r.pool.QueryRow(ctx, `
		SELECT count(*)
		FROM fulfillment_start_requests f
		JOIN orders o ON o.id = f.order_id
		WHERE f.reconciled_at IS NULL
		  AND o.status = ANY($1)
		  AND o.updated_at < now() - make_interval(secs => $2::float8)
	`, []string{orderStatusConfirmed, orderStatusFailed, orderStatusCompleted}, settleDelay.Seconds()).Scan(&n)
	return n, err
}

// MarkReconciled settles the row and clears any recorded breach — the
// disagreement is gone, so the reason it could not be repaired is stale.
//
// reconciled_at IS NULL keeps the FIRST settlement's timestamp. Re-settling would
// be harmless for the scan (the row is already out of it) but it would overwrite
// the only answer to "when did this order's stock agree?", which is the question
// an operator asks when reconstructing an incident.
func (r *PostgresStartRequestRepository) MarkReconciled(ctx context.Context, orderID string) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE fulfillment_start_requests
		SET reconciled_at = now(), reconcile_breach_code = NULL, updated_at = now()
		WHERE order_id = $1 AND reconciled_at IS NULL
	`, orderID)
	return err
}

// MarkReconcileBreach records why a disagreement cannot be repaired, leaving the
// row unsettled so it stays in the backlog.
func (r *PostgresStartRequestRepository) MarkReconcileBreach(ctx context.Context, orderID, code string) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE fulfillment_start_requests
		SET reconcile_breach_code = $2, updated_at = now()
		WHERE order_id = $1 AND reconciled_at IS NULL
	`, orderID, truncateErrCode(code))
	return err
}

// CountOrdersInStatus backs the manual_review / stuck-cancelling backlog
// gauges. It reads orders directly (idx_orders_status) rather than joining
// the outbox: a cancelling order has no unsettled outbox row, and the gauge
// must see it anyway.
//
// olderThan uses updated_at with the same session-timezone caveat as the
// reconcile scan above: every status transition writes updated_at = NOW(),
// so age-since-last-transition is exactly the "stuck for how long" answer.
func (r *PostgresStartRequestRepository) CountOrdersInStatus(ctx context.Context,
	status string, olderThan time.Duration) (int, error) {
	var n int
	err := r.pool.QueryRow(ctx, `
		SELECT count(*) FROM orders
		WHERE status = $1
		  AND updated_at < now() - make_interval(secs => $2::float8)
	`, status, olderThan.Seconds()).Scan(&n)
	return n, err
}
