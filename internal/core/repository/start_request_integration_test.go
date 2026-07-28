//go:build integration

package repository

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/duynhlab/order-service/internal/core/domain"
)

// seedOrder inserts a minimal order and returns its id, so outbox rows have a
// real parent to reference (the FK is what ties the two together).
func seedOrder(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var id string
	err := pool.QueryRow(context.Background(), `
		INSERT INTO orders (user_id, status, subtotal, shipping, tax, discount, total, created_at)
		VALUES (7, 'pending', 1000, 300, 0, 0, 1300, now())
		RETURNING id
	`).Scan(&id)
	if err != nil {
		t.Fatalf("seed order: %v", err)
	}
	return id
}

func newStartRequestRepo(t *testing.T) (*PostgresStartRequestRepository, *PostgresTransactionManager, *pgxpool.Pool) {
	t.Helper()
	pool := newTestDB(t)
	return NewPostgresStartRequestRepository(pool), NewPostgresTransactionManager(pool), pool
}

// The whole point of the outbox: the row lands in the ORDER's transaction, so a
// rollback leaves neither the order nor the start request behind.
func TestStartRequest_EnqueueIsAtomicWithTheTransaction(t *testing.T) {
	repo, txm, pool := newStartRequestRepo(t)
	ctx := context.Background()
	orderID := seedOrder(t, pool)

	tx, err := txm.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := repo.EnqueueWithTx(ctx, tx, orderID, "tok_visa_ok"); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	stats, err := repo.Stats(ctx)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats.Pending != 0 {
		t.Errorf("pending = %d after rollback, want 0 — the row escaped the transaction", stats.Pending)
	}
}

// A create retried after a partial failure must not be turned into an error by
// an outbox row that is already correct.
func TestStartRequest_EnqueueIsIdempotent(t *testing.T) {
	repo, txm, pool := newStartRequestRepo(t)
	ctx := context.Background()
	orderID := seedOrder(t, pool)

	for i := 0; i < 2; i++ {
		tx, err := txm.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if err := repo.EnqueueWithTx(ctx, tx, orderID, "tok_visa_ok"); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit %d: %v", i, err)
		}
	}

	stats, err := repo.Stats(ctx)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats.Pending != 1 {
		t.Errorf("pending = %d after two enqueues, want 1", stats.Pending)
	}
}

