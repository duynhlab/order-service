//go:build integration

package repository

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/duynhlab/order-service/internal/core/domain"
	"github.com/jackc/pgx/v5/pgxpool"
)

// seedStatusOrder inserts a bare order row in the given status and returns
// its id. Raw INSERT on purpose: these tests exercise the writer against
// arbitrary starting states, including ones Create can no longer produce.
func seedStatusOrder(t *testing.T, pool *pgxpool.Pool, status string) string {
	t.Helper()
	var id string
	err := pool.QueryRow(context.Background(), `
		INSERT INTO orders (user_id, subtotal, shipping, total, status)
		VALUES (7, 1000, 500, 1500, $1)
		RETURNING id::text
	`, status).Scan(&id)
	if err != nil {
		t.Fatalf("seed order: %v", err)
	}
	return id
}

type orderRow struct {
	Status             string
	Version            int64
	FailureCode        *string
	CancellationReason *string
	ManualReviewReason *string
	WorkflowID         *string
	RunID              *string
	ConfirmedAt        *string
	CompletedAt        *string
	CancelledAt        *string
	UpdatedAtChanged   bool
}

func readOrderRow(t *testing.T, pool *pgxpool.Pool, id string) orderRow {
	t.Helper()
	var row orderRow
	err := pool.QueryRow(context.Background(), `
		SELECT status, version, failure_code, cancellation_reason, manual_review_reason,
		       workflow_id, run_id,
		       confirmed_at::text, completed_at::text, cancelled_at::text,
		       updated_at > created_at
		FROM orders WHERE id = $1
	`, id).Scan(&row.Status, &row.Version, &row.FailureCode, &row.CancellationReason,
		&row.ManualReviewReason, &row.WorkflowID, &row.RunID,
		&row.ConfirmedAt, &row.CompletedAt, &row.CancelledAt, &row.UpdatedAtChanged)
	if err != nil {
		t.Fatalf("read order %s: %v", id, err)
	}
	return row
}

