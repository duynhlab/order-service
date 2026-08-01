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

// OrderService handles order business logic
type OrderService struct {
	orderRepo domain.OrderRepository
	txManager domain.TransactionManager
	// startRequests writes the outbox row inside the order's transaction. The
	// logic layer needs the enqueue and nothing else from the repository, but
	// EnqueueWithTx lives on the full interface, so this stays the full type and
	// the request-facing close goes through startCloser instead.
	startRequests domain.StartRequestRepository
	// startCloser is the ONE user-scoped operation the request path may perform.
	startCloser domain.StartRequestCloser
	projection  domain.ProcessingProjector
	// statusTxWriter + cancellations exist for exactly one composed write:
	// the cancel path's CAS + outbox arm in a single transaction.
	statusTxWriter domain.OrderStatusTxWriter
	cancellations  domain.CancellationRequestStore
}

// NewOrderService creates a new OrderService with repository injection
func NewOrderService(orderRepo domain.OrderRepository, txManager domain.TransactionManager,
	startRequests domain.StartRequestRepository, startCloser domain.StartRequestCloser,
	projection domain.ProcessingProjector, statusTxWriter domain.OrderStatusTxWriter,
	cancellations domain.CancellationRequestStore) *OrderService {
	return &OrderService{
		orderRepo:      orderRepo,
		txManager:      txManager,
		startRequests:  startRequests,
		startCloser:    startCloser,
		projection:     projection,
		statusTxWriter: statusTxWriter,
		cancellations:  cancellations,
	}
}

