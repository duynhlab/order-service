package v1

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/duynhlab/order-service/middleware"
	"github.com/duynhlab/pkg/httpx"
	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
)

// paymentEnrichTimeout bounds the payment-enrichment call so an unreachable
// payment service can only add this much latency to the details response
// before the field is simply omitted.
const paymentEnrichTimeout = 2 * time.Second

// Shipment represents a shipment response from the shipping service
type Shipment struct {
	ID                int     `json:"id"`
	OrderID           int     `json:"order_id"`
	TrackingNumber    string  `json:"tracking_number"`
	Carrier           string  `json:"carrier,omitempty"`
	Status            string  `json:"status"`
	EstimatedDelivery *string `json:"estimated_delivery,omitempty"`
	CreatedAt         string  `json:"created_at"`
	UpdatedAt         string  `json:"updated_at"`
}

// OrderDetailsResponse is the aggregated response containing order, shipment
// and (when payments are enabled) the order's payment snapshot.
type OrderDetailsResponse struct {
	Order    interface{}  `json:"order"`
	Shipment *Shipment    `json:"shipment,omitempty"`
	Payment  *PaymentInfo `json:"payment,omitempty"`
}

// PaymentFetcher abstracts the payment gRPC client so the aggregation can be
// tested with a fake; *PaymentGRPCClient satisfies it.
type PaymentFetcher interface {
	GetPaymentByOrderID(ctx context.Context, orderID int64) (*PaymentInfo, error)
}

// shipmentFetcher abstracts the shipping client so order can fetch a shipment
// over gRPC (*ShippingGRPCClient). It returns a *Shipment for the aggregated
// order-details response.
type shipmentFetcher interface {
	GetShipmentByOrderID(ctx context.Context, orderID string) (*Shipment, error)
}

// GetOrderDetails handles GET /order/v1/private/orders/:id/details
// Returns order with shipment info (aggregation endpoint)
func (h *OrderHandler) GetOrderDetails(c *gin.Context) {
	ctx, span := middleware.StartSpan(c.Request.Context(), "http.request", trace.WithAttributes(
		attribute.String("layer", "web"),
		attribute.String("method", c.Request.Method),
		attribute.String("path", c.Request.URL.Path),
		attribute.String("endpoint.type", "aggregation"),
	))
	defer span.End()

	zapLogger := middleware.GetLoggerFromGinContext(c)

	// Get userID from auth context (required - no fallback)
	userID := c.GetString("user_id")
	if userID == "" {
		zapLogger.Warn("GetOrderDetails: no user_id in context")
		httpx.RespondError(c, http.StatusUnauthorized, httpx.CodeUnauthorized, errAuthRequired)
		return
	}

	orderID := c.Param("id")
	span.SetAttributes(attribute.String("order.id", orderID))

	order, err := h.orderService.GetOrder(ctx, userID, orderID)
	if err != nil {
		span.RecordError(err)
		zapLogger.Error("Failed to get order", zap.Error(err), zap.String("order_id", orderID))
		writeOrderLookupError(c, err)
		return
	}

	// Try to get shipment (non-blocking - order may not have shipment yet)
	var shipment *Shipment
	if h.shippingClient != nil {
		shipment, err = h.shippingClient.GetShipmentByOrderID(ctx, orderID)
		if err != nil {
			// Log but don't fail - shipment is optional
			zapLogger.Warn("Could not fetch shipment", zap.Error(err), zap.String("order_id", orderID))
			span.SetAttributes(attribute.Bool("shipment.fetch_error", true))
		}
		if shipment != nil {
			span.SetAttributes(
				attribute.Bool("shipment.found", true),
				attribute.String("shipment.status", shipment.Status),
			)
		} else {
			span.SetAttributes(attribute.Bool("shipment.found", false))
		}
	}

	// Payment enrichment (soft-fail, like shipment): only when the payment
	// integration is enabled, and never blocking the details response for long —
	// a missing/unreachable payment service just omits the field.
	var payment *PaymentInfo
	if h.paymentEnabled && h.paymentClient != nil {
		if oid, parseErr := strconv.ParseInt(orderID, 10, 64); parseErr == nil {
			pctx, cancel := context.WithTimeout(ctx, paymentEnrichTimeout)
			var fetchErr error
			payment, fetchErr = h.paymentClient.GetPaymentByOrderID(pctx, oid)
			cancel()
			if fetchErr != nil {
				zapLogger.Warn("Could not fetch payment", zap.Error(fetchErr), zap.String("order_id", orderID))
				span.SetAttributes(attribute.Bool("payment.fetch_error", true))
				payment = nil
			}
			span.SetAttributes(attribute.Bool("payment.found", payment != nil))
		}
	}

	response := OrderDetailsResponse{
		Order:    toOrderResponse(*order),
		Shipment: shipment,
		Payment:  payment,
	}

	zapLogger.Info("Order details retrieved",
		zap.String("order_id", orderID),
		zap.Bool("has_shipment", shipment != nil),
		zap.Bool("has_payment", payment != nil),
	)
	c.JSON(http.StatusOK, response)
}
