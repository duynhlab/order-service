package v1

import (
	"context"
	"errors"

	"github.com/duynhlab/order-service/internal/core/domain"
	"github.com/duynhlab/order-service/middleware"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// attrUserID is the tracing-span attribute key for the authenticated user id.
const attrUserID = "user.id"

// demoShippingMinor is the fixed demo shipping fee in minor units ($5.00).
const demoShippingMinor int64 = 500

// OrderService handles order business logic
type OrderService struct {
	orderRepo domain.OrderRepository
	txManager domain.TransactionManager
}

// NewOrderService creates a new OrderService with repository injection
func NewOrderService(orderRepo domain.OrderRepository, txManager domain.TransactionManager) *OrderService {
	return &OrderService{
		orderRepo: orderRepo,
		txManager: txManager,
	}
}

// ListOrders retrieves a page of orders for a user, returning the page and the
// total count of the user's orders (for pagination).
func (s *OrderService) ListOrders(ctx context.Context, userID string, limit, offset int) ([]domain.Order, int, error) {
	ctx, span := middleware.StartSpan(ctx, "order.list", trace.WithAttributes(
		attribute.String("layer", "logic"),
		attribute.String(attrUserID, userID),
	))
	defer span.End()

	total, err := s.orderRepo.CountByUserID(ctx, userID)
	if err != nil {
		span.RecordError(err)
		return nil, 0, err
	}

	// Call repository
	orders, err := s.orderRepo.FindByUserID(ctx, userID, limit, offset)
	if err != nil {
		span.RecordError(err)
		return nil, 0, err
	}

	span.SetAttributes(attribute.Int("orders.count", len(orders)))
	return orders, total, nil
}

// GetOrder retrieves a single order by ID, scoped to the owning user
func (s *OrderService) GetOrder(ctx context.Context, userID, id string) (*domain.Order, error) {
	ctx, span := middleware.StartSpan(ctx, "order.get", trace.WithAttributes(
		attribute.String("layer", "logic"),
		attribute.String(attrUserID, userID),
		attribute.String("order.id", id),
	))
	defer span.End()

	// Call repository
	order, err := s.orderRepo.FindByID(ctx, userID, id)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			span.SetAttributes(attribute.Bool("order.found", false))
			return nil, ErrOrderNotFound
		}
		span.RecordError(err)
		return nil, err
	}

	span.SetAttributes(attribute.Bool("order.found", true))
	return order, nil
}

// GetByIdempotencyKey returns the order previously created with the given key for
// this user, or (nil, nil) if none exists. Used by the web layer to make order
// creation idempotent (a retry returns the existing order rather than a duplicate).
func (s *OrderService) GetByIdempotencyKey(ctx context.Context, userID, key string) (*domain.Order, error) {
	existing, err := s.orderRepo.FindByIdempotencyKey(ctx, userID, key)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return existing, nil
}

