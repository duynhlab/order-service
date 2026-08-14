package repository

import (
	"context"
	"errors"
	"fmt"
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
// The outbox row is joined in rather than read separately, and that is a
// correctness choice, not a round-trip saving. A replayed order is started by
// whoever replays it, and the branch it belongs on is the one its row recorded —
// so the two values must come from ONE snapshot. They do here by construction:
// order and row are written in the same transaction, so no snapshot can hold the
// order without the row, and a lagging replica cannot answer with a mismatched
// pair. LEFT JOIN because orders that predate the outbox have no row.
func (r *PostgresOrderRepository) FindByIdempotencyKey(ctx context.Context, userID, key string) (*domain.Order, error) {
	var idInt int
	var participant string
	err := r.pool.QueryRow(ctx, `
		SELECT o.id, COALESCE(f.participant, '')
		FROM orders o
		LEFT JOIN fulfillment_start_requests f ON f.order_id = o.id
		WHERE o.idempotency_key = $1 AND o.user_id = $2
	`, key, userID).Scan(&idInt, &participant)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	order, err := r.FindByID(ctx, userID, strconv.Itoa(idInt))
	if err != nil {
		return nil, err
	}
	order.StockParticipant = participant
	return order, nil
}

// FindByID retrieves an order by ID, scoped to the owning user
func (r *PostgresOrderRepository) FindByID(ctx context.Context, userID, id string) (*domain.Order, error) {
	query := `
		SELECT id, user_id, status, subtotal, shipping, tax, discount, total, created_at, version
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
		&order.Tax,
		&order.Discount,
		&order.Total,
		&order.CreatedAt,
		&order.Version,
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

// LoadForFulfillment reads an order by id with NO user scope, for the
// fulfillment dispatcher (RFC-0021 P3). It satisfies domain.OrderLoader, which
// exists precisely so this cannot be reached through the interface the request
// path holds — see that interface's doc for why.
func (r *PostgresOrderRepository) LoadForFulfillment(ctx context.Context, orderID string) (*domain.Order, error) {
	var order domain.Order
	var idInt int
	err := r.pool.QueryRow(ctx, `
		SELECT id, user_id, status, subtotal, shipping, tax, discount, total, created_at
		FROM orders
		WHERE id = $1
	`, orderID).Scan(
		&idInt, &order.UserID, &order.Status, &order.Subtotal,
		&order.Shipping, &order.Tax, &order.Discount, &order.Total, &order.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	order.ID = strconv.Itoa(idInt)

	items, err := r.loadItems(ctx, idInt)
	if err != nil {
		return nil, err
	}
	order.Items = items
	return &order, nil
}

// loadItems reads an order's line items. Shared by the scoped and unscoped
// readers so the two cannot drift in what they consider an order.
func (r *PostgresOrderRepository) loadItems(ctx context.Context, orderID int) ([]domain.OrderItem, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT product_id, product_name, quantity, price, subtotal
		FROM order_items
		WHERE order_id = $1
	`, orderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var items []domain.OrderItem
	for rows.Next() {
		var item domain.OrderItem
		if err := rows.Scan(&item.ProductID, &item.ProductName, &item.Quantity, &item.Price, &item.Subtotal); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// Terminal order statuses the reconciler cares about, aliased from the
// domain vocabulary (the FSM in core/domain is the single authority since
// RFC-0021 P5).
const (
	orderStatusConfirmed = string(domain.OrderStatusConfirmed)
	orderStatusFailed    = string(domain.OrderStatusFailed)
	// completed is confirmed-plus-bookkeeping: stock committed, same
	// settlement expectations. It joins the reconcile scans so an order that
	// completes before its settle delay elapses does not vanish from them.
	orderStatusCompleted = string(domain.OrderStatusCompleted)
)

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
		SELECT id, user_id, status, subtotal, shipping, tax, discount, total, created_at
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
		err := rows.Scan(&idInt, &order.UserID, &order.Status, &order.Subtotal, &order.Shipping, &order.Tax, &order.Discount, &order.Total, &order.CreatedAt)
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
		INSERT INTO orders (user_id, status, subtotal, shipping, tax, discount, total, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING id
	`

	var id int
	err := r.pool.QueryRow(ctx, query,
		order.UserID,
		order.Status,
		order.Subtotal,
		order.Shipping,
		order.Tax,
		order.Discount,
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
		INSERT INTO orders (user_id, status, subtotal, shipping, tax, discount, total, idempotency_key, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
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
		order.Tax,
		order.Discount,
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

// ListAll returns one cross-customer page, newest first, plus the unpaged
// total (RFC-0023 protected reads — the operator's view has no owner scope).
func (r *PostgresOrderRepository) ListAll(ctx context.Context, status string, limit, offset int) ([]domain.Order, int, error) {
	where := ""
	args := []any{}
	if status != "" {
		args = append(args, status)
		where = " WHERE status = $1"
	}

	var total int
	if err := r.pool.QueryRow(ctx, "SELECT count(*) FROM orders"+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count orders: %w", err)
	}

	query := fmt.Sprintf(`
		SELECT id, user_id, status, subtotal, shipping, tax, discount, total, created_at
		FROM orders%s
		ORDER BY created_at DESC, id DESC
		LIMIT $%d OFFSET $%d`, where, len(args)+1, len(args)+2)
	rows, err := r.pool.Query(ctx, query, append(args, limit, offset)...)
	if err != nil {
		return nil, 0, fmt.Errorf("list orders: %w", err)
	}
	defer rows.Close()

	orders := make([]domain.Order, 0)
	for rows.Next() {
		var order domain.Order
		var idInt int
		if err := rows.Scan(&idInt, &order.UserID, &order.Status, &order.Subtotal, &order.Shipping,
			&order.Tax, &order.Discount, &order.Total, &order.CreatedAt); err != nil {
			return nil, 0, fmt.Errorf("scan order: %w", err)
		}
		order.ID = strconv.Itoa(idInt)
		orders = append(orders, order)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate orders: %w", err)
	}
	return orders, total, nil
}

// FindByIDUnscoped loads one order with its items for the operator case view
// (RFC-0023) — deliberately no owner filter; the role gate is the authority.
func (r *PostgresOrderRepository) FindByIDUnscoped(ctx context.Context, id string) (*domain.Order, error) {
	var order domain.Order
	var idInt int
	err := r.pool.QueryRow(ctx, `
		SELECT id, user_id, status, subtotal, shipping, tax, discount, total, created_at, version
		FROM orders
		WHERE id = $1`, id).Scan(
		&idInt, &order.UserID, &order.Status, &order.Subtotal, &order.Shipping,
		&order.Tax, &order.Discount, &order.Total, &order.CreatedAt, &order.Version,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	order.ID = strconv.Itoa(idInt)

	rows, err := r.pool.Query(ctx, `
		SELECT product_id, product_name, quantity, price, subtotal
		FROM order_items
		WHERE order_id = $1`, idInt)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var item domain.OrderItem
		if err := rows.Scan(&item.ProductID, &item.ProductName, &item.Quantity, &item.Price, &item.Subtotal); err != nil {
			return nil, err
		}
		order.Items = append(order.Items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return &order, nil
}
