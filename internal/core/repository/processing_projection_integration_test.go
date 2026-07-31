//go:build integration

package repository

import (
	"context"
	"errors"
	"testing"

	"github.com/duynhlab/order-service/internal/core/domain"
)

func TestProcessingProjection_Integration(t *testing.T) {
	pool := newTestDB(t)
	repo := NewPostgresOrderRepository(pool)
	ctx := context.Background()

	t.Run("upsert is last-writer-wins and keeps the last successful step", func(t *testing.T) {
		id := seedStatusOrder(t, pool, "pending")

		if err := repo.UpsertProcessingStage(ctx, domain.ProcessingUpdate{
			OrderID: id, Stage: domain.StagePaymentAuthorized, LastStep: "AUTHORIZE_PAYMENT",
		}); err != nil {
			t.Fatalf("first upsert: %v", err)
		}
		// COMPENSATING carries no step of its own: the previous step must
		// survive so the row still says what last worked.
		if err := repo.UpsertProcessingStage(ctx, domain.ProcessingUpdate{
			OrderID: id, Stage: domain.StageCompensating, LastErrorCode: "INSUFFICIENT_STOCK",
		}); err != nil {
			t.Fatalf("second upsert: %v", err)
		}

		st, err := repo.ReadProcessingState(ctx, id)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if st.Stage != domain.StageCompensating || st.LastStep != "AUTHORIZE_PAYMENT" || st.LastErrorCode != "INSUFFICIENT_STOCK" {
			t.Errorf("state = %+v", st)
		}

		// A later stage clears the error — a stale code must not outlive the
		// stage that superseded it.
		if err := repo.UpsertProcessingStage(ctx, domain.ProcessingUpdate{
			OrderID: id, Stage: domain.StageDone, LastStep: "FAIL_ORDER",
		}); err != nil {
			t.Fatalf("third upsert: %v", err)
		}
		st, err = repo.ReadProcessingState(ctx, id)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if st.Stage != domain.StageDone || st.LastStep != "FAIL_ORDER" || st.LastErrorCode != "" {
			t.Errorf("state = %+v", st)
		}
	})

	t.Run("the stage CHECK refuses out-of-vocabulary values", func(t *testing.T) {
		id := seedStatusOrder(t, pool, "pending")
		err := repo.UpsertProcessingStage(ctx, domain.ProcessingUpdate{
			OrderID: id, Stage: "SHIPPED",
		})
		if err == nil {
			t.Fatal("unknown stage must violate the CHECK")
		}
	})

	t.Run("the transactional seed rides the order's own transaction", func(t *testing.T) {
		id := seedStatusOrder(t, pool, "pending")
		tm := NewPostgresTransactionManager(pool)
		tx, err := tm.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if err := repo.UpsertProcessingStageWithTx(ctx, tx, domain.ProcessingUpdate{
			OrderID: id, Stage: domain.StageOrderCreated,
		}); err != nil {
			t.Fatalf("seed: %v", err)
		}
		// Not visible before commit, visible after — that is the point.
		if _, err := repo.ReadProcessingState(ctx, id); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("pre-commit read: got %v, want ErrNotFound", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}
		st, err := repo.ReadProcessingState(ctx, id)
		if err != nil || st.Stage != domain.StageOrderCreated {
			t.Fatalf("post-commit read: %+v err=%v", st, err)
		}
	})

	t.Run("an order without a projection row reads as not found", func(t *testing.T) {
		id := seedStatusOrder(t, pool, "pending")
		if _, err := repo.ReadProcessingState(ctx, id); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("got %v, want ErrNotFound", err)
		}
	})
}
