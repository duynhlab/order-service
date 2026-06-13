package v1

import (
	"context"
	"net/http"

	"github.com/duynhlab/order-service/middleware"
	"github.com/duynhlab/pkg/httpx"
	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
)

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

// OrderDetailsResponse is the aggregated response containing order and shipment
type OrderDetailsResponse struct {
	Order    interface{} `json:"order"`
	Shipment *Shipment   `json:"shipment,omitempty"`
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

	response := OrderDetailsResponse{
		Order:    order,
		Shipment: shipment,
	}

	zapLogger.Info("Order details retrieved",
		zap.String("order_id", orderID),
		zap.Bool("has_shipment", shipment != nil),
	)
	c.JSON(http.StatusOK, response)
}
