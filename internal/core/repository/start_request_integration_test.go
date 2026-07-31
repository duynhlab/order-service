//go:build integration

package repository

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/duynhlab/order-service/internal/core/domain"
)

// seedUserID owns every order seedOrder inserts; the user-scoped reads need it.
const seedUserID = "7"

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
	if err := repo.EnqueueWithTx(ctx, tx, orderID, "tok_visa_ok", "inventory"); err != nil {
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
		if err := repo.EnqueueWithTx(ctx, tx, orderID, "tok_visa_ok", "inventory"); err != nil {
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
	if err := repo.EnqueueWithTx(ctx, tx, orderID, token, "inventory"); err != nil {
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

// seedOrderWithItems inserts an order and its line items, so the loader can be
// checked on the thing the dispatcher actually needs: the items it will put into
// the workflow input.
func seedOrderWithItems(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	ctx := context.Background()
	id := seedOrder(t, pool)
	for _, it := range []struct {
		productID string
		name      string
		qty       int
		price     int64
	}{
		{"1", "Wireless Mouse", 2, 2999},
		{"9", "USB-C Hub", 1, 3999},
	} {
		_, err := pool.Exec(ctx, `
			INSERT INTO order_items (order_id, product_id, product_name, quantity, price, subtotal)
			VALUES ($1, $2, $3, $4, $5, $6)
		`, id, it.productID, it.name, it.qty, it.price, it.price*int64(it.qty))
		if err != nil {
			t.Fatalf("seed item %s: %v", it.productID, err)
		}
	}
	return id
}

// LoadForFulfillment is what the dispatcher builds its workflow input from, so
// what matters is that the ITEMS come back complete: a dropped line means a saga
// that reserves the wrong quantity, and a saga that charges for one thing and
// reserves another.
func TestLoadForFulfillment_ReturnsTheOrderWithItems(t *testing.T) {
	pool := newTestDB(t)
	repo := NewPostgresOrderRepository(pool)
	ctx := context.Background()
	orderID := seedOrderWithItems(t, pool)

	order, err := repo.LoadForFulfillment(ctx, orderID)
	if err != nil {
		t.Fatalf("LoadForFulfillment: %v", err)
	}

	if order.ID != orderID {
		t.Errorf("id = %q, want %q", order.ID, orderID)
	}
	if order.UserID != "7" || order.Status != "pending" || order.Total != 1300 {
		t.Errorf("order = %+v, want user 7 / pending / total 1300", order)
	}
	if len(order.Items) != 2 {
		t.Fatalf("items = %d, want 2 — a dropped line makes the saga reserve the wrong quantity", len(order.Items))
	}
	byProduct := map[string]domain.OrderItem{}
	for _, it := range order.Items {
		byProduct[it.ProductID] = it
	}
	if got := byProduct["1"]; got.Quantity != 2 || got.Price != 2999 || got.ProductName != "Wireless Mouse" {
		t.Errorf("item 1 = %+v, want qty 2 price 2999 name Wireless Mouse", got)
	}
	if got := byProduct["9"]; got.Quantity != 1 || got.Subtotal != 3999 {
		t.Errorf("item 9 = %+v, want qty 1 subtotal 3999", got)
	}
}

// Unscoped by design: the dispatcher has an order id from the outbox and no user
// context. This pins that the loader does NOT require one — the property that
// makes it dangerous on the request path, and the reason it lives on a separate
// interface.
func TestLoadForFulfillment_NeedsNoUserScope(t *testing.T) {
	pool := newTestDB(t)
	repo := NewPostgresOrderRepository(pool)
	ctx := context.Background()
	orderID := seedOrderWithItems(t, pool)

	// The user-scoped reader refuses a stranger; the fulfillment loader does not
	// take a user at all.
	if _, err := repo.FindByID(ctx, "999", orderID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("FindByID with the wrong user = %v, want ErrNotFound", err)
	}
	if _, err := repo.LoadForFulfillment(ctx, orderID); err != nil {
		t.Errorf("LoadForFulfillment = %v, want it to succeed without a user", err)
	}
}

// A missing order must be ErrNotFound, not a zero-valued order: the dispatcher
// branches on it to mark the row terminal instead of retrying forever.
func TestLoadForFulfillment_MissingOrderIsNotFound(t *testing.T) {
	pool := newTestDB(t)
	repo := NewPostgresOrderRepository(pool)

	order, err := repo.LoadForFulfillment(context.Background(), "424242")
	if !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
	if order != nil {
		t.Errorf("order = %+v, want nil", order)
	}
}

// An order with no items still loads. The dispatcher must not confuse "no items"
// with an error, or a malformed order would be retried until the attempt cap.
func TestLoadForFulfillment_OrderWithoutItems(t *testing.T) {
	pool := newTestDB(t)
	repo := NewPostgresOrderRepository(pool)
	orderID := seedOrder(t, pool)

	order, err := repo.LoadForFulfillment(context.Background(), orderID)
	if err != nil {
		t.Fatalf("LoadForFulfillment: %v", err)
	}
	if len(order.Items) != 0 {
		t.Errorf("items = %d, want 0", len(order.Items))
	}
}

// seedTerminalOrder inserts an order in a terminal status that last changed `age`
// ago, plus the outbox row the reconciler scans from, so the window can be
// exercised against real SQL rather than a mock.
//
// `age` is applied by the DATABASE (`now() - make_interval`), not by this process.
// That is not tidiness: orders.updated_at is `TIMESTAMP` WITHOUT time zone, and
// production writes it with SQL `NOW()`, so the stored value is the database
// session's wall clock. Handing pgx a Go time.Time instead stores THIS process's
// wall clock, which differs by the session's UTC offset — under a +07 offset every
// row lands hours in the future and the scan returns nothing. Aging rows the way
// production writes them keeps the test measuring the query rather than pgx's
// timestamp encoding.
//
// The outbox row is not optional scaffolding: the scan JOINs it, so an order
// without one is invisible to the reconciler. That is correct — every order
// created since this table exists has a row (the enqueue is in the order's own
// transaction, and a failed enqueue fails the create), and orders that predate it
// are pre-cutover product-path orders with no reservation to reconcile.
func seedTerminalOrder(t *testing.T, pool *pgxpool.Pool, status string, age time.Duration, participant string) string {
	t.Helper()
	var id string
	err := pool.QueryRow(context.Background(), `
		INSERT INTO orders (user_id, status, subtotal, shipping, tax, discount, total, created_at, updated_at)
		VALUES (7, $1, 1000, 300, 0, 0, 1300,
		        now() - make_interval(secs => $2::float8),
		        now() - make_interval(secs => $2::float8))
		RETURNING id
	`, status, age.Seconds()).Scan(&id)
	if err != nil {
		t.Fatalf("seed terminal order: %v", err)
	}
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO fulfillment_start_requests (order_id, status, participant)
		VALUES ($1, 'DISPATCHED', $2)
	`, id, nullableParticipant(participant)); err != nil {
		t.Fatalf("seed outbox row: %v", err)
	}
	return id
}

// nullableParticipant writes NULL for "", which is what a row created before the
// column existed looks like.
func nullableParticipant(p string) *string {
	if p == "" {
		return nil
	}
	return &p
}

// The reconciler's candidate query decides whether an inconsistency is ever
// noticed, so the window, the status filter and the ordering are pinned against
// real SQL.
//
// The window is passed as DURATIONS and evaluated by the DATABASE. That is the
// point of the signature: orders.updated_at is written by the database, so a
// boundary this process computed from its own clock would be compared against it
// across whatever skew separates the two.
func TestListForReconcile_WindowAndStatusFilter(t *testing.T) {
	repo, _, pool := newStartRequestRepo(t)
	ctx := context.Background()

	inWindowConfirmed := seedTerminalOrder(t, pool, "confirmed", time.Hour, "inventory")
	inWindowFailed := seedTerminalOrder(t, pool, "failed", 2*time.Hour, "inventory")
	// Excluded: still running, so its saga may yet finish the job.
	seedTerminalOrder(t, pool, "pending", time.Hour, "inventory")
	// Excluded: settled seconds ago — the reconciler must not race the saga's
	// own commit.
	seedTerminalOrder(t, pool, "confirmed", 10*time.Second, "inventory")
	// Excluded: older than the window, so it is an incident with a human on it
	// rather than something to re-attempt on a timer.
	seedTerminalOrder(t, pool, "confirmed", 48*time.Hour, "inventory")

	got, err := repo.ListForReconcile(ctx, 5*time.Minute, 24*time.Hour, 200)
	if err != nil {
		t.Fatalf("ListForReconcile: %v", err)
	}

	ids := map[string]string{}
	for _, c := range got {
		ids[c.OrderID] = c.Status
	}
	if len(got) != 2 {
		t.Fatalf("got %d candidates %v, want exactly the two terminal orders inside the window", len(got), ids)
	}
	if ids[inWindowConfirmed] != "confirmed" || ids[inWindowFailed] != "failed" {
		t.Errorf("candidates = %v, want %s confirmed and %s failed", ids, inWindowConfirmed, inWindowFailed)
	}

	// Oldest first, so a backlog drains in the order it accumulated.
	if got[0].OrderID != inWindowFailed {
		t.Errorf("first candidate = %s, want the older one (%s)", got[0].OrderID, inWindowFailed)
	}
	// The participant travels with the candidate: without it, a missing
	// reservation cannot be told apart from a product-path order that never had
	// one.
	if got[0].Participant != "inventory" {
		t.Errorf("participant = %q, want inventory", got[0].Participant)
	}
}

func TestListForReconcile_RespectsTheLimit(t *testing.T) {
	repo, _, pool := newStartRequestRepo(t)
	for i := 0; i < 3; i++ {
		seedTerminalOrder(t, pool, "confirmed", time.Duration(i+1)*time.Hour, "inventory")
	}

	got, err := repo.ListForReconcile(context.Background(), 5*time.Minute, 24*time.Hour, 2)
	if err != nil {
		t.Fatalf("ListForReconcile: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("got %d candidates, want 2 (the limit)", len(got))
	}
}

// A settled row LEAVES the scan. This is the property that replaced re-examining
// every terminal order in the window on every pass: with consistent orders eating
// the batch, at ordinary volume the scan only ever reached the oldest few hours
// and newer inconsistencies aged out unexamined — while the backlog gauge,
// computed over the examined prefix, read zero.
func TestMarkReconciled_RemovesOnlyThatOrderFromTheScan(t *testing.T) {
	repo, _, pool := newStartRequestRepo(t)
	ctx := context.Background()
	orderID := seedTerminalOrder(t, pool, "confirmed", time.Hour, "inventory")
	// A SECOND order nobody settles. Without it, an UPDATE that forgot its WHERE
	// clause — one repair settling the entire backlog — is indistinguishable from a
	// correct one.
	bystander := seedTerminalOrder(t, pool, "failed", 2*time.Hour, "inventory")

	if n, err := repo.CountUnreconciled(ctx, 5*time.Minute); err != nil || n != 2 {
		t.Fatalf("CountUnreconciled = %d, %v; want 2, nil", n, err)
	}
	if err := repo.MarkReconciled(ctx, orderID); err != nil {
		t.Fatalf("MarkReconciled: %v", err)
	}

	got, err := repo.ListForReconcile(ctx, 5*time.Minute, 24*time.Hour, 200)
	if err != nil {
		t.Fatalf("ListForReconcile: %v", err)
	}
	if len(got) != 1 || got[0].OrderID != bystander {
		t.Errorf("candidates after settling one order = %+v, want only %s", got, bystander)
	}
	if n, err := repo.CountUnreconciled(ctx, 5*time.Minute); err != nil || n != 1 {
		t.Errorf("CountUnreconciled = %d, %v; want 1, nil — the gauge reads the same rows as the scan", n, err)
	}
}

// A breach stays UNSETTLED — it is genuinely still inconsistent — but it must not
// starve the queue. An unrepairable breach never leaves the scan on its own, so an
// unqualified oldest-first order would park a batch of them at the head of every
// pass and never reach the fresh, repairable work behind them.
func TestListForReconcile_KnownBreachesSortBehindFreshWork(t *testing.T) {
	repo, _, pool := newStartRequestRepo(t)
	ctx := context.Background()

	// The breach is the OLDER row, so an unqualified `ORDER BY updated_at` would
	// return it first and this test would catch that.
	breached := seedTerminalOrder(t, pool, "failed", 10*time.Hour, "inventory")
	fresh := seedTerminalOrder(t, pool, "confirmed", time.Hour, "inventory")

	if err := repo.MarkReconcileBreach(ctx, breached, "breach"); err != nil {
		t.Fatalf("MarkReconcileBreach: %v", err)
	}

	got, err := repo.ListForReconcile(ctx, 5*time.Minute, 24*time.Hour, 200)
	if err != nil {
		t.Fatalf("ListForReconcile: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d candidates, want both (a breach stays in the backlog)", len(got))
	}
	if got[0].OrderID != fresh {
		t.Errorf("first candidate = %s, want the repairable one (%s) ahead of the known breach", got[0].OrderID, fresh)
	}
	// The code comes back on the candidate, which is how the reconciler knows to
	// report the breach once rather than once per pass.
	if got[1].OrderID != breached || got[1].BreachCode != "breach" {
		t.Errorf("second candidate = %+v, want %s carrying its recorded breach code", got[1], breached)
	}
}

// A repair that lands after a breach was recorded must clear the code, or the
// order would stay behind fresh work and keep reading as a known breach.
func TestMarkReconciled_ClearsARecordedBreach(t *testing.T) {
	repo, _, pool := newStartRequestRepo(t)
	ctx := context.Background()
	orderID := seedTerminalOrder(t, pool, "confirmed", time.Hour, "inventory")

	if err := repo.MarkReconcileBreach(ctx, orderID, "breach"); err != nil {
		t.Fatalf("MarkReconcileBreach: %v", err)
	}
	if err := repo.MarkReconciled(ctx, orderID); err != nil {
		t.Fatalf("MarkReconciled: %v", err)
	}

	var code *string
	if err := pool.QueryRow(ctx, `
		SELECT reconcile_breach_code FROM fulfillment_start_requests WHERE order_id = $1
	`, orderID).Scan(&code); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if code != nil {
		t.Errorf("reconcile_breach_code = %q after a successful repair, want NULL", *code)
	}
}

// An order written before the participant column existed reads as "" rather than
// failing the scan, and "" must NOT be treated as the inventory path — a missing
// reservation there is normal, not a breach.
func TestListForReconcile_NullParticipantReadsAsEmpty(t *testing.T) {
	repo, _, pool := newStartRequestRepo(t)
	seedTerminalOrder(t, pool, "confirmed", time.Hour, "")

	got, err := repo.ListForReconcile(context.Background(), 5*time.Minute, 24*time.Hour, 200)
	if err != nil {
		t.Fatalf("ListForReconcile: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d candidates, want 1", len(got))
	}
	if got[0].Participant != "" {
		t.Errorf("participant = %q, want empty for a legacy row", got[0].Participant)
	}
}

// The gauge must count the whole unsettled population, not one batch of it:
// capping it would make a backlog of 500 read as 200 and hide the true size of an
// incident.
func TestCountUnreconciled_IsNotCappedByTheScanBatch(t *testing.T) {
	repo, _, pool := newStartRequestRepo(t)
	for i := 0; i < 4; i++ {
		seedTerminalOrder(t, pool, "confirmed", time.Duration(i+1)*time.Hour, "inventory")
	}

	got, err := repo.ListForReconcile(context.Background(), 5*time.Minute, 24*time.Hour, 2)
	if err != nil {
		t.Fatalf("ListForReconcile: %v", err)
	}
	n, err := repo.CountUnreconciled(context.Background(), 5*time.Minute)
	if err != nil {
		t.Fatalf("CountUnreconciled: %v", err)
	}
	if len(got) != 2 || n != 4 {
		t.Errorf("scan = %d (limit 2), backlog = %d; want the backlog to report all 4 unsettled orders", len(got), n)
	}
}

// The gauge must count the SAME population the scan examines — minus the window.
// Seeding every excluded shape and counting is the only way to catch a filter
// dropped from one query but not the other.
func TestCountUnreconciled_AppliesTheStatusAndSettleFilters(t *testing.T) {
	repo, _, pool := newStartRequestRepo(t)
	ctx := context.Background()

	seedTerminalOrder(t, pool, "confirmed", time.Hour, "inventory")      // counted
	seedTerminalOrder(t, pool, "failed", 2*time.Hour, "inventory")       // counted
	seedTerminalOrder(t, pool, "pending", time.Hour, "inventory")        // not terminal
	seedTerminalOrder(t, pool, "confirmed", 10*time.Second, "inventory") // still settling

	n, err := repo.CountUnreconciled(ctx, 5*time.Minute)
	if err != nil {
		t.Fatalf("CountUnreconciled: %v", err)
	}
	if n != 2 {
		t.Errorf("backlog = %d, want 2 — a non-terminal order is not a backlog item, and neither is one that settled seconds ago", n)
	}
}

// RFC-0021 P5: the new statuses split between the two sides of the terminal
// set. completed is confirmed-plus-bookkeeping and MUST stay in both the
// scan and the backlog (an order that completes before its settle delay
// elapses would otherwise vanish from settlement forever); manual_review is
// parked for a human and cancelled settles its own stock through the
// cancellation workflow, so neither belongs to the reconciler.
func TestReconcileQueries_NewStatusTerminalSet(t *testing.T) {
	repo, _, pool := newStartRequestRepo(t)
	ctx := context.Background()

	completedID := seedTerminalOrder(t, pool, "completed", time.Hour, "inventory") // counted + scanned
	seedTerminalOrder(t, pool, "manual_review", time.Hour, "inventory")            // parked for a human
	seedTerminalOrder(t, pool, "cancelling", time.Hour, "inventory")               // workflow in progress
	seedTerminalOrder(t, pool, "cancelled", time.Hour, "inventory")                // cancellation settled it

	got, err := repo.ListForReconcile(ctx, 5*time.Minute, 24*time.Hour, 200)
	if err != nil {
		t.Fatalf("ListForReconcile: %v", err)
	}
	if len(got) != 1 || got[0].OrderID != completedID || got[0].Status != "completed" {
		t.Errorf("scan = %+v, want exactly the completed order", got)
	}

	n, err := repo.CountUnreconciled(ctx, 5*time.Minute)
	if err != nil {
		t.Fatalf("CountUnreconciled: %v", err)
	}
	if n != 1 {
		t.Errorf("backlog = %d, want 1 — only the completed order is the reconciler's business", n)
	}
}

// CountOrdersInStatus feeds the manual_review / stuck-cancelling gauges.
func TestCountOrdersInStatus(t *testing.T) {
	repo, _, pool := newStartRequestRepo(t)
	ctx := context.Background()

	seedTerminalOrder(t, pool, "manual_review", time.Hour, "inventory")
	seedTerminalOrder(t, pool, "manual_review", time.Second, "inventory")
	seedTerminalOrder(t, pool, "cancelling", time.Hour, "inventory")
	seedTerminalOrder(t, pool, "cancelling", time.Minute, "inventory")

	if n, err := repo.CountOrdersInStatus(ctx, "manual_review", 0); err != nil || n != 2 {
		t.Errorf("manual_review count = %d err=%v, want 2 (un-aged)", n, err)
	}
	if n, err := repo.CountOrdersInStatus(ctx, "cancelling", 15*time.Minute); err != nil || n != 1 {
		t.Errorf("stuck cancelling = %d err=%v, want 1 — a workflow a minute in is not stuck", n, err)
	}
}

// The backlog is deliberately UNWINDOWED while the scan is not. An unrepairable
// breach stays unsettled by design, so a windowed count would return to zero 24h
// later while stock was still consumed against an order that never happened — the
// same "reads zero while something is wrong" failure the settle column removed,
// on a 24h fuse. It would also make the kill switch destructive: with the
// reconciler off longer than the window, every affected order would vanish from
// the gauge as well as from the scan.
func TestCountUnreconciled_StillCountsOrdersOlderThanTheScanWindow(t *testing.T) {
	repo, _, pool := newStartRequestRepo(t)
	ctx := context.Background()
	seedTerminalOrder(t, pool, "failed", 48*time.Hour, "inventory")

	got, err := repo.ListForReconcile(ctx, 5*time.Minute, 24*time.Hour, 200)
	if err != nil {
		t.Fatalf("ListForReconcile: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("scan returned %d candidates older than its window, want none", len(got))
	}

	n, err := repo.CountUnreconciled(ctx, 5*time.Minute)
	if err != nil {
		t.Fatalf("CountUnreconciled: %v", err)
	}
	if n != 1 {
		t.Errorf("backlog = %d, want 1 — an unresolved order must not age out of the number an operator watches", n)
	}
}

// An order with no outbox row is invisible to the reconciler, and that is the
// intended contract: the scan JOINs the outbox. Asserted explicitly because every
// other test seeds the row, so a JOIN silently widened to a LEFT JOIN would be
// undetectable — and it would produce candidates with an empty participant, which
// the reconciler reads as "product path".
func TestListForReconcile_IgnoresOrdersWithNoOutboxRow(t *testing.T) {
	repo, _, pool := newStartRequestRepo(t)
	ctx := context.Background()
	var id string
	if err := pool.QueryRow(ctx, `
		INSERT INTO orders (user_id, status, subtotal, shipping, tax, discount, total, created_at, updated_at)
		VALUES (7, 'confirmed', 1000, 300, 0, 0, 1300,
		        now() - interval '1 hour', now() - interval '1 hour')
		RETURNING id
	`).Scan(&id); err != nil {
		t.Fatalf("seed: %v", err)
	}

	got, err := repo.ListForReconcile(ctx, 5*time.Minute, 24*time.Hour, 200)
	if err != nil {
		t.Fatalf("ListForReconcile: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %+v for an order with no outbox row, want none", got)
	}
	n, err := repo.CountUnreconciled(ctx, 5*time.Minute)
	if err != nil {
		t.Fatalf("CountUnreconciled: %v", err)
	}
	if n != 0 {
		t.Errorf("backlog = %d for an order with no outbox row, want 0 — scan and gauge must agree", n)
	}
}

// The window bounds are pinned to within a second on both sides. Without this,
// `<` vs `<=` and `>=` vs `>` are free to flip, and the seeds elsewhere (10s / 1h /
// 48h against 5min / 24h) are far too coarse to notice.
func TestListForReconcile_WindowBoundsAreTightOnBothSides(t *testing.T) {
	repo, _, pool := newStartRequestRepo(t)
	ctx := context.Background()

	justInsideSettle := seedTerminalOrder(t, pool, "confirmed", 5*time.Minute+2*time.Second, "inventory")
	justInsideWindow := seedTerminalOrder(t, pool, "failed", 24*time.Hour-2*time.Second, "inventory")
	seedTerminalOrder(t, pool, "confirmed", 5*time.Minute-2*time.Second, "inventory") // still settling
	seedTerminalOrder(t, pool, "failed", 24*time.Hour+2*time.Second, "inventory")     // aged out

	got, err := repo.ListForReconcile(ctx, 5*time.Minute, 24*time.Hour, 200)
	if err != nil {
		t.Fatalf("ListForReconcile: %v", err)
	}
	ids := map[string]bool{}
	for _, c := range got {
		ids[c.OrderID] = true
	}
	if len(got) != 2 || !ids[justInsideSettle] || !ids[justInsideWindow] {
		t.Errorf("candidates = %+v, want exactly the two just inside the bounds (%s, %s)",
			got, justInsideSettle, justInsideWindow)
	}
}

// A breach must not be stamped onto an order that has already been settled: the
// row would carry a breach code invisible to the scan, and "when did this order
// agree?" would read as "it did, and also it is broken".
func TestMarkReconcileBreach_WillNotStampASettledRow(t *testing.T) {
	repo, _, pool := newStartRequestRepo(t)
	ctx := context.Background()
	orderID := seedTerminalOrder(t, pool, "confirmed", time.Hour, "inventory")

	if err := repo.MarkReconciled(ctx, orderID); err != nil {
		t.Fatalf("MarkReconciled: %v", err)
	}
	if err := repo.MarkReconcileBreach(ctx, orderID, "STOCK_RETURNED"); err != nil {
		t.Fatalf("MarkReconcileBreach: %v", err)
	}

	var code *string
	if err := pool.QueryRow(ctx, `
		SELECT reconcile_breach_code FROM fulfillment_start_requests WHERE order_id = $1
	`, orderID).Scan(&code); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if code != nil {
		t.Errorf("reconcile_breach_code = %q on a settled row, want NULL", *code)
	}
}

// The claimed row carries the participant the API recorded, because the dispatcher
// starts the saga with it rather than with the worker's own flag.
func TestClaimDue_ReturnsTheRecordedParticipant(t *testing.T) {
	repo, txm, pool := newStartRequestRepo(t)
	ctx := context.Background()
	orderID := seedOrder(t, pool)

	tx, err := txm.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := repo.EnqueueWithTx(ctx, tx, orderID, "tok_visa_ok", "inventory"); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	claimed, err := repo.ClaimDue(ctx, 10, time.Minute)
	if err != nil {
		t.Fatalf("ClaimDue: %v", err)
	}
	if len(claimed) != 1 {
		t.Fatalf("claimed %d rows, want 1", len(claimed))
	}
	if claimed[0].Participant != "inventory" {
		t.Errorf("participant = %q, want inventory — the dispatcher starts the saga with this, not with its own flag",
			claimed[0].Participant)
	}
}

// The participant a replayed order is started with comes from this JOIN, so it has
// to be keyed on the order — two orders on different branches must not read each
// other's. Seeding both in one container is also what makes that provable: a
// single-row fixture cannot tell "reads this order's row" from "reads any row".
func TestFindByIdempotencyKey_CarriesThisOrdersParticipant(t *testing.T) {
	outbox, txm, pool := newStartRequestRepo(t)
	orders := NewPostgresOrderRepository(pool)
	ctx := context.Background()

	seeded := map[string]string{}
	for _, participant := range []string{"product", "inventory"} {
		orderID := seedOrder(t, pool)
		key := "key-" + participant
		if _, err := pool.Exec(ctx,
			`UPDATE orders SET idempotency_key = $2 WHERE id = $1`, orderID, key); err != nil {
			t.Fatalf("set idempotency key: %v", err)
		}
		tx, err := txm.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if err := outbox.EnqueueWithTx(ctx, tx, orderID, "tok_visa_ok", participant); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}
		seeded[key] = participant
	}

	for key, want := range seeded {
		order, err := orders.FindByIdempotencyKey(ctx, seedUserID, key)
		if err != nil {
			t.Fatalf("FindByIdempotencyKey(%q): %v", key, err)
		}
		if order.StockParticipant != want {
			t.Errorf("participant for %q = %q, want %q", key, order.StockParticipant, want)
		}
	}
}

// An order with no outbox row (they predate it) must still be found, with an empty
// participant — which resolves to the product path, not to a reader's flag.
func TestFindByIdempotencyKey_OrderWithNoOutboxRowHasNoParticipant(t *testing.T) {
	_, _, pool := newStartRequestRepo(t)
	orders := NewPostgresOrderRepository(pool)
	ctx := context.Background()

	orderID := seedOrder(t, pool)
	if _, err := pool.Exec(ctx,
		`UPDATE orders SET idempotency_key = 'key-no-row' WHERE id = $1`, orderID); err != nil {
		t.Fatalf("set idempotency key: %v", err)
	}

	order, err := orders.FindByIdempotencyKey(ctx, seedUserID, "key-no-row")
	if err != nil {
		t.Fatalf("FindByIdempotencyKey: %v — the LEFT JOIN must not drop the order", err)
	}
	if order.StockParticipant != "" {
		t.Errorf("participant = %q, want empty", order.StockParticipant)
	}
}

// The column routes stock writes, so the database — not a comment — is what keeps
// an unusable value out of it. Without the CHECK, a hand-edited row stalls the
// saga on an unknown participant.
func TestStartRequest_ParticipantIsConstrainedToTheEnum(t *testing.T) {
	repo, txm, pool := newStartRequestRepo(t)
	ctx := context.Background()
	orderID := seedOrder(t, pool)

	tx, err := txm.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := repo.EnqueueWithTx(ctx, tx, orderID, "tok_visa_ok", "warehouse"); err == nil {
		t.Error("enqueue accepted participant \"warehouse\"; the CHECK constraint is missing")
	}
	_ = tx.Rollback(ctx)
}
