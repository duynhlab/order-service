package repository

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/duynhlab/order-service/internal/core/domain"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// pgUniqueViolation is the PostgreSQL SQLSTATE for a unique-constraint violation.
const pgUniqueViolation = "23505"

// isUniqueViolation reports whether err is a Postgres unique-constraint
// violation (SQLSTATE 23505), e.g. a racing double-submit that trips the
// idempotency-key index after the pre-check missed it.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation
}

// PostgresOrderRepository implements OrderRepository using PostgreSQL with pgx
type PostgresOrderRepository struct {
	pool *pgxpool.Pool
}

// NewPostgresOrderRepository creates a new PostgreSQL order repository
func NewPostgresOrderRepository(pool *pgxpool.Pool) *PostgresOrderRepository {
	return &PostgresOrderRepository{pool: pool}
}

// FindByIdempotencyKey retrieves the order previously created with the given key
// for this user, or domain.ErrNotFound. Used to make CreateOrder idempotent.
func (r *PostgresOrderRepository) FindByIdempotencyKey(ctx context.Context, userID, key string) (*domain.Order, error) {
	var idInt int
	err := r.pool.QueryRow(ctx,
		`SELECT id FROM orders WHERE idempotency_key = $1 AND user_id = $2`,
		key, userID,
	).Scan(&idInt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return r.FindByID(ctx, userID, strconv.Itoa(idInt))
}

// FindByID retrieves an order by ID, scoped to the owning user
func (r *PostgresOrderRepository) FindByID(ctx context.Context, userID, id string) (*domain.Order, error) {
	query := `
		SELECT id, user_id, status, subtotal, shipping, total, created_at
		FROM orders
		WHERE id = $1 AND user_id = $2
	`

	var order domain.Order
	var idInt int
	err := r.pool.QueryRow(ctx, query, id, userID).Scan(
		&idInt,
		&order.UserID,
		&order.Status,
		&order.Subtotal,
		&order.Shipping,
		&order.Total,
		&order.CreatedAt,
	)

	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, err
	}

	order.ID = strconv.Itoa(idInt)

	// Get order items
	itemsQuery := `
		SELECT product_id, product_name, quantity, price, subtotal
		FROM order_items
		WHERE order_id = $1
	`

	rows, err := r.pool.Query(ctx, itemsQuery, idInt)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var item domain.OrderItem
		err := rows.Scan(&item.ProductID, &item.ProductName, &item.Quantity, &item.Price, &item.Subtotal)
		if err != nil {
			return nil, err
		}
		order.Items = append(order.Items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return &order, nil
}

// CountByUserID returns the total number of orders for a user (for pagination).
func (r *PostgresOrderRepository) CountByUserID(ctx context.Context, userID string) (int, error) {
	var total int
	err := r.pool.QueryRow(ctx, `SELECT count(*) FROM orders WHERE user_id = $1`, userID).Scan(&total)
	if err != nil {
		return 0, err
	}
	return total, nil
}

// FindByUserID retrieves a page of orders for a user
func (r *PostgresOrderRepository) FindByUserID(ctx context.Context, userID string, limit, offset int) ([]domain.Order, error) {
	query := `
		SELECT id, user_id, status, subtotal, shipping, total, created_at
		FROM orders
		WHERE user_id = $1
		ORDER BY created_at DESC
		LIMIT $2 OFFSET $3
	`

	rows, err := r.pool.Query(ctx, query, userID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var orders []domain.Order
	for rows.Next() {
		var order domain.Order
		var idInt int
		err := rows.Scan(&idInt, &order.UserID, &order.Status, &order.Subtotal, &order.Shipping, &order.Total, &order.CreatedAt)
		if err != nil {
			return nil, err
		}
		order.ID = strconv.Itoa(idInt)
		orders = append(orders, order)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return orders, nil
}

// Create creates a new order
func (r *PostgresOrderRepository) Create(ctx context.Context, order *domain.Order) error {
	query := `
		INSERT INTO orders (user_id, status, subtotal, shipping, total, created_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id
	`

	var id int
	err := r.pool.QueryRow(ctx, query,
		order.UserID,
		order.Status,
		order.Subtotal,
		order.Shipping,
		order.Total,
		time.Now(),
	).Scan(&id)

	if err != nil {
		return err
	}

	order.ID = strconv.Itoa(id)

	// Insert order items
	for _, item := range order.Items {
		itemQuery := `
			INSERT INTO order_items (order_id, product_id, product_name, quantity, price, subtotal)
			VALUES ($1, $2, $3, $4, $5, $6)
		`
		_, err := r.pool.Exec(ctx, itemQuery, id, item.ProductID, item.ProductName, item.Quantity, item.Price, item.Subtotal)
		if err != nil {
			return err
		}
	}

	return nil
}

// CreateWithTx creates a new order within a transaction
func (r *PostgresOrderRepository) CreateWithTx(ctx context.Context, tx domain.Transaction, order *domain.Order) error {
	pgxTx, ok := tx.(*PostgresTransaction)
	if !ok {
		return errors.New("invalid transaction type")
	}

	query := `
		INSERT INTO orders (user_id, status, subtotal, shipping, total, idempotency_key, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING id
	`

	// Pass NULL (not "") when no key, so the partial unique index doesn't collide
	// across keyless orders.
	var idemKey *string
	if order.IdempotencyKey != "" {
		idemKey = &order.IdempotencyKey
	}

	var id int
	err := pgxTx.QueryRow(ctx, query,
		order.UserID,
		order.Status,
		order.Subtotal,
		order.Shipping,
		order.Total,
		idemKey,
		time.Now(),
	).Scan(&id)

	if err != nil {
		// A concurrent double-submit can race past the FindByIdempotencyKey
		// pre-check and hit the (user_id, idempotency_key) unique index here.
		// Surface it as ErrConflict so the logic layer replays the existing
		// order (201) instead of returning an opaque 500.
		if isUniqueViolation(err) {
			return domain.ErrConflict
		}
		return err
	}

	order.ID = strconv.Itoa(id)

	// Insert order items
	for _, item := range order.Items {
		itemQuery := `
			INSERT INTO order_items (order_id, product_id, product_name, quantity, price, subtotal)
			VALUES ($1, $2, $3, $4, $5, $6)
		`
		err := pgxTx.Exec(ctx, itemQuery, id, item.ProductID, item.ProductName, item.Quantity, item.Price, item.Subtotal)
		if err != nil {
			return err
		}
	}

	return nil
}

// UpdateStatus updates the status of an order
func (r *PostgresOrderRepository) UpdateStatus(ctx context.Context, id, status string) error {
	query := `
		UPDATE orders
		SET status = $1, updated_at = NOW()
		WHERE id = $2
	`

	result, err := r.pool.Exec(ctx, query, status, id)
	if err != nil {
		return err
	}

	if result.RowsAffected() == 0 {
		return domain.ErrNotFound
	}

	return nil
}
