//go:build integration

package repository

import (
	"context"
	"errors"
	"testing"

	"github.com/duynhlab/order-service/internal/core/domain"
)

// TestProtectedOrderReads_Integration proves the unscoped operator reads
// over the real schema (RFC-0023 slice A): cross-customer list with status
// filter + paging, and the case view with items.
func TestProtectedOrderReads_Integration(t *testing.T) {
	pool := newTestDB(t)
	repo := NewPostgresOrderRepository(pool)
	ctx := context.Background()

	mk := func(user, status string, total int64) *domain.Order {
		o := &domain.Order{
			UserID: user, Status: status, Subtotal: total, Total: total,
			Items: []domain.OrderItem{{ProductID: "1", ProductName: "Mouse", Quantity: 1, Price: total, Subtotal: total}},
		}
		if err := repo.Create(ctx, o); err != nil {
			t.Fatalf("seed order for %s: %v", user, err)
		}
		return o
	}
	a := mk("po-u1", "completed", 1000)
	mk("po-u2", "manual_review", 2000)
	mk("po-u3", "manual_review", 3000)

	// Cross-customer: sees all three owners.
	items, total, err := repo.ListAll(ctx, "", 50, 0)
	if err != nil || total < 3 {
		t.Fatalf("list all = (total %d, %v), want >=3", total, err)
	}
	owners := map[string]bool{}
	for _, o := range items {
		owners[o.UserID] = true
	}
	if !owners["po-u1"] || !owners["po-u2"] || !owners["po-u3"] {
		t.Fatalf("list is owner-scoped somehow: %v", owners)
	}

	// Status filter feeds the backlog cards.
	mr, mrTotal, err := repo.ListAll(ctx, "manual_review", 50, 0)
	if err != nil || mrTotal != 2 || len(mr) != 2 {
		t.Fatalf("manual_review filter = (%d, total %d, %v), want 2", len(mr), mrTotal, err)
	}

	// Paging.
	p1, _, _ := repo.ListAll(ctx, "", 1, 0)
	p2, _, err := repo.ListAll(ctx, "", 1, 1)
	if err != nil || len(p1) != 1 || len(p2) != 1 || p1[0].ID == p2[0].ID {
		t.Fatalf("paging broken: %v vs %v (%v)", p1, p2, err)
	}

	// Case view: items ride along, no owner filter.
	got, err := repo.FindByIDUnscoped(ctx, a.ID)
	if err != nil || got.UserID != "po-u1" || len(got.Items) != 1 {
		t.Fatalf("case view = %+v, %v", got, err)
	}
	if _, err := repo.FindByIDUnscoped(ctx, "999999"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing id: want ErrNotFound, got %v", err)
	}
}
