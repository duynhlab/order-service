package domain

import "context"

// OrderRepository defines the interface for order data access
type OrderRepository interface {
	FindByID(ctx context.Context, userID, id string) (*Order, error)
	FindByUserID(ctx context.Context, userID string, limit, offset int) ([]Order, error)
	// CountByUserID returns the total number of orders for a user (for pagination).
	CountByUserID(ctx context.Context, userID string) (int, error)
	// FindByIdempotencyKey returns the order previously created with the given
	// key for this user, or ErrNotFound. Used to make order creation idempotent.
	FindByIdempotencyKey(ctx context.Context, userID, key string) (*Order, error)
	Create(ctx context.Context, order *Order) error

	// Transaction support
	CreateWithTx(ctx context.Context, tx Transaction, order *Order) error

	// ListAll is the Backoffice's cross-customer read (RFC-0023 slice A):
	// every scope-bearing query above bakes user_id into the SQL, so the
	// operator view needs its own explicitly-unscoped path. One page (newest
	// first) plus the unpaged total; status narrows when set.
	ListAll(ctx context.Context, status string, limit, offset int) ([]Order, int, error)

	// FindByIDUnscoped is the operator case view: one order with items,
	// without an owner filter. The customer path (FindByID) keeps its baked
	// scope untouched.
	FindByIDUnscoped(ctx context.Context, id string) (*Order, error)
}
