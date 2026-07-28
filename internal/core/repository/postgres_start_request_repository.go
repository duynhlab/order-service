package repository

import (
	"context"
	"errors"
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
func (r *PostgresStartRequestRepository) EnqueueWithTx(ctx context.Context, tx domain.Transaction, orderID, paymentMethod string) error {
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
		INSERT INTO fulfillment_start_requests (order_id, status, payment_method)
		VALUES ($1, $2, $3)
		ON CONFLICT (order_id) DO NOTHING
	`, orderID, domain.StartRequestPending, token)
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
		          attempts, next_attempt_at, created_at, COALESCE(last_error_code, '')
	`, domain.StartRequestPending, limit, lease.Seconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var claimed []domain.FulfillmentStartRequest
	for rows.Next() {
		var req domain.FulfillmentStartRequest
		if err := rows.Scan(&req.OrderID, &req.Status, &req.PaymentMethod, &req.PaymentMethodCleared,
			&req.Attempts, &req.NextAttemptAt, &req.CreatedAt, &req.LastErrorCode); err != nil {
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
