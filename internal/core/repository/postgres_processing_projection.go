package repository

import (
	"context"
	"errors"
	"time"

	"github.com/duynhlab/order-service/internal/core/domain"
	"github.com/jackc/pgx/v5"
)

// upsertProjectionSQL is one statement so a stage write is atomic and
// last-writer-wins: the projection answers "where is processing NOW", so
// there is nothing to merge — the newest boundary is the truth.
//
// last_error_code is overwritten (or cleared) by every write on purpose: a
// stale error from a compensation two stages ago must not outlive the stage
// that superseded it. last_successful_step keeps its previous value when the
// stage isn't tied to a step (COALESCE), so COMPENSATING still shows the
// last thing that worked.
// The WHERE clause is the anti-zombie guard: a terminal row (DONE,
// MANUAL_REVIEW) can only be overwritten by another terminal stage or by
// CANCELLING (a new episode legally reopens a DONE order). A late-landing
// duplicate of an earlier boundary — an activity attempt that timed out
// client-side while its statement was still in flight — can therefore go
// stale-lost at worst, never regress a settled row.
const upsertProjectionSQL = `
	INSERT INTO order_processing_projection (order_id, stage, last_successful_step, last_error_code, updated_at)
	VALUES ($1, $2, NULLIF($3, ''), NULLIF($4, ''), now())
	ON CONFLICT (order_id) DO UPDATE SET
		stage                = EXCLUDED.stage,
		last_successful_step = COALESCE(EXCLUDED.last_successful_step, order_processing_projection.last_successful_step),
		last_error_code      = EXCLUDED.last_error_code,
		updated_at           = now()
	WHERE order_processing_projection.stage NOT IN ('DONE', 'MANUAL_REVIEW')
	   OR EXCLUDED.stage IN ('DONE', 'MANUAL_REVIEW', 'CANCELLING')
`

// UpsertProcessingStage records the order's current processing stage.
// Best-effort by contract — the caller counts and swallows the error.
func (r *PostgresOrderRepository) UpsertProcessingStage(ctx context.Context, u domain.ProcessingUpdate) error {
	_, err := r.pool.Exec(ctx, upsertProjectionSQL, u.OrderID, string(u.Stage), u.LastStep, u.LastErrorCode)
	return err
}

// UpsertProcessingStageWithTx seeds ORDER_CREATED inside the order's own
// transaction — the one place the projection IS transactional, so the row
// exists from the order's first moment and /details never renders a created
// order with no processing block.
func (r *PostgresOrderRepository) UpsertProcessingStageWithTx(ctx context.Context, tx domain.Transaction, u domain.ProcessingUpdate) error {
	pgxTx, ok := tx.(*PostgresTransaction)
	if !ok {
		return errors.New("invalid transaction type")
	}
	_, err := pgxTx.tx.Exec(ctx, upsertProjectionSQL, u.OrderID, string(u.Stage), u.LastStep, u.LastErrorCode)
	return err
}

// ReadProcessingState returns the projection row for /details, or
// domain.ErrNotFound when the order predates the projection.
func (r *PostgresOrderRepository) ReadProcessingState(ctx context.Context, orderID string) (*domain.ProcessingState, error) {
	var st domain.ProcessingState
	var step, errCode *string
	var updatedAt time.Time
	err := r.pool.QueryRow(ctx, `
		SELECT stage, last_successful_step, last_error_code, updated_at
		FROM order_processing_projection WHERE order_id = $1
	`, orderID).Scan(&st.Stage, &step, &errCode, &updatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if step != nil {
		st.LastStep = *step
	}
	if errCode != nil {
		st.LastErrorCode = *errCode
	}
	st.UpdatedAt = updatedAt.UTC().Format(time.RFC3339)
	return &st, nil
}