// CreateOrder creates a new order with transaction support
func (s *OrderService) CreateOrder(ctx context.Context, req domain.CreateOrderRequest) (*domain.Order, error) {
	ctx, span := middleware.StartSpan(ctx, "order.create", trace.WithAttributes(
		attribute.String("layer", "logic"),
		attribute.String(attrUserID, req.UserID),
	))
	defer span.End()

	// Business validation
	if len(req.Items) == 0 {
		span.SetAttributes(attribute.Bool("order.created", false))
		return nil, ErrInvalidOrder
	}
	for _, item := range req.Items {
		if item.Quantity <= 0 || item.Price < 0 || item.ProductID == "" {
			span.SetAttributes(attribute.Bool("order.created", false))
			return nil, ErrInvalidOrder
		}
	}

	// Enrich order items: Subtotal, ProductName (fallback if empty)
	enrichedItems := make([]domain.OrderItem, len(req.Items))
	var subtotal int64
	for i, item := range req.Items {
		itemSubtotal := item.Price * int64(item.Quantity)
		subtotal += itemSubtotal

		productName := item.ProductName
		if productName == "" {
			productName = "Product " + item.ProductID
		}

		enrichedItems[i] = domain.OrderItem{
			ProductID:   item.ProductID,
			ProductName: productName,
			Quantity:    item.Quantity,
			Price:       item.Price,
			Subtotal:    itemSubtotal,
		}
	}

	// Totals: the machine caller (checkout, RFC-0015 P4) provides the quoted
	// fee, tax, and promo discount — the saga charges THIS total, so it must
	// equal the session total the shopper confirmed. The legacy REST path
	// keeps the demo fee until its P6 retirement.
	shipping := demoShippingMinor
	var tax, discount int64
	if req.TotalsProvided {
		shipping, tax, discount = req.ShippingFeeMinor, req.TaxMinor, req.DiscountMinor
	}

	// Create order domain model
	order := &domain.Order{
		UserID:         req.UserID,
		Items:          enrichedItems,
		Subtotal:       subtotal,
		Shipping:       shipping,
		Tax:            tax,
		Discount:       discount,
		Total:          subtotal + shipping + tax - discount,
		Status:         "pending",
		IdempotencyKey: req.IdempotencyKey,
	}

	// Begin transaction
	tx, err := s.txManager.Begin(ctx)
	if err != nil {
		span.RecordError(err)
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }() // Rollback if not committed

	// Create order with transaction. A racing double-submit trips the
	// (user_id, idempotency_key) unique index after the handler's pre-check
	// missed it — replay the already-committed order so the handler still
	// responds 201, matching the normal idempotent-replay path.
	err = s.orderRepo.CreateWithTx(ctx, tx, order)
	if errors.Is(err, domain.ErrConflict) {
		_ = tx.Rollback(ctx)
		return s.replayIdempotentOrder(ctx, span, req)
	}
	if err != nil {
		span.RecordError(err)
		return nil, err
	}

	// TODO: Update inventory (when inventory service is available)
	// for _, item := range order.Items {
	//     err = s.inventoryRepo.DecrementStockWithTx(ctx, tx, item.ProductID, item.Quantity)
	//     if err != nil {
	//         return nil, ErrInsufficientStock
	//     }
	// }

	// TODO: Clear cart (when cart clearing with transaction is needed)
	// err = s.cartRepo.ClearWithTx(ctx, tx, req.UserID)
	// if err != nil {
	//     return nil, err
	// }

	// Commit transaction
	if err := tx.Commit(ctx); err != nil {
		span.RecordError(err)
		return nil, err
	}

	// Record the order value exactly once, here on the genuine-creation path —
	// never on the idempotent replay path, which returns an already-recorded order.
	recordOrderValue(ctx, order.Total, req.TotalsProvided)

	span.SetAttributes(
		attribute.String("order.id", order.ID),
		attribute.Bool("order.created", true),
	)
	span.AddEvent("order.created")

	return order, nil
}

// replayIdempotentOrder returns the already-committed order for the same
// (user, idempotency key) after a create hit the unique-key conflict, so a
// racing retry replays instead of erroring. The caller has rolled back the
// failed insert's transaction.
func (s *OrderService) replayIdempotentOrder(ctx context.Context, span trace.Span, req domain.CreateOrderRequest) (*domain.Order, error) {
	existing, err := s.orderRepo.FindByIdempotencyKey(ctx, req.UserID, req.IdempotencyKey)
	if err != nil {
		span.RecordError(err)
		return nil, err
	}
	span.SetAttributes(
		attribute.String("order.id", existing.ID),
		attribute.Bool("order.replayed", true),
	)
	return existing, nil
}

// UpdateOrderStatus updates the status of an order
func (s *OrderService) UpdateOrderStatus(ctx context.Context, id, status string) error {
	ctx, span := middleware.StartSpan(ctx, "order.update_status", trace.WithAttributes(
		attribute.String("layer", "logic"),
		attribute.String("order.id", id),
		attribute.String("status", status),
	))
	defer span.End()

	// Call repository
	err := s.orderRepo.UpdateStatus(ctx, id, status)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return ErrOrderNotFound
		}
		span.RecordError(err)
		return err
	}

	span.SetAttributes(attribute.Bool("status.updated", true))
	return nil
}