func countHistory(t *testing.T, pool *pgxpool.Pool, id string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM order_status_history WHERE order_id = $1`, id).Scan(&n); err != nil {
		t.Fatalf("count history: %v", err)
	}
	return n
}

func TestApplyStatusCommand_Integration(t *testing.T) {
	pool := newTestDB(t)
	repo := NewPostgresOrderRepository(pool)
	ctx := context.Background()

	t.Run("confirm writes status, version, metadata, history", func(t *testing.T) {
		id := seedStatusOrder(t, pool, "pending")
		cmd, err := domain.NewConfirmCommand(id)
		if err != nil {
			t.Fatalf("constructor: %v", err)
		}
		cmd = cmd.WithWorkflowIdentity("order-fulfillment-"+id, "run-1")

		replayed, err := repo.ApplyStatusCommand(ctx, cmd)
		if err != nil || replayed {
			t.Fatalf("apply: replayed=%v err=%v", replayed, err)
		}
		row := readOrderRow(t, pool, id)
		if row.Status != "confirmed" || row.Version != 2 {
			t.Errorf("row = %+v", row)
		}
		if row.ConfirmedAt == nil || row.WorkflowID == nil || *row.WorkflowID != "order-fulfillment-"+id {
			t.Errorf("metadata missing: %+v", row)
		}
		if !row.UpdatedAtChanged {
			t.Error("updated_at must move — the reconciler window keys on it")
		}
		if n := countHistory(t, pool, id); n != 1 {
			t.Errorf("history rows = %d, want 1", n)
		}
	})

	t.Run("replaying the same command changes nothing and reports it", func(t *testing.T) {
		id := seedStatusOrder(t, pool, "pending")
		cmd, _ := domain.NewConfirmCommand(id)
		if _, err := repo.ApplyStatusCommand(ctx, cmd); err != nil {
			t.Fatalf("first apply: %v", err)
		}
		replayed, err := repo.ApplyStatusCommand(ctx, cmd)
		if err != nil || !replayed {
			t.Fatalf("replay: replayed=%v err=%v", replayed, err)
		}
		row := readOrderRow(t, pool, id)
		if row.Version != 2 {
			t.Errorf("replay must not bump version: %+v", row)
		}
		if n := countHistory(t, pool, id); n != 1 {
			t.Errorf("history rows = %d, want 1", n)
		}
	})

	t.Run("a command id reused for a different destination conflicts", func(t *testing.T) {
		id := seedStatusOrder(t, pool, "pending")
		cmd, _ := domain.NewConfirmCommand(id)
		if _, err := repo.ApplyStatusCommand(ctx, cmd); err != nil {
			t.Fatalf("first apply: %v", err)
		}
		complete, _ := domain.NewCompleteCommand(id)
		if _, err := repo.ApplyStatusCommand(ctx, complete); err != nil {
			t.Fatalf("complete: %v", err)
		}
		// Reuse the recorded confirm id but ask for a different destination —
		// a resolve-shaped id keeps the forgery past the grammar check.
		forged := cmd
		forged.To = domain.OrderStatusCancelling
		forged.ActorType = domain.ActorUser
		forged.ActorID = "7"
		forged.CommandID = "cancel:" + id + ":v1"
		if _, err := repo.ApplyStatusCommand(ctx, forged); err != nil {
			t.Fatalf("cancel: %v", err)
		}
		// Same cancel id, different destination.
		forged.To = domain.OrderStatusCancelled
		_, err := repo.ApplyStatusCommand(ctx, forged)
		if !errors.Is(err, domain.ErrInvalidInput) && !errors.Is(err, domain.ErrIdempotencyConflict) {
			t.Fatalf("got %v, want validation or idempotency conflict", err)
		}
	})

	t.Run("illegal transitions are refused from the current state", func(t *testing.T) {
		id := seedStatusOrder(t, pool, "failed")
		cmd, _ := domain.NewConfirmCommand(id)
		if _, err := repo.ApplyStatusCommand(ctx, cmd); !errors.Is(err, domain.ErrInvalidTransition) {
			t.Fatalf("confirm on failed: got %v, want ErrInvalidTransition", err)
		}
		if n := countHistory(t, pool, id); n != 0 {
			t.Errorf("refused command must leave no history, got %d", n)
		}
	})

	t.Run("actor matrix is enforced under the lock", func(t *testing.T) {
		id := seedStatusOrder(t, pool, "pending")
		// A forged USER command on a workflow-only edge passes shape checks
		// but must die on the actor matrix.
		cmd, _ := domain.NewConfirmCommand(id)
		cmd.ActorType = domain.ActorUser
		cmd.ActorID = "7"
		if _, err := repo.ApplyStatusCommand(ctx, cmd); !errors.Is(err, domain.ErrInvalidTransition) {
			t.Fatalf("user confirm: got %v, want ErrInvalidTransition", err)
		}
	})

	t.Run("malformed commands are refused before touching the row", func(t *testing.T) {
		id := seedStatusOrder(t, pool, "pending")
		cmd, _ := domain.NewFailCommand(id, domain.ReasonPaymentDeclined)
		cmd.Reason = "pq: server exploded"
		if _, err := repo.ApplyStatusCommand(ctx, cmd); !errors.Is(err, domain.ErrInvalidInput) {
			t.Fatalf("got %v, want ErrInvalidInput", err)
		}
	})

	t.Run("missing order returns ErrNotFound", func(t *testing.T) {
		cmd, _ := domain.NewConfirmCommand("999999")
		if _, err := repo.ApplyStatusCommand(ctx, cmd); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("got %v, want ErrNotFound", err)
		}
	})

	t.Run("fail and manual-review record their reasons", func(t *testing.T) {
		id := seedStatusOrder(t, pool, "pending")
		cmd, _ := domain.NewFailCommand(id, domain.ReasonInsufficientStock)
		if _, err := repo.ApplyStatusCommand(ctx, cmd); err != nil {
			t.Fatalf("fail: %v", err)
		}
		row := readOrderRow(t, pool, id)
		if row.Status != "failed" || row.FailureCode == nil || *row.FailureCode != "INSUFFICIENT_STOCK" {
			t.Errorf("row = %+v", row)
		}

		id2 := seedStatusOrder(t, pool, "pending")
		mr, _ := domain.NewMarkManualReviewCommand(id2, domain.ReasonCompensationIncomplete)
		if _, err := repo.ApplyStatusCommand(ctx, mr); err != nil {
			t.Fatalf("manual review: %v", err)
		}
		row2 := readOrderRow(t, pool, id2)
		if row2.Status != "manual_review" || row2.ManualReviewReason == nil {
			t.Errorf("row = %+v", row2)
		}
	})

	t.Run("full cancellation episode with epoch-scoped commands", func(t *testing.T) {
		id := seedStatusOrder(t, pool, "confirmed")
		row := readOrderRow(t, pool, id)

		cancel, err := domain.NewRequestCancellationCommand(id, "7", row.Version)
		if err != nil {
			t.Fatalf("constructor: %v", err)
		}
		if _, err := repo.ApplyStatusCommand(ctx, cancel); err != nil {
			t.Fatalf("cancel: %v", err)
		}
		row = readOrderRow(t, pool, id)
		if row.Status != "cancelling" || row.CancellationReason == nil || *row.CancellationReason != "CUSTOMER_REQUEST" {
			t.Errorf("row = %+v", row)
		}

		finish, err := domain.NewCompleteCancellationCommand(id, 1)
		if err != nil {
			t.Fatalf("constructor: %v", err)
		}
		if _, err := repo.ApplyStatusCommand(ctx, finish); err != nil {
			t.Fatalf("complete cancellation: %v", err)
		}
		row = readOrderRow(t, pool, id)
		if row.Status != "cancelled" || row.CancelledAt == nil || row.Version != 3 {
			t.Errorf("row = %+v", row)
		}
		if n := countHistory(t, pool, id); n != 2 {
			t.Errorf("history rows = %d, want 2", n)
		}
	})

	t.Run("operator resolve leaves manual_review with audit fields", func(t *testing.T) {
		id := seedStatusOrder(t, pool, "manual_review")
		cmd, err := domain.NewResolveManualReviewCommand(id, domain.OrderStatusFailed, "ops-1", "refund verified in provider", 1)
		if err != nil {
			t.Fatalf("constructor: %v", err)
		}
		if _, err := repo.ApplyStatusCommand(ctx, cmd); err != nil {
			t.Fatalf("resolve: %v", err)
		}
		var actorID, note string
		err = pool.QueryRow(ctx, `
			SELECT actor_id, note FROM order_status_history
			WHERE order_id = $1 AND actor_type = 'OPERATOR'
		`, id).Scan(&actorID, &note)
		if err != nil || actorID != "ops-1" || note == "" {
			t.Errorf("audit row: actor=%q note=%q err=%v", actorID, note, err)
		}
	})

	t.Run("two racing distinct commands serialize under the lock", func(t *testing.T) {
		id := seedStatusOrder(t, pool, "pending")
		confirm, _ := domain.NewConfirmCommand(id)
		fail, _ := domain.NewFailCommand(id, domain.ReasonPaymentDeclined)

		var wg sync.WaitGroup
		errs := make([]error, 2)
		wg.Add(2)
		go func() { defer wg.Done(); _, errs[0] = repo.ApplyStatusCommand(ctx, confirm) }()
		go func() { defer wg.Done(); _, errs[1] = repo.ApplyStatusCommand(ctx, fail) }()
		wg.Wait()

		var applied, refused int
		for _, err := range errs {
			switch {
			case err == nil:
				applied++
			case errors.Is(err, domain.ErrInvalidTransition):
				refused++
			default:
				t.Fatalf("unexpected error class: %v", err)
			}
		}
		if applied != 1 || refused != 1 {
			t.Fatalf("applied=%d refused=%d, want exactly one of each", applied, refused)
		}
		if n := countHistory(t, pool, id); n != 1 {
			t.Errorf("history rows = %d, want 1", n)
		}
	})

	t.Run("two racing identical commands: one applies, one replays or retries", func(t *testing.T) {
		id := seedStatusOrder(t, pool, "pending")
		cmd, _ := domain.NewConfirmCommand(id)

		var wg sync.WaitGroup
		results := make([]struct {
			replayed bool
			err      error
		}, 2)
		wg.Add(2)
		for i := 0; i < 2; i++ {
			go func(i int) {
				defer wg.Done()
				results[i].replayed, results[i].err = repo.ApplyStatusCommand(ctx, cmd)
			}(i)
		}
		wg.Wait()

		var applied, replayedOrRetryable int
		for _, r := range results {
			switch {
			case r.err == nil && !r.replayed:
				applied++
			case r.err == nil && r.replayed:
				replayedOrRetryable++
			case errors.Is(r.err, domain.ErrConcurrencyConflict):
				replayedOrRetryable++
			default:
				t.Fatalf("unexpected: %+v", r)
			}
		}
		if applied != 1 || replayedOrRetryable != 1 {
			t.Fatalf("applied=%d other=%d, want 1/1", applied, replayedOrRetryable)
		}
		row := readOrderRow(t, pool, id)
		if row.Version != 2 {
			t.Errorf("version = %d, want 2 (exactly one apply)", row.Version)
		}
	})
}
