package v1

import (
	"context"
	"errors"
	"net/http"

	"github.com/duynhlab/order-service/internal/core/domain"
	"github.com/duynhlab/order-service/internal/fulfillment"
	logicv1 "github.com/duynhlab/order-service/internal/logic/v1"
	"github.com/duynhlab/order-service/middleware"
	"github.com/duynhlab/pkg/httpx"
	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"go.temporal.io/sdk/client"
	"go.uber.org/zap"
)

// errAuthRequired is the response message when a request lacks a valid user.
const errAuthRequired = "Authentication required"

// WorkflowStarter starts a Temporal workflow. *client.Client (go.temporal.io/sdk)
// satisfies it; kept as an interface so the handler is testable.
type WorkflowStarter interface {
	ExecuteWorkflow(ctx context.Context, options client.StartWorkflowOptions, workflow any, args ...any) (client.WorkflowRun, error)
}

// OrderHandler holds the order service dependency and the downstream clients
// used by the order web layer (cart, shipping) plus the Temporal starter that
// kicks off the order-fulfillment saga after an order is created.
type OrderHandler struct {
	orderService   *logicv1.OrderService
	cartClient     *CartClient
	shippingClient shipmentFetcher
	// temporal is nil when Temporal is unavailable; CreateOrder then leaves the
	// order in "pending" (the saga isn't started) rather than failing checkout.
	temporal  WorkflowStarter
	taskQueue string
	// paymentClient enriches order details with the payment snapshot (soft-fail;
	// nil when the payment gRPC dial failed at startup).
	paymentClient PaymentFetcher
}

// NewOrderHandler creates a new order handler with dependency injection.
func NewOrderHandler(
	orderService *logicv1.OrderService,
	cartClient *CartClient,
	shippingClient shipmentFetcher,
	temporal WorkflowStarter,
	taskQueue string,
	paymentClient PaymentFetcher,
) *OrderHandler {
	return &OrderHandler{
		orderService:   orderService,
		cartClient:     cartClient,
		shippingClient: shippingClient,
		temporal:       temporal,
		taskQueue:      taskQueue,
		paymentClient:  paymentClient,
	}
}

// writeOrderLookupError maps an order-lookup error to the HTTP error envelope:
// 404 when the order is missing, 500 otherwise. Shared by GetOrder and
// GetOrderDetails so the mapping lives in one place.
func writeOrderLookupError(c *gin.Context, err error) {
	if errors.Is(err, logicv1.ErrOrderNotFound) {
		httpx.RespondError(c, http.StatusNotFound, httpx.CodeNotFound, "Order not found")
		return
	}
	httpx.RespondError(c, http.StatusInternalServerError, httpx.CodeInternal, "Internal server error")
}

// beginAuthed starts the web request span and resolves the authenticated user
// id. On missing auth it writes 401, ends the span, and returns ok=false (the
// caller must return immediately). On success the caller owns the span and must
// defer span.End().
func (h *OrderHandler) beginAuthed(c *gin.Context, op string) (context.Context, trace.Span, *zap.Logger, string, bool) {
	ctx, span := middleware.StartSpan(c.Request.Context(), "http.request", trace.WithAttributes(
		attribute.String("layer", "web"),
		attribute.String("method", c.Request.Method),
		attribute.String("path", c.Request.URL.Path),
	))
	zapLogger := middleware.GetLoggerFromGinContext(c)
	userID := c.GetString("user_id")
	if userID == "" {
		zapLogger.Warn(op + ": no user_id in context")
		httpx.RespondError(c, http.StatusUnauthorized, httpx.CodeUnauthorized, errAuthRequired)
		span.End()
		return ctx, span, zapLogger, "", false
	}
	return ctx, span, zapLogger, userID, true
}

func (h *OrderHandler) ListOrders(c *gin.Context) {
	ctx, span, zapLogger, userID, ok := h.beginAuthed(c, "ListOrders")
	if !ok {
		return
	}
	defer span.End()

	page, pageSize := httpx.ParsePage(c)
	orders, total, err := h.orderService.ListOrders(ctx, userID, pageSize, httpx.Offset(page, pageSize))
	if err != nil {
		span.RecordError(err)
		zapLogger.Error("Failed to list orders", zap.Error(err))
		httpx.RespondError(c, http.StatusInternalServerError, httpx.CodeInternal, "Internal server error")
		return
	}

	zapLogger.Info("Orders listed", zap.Int("count", len(orders)))
	c.JSON(http.StatusOK, httpx.NewPaginated(toOrderResponses(orders), page, pageSize, total))
}

