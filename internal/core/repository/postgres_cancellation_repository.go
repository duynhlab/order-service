package repository

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/duynhlab/order-service/internal/core/domain"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgresCancellationRepository implements domain.CancellationRequestStore.
// It mirrors the fulfillment start outbox's lease semantics; see that
// repository for the reasoning behind each shape.
type PostgresCancellationRepository struct {
	pool *pgxpool.Pool
}

// NewPostgresCancellationRepository creates the cancellation outbox store.
func NewPostgresCancellationRepository(pool *pgxpool.Pool) *PostgresCancellationRepository {
	return &PostgresCancellationRepository{pool: pool}
}

// ArmWithTx writes (or re-arms) the PENDING row in the CAS's own transaction.
//
// ON CONFLICT resets the row rather than erroring: the PK keeps one live row
// per order, and a second legal episode (manual_review → confirmed → cancel
// again, new epoch) must reuse the slot. The CAS in the same transaction is
// what makes a stale re-arm impossible — a cancel whose transition is
// refused never commits this row.
func (r *PostgresCancellationRepository) ArmWithTx(ctx context.Context, tx domain.Transaction, orderID string, epoch int64) error {
	pgxTx, ok := tx.(*PostgresTransaction)
	if !ok {
		return errors.New("invalid transaction type")
	}
	_, err := pgxTx.tx.Exec(ctx, `
		INSERT INTO cancellation_requests (order_id, epoch)
		VALUES ($1, $2)
		ON CONFLICT (order_id) DO UPDATE SET
			status          = 'PENDING',
			epoch           = EXCLUDED.epoch,
			attempts        = 0,
			next_attempt_at = now(),
			last_error_code = NULL,
			updated_at      = now()
	`, orderID, epoch)
	return err
}

// MarkDispatched closes the row: a workflow with THIS EPISODE's id exists.
// The epoch predicate is the stale-lease guard — a claim from a previous
// episode that lands after a re-arm matches zero rows.
func (r *PostgresCancellationRepository) MarkDispatched(ctx context.Context, orderID string, epoch int64) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE cancellation_requests
		SET status = 'DISPATCHED', updated_at = now()
		WHERE order_id = $1 AND epoch = $2
	`, orderID, epoch)
	return err
}

// ClaimDue atomically leases due PENDING rows (SKIP LOCKED, same shape as
// the fulfillment outbox).
func (r *PostgresCancellationRepository) ClaimDue(ctx context.Context, limit int, lease time.Duration) ([]domain.CancellationRequest, error) {
	rows, err := r.pool.Query(ctx, `
		UPDATE cancellation_requests
		SET attempts = attempts + 1,
		    next_attempt_at = now() + make_interval(secs => $2::float8),
		    updated_at = now()
		WHERE order_id IN (
			SELECT order_id
			FROM cancellation_requests
			WHERE status = 'PENDING' AND next_attempt_at <= now()
			ORDER BY next_attempt_at
			LIMIT $1
			FOR UPDATE SKIP LOCKED
		)
		AND status = 'PENDING' AND next_attempt_at <= now()
		RETURNING order_id, status, epoch, attempts, next_attempt_at, created_at,
		          COALESCE(last_error_code, '')
	`, limit, lease.Seconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var claimed []domain.CancellationRequest
	for rows.Next() {
		var req domain.CancellationRequest
		var id int
		if err := rows.Scan(&id, &req.Status, &req.Epoch, &req.Attempts,
			&req.NextAttemptAt, &req.CreatedAt, &req.LastErrorCode); err != nil {
			return nil, err
		}
		req.OrderID = strconv.Itoa(id)
		claimed = append(claimed, req)
	}
	return claimed, rows.Err()
}

// Reschedule keeps the row PENDING and due at next.
func (r *PostgresCancellationRepository) Reschedule(ctx context.Context, orderID string, epoch int64, next time.Time, errCode string) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE cancellation_requests
		SET next_attempt_at = $3, last_error_code = $4, updated_at = now()
		WHERE order_id = $1 AND epoch = $2 AND status = 'PENDING'
	`, orderID, epoch, next, truncateErrCode(errCode))
	return err
}

// MarkFailed makes the row terminal; the order stays `cancelling` and the
// stuck-cancelling alert owns the escalation.
func (r *PostgresCancellationRepository) MarkFailed(ctx context.Context, orderID string, epoch int64, errCode string) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE cancellation_requests
		SET status = 'FAILED', last_error_code = $3, updated_at = now()
		WHERE order_id = $1 AND epoch = $2 AND status = 'PENDING'
	`, orderID, epoch, truncateErrCode(errCode))
	return err
}

// Stats backs the cancellation-outbox gauges (open rows only, like the
// fulfillment outbox).
func (r *PostgresCancellationRepository) Stats(ctx context.Context) (domain.CancellationRequestStats, error) {
	var s domain.CancellationRequestStats
	var oldest *float64
	err := r.pool.QueryRow(ctx, `
		SELECT
			count(*) FILTER (WHERE status = 'PENDING'),
			count(*) FILTER (WHERE status = 'FAILED'),
			EXTRACT(EPOCH FROM now() - min(created_at) FILTER (WHERE status = 'PENDING'))
		FROM cancellation_requests
		WHERE status <> 'DISPATCHED'
	`).Scan(&s.Pending, &s.Failed, &oldest)
	if err != nil {
		return domain.CancellationRequestStats{}, err
	}
	if oldest != nil {
		s.OldestPendingAge = time.Duration(*oldest * float64(time.Second))
	}
	return s, nil
}

// CloseDispatchedForUser closes the row only when the order belongs to
// userID — the request path's scoped close (see domain.CancellationCloser).
func (r *PostgresCancellationRepository) CloseDispatchedForUser(ctx context.Context, userID, orderID string, epoch int64) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE cancellation_requests cr
		SET status = 'DISPATCHED', updated_at = now()
		FROM orders o
		WHERE cr.order_id = $2 AND cr.epoch = $3 AND o.id = cr.order_id AND o.user_id = $1
	`, userID, orderID, epoch)
	return err
}
