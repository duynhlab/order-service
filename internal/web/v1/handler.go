package v1

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/duynhlab/order-service/internal/core/domain"
	logicv1 "github.com/duynhlab/order-service/internal/logic/v1"
	"github.com/duynhlab/order-service/middleware"
	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
)

// errAuthRequired is the response message when a request lacks a valid user.
const errAuthRequired = "Authentication required"

// OrderHandler holds the order service dependency and the downstream clients
// used by the order web layer (cart, notification, shipping).
type OrderHandler struct {
	orderService       *logicv1.OrderService
	cartClient         *CartClient
	notificationClient *NotificationGRPCClient
	shippingClient     shipmentFetcher
}

// NewOrderHandler creates a new order handler with dependency injection.
func NewOrderHandler(
	orderService *logicv1.OrderService,
	cartClient *CartClient,
	notificationClient *NotificationGRPCClient,
	shippingClient shipmentFetcher,
) *OrderHandler {
	return &OrderHandler{
		orderService:       orderService,
		cartClient:         cartClient,
		notificationClient: notificationClient,
		shippingClient:     shippingClient,
	}
}

func (h *OrderHandler) ListOrders(c *gin.Context) {
	ctx, span := middleware.StartSpan(c.Request.Context(), "http.request", trace.WithAttributes(
		attribute.String("layer", "web"),
		attribute.String("method", c.Request.Method),
		attribute.String("path", c.Request.URL.Path),
	))
	defer span.End()

	zapLogger := middleware.GetLoggerFromGinContext(c)

	// Get userID from auth context (required - no fallback)
	userID := c.GetString("user_id")
	if userID == "" {
		zapLogger.Warn("ListOrders: no user_id in context")
		c.JSON(http.StatusUnauthorized, gin.H{"error": errAuthRequired})
		return
	}

	orders, err := h.orderService.ListOrders(ctx, userID)
	if err != nil {
		span.RecordError(err)
		zapLogger.Error("Failed to list orders", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
		return
	}

	zapLogger.Info("Orders listed", zap.Int("count", len(orders)))
	c.JSON(http.StatusOK, orders)
}

func (h *OrderHandler) GetOrder(c *gin.Context) {
	ctx, span := middleware.StartSpan(c.Request.Context(), "http.request", trace.WithAttributes(
		attribute.String("layer", "web"),
		attribute.String("method", c.Request.Method),
		attribute.String("path", c.Request.URL.Path),
	))
	defer span.End()

	zapLogger := middleware.GetLoggerFromGinContext(c)

	// Get userID from auth context (required - no fallback)
	userID := c.GetString("user_id")
	if userID == "" {
		zapLogger.Warn("GetOrder: no user_id in context")
		c.JSON(http.StatusUnauthorized, gin.H{"error": errAuthRequired})
		return
	}

	id := c.Param("id")
	span.SetAttributes(attribute.String("order.id", id))

	order, err := h.orderService.GetOrder(ctx, userID, id)
	if err != nil {
		span.RecordError(err)
		zapLogger.Error("Failed to get order", zap.Error(err))

		switch {
		case errors.Is(err, logicv1.ErrOrderNotFound):
			c.JSON(http.StatusNotFound, gin.H{"error": "Order not found"})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
		}
		return
	}

	zapLogger.Info("Order retrieved", zap.String("order_id", id))
	c.JSON(http.StatusOK, order)
}

// handleIdempotentReplay returns true (and writes the HTTP response) if the
// request's Idempotency-Key already produced an order, or if the lookup failed.
// It returns false to let creation proceed. Must run before the cart is read.
func (h *OrderHandler) handleIdempotentReplay(ctx context.Context, c *gin.Context, userID, key string) bool {
	if key == "" {
		return false
	}
	zapLogger := middleware.GetLoggerFromGinContext(c)
	existing, err := h.orderService.GetByIdempotencyKey(ctx, userID, key)
	if err != nil {
		trace.SpanFromContext(ctx).RecordError(err)
		zapLogger.Error("CreateOrder: idempotency lookup failed", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
		return true
	}
	if existing != nil {
		zapLogger.Info("CreateOrder: idempotent replay", zap.String("order_id", existing.ID))
		c.JSON(http.StatusCreated, existing)
		return true
	}
	return false
}

// loadCartItems sources order items from the authenticated user's cart — the
// server-side source of truth for pricing. Client-supplied items/prices are
// ignored. On failure it writes the response and returns ok=false.
func (h *OrderHandler) loadCartItems(ctx context.Context, c *gin.Context) ([]domain.OrderItem, bool) {
	zapLogger := middleware.GetLoggerFromGinContext(c)
	if h.cartClient == nil {
		zapLogger.Error("CreateOrder: cart client not configured")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
		return nil, false
	}
	cart, err := h.cartClient.GetCart(ctx, c.GetHeader("Authorization"))
	if err != nil {
		trace.SpanFromContext(ctx).RecordError(err)
		zapLogger.Error("CreateOrder: failed to read cart", zap.Error(err))
		c.JSON(http.StatusBadGateway, gin.H{"error": "Unable to read cart"})
		return nil, false
	}
	if len(cart.Items) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Cart is empty"})
		return nil, false
	}
	items := make([]domain.OrderItem, len(cart.Items))
	for i, it := range cart.Items {
		items[i] = domain.OrderItem{
			ProductID:   it.ProductID,
			ProductName: it.ProductName,
			Quantity:    it.Quantity,
			Price:       it.ProductPrice, // authoritative server-side price
		}
	}
	return items, true
}

// clearCartBestEffort clears the user's cart after a committed order. Failures
// are logged, never fatal — the order is already persisted.
func (h *OrderHandler) clearCartBestEffort(c *gin.Context, zapLogger *zap.Logger) {
	if h.cartClient == nil {
		zapLogger.Warn("Cart client not initialized")
		return
	}
	// Detach from the request context so a client disconnect can't cancel this.
	clearCtx, cancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), 3*time.Second)
	defer cancel()
	if err := h.cartClient.ClearCart(clearCtx, c.GetHeader("Authorization")); err != nil {
		trace.SpanFromContext(c.Request.Context()).RecordError(err)
		zapLogger.Warn("Best-effort cart clear failed", zap.Error(err))
	}
}

