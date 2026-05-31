package domain

import "context"

// OrderRepository defines the interface for order data access
type OrderRepository interface {
	FindByID(ctx context.Context, userID, id string) (*Order, error)
	FindByUserID(ctx context.Context, userID string) ([]Order, error)
	// FindByIdempotencyKey returns the order previously created with the given
	// key for this user, or ErrNotFound. Used to make order creation idempotent.
	FindByIdempotencyKey(ctx context.Context, userID, key string) (*Order, error)
	Create(ctx context.Context, order *Order) error
	UpdateStatus(ctx context.Context, id, status string) error

	// Transaction support
	CreateWithTx(ctx context.Context, tx Transaction, order *Order) error
}
