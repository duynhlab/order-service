//go:build integration

// Integration tests for the PostgreSQL order repository + transaction manager.
// They run a real Postgres via testcontainers-go and apply the service's
// migrations, so they exercise the actual SQL (not a mock). Run with:
//
//	go test -tags=integration ./internal/core/repository/...
//
// Requires a reachable Docker daemon. Excluded from the default `go test ./...`
// unit run by the `integration` build tag.
package repository

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/duynhlab/order-service/internal/core/domain"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// newTestDB starts a throwaway Postgres, applies the migrations, and returns a
// pool for the repository under test. Everything is torn down via t.Cleanup.
func newTestDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()

	container, err := postgres.Run(ctx, "postgres:16-alpine",
		postgres.WithDatabase("order"),
		postgres.WithUsername("order"),
		postgres.WithPassword("secret"),
		// ForListeningPort resets during the initdb restart and can hang; wait
		// for the "ready to accept connections" log to appear the 2nd time
		// (after initdb's first transient start), the reliable Postgres signal.
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(90*time.Second),
		),
	)
	if err != nil {
		t.Fatalf("start postgres container: %v", err)
	}
	t.Cleanup(func() { _ = container.Terminate(ctx) })

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}

	applyMigrations(t, ctx, dsn)

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("new pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// applyMigrations runs every db/migrations/sql/*.up.sql in lexical order using a
// simple-protocol connection (so multi-statement files execute in one round).
func applyMigrations(t *testing.T, ctx context.Context, dsn string) {
	t.Helper()
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	cfg.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect for migrations: %v", err)
	}
	defer conn.Close(ctx)

	dir := filepath.Join("..", "..", "..", "db", "migrations", "sql")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read migrations dir: %v", err)
	}
	var files []string
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() && len(name) > 7 && name[len(name)-7:] == ".up.sql" {
			files = append(files, name)
		}
	}
	sort.Strings(files)
	for _, f := range files {
		sqlBytes, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		if _, err := conn.Exec(ctx, string(sqlBytes)); err != nil {
			t.Fatalf("apply migration %s: %v", f, err)
		}
	}
}

func sampleOrder(userID, idemKey string) *domain.Order {
	return &domain.Order{
		UserID:         userID,
		Status:         "pending",
		Subtotal:       100,
		Shipping:       10,
		Total:          110,
		IdempotencyKey: idemKey,
		Items: []domain.OrderItem{
			{ProductID: "p1", ProductName: "Widget", Quantity: 2, Price: 50, Subtotal: 100},
		},
	}
}

func TestOrderRepository_Integration(t *testing.T) {
	pool := newTestDB(t)
	repo := NewPostgresOrderRepository(pool)
	tm := NewPostgresTransactionManager(pool)
	ctx := context.Background()
	const userID = "itest-user-1" // not present in seed data

	t.Run("Create then FindByID round-trips", func(t *testing.T) {
		o := sampleOrder(userID, "")
		if err := repo.Create(ctx, o); err != nil {
			t.Fatalf("Create: %v", err)
		}
		if o.ID == "" {
			t.Fatal("Create did not set order ID")
		}
		got, err := repo.FindByID(ctx, userID, o.ID)
		if err != nil {
			t.Fatalf("FindByID: %v", err)
		}
		if got.Status != "pending" || got.Total != 110 || len(got.Items) != 1 {
			t.Errorf("round-trip mismatch: %+v", got)
		}
	})

	t.Run("Count / FindByUserID reflect the user's orders", func(t *testing.T) {
		n, err := repo.CountByUserID(ctx, userID)
		if err != nil {
			t.Fatalf("CountByUserID: %v", err)
		}
		if n < 1 {
			t.Errorf("CountByUserID = %d, want >= 1", n)
		}
		list, err := repo.FindByUserID(ctx, userID, 10, 0)
		if err != nil {
			t.Fatalf("FindByUserID: %v", err)
		}
		if len(list) != n {
			t.Errorf("FindByUserID returned %d, Count said %d", len(list), n)
		}
	})

	t.Run("UpdateStatus changes the status", func(t *testing.T) {
		o := sampleOrder(userID, "")
		if err := repo.Create(ctx, o); err != nil {
			t.Fatalf("Create: %v", err)
		}
		if err := repo.UpdateStatus(ctx, o.ID, "shipped"); err != nil {
			t.Fatalf("UpdateStatus: %v", err)
		}
		got, err := repo.FindByID(ctx, userID, o.ID)
		if err != nil {
			t.Fatalf("FindByID: %v", err)
		}
		if got.Status != "shipped" {
			t.Errorf("status = %q, want shipped", got.Status)
		}
	})

	t.Run("missing rows return ErrNotFound", func(t *testing.T) {
		if _, err := repo.FindByID(ctx, userID, "987654"); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("FindByID(missing) err = %v, want ErrNotFound", err)
		}
		if err := repo.UpdateStatus(ctx, "987654", "x"); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("UpdateStatus(missing) err = %v, want ErrNotFound", err)
		}
		if _, err := repo.FindByIdempotencyKey(ctx, userID, "nope"); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("FindByIdempotencyKey(missing) err = %v, want ErrNotFound", err)
		}
	})

	t.Run("CreateWithTx + Commit is found by idempotency key", func(t *testing.T) {
		tx, err := tm.Begin(ctx)
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		defer func() { _ = tx.Rollback(ctx) }() // release the conn even if a later step fails; no-op after Commit
		o := sampleOrder(userID, "idem-commit")
		if err := repo.CreateWithTx(ctx, tx, o); err != nil {
			t.Fatalf("CreateWithTx: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("Commit: %v", err)
		}
		got, err := repo.FindByIdempotencyKey(ctx, userID, "idem-commit")
		if err != nil {
			t.Fatalf("FindByIdempotencyKey after commit: %v", err)
		}
		if got.ID != o.ID {
			t.Errorf("idempotency lookup id %q != created %q", got.ID, o.ID)
		}
	})

	t.Run("CreateWithTx + Rollback leaves no row", func(t *testing.T) {
		tx, err := tm.Begin(ctx)
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		defer func() { _ = tx.Rollback(ctx) }() // release the conn even if a later step fails; no-op after explicit Rollback
		o := sampleOrder(userID, "idem-rollback")
		if err := repo.CreateWithTx(ctx, tx, o); err != nil {
			t.Fatalf("CreateWithTx: %v", err)
		}
		if err := tx.Rollback(ctx); err != nil {
			t.Fatalf("Rollback: %v", err)
		}
		if _, err := repo.FindByIdempotencyKey(ctx, userID, "idem-rollback"); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("after rollback err = %v, want ErrNotFound", err)
		}
	})
}