// MarkFulfillmentStarted closes the order's outbox row and clears its payment
// token, recording that the saga is running.
//
// Best-effort on purpose, and safe to be: by the time a caller reaches here the
// workflow IS started, so a failure only leaves a PENDING row the dispatcher
// will pick up and re-attempt — and Temporal answers that attempt with
// AlreadyStarted, which the dispatcher treats as success. The cost of failing
// here is one redundant round trip, never a lost or duplicated saga. The error
// is returned rather than swallowed so the transport can log it with its own
// request context.
// It is scoped by user: both transports have the id of the user whose order they
// just created, and passing it means a future "retry my fulfillment" endpoint
// cannot be turned into a way to close a stranger's row.
func (s *OrderService) MarkFulfillmentStarted(ctx context.Context, userID, orderID string) error {
	return s.startCloser.MarkDispatchedForUser(ctx, userID, orderID)
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
	// equal the session total the shopper confirmed. Checkout is the ONLY
	// caller since the legacy REST create was removed (RFC-0021 P5).
	shipping, tax, discount := req.ShippingFeeMinor, req.TaxMinor, req.DiscountMinor

	// Create order domain model
	order := &domain.Order{
		UserID:         req.UserID,
		Items:          enrichedItems,
		Subtotal:       subtotal,
		Shipping:       shipping,
		Tax:            tax,
		Discount:       discount,
		Total:          subtotal + shipping + tax - discount,
		Status:         string(domain.OrderStatusPending),
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

	// The outbox row goes in the SAME transaction as the order (RFC-0021 P3).
	// This is the whole durability argument: after the commit below, either the
	// order exists with a record that its saga is owed, or neither exists. A
	// crash between COMMIT and the caller's inline start is then a retry rather
	// than an order stuck `pending` with nothing left to drive it.
	//
	// It is NOT best-effort. Failing the create is the correct outcome: an order
	// whose saga nothing remembers to start is worse than no order, because the
	// customer sees a created order that never progresses.
	if err := s.startRequests.EnqueueWithTx(ctx, tx, order.ID, req.PaymentMethod, req.StockParticipant); err != nil {
		span.RecordError(err)
		return nil, err
	}

	if err := s.seedProjection(ctx, tx, order.ID); err != nil {
		span.RecordError(err)
		return nil, err
	}

	// The order carries the participant it was stamped with, so every caller reads
	// it from ONE place — the order — whether this request created it or replayed
	// somebody else's. No re-read: this transaction wrote the row, so the value in
	// hand is the row's.
	order.StockParticipant = req.StockParticipant

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
	recordOrderValue(ctx, order.Total)

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

// seedProjection writes the ORDER_CREATED projection row inside the order's
// own transaction — the one place the projection is transactional, so
// /details never renders a created order with no processing block
// (RFC-0021 P5). Later stage writes are best-effort from the workflows.
func (s *OrderService) seedProjection(ctx context.Context, tx domain.Transaction, orderID string) error {
	return s.projection.UpsertProcessingStageWithTx(ctx, tx, domain.ProcessingUpdate{
		OrderID: orderID, Stage: domain.StageOrderCreated,
	})
}

// CancelOutcome is CancelOrder's result: the order as last read, the epoch
// the episode was opened with, and whether the call was an idempotent
// replay of an already-cancelling/cancelled order.
type CancelOutcome struct {
	Order    *domain.Order
	Epoch    int64
	Replayed bool
}

// CancelOrder opens a cancellation episode for the user's own order
// (RFC-0021 P5): in ONE transaction, the confirmed/completed → cancelling
// CAS and the cancellation-outbox arm commit together, so a crash between
// them cannot leave an order telling the customer it is cancelling with
// nothing left to drive it. The caller inline-starts the workflow after the
// commit; the dispatcher sweeps whatever the inline path misses.
//
// The epoch is the orders.version READ BY THIS SERVER while holding the
// request — never client-supplied — and namespaces the episode's command
// ids and workflow id.
func (s *OrderService) CancelOrder(ctx context.Context, userID, orderID string) (CancelOutcome, error) {
	order, err := s.orderRepo.FindByID(ctx, userID, orderID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return CancelOutcome{}, ErrOrderNotFound
		}
		return CancelOutcome{}, err
	}

	switch domain.OrderStatus(order.Status) { //nolint:exhaustive // default IS the refusal arm
	case domain.OrderStatusCancelling, domain.OrderStatusCancelled:
		// Idempotent replay: the episode already exists (or finished).
		return CancelOutcome{Order: order, Epoch: order.Version, Replayed: true}, nil
	case domain.OrderStatusConfirmed, domain.OrderStatusCompleted:
		// Cancellable; fall through.
	default:
		// pending / failed / manual_review — and any unknown legacy value.
		return CancelOutcome{}, ErrOrderNotCancellable
	}

	cmd, err := domain.NewRequestCancellationCommand(orderID, userID, order.Version)
	if err != nil {
		return CancelOutcome{}, err
	}

	tx, err := s.txManager.Begin(ctx)
	if err != nil {
		return CancelOutcome{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	replayed, err := s.statusTxWriter.ApplyStatusCommandWithTx(ctx, tx, cmd)
	if err != nil {
		// The CAS lost a race (the saga completed the order, another cancel
		// won, an operator moved it). Reload once and answer from the truth
		// instead of surfacing a raw conflict.
		if errors.Is(err, domain.ErrInvalidTransition) || errors.Is(err, domain.ErrConcurrencyConflict) {
			_ = tx.Rollback(ctx)
			fresh, ferr := s.orderRepo.FindByID(ctx, userID, orderID)
			if ferr == nil {
				switch domain.OrderStatus(fresh.Status) { //nolint:exhaustive // only the replay states matter here
				case domain.OrderStatusCancelling, domain.OrderStatusCancelled:
					return CancelOutcome{Order: fresh, Epoch: fresh.Version, Replayed: true}, nil
				}
			}
			return CancelOutcome{}, ErrOrderNotCancellable
		}
		return CancelOutcome{}, err
	}

	if err := s.cancellations.ArmWithTx(ctx, tx, orderID, order.Version); err != nil {
		return CancelOutcome{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return CancelOutcome{}, err
	}

	order.Status = string(domain.OrderStatusCancelling)
	// A same-epoch race (two concurrent cancels) replays the ledger's
	// outcome: report it as the replay it is, so the transport answers 200.
	return CancelOutcome{Order: order, Epoch: order.Version, Replayed: replayed}, nil
}
