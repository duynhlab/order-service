package v1

import (
	"context"
	"errors"
	"net/http"

	"github.com/duynhlab/order-service/internal/core/domain"
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
// used by the order web layer (shipping, payment, inventory) plus the Temporal
// starter that kicks off the cancellation workflow (order creation is the
// checkout service's gRPC call since RFC-0015 P4; the legacy REST create was
// removed in RFC-0021 P5).
type OrderHandler struct {
	orderService   *logicv1.OrderService
	shippingClient shipmentFetcher
	// temporal may be not-ready (cancellation.Ready) while Temporal is
	// unreachable; CancelOrder then leaves the outbox row PENDING for the
	// dispatcher rather than failing the request.
	temporal  WorkflowStarter
	taskQueue string
	// paymentClient enriches order details with the payment snapshot (soft-fail;
	// nil when the payment gRPC dial failed at startup).
	paymentClient PaymentFetcher
	// cancelCloser is the request path's ONLY cancellation-outbox operation,
	// user-scoped (see domain.CancellationCloser).
	cancelCloser domain.CancellationCloser
	// processing + inventoryClient enrich /details (soft-fail; nil = block
	// simply absent).
	processing      processingFetcher
	inventoryClient ReservationFetcher
}

// NewOrderHandler creates a new order handler with dependency injection.
func NewOrderHandler(
	orderService *logicv1.OrderService,
	shippingClient shipmentFetcher,
	temporal WorkflowStarter,
	taskQueue string,
	paymentClient PaymentFetcher,
	cancelCloser domain.CancellationCloser,
	processing processingFetcher,
	inventoryClient ReservationFetcher,
) *OrderHandler {
	return &OrderHandler{
		orderService:    orderService,
		shippingClient:  shippingClient,
		temporal:        temporal,
		taskQueue:       taskQueue,
		paymentClient:   paymentClient,
		cancelCloser:    cancelCloser,
		processing:      processing,
		inventoryClient: inventoryClient,
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

// beginAuthed resolves the otelgin server span and the request logger, then the
// authenticated user id from the auth context. The web layer does not mint its
// own span — otelgin already opened the server span for this request
// (method/route are on it), so handlers annotate that span via the returned
// handle. On missing auth it writes 401 and returns ok=false (the caller must
// return immediately). The caller must NOT end the span; otelgin owns its
// lifecycle.
func (h *OrderHandler) beginAuthed(c *gin.Context, op string) (context.Context, trace.Span, *zap.Logger, string, bool) {
	ctx := c.Request.Context()
	span := trace.SpanFromContext(ctx)
	zapLogger := middleware.GetLoggerFromGinContext(c)
	userID := c.GetString("user_id")
	if userID == "" {
		zapLogger.Warn(op + ": no user_id in context")
		httpx.RespondError(c, http.StatusUnauthorized, httpx.CodeUnauthorized, errAuthRequired)
		return ctx, span, zapLogger, "", false
	}
	return ctx, span, zapLogger, userID, true
}

func (h *OrderHandler) ListOrders(c *gin.Context) {
	ctx, span, zapLogger, userID, ok := h.beginAuthed(c, "ListOrders")
	if !ok {
		return
	}

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
