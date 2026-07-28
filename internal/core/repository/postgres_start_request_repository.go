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
// calls it best-effort, and a row already DISPATCHED by the dispatcher is a
// success, not a conflict.
func (r *PostgresStartRequestRepository) MarkDispatched(ctx context.Context, orderID string) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE fulfillment_start_requests
		SET status = $2, payment_method = NULL, last_error_code = NULL, updated_at = now()
		WHERE order_id = $1 AND status <> $2
	`, orderID, domain.StartRequestDispatched)
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
		    next_attempt_at = now() + $3::interval,
		    updated_at = now()
		WHERE order_id IN (
			SELECT order_id
			FROM fulfillment_start_requests
			WHERE status = $1 AND next_attempt_at <= now()
			ORDER BY next_attempt_at
			LIMIT $2
			FOR UPDATE SKIP LOCKED
		)
		RETURNING order_id, status, COALESCE(payment_method, ''), attempts,
		          next_attempt_at, COALESCE(last_error_code, '')
	`, domain.StartRequestPending, limit, lease.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var claimed []domain.FulfillmentStartRequest
	for rows.Next() {
		var req domain.FulfillmentStartRequest
		if err := rows.Scan(&req.OrderID, &req.Status, &req.PaymentMethod,
			&req.Attempts, &req.NextAttemptAt, &req.LastErrorCode); err != nil {
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
		SET status = $2, payment_method = NULL, last_error_code = $3, updated_at = now()
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
	err := r.pool.QueryRow(ctx, `
		SELECT
			count(*) FILTER (WHERE status = $1),
			count(*) FILTER (WHERE status = $2),
			max(EXTRACT(EPOCH FROM (now() - created_at))) FILTER (WHERE status = $1)
		FROM fulfillment_start_requests
	`, domain.StartRequestPending, domain.StartRequestFailed).
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
	if len(code) > errCodeMax {
		code = code[:errCodeMax]
	}
	return &code
}
