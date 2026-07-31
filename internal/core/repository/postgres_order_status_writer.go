package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/duynhlab/order-service/internal/core/domain"
	"github.com/jackc/pgx/v5"
)

// ApplyStatusCommand is the order aggregate's only status write (RFC-0021
// P5). One transaction, under a row lock, in this order:
//
//  1. re-validate the command (constructors are convenience, this is the
//     boundary — commands cross the Temporal payload boundary),
//  2. SELECT ... FOR UPDATE the order row,
//  3. replay check by (order_id, command_id) against the history,
//  4. FSM + actor validation against the CURRENT row state,
//  5. append the history row,
//  6. the guarded UPDATE: status, version+1, updated_at, and the
//     per-transition metadata columns.
//
// The row lock is what lets three writer classes (workflow activities, the
// user cancel path, operator resolves) interleave without an application
// retry loop: whoever holds the lock sees settled state, everyone else
// queues. The version column still increments on every transition — the
// HTTP paths use it as the cancellation-episode epoch — but the lock, not
// the version predicate, is what serializes writers.
//
// updated_at is written unconditionally: the reconciler's scan window keys
// on it (see postgres_start_request_repository.go), and a terminal
// transition that skipped it would silently hide the order from settlement.
func (r *PostgresOrderRepository) ApplyStatusCommand(ctx context.Context, cmd domain.StatusCommand) (bool, error) {
	if err := cmd.Validate(); err != nil {
		return false, err
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin status command %s: %w", cmd.CommandID, err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback after commit is a no-op

	var current string
	var version int64
	err = tx.QueryRow(ctx,
		`SELECT status, version FROM orders WHERE id = $1 FOR UPDATE`,
		cmd.OrderID,
	).Scan(&current, &version)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, domain.ErrNotFound
	}
	if err != nil {
		return false, fmt.Errorf("lock order %s: %w", cmd.OrderID, err)
	}

	// Replay check: a command id that already has a history row was applied
	// once; same destination means "already done", a different one means the
	// caller is trying to reuse an idempotency anchor for different work.
	var recordedTo string
	err = tx.QueryRow(ctx,
		`SELECT to_status FROM order_status_history WHERE order_id = $1 AND command_id = $2`,
		cmd.OrderID, cmd.CommandID,
	).Scan(&recordedTo)
	switch {
	case err == nil:
		if recordedTo == string(cmd.To) {
			return true, tx.Commit(ctx)
		}
		return false, fmt.Errorf("command %s recorded %s, asked %s: %w",
			cmd.CommandID, recordedTo, cmd.To, domain.ErrIdempotencyConflict)
	case errors.Is(err, pgx.ErrNoRows):
		// Fresh command; fall through.
	default:
		return false, fmt.Errorf("replay check %s: %w", cmd.CommandID, err)
	}

	from := domain.OrderStatus(current)
	if !domain.CanTransition(from, cmd.To) {
		return false, fmt.Errorf("order %s is %s: %w", cmd.OrderID, current, domain.ErrInvalidTransition)
	}
	if !domain.ActorAllowed(cmd.ActorType, from, cmd.To) {
		return false, fmt.Errorf("actor %s may not drive %s → %s: %w",
			cmd.ActorType, current, cmd.To, domain.ErrInvalidTransition)
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO order_status_history
			(order_id, from_status, to_status, reason_code, actor_type, actor_id, note, command_id)
		VALUES ($1, $2, $3, NULLIF($4, ''), $5, NULLIF($6, ''), NULLIF($7, ''), $8)
	`, cmd.OrderID, current, string(cmd.To), string(cmd.Reason),
		string(cmd.ActorType), cmd.ActorID, cmd.Note, cmd.CommandID)
	if err != nil {
		if isUniqueViolation(err) {
			// Two connections raced the same fresh command and the other one
			// held the insert; retrying re-reads and replays.
			return false, fmt.Errorf("command %s raced: %w", cmd.CommandID, domain.ErrConcurrencyConflict)
		}
		return false, fmt.Errorf("record history %s: %w", cmd.CommandID, err)
	}

	// The row is locked, so the predicates cannot miss; 0 rows here is a
	// broken invariant, not a lost race.
	result, err := tx.Exec(ctx, updateForTransition(cmd.To),
		cmd.OrderID, current, version, string(cmd.To),
		string(cmd.Reason), cmd.WorkflowID, cmd.RunID)
	if err != nil {
		return false, fmt.Errorf("apply %s: %w", cmd.CommandID, err)
	}
	if result.RowsAffected() != 1 {
		return false, fmt.Errorf("apply %s touched %d rows under lock: %w",
			cmd.CommandID, result.RowsAffected(), domain.ErrConcurrencyConflict)
	}

	return false, tx.Commit(ctx)
}

// updateForTransition returns the guarded UPDATE for a destination status.
// Static SQL per destination — nothing caller-controlled is concatenated.
// Every variant takes the same 7 parameters so the caller stays uniform:
// $1 id, $2 expected status, $3 expected version, $4 new status, $5 reason,
// $6 workflow id, $7 run id (unused ones are discarded via a no-op predicate).
func updateForTransition(to domain.OrderStatus) string {
	const guard = ` WHERE id = $1 AND status = $2 AND version = $3`
	base := `UPDATE orders SET status = $4, version = version + 1, updated_at = NOW()`
	switch to {
	case domain.OrderStatusConfirmed:
		return base + `, confirmed_at = NOW(),
			workflow_id = COALESCE(NULLIF($6, ''), workflow_id),
			run_id = COALESCE(NULLIF($7, ''), run_id),
			failure_code = NULL, manual_review_reason = NULL` + guard + ` AND $5 = $5`
	case domain.OrderStatusCompleted:
		return base + `, completed_at = NOW()` + guard + ` AND $5 = $5 AND $6 = $6 AND $7 = $7`
	case domain.OrderStatusFailed:
		return base + `, failure_code = NULLIF($5, ''), manual_review_reason = NULL` +
			guard + ` AND $6 = $6 AND $7 = $7`
	case domain.OrderStatusManualReview:
		return base + `, manual_review_reason = NULLIF($5, '')` + guard + ` AND $6 = $6 AND $7 = $7`
	case domain.OrderStatusCancelling:
		return base + `, cancellation_reason = NULLIF($5, '')` + guard + ` AND $6 = $6 AND $7 = $7`
	case domain.OrderStatusCancelled:
		return base + `, cancelled_at = NOW()` + guard + ` AND $5 = $5 AND $6 = $6 AND $7 = $7`
	case domain.OrderStatusPending:
		// No edge lands on pending; Validate() and the FSM refuse it long
		// before this switch, so reaching here is a programming error.
		panic("updateForTransition: pending is never a destination")
	default:
		// Validate() guarantees a known destination; keep the invariant loud.
		panic(fmt.Sprintf("updateForTransition: unknown status %q", to))
	}
}
