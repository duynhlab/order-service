//go:build integration

package repository

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/duynhlab/order-service/internal/core/domain"
)

func TestCancellationOutbox_Integration(t *testing.T) {
	pool := newTestDB(t)
	orders := NewPostgresOrderRepository(pool)
	outbox := NewPostgresCancellationRepository(pool)
	tm := NewPostgresTransactionManager(pool)
	ctx := context.Background()

	armed := func(t *testing.T, id string, epoch int64) {
		t.Helper()
		tx, err := tm.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if err := outbox.ArmWithTx(ctx, tx, id, epoch); err != nil {
			t.Fatalf("arm: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}

	t.Run("the CAS and the arm commit together", func(t *testing.T) {
		id := seedStatusOrder(t, pool, "confirmed")
		cmd, err := domain.NewRequestCancellationCommand(id, "7", 1)
		if err != nil {
			t.Fatalf("constructor: %v", err)
		}

		tx, err := tm.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if _, err := orders.ApplyStatusCommandWithTx(ctx, tx, cmd); err != nil {
			t.Fatalf("CAS: %v", err)
		}
		if err := outbox.ArmWithTx(ctx, tx, id, 1); err != nil {
			t.Fatalf("arm: %v", err)
		}
		// Neither write is visible before the commit.
		row := readOrderRow(t, pool, id)
		if row.Status != "confirmed" {
			t.Fatalf("pre-commit status = %s", row.Status)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}

		row = readOrderRow(t, pool, id)
		if row.Status != "cancelling" {
			t.Errorf("status = %s, want cancelling", row.Status)
		}
		claimed, err := outbox.ClaimDue(ctx, 10, time.Minute)
		if err != nil || len(claimed) != 1 || claimed[0].OrderID != id || claimed[0].Epoch != 1 {
			t.Errorf("claimed = %+v err=%v, want the armed row", claimed, err)
		}
	})

	t.Run("a rolled-back CAS never leaves an armed row", func(t *testing.T) {
		id := seedStatusOrder(t, pool, "pending") // cancel of pending is refused
		cmd, _ := domain.NewRequestCancellationCommand(id, "7", 1)

		tx, err := tm.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if _, err := orders.ApplyStatusCommandWithTx(ctx, tx, cmd); !errors.Is(err, domain.ErrInvalidTransition) {
			t.Fatalf("CAS on pending: got %v, want ErrInvalidTransition", err)
		}
		_ = tx.Rollback(ctx)

		claimed, err := outbox.ClaimDue(ctx, 10, time.Minute)
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
		for _, c := range claimed {
			if c.OrderID == id {
				t.Errorf("refused cancel left an armed row: %+v", c)
			}
		}
	})

	t.Run("re-arming resets the row for a new epoch", func(t *testing.T) {
		id := seedStatusOrder(t, pool, "confirmed")
		armed(t, id, 1)
		if err := outbox.MarkFailed(ctx, id, 1, "START_FAILED"); err != nil {
			t.Fatalf("fail: %v", err)
		}
		armed(t, id, 5)

		claimed, err := outbox.ClaimDue(ctx, 10, time.Minute)
		if err != nil || len(claimed) != 1 {
			t.Fatalf("claimed = %+v err=%v", claimed, err)
		}
		if claimed[0].Epoch != 5 || claimed[0].Attempts != 1 || claimed[0].LastErrorCode != "" {
			t.Errorf("re-armed row = %+v, want epoch 5 with reset bookkeeping", claimed[0])
		}
	})

	t.Run("lease, reschedule, fail and stats behave like the fulfillment outbox", func(t *testing.T) {
		// Subtests share one database, so every assertion here is scoped to
		// THIS order (claims filtered by id, stats read as deltas) — leased
		// rows from earlier subtests legitimately stay PENDING.
		mine := func(rows []domain.CancellationRequest, id string) int {
			n := 0
			for _, r := range rows {
				if r.OrderID == id {
					n++
				}
			}
			return n
		}
		before, err := outbox.Stats(ctx)
		if err != nil {
			t.Fatalf("stats: %v", err)
		}

		id := seedStatusOrder(t, pool, "confirmed")
		armed(t, id, 2)

		claimed, err := outbox.ClaimDue(ctx, 10, time.Minute)
		if err != nil || mine(claimed, id) != 1 {
			t.Fatalf("first claim = %+v err=%v, want this row claimed", claimed, err)
		}
		// Leased: a second claim must not see it again.
		again, err := outbox.ClaimDue(ctx, 10, time.Minute)
		if err != nil || mine(again, id) != 0 {
			t.Fatalf("second claim = %+v err=%v, want this row leased", again, err)
		}

		if err := outbox.Reschedule(ctx, id, 2, time.Now().Add(-time.Second), "START_FAILED"); err != nil {
			t.Fatalf("reschedule: %v", err)
		}
		// A stale-epoch fail must be a no-op — that is the I3 guard.
		if err := outbox.MarkFailed(ctx, id, 1, "START_FAILED"); err != nil {
			t.Fatalf("stale fail: %v", err)
		}
		mid, err := outbox.Stats(ctx)
		if err != nil {
			t.Fatalf("stats: %v", err)
		}
		if mid.Failed != before.Failed {
			t.Errorf("a stale-epoch MarkFailed must not fail the row (delta %d)", mid.Failed-before.Failed)
		}
		if err := outbox.MarkFailed(ctx, id, 2, "START_FAILED"); err != nil {
			t.Fatalf("fail: %v", err)
		}
		after, err := outbox.Stats(ctx)
		if err != nil {
			t.Fatalf("stats: %v", err)
		}
		if after.Failed != before.Failed+1 {
			t.Errorf("failed delta = %d → %d, want +1", before.Failed, after.Failed)
		}
	})

	t.Run("the request-path close is user-scoped", func(t *testing.T) {
		id := seedStatusOrder(t, pool, "confirmed") // seeded owner user_id=7
		armed(t, id, 3)

		status := func(t *testing.T) string {
			t.Helper()
			var got string
			if err := pool.QueryRow(ctx,
				`SELECT status FROM cancellation_requests WHERE order_id = $1`, id).Scan(&got); err != nil {
				t.Fatalf("read row: %v", err)
			}
			return got
		}

		// The wrong user cannot close it.
		if err := outbox.CloseDispatchedForUser(ctx, "999", id, 3); err != nil {
			t.Fatalf("scoped close (wrong user): %v", err)
		}
		if got := status(t); got != "PENDING" {
			t.Fatalf("row = %s after a non-owner close, want PENDING", got)
		}
		if err := outbox.CloseDispatchedForUser(ctx, "7", id, 3); err != nil {
			t.Fatalf("scoped close: %v", err)
		}
		if got := status(t); got != "DISPATCHED" {
			t.Errorf("row = %s after the owner's close, want DISPATCHED", got)
		}
	})
}