func enqueue(t *testing.T, repo *PostgresStartRequestRepository, txm *PostgresTransactionManager, orderID, token string) {
	t.Helper()
	ctx := context.Background()
	tx, err := txm.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := repo.EnqueueWithTx(ctx, tx, orderID, token); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// Claiming leases: it returns the row WITH its token, burns an attempt, and
// pushes the row out of the due window so a second claim finds nothing.
func TestStartRequest_ClaimLeasesAndBurnsAnAttempt(t *testing.T) {
	repo, txm, pool := newStartRequestRepo(t)
	ctx := context.Background()
	orderID := seedOrder(t, pool)
	enqueue(t, repo, txm, orderID, "tok_visa_ok")

	claimed, err := repo.ClaimDue(ctx, 10, time.Minute)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(claimed) != 1 {
		t.Fatalf("claimed %d rows, want 1", len(claimed))
	}
	got := claimed[0]
	if got.OrderID != orderID {
		t.Errorf("order id = %q, want %q", got.OrderID, orderID)
	}
	if got.PaymentMethod != "tok_visa_ok" {
		t.Errorf("payment method = %q — the dispatcher cannot charge the right token without it", got.PaymentMethod)
	}
	if got.Attempts != 1 {
		t.Errorf("attempts = %d, want 1 — a claim must burn an attempt so a poison row cannot retry forever", got.Attempts)
	}

	// The lease is what stops a second dispatcher (or the next tick) from
	// double-starting while the first attempt is still in flight.
	again, err := repo.ClaimDue(ctx, 10, time.Minute)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if len(again) != 0 {
		t.Errorf("second claim returned %d rows, want 0 — the lease did not hold", len(again))
	}
}

func TestStartRequest_MarkDispatchedClearsTheToken(t *testing.T) {
	repo, txm, pool := newStartRequestRepo(t)
	ctx := context.Background()
	orderID := seedOrder(t, pool)
	enqueue(t, repo, txm, orderID, "tok_visa_ok")

	if err := repo.MarkDispatched(ctx, orderID); err != nil {
		t.Fatalf("mark dispatched: %v", err)
	}

	var status string
	var token *string
	err := pool.QueryRow(ctx, `SELECT status, payment_method FROM fulfillment_start_requests WHERE order_id = $1`, orderID).
		Scan(&status, &token)
	if err != nil {
		t.Fatalf("read row: %v", err)
	}
	if status != domain.StartRequestDispatched {
		t.Errorf("status = %q, want %q", status, domain.StartRequestDispatched)
	}
	if token != nil {
		t.Errorf("payment_method = %q, want NULL — the token must not outlive the dispatch", *token)
	}

	// A DISPATCHED row is not due, so the dispatcher never sees it again.
	claimed, err := repo.ClaimDue(ctx, 10, time.Minute)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(claimed) != 0 {
		t.Errorf("claimed %d dispatched rows, want 0", len(claimed))
	}
}

// Reschedule keeps the row PENDING but not due; MarkFailed takes it out of the
// dispatcher's reach entirely.
func TestStartRequest_RescheduleThenFail(t *testing.T) {
	repo, txm, pool := newStartRequestRepo(t)
	ctx := context.Background()
	orderID := seedOrder(t, pool)
	enqueue(t, repo, txm, orderID, "tok_visa_ok")

	if _, err := repo.ClaimDue(ctx, 10, time.Second); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := repo.Reschedule(ctx, orderID, time.Now().Add(time.Hour), "UNAVAILABLE"); err != nil {
		t.Fatalf("reschedule: %v", err)
	}

	claimed, err := repo.ClaimDue(ctx, 10, time.Minute)
	if err != nil {
		t.Fatalf("claim after reschedule: %v", err)
	}
	if len(claimed) != 0 {
		t.Errorf("claimed %d rows due in an hour, want 0", len(claimed))
	}

	if err := repo.MarkFailed(ctx, orderID, "UNAVAILABLE"); err != nil {
		t.Fatalf("mark failed: %v", err)
	}
	// A terminal row must not keep the payment token, whichever terminal state
	// it reached.
	var failedToken *string
	if err := pool.QueryRow(ctx, `SELECT payment_method FROM fulfillment_start_requests WHERE order_id = $1`, orderID).Scan(&failedToken); err != nil {
		t.Fatalf("read failed row: %v", err)
	}
	if failedToken != nil {
		t.Errorf("payment_method = %q on a FAILED row, want NULL", *failedToken)
	}
	stats, err := repo.Stats(ctx)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats.Failed != 1 || stats.Pending != 0 {
		t.Errorf("stats = %+v, want 1 failed and 0 pending", stats)
	}
	if stats.OldestPendingAge != 0 {
		t.Errorf("oldest pending age = %v with nothing pending, want 0", stats.OldestPendingAge)
	}
}

// Stats is what the outbox alerts read, so the age has to be real rather than a
// count-derived guess.
func TestStartRequest_StatsReportsOldestPendingAge(t *testing.T) {
	repo, txm, pool := newStartRequestRepo(t)
	ctx := context.Background()
	orderID := seedOrder(t, pool)
	enqueue(t, repo, txm, orderID, "tok_visa_ok")

	// Backdate the row rather than sleeping: the query reads created_at.
	if _, err := pool.Exec(ctx, `UPDATE fulfillment_start_requests SET created_at = now() - interval '10 minutes' WHERE order_id = $1`, orderID); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	stats, err := repo.Stats(ctx)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats.Pending != 1 {
		t.Fatalf("pending = %d, want 1", stats.Pending)
	}
	if stats.OldestPendingAge < 9*time.Minute {
		t.Errorf("oldest pending age = %v, want about 10m", stats.OldestPendingAge)
	}
}

// The user-scoped close is the only outbox write the request path gets, so it
// must refuse an order that belongs to someone else — that is the whole reason
// the narrow interface exists.
func TestStartRequest_MarkDispatchedForUserIsScoped(t *testing.T) {
	repo, txm, pool := newStartRequestRepo(t)
	ctx := context.Background()
	orderID := seedOrder(t, pool) // user_id 7
	enqueue(t, repo, txm, orderID, "tok_visa_ok")

	// A different user must not be able to close it.
	if err := repo.MarkDispatchedForUser(ctx, "999", orderID); err != nil {
		t.Fatalf("MarkDispatchedForUser (wrong user): %v", err)
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM fulfillment_start_requests WHERE order_id = $1`, orderID).Scan(&status); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if status != domain.StartRequestPending {
		t.Fatalf("status = %q after a foreign user's close, want it untouched at PENDING", status)
	}

	// The owner can.
	if err := repo.MarkDispatchedForUser(ctx, "7", orderID); err != nil {
		t.Fatalf("MarkDispatchedForUser (owner): %v", err)
	}
	var token *string
	if err := pool.QueryRow(ctx, `SELECT status, payment_method FROM fulfillment_start_requests WHERE order_id = $1`, orderID).
		Scan(&status, &token); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if status != domain.StartRequestDispatched || token != nil {
		t.Errorf("status = %q token = %v, want DISPATCHED and NULL", status, token)
	}
}

// A terminal row must record that its token WAS cleared, so an empty token can
// be told apart from an order that never had one. Without that distinction a
// hand-requeued row would start a saga that charges the demo token.
func TestStartRequest_TerminalRowsRecordTheClearedToken(t *testing.T) {
	repo, txm, pool := newStartRequestRepo(t)
	ctx := context.Background()

	withToken := seedOrder(t, pool)
	enqueue(t, repo, txm, withToken, "tok_visa_ok")
	if err := repo.MarkFailed(ctx, withToken, "UNAVAILABLE"); err != nil {
		t.Fatalf("mark failed: %v", err)
	}

	withoutToken := seedOrder(t, pool)
	enqueue(t, repo, txm, withoutToken, "") // a REST order carries no token
	if err := repo.MarkFailed(ctx, withoutToken, "UNAVAILABLE"); err != nil {
		t.Fatalf("mark failed: %v", err)
	}

	for _, tc := range []struct {
		orderID string
		want    bool
	}{{withToken, true}, {withoutToken, false}} {
		var cleared bool
		if err := pool.QueryRow(ctx, `SELECT payment_method_cleared FROM fulfillment_start_requests WHERE order_id = $1`, tc.orderID).Scan(&cleared); err != nil {
			t.Fatalf("read row %s: %v", tc.orderID, err)
		}
		if cleared != tc.want {
			t.Errorf("order %s: payment_method_cleared = %v, want %v", tc.orderID, cleared, tc.want)
		}
	}
}
