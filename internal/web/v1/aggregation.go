package v1

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/duynhlab/order-service/middleware"
	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"

	logicv1 "github.com/duynhlab/order-service/internal/logic/v1"
)

// ShippingClient handles HTTP calls to the shipping service
type ShippingClient struct {
	baseURL    string
	httpClient *http.Client
}

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

// NewShippingClient creates a new shipping service client
func NewShippingClient(baseURL string) *ShippingClient {
	return &ShippingClient{
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: 5 * time.Second,
		},
	}
}

// GetShipmentByOrderID fetches shipment info for an order
func (c *ShippingClient) GetShipmentByOrderID(ctx context.Context, orderID string) (*Shipment, error) {
	// Internal shipping endpoint — not routed through Kong, reached via in-cluster DNS.
	url := fmt.Sprintf("%s/shipping/v1/internal/orders/%s", c.baseURL, orderID)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("create shipping request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("shipping service call failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		// No shipment yet - this is not an error
		return nil, nil
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("shipping service returned status %d", resp.StatusCode)
	}

	var shipment Shipment
	if err := json.NewDecoder(resp.Body).Decode(&shipment); err != nil {
		return nil, fmt.Errorf("failed to decode shipment response: %w", err)
	}

	return &shipment, nil
}

// shipmentFetcher abstracts the shipping client so order can fetch a shipment
// over either REST (*ShippingClient) or gRPC (*ShippingGRPCClient), selected at
// startup. Both return the same *Shipment so the aggregated response is identical.
type shipmentFetcher interface {
	GetShipmentByOrderID(ctx context.Context, orderID string) (*Shipment, error)
}

// Global shipping client (set during init)
var shippingClient shipmentFetcher

// SetShippingClient sets the shipping client for aggregation handlers
func SetShippingClient(client shipmentFetcher) {
	shippingClient = client
}

// GetOrderDetails handles GET /order/v1/private/orders/:id/details
// Returns order with shipment info (aggregation endpoint)
func GetOrderDetails(c *gin.Context) {
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
		c.JSON(http.StatusUnauthorized, gin.H{"error": errAuthRequired})
		return
	}

	orderID := c.Param("id")
	span.SetAttributes(attribute.String("order.id", orderID))

	// Get order from handler (use the global handler)
	if handler == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Service not initialized"})
		return
	}

	order, err := handler.orderService.GetOrder(ctx, userID, orderID)
	if err != nil {
		span.RecordError(err)
		zapLogger.Error("Failed to get order", zap.Error(err), zap.String("order_id", orderID))

		switch {
		case errors.Is(err, logicv1.ErrOrderNotFound):
			c.JSON(http.StatusNotFound, gin.H{"error": "Order not found"})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
		}
		return
	}

	// Try to get shipment (non-blocking - order may not have shipment yet)
	var shipment *Shipment
	if shippingClient != nil {
		shipment, err = shippingClient.GetShipmentByOrderID(ctx, orderID)
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