func (h *OrderHandler) GetOrder(c *gin.Context) {
	ctx, span, zapLogger, userID, ok := h.beginAuthed(c, "GetOrder")
	if !ok {
		return
	}
	defer span.End()

	id := c.Param("id")
	span.SetAttributes(attribute.String("order.id", id))

	order, err := h.orderService.GetOrder(ctx, userID, id)
	if err != nil {
		span.RecordError(err)
		zapLogger.Error("Failed to get order", zap.Error(err))
		writeOrderLookupError(c, err)
		return
	}

	zapLogger.Info("Order retrieved", zap.String("order_id", id))
	c.JSON(http.StatusOK, toOrderResponse(*order))
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
		httpx.RespondError(c, http.StatusInternalServerError, httpx.CodeInternal, "Internal server error")
		return true
	}
	if existing != nil {
		zapLogger.Info("CreateOrder: idempotent replay", zap.String("order_id", existing.ID))
		c.JSON(http.StatusCreated, toOrderResponse(*existing))
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
		httpx.RespondError(c, http.StatusInternalServerError, httpx.CodeInternal, "Internal server error")
		return nil, false
	}
	cart, err := h.cartClient.GetCart(ctx, c.GetHeader("Authorization"))
	if err != nil {
		trace.SpanFromContext(ctx).RecordError(err)
		zapLogger.Error("CreateOrder: failed to read cart", zap.Error(err))
		httpx.RespondError(c, http.StatusBadGateway, httpx.CodeInternal, "Unable to read cart")
		return nil, false
	}
	if len(cart.Items) == 0 {
		httpx.RespondError(c, http.StatusBadRequest, httpx.CodeValidation, "Cart is empty")
		return nil, false
	}
	items := make([]domain.OrderItem, len(cart.Items))
	for i, it := range cart.Items {
		items[i] = domain.OrderItem{
			ProductID:   it.ProductID,
			ProductName: it.ProductName,
			Quantity:    it.Quantity,
			Price:       domain.MinorUnits(it.ProductPrice), // dollars → minor units at ingress
		}
	}
	return items, true
}

// startFulfillment kicks off the durable order-fulfillment saga for a committed
// order (status "pending"). It returns immediately — the workflow drives the
// order to "confirmed"/"failed" and handles notification + cart-clear. A start
// failure is logged, never fatal: the order is already persisted and can be
// reconciled. No bearer token is passed: the saga's best-effort cart-clear step
// uses cart's tokenless internal endpoint (identified by user ID).
func (h *OrderHandler) startFulfillment(c *gin.Context, zapLogger *zap.Logger, order *domain.Order, paymentMethod string) {
	if h.temporal == nil {
		zapLogger.Warn("Temporal unavailable; order left pending without a fulfillment saga",
			zap.String("order_id", order.ID))
		return
	}

	// Delegate to the shared starter (input mapping, detached 5s context,
	// workflow-id dedup — internal/fulfillment). Web semantics unchanged:
	// default reuse policy, every start failure — including AlreadyStarted —
	// logged like before.
	if err := fulfillment.Start(c.Request.Context(), h.temporal, h.taskQueue, order, paymentMethod, fulfillment.Options{}); err != nil {
		trace.SpanFromContext(c.Request.Context()).RecordError(err)
		zapLogger.Error("Failed to start fulfillment workflow", zap.String("order_id", order.ID), zap.Error(err))
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
		httpx.RespondError(c, http.StatusBadRequest, httpx.CodeValidation, sanitizeValidationError(err))
		return
	}

	// Validate the optional checkout payment token BEFORE anything persists or
	// rides the workflow input: the input is durable Temporal history, so a
	// PAN-shaped string must be rejected here, not first at the payment service
	// (which stays the authoritative validator). Empty is allowed — the saga
	// falls back to its demo token for API-created orders. Never silently
	// substitute an instrument for malformed input.
	if req.PaymentMethod != "" && !isTestToken(req.PaymentMethod) {
		httpx.RespondError(c, http.StatusBadRequest, httpx.CodeValidation,
			`payment_method must be an opaque "tok_" token`)
		return
	}

	// Inject user_id from auth context - never trust client
	userID := c.GetString("user_id")
	if userID == "" {
		zapLogger.Warn("CreateOrder: no user_id in context")
		httpx.RespondError(c, http.StatusUnauthorized, httpx.CodeUnauthorized, errAuthRequired)
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
			httpx.RespondError(c, http.StatusBadRequest, httpx.CodeValidation, "Invalid order")
		default:
			httpx.RespondError(c, http.StatusInternalServerError, httpx.CodeInternal, "Internal server error")
		}
		return
	}

	zapLogger.Info("Order created", zap.String("order_id", order.ID))
	h.startFulfillment(c, zapLogger, order, req.PaymentMethod)

	c.JSON(http.StatusCreated, toOrderResponse(*order))
}