// publishOrderCreated sends a best-effort "order placed" notification after a
// committed order. Failures are logged, never fatal — the order is already
// persisted.
func (h *OrderHandler) publishOrderCreated(c *gin.Context, zapLogger *zap.Logger, order *domain.Order) {
	if h.notificationClient == nil {
		zapLogger.Warn("Notification client not initialized")
		return
	}
	// Detach from the request context so a client disconnect can't cancel this.
	notifyCtx, cancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), 3*time.Second)
	defer cancel()
	if err := h.notificationClient.PublishOrderCreated(notifyCtx, order.UserID, order.ID, order.Total); err != nil {
		trace.SpanFromContext(c.Request.Context()).RecordError(err)
		zapLogger.Warn("Best-effort order-created notification failed", zap.Error(err))
	}
}

func (h *OrderHandler) CreateOrder(c *gin.Context) {
	ctx, span := middleware.StartSpan(c.Request.Context(), "http.request", trace.WithAttributes(
		attribute.String("layer", "web"),
		attribute.String("method", c.Request.Method),
		attribute.String("path", c.Request.URL.Path),
	))
	defer span.End()

	zapLogger := middleware.GetLoggerFromGinContext(c)

	var req domain.CreateOrderRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		span.SetAttributes(attribute.Bool("request.valid", false))
		span.RecordError(err)
		zapLogger.Error("Invalid request", zap.Error(err))
		c.JSON(http.StatusBadRequest, gin.H{"error": sanitizeValidationError(err)})
		return
	}

	// Inject user_id from auth context - never trust client
	userID := c.GetString("user_id")
	if userID == "" {
		zapLogger.Warn("CreateOrder: no user_id in context")
		c.JSON(http.StatusUnauthorized, gin.H{"error": errAuthRequired})
		return
	}
	req.UserID = userID
	req.IdempotencyKey = c.GetHeader("Idempotency-Key")

	// Idempotency replay must run BEFORE reading the cart: a retry happens after
	// the first successful order already cleared the cart.
	if h.handleIdempotentReplay(ctx, c, userID, req.IdempotencyKey) {
		return
	}

	items, ok := h.loadCartItems(ctx, c)
	if !ok {
		return
	}
	req.Items = items

	span.SetAttributes(attribute.Bool("request.valid", true))
	order, err := h.orderService.CreateOrder(ctx, req)
	if err != nil {
		span.RecordError(err)
		zapLogger.Error("Failed to create order", zap.Error(err))

		switch {
		case errors.Is(err, logicv1.ErrInvalidOrder):
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid order"})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
		}
		return
	}

	zapLogger.Info("Order created", zap.String("order_id", order.ID))
	h.clearCartBestEffort(c, zapLogger)
	h.publishOrderCreated(c, zapLogger, order)

	c.JSON(http.StatusCreated, order)
}
