package v1

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/duynhlab/order-service/internal/core/domain"
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

// OrderDetailsResponse is the aggregated response: the order plus soft-fail
// enrichment blocks. Expand-first (RFC-0021 P5): the pre-P5 fields are
// byte-identical; `processing`, `inventory` and `degraded` are additive.
//
// A block that is genuinely ABSENT (no shipment yet, no reservation on the
// product path, an order predating the projection) is null/omitted and NOT
// in `degraded`; a block whose fetch FAILED is null AND listed in `degraded`
// — that distinction is the contract the SPA renders "temporarily
// unavailable" badges from.
type OrderDetailsResponse struct {
	Order      interface{}      `json:"order"`
	Shipment   *Shipment        `json:"shipment,omitempty"`
	Payment    *PaymentInfo     `json:"payment,omitempty"`
	Processing *ProcessingBlock `json:"processing,omitempty"`
	Inventory  *InventoryBlock  `json:"inventory,omitempty"`
	// Degraded lists the blocks whose fetch failed, as bounded tokens:
	// shipment | payment | inventory | processing.
	Degraded []string `json:"degraded,omitempty"`
}

// ProcessingBlock is the read side of the processing-stage projection.
type ProcessingBlock struct {
	Stage         string `json:"stage"`
	LastStep      string `json:"last_step,omitempty"`
	LastErrorCode string `json:"last_error_code,omitempty"`
	UpdatedAt     string `json:"updated_at"`
}

// InventoryBlock is the order's reservation snapshot.
type InventoryBlock struct {
	Status string `json:"status"`
}

// processingFetcher reads the projection row; *repository.
// PostgresOrderRepository satisfies it.
type processingFetcher interface {
	ReadProcessingState(ctx context.Context, orderID string) (*domain.ProcessingState, error)
}

// ReservationFetcher reads the order's reservation status;
// *InventoryGRPCClient satisfies it. "" means no reservation exists.
// Exported (like PaymentFetcher) so main can hold a nil interface without
// the typed-nil footgun.
type ReservationFetcher interface {
	GetReservationStatus(ctx context.Context, orderID string) (string, error)
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
	ctx := c.Request.Context()
	span := trace.SpanFromContext(ctx)

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

	// Every enrichment below is soft-fail: a failed fetch degrades its own
	// block, never the response.
	var degraded []string

	// Try to get shipment (non-blocking - order may not have shipment yet)
	var shipment *Shipment
	if h.shippingClient != nil {
		shipment, err = h.shippingClient.GetShipmentByOrderID(ctx, orderID)
		if err != nil {
			// Log but don't fail - shipment is optional
			zapLogger.Warn("Could not fetch shipment", zap.Error(err), zap.String("order_id", orderID))
			span.SetAttributes(attribute.Bool("shipment.fetch_error", true))
			degraded = append(degraded, "shipment")
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

	// Payment enrichment (soft-fail, like shipment): never blocks the details
	// response for long — a missing/unreachable payment service just omits the
	// field. paymentClient is nil only when the startup gRPC dial failed.
	var payment *PaymentInfo
	if h.paymentClient != nil {
		if oid, parseErr := strconv.ParseInt(orderID, 10, 64); parseErr == nil {
			pctx, cancel := context.WithTimeout(ctx, paymentEnrichTimeout)
			var fetchErr error
			payment, fetchErr = h.paymentClient.GetPaymentByOrderID(pctx, oid)
			cancel()
			if fetchErr != nil {
				zapLogger.Warn("Could not fetch payment", zap.Error(fetchErr), zap.String("order_id", orderID))
				span.SetAttributes(attribute.Bool("payment.fetch_error", true))
				payment = nil
				degraded = append(degraded, "payment")
			}
			span.SetAttributes(attribute.Bool("payment.found", payment != nil))
		}
	}

	inventory, degraded := h.fetchInventoryBlock(ctx, span, zapLogger, orderID, degraded)
	processing, degraded := h.fetchProcessingBlock(ctx, span, zapLogger, orderID, degraded)

	response := OrderDetailsResponse{
		Order:      toOrderResponse(*order),
		Shipment:   shipment,
		Payment:    payment,
		Processing: processing,
		Inventory:  inventory,
		Degraded:   degraded,
	}

	zapLogger.Info("Order details retrieved",
		zap.String("order_id", orderID),
		zap.Bool("has_shipment", shipment != nil),
		zap.Bool("has_payment", payment != nil),
		zap.Strings("degraded", degraded),
	)
	c.JSON(http.StatusOK, response)
}

// fetchInventoryBlock reads the reservation snapshot (RFC-0021 P5). "" = no
// reservation — the normal product-path answer, rendered as absence, not
// degradation.
func (h *OrderHandler) fetchInventoryBlock(ctx context.Context, span trace.Span,
	zapLogger *zap.Logger, orderID string, degraded []string) (*InventoryBlock, []string) {
	if h.inventoryClient == nil {
		return nil, degraded
	}
	ictx, cancel := context.WithTimeout(ctx, paymentEnrichTimeout)
	resStatus, err := h.inventoryClient.GetReservationStatus(ictx, orderID)
	cancel()
	switch {
	case err != nil:
		zapLogger.Warn("Could not fetch reservation", zap.Error(err), zap.String("order_id", orderID))
		span.SetAttributes(attribute.Bool("inventory.fetch_error", true))
		return nil, append(degraded, "inventory")
	case resStatus != "":
		return &InventoryBlock{Status: resStatus}, degraded
	default:
		return nil, degraded
	}
}

// fetchProcessingBlock reads the processing projection (RFC-0021 P5). Orders
// predating the projection have no row — absence, not degradation. NOTE:
// after a reconciler repair the terminal truth is orders.status; the
// projection is a UX hint and consumers must never treat it as an override.
func (h *OrderHandler) fetchProcessingBlock(ctx context.Context, span trace.Span,
	zapLogger *zap.Logger, orderID string, degraded []string) (*ProcessingBlock, []string) {
	if h.processing == nil {
		return nil, degraded
	}
	st, err := h.processing.ReadProcessingState(ctx, orderID)
	switch {
	case err == nil:
		return &ProcessingBlock{
			Stage:         string(st.Stage),
			LastStep:      st.LastStep,
			LastErrorCode: st.LastErrorCode,
			UpdatedAt:     st.UpdatedAt,
		}, degraded
	case errors.Is(err, domain.ErrNotFound):
		return nil, degraded
	default:
		zapLogger.Warn("Could not read the processing projection", zap.Error(err), zap.String("order_id", orderID))
		span.SetAttributes(attribute.Bool("processing.fetch_error", true))
		return nil, append(degraded, "processing")
	}
}
