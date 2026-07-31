package v1

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.uber.org/zap"

	"github.com/duynhlab/order-service/internal/cancellation"
	logicv1 "github.com/duynhlab/order-service/internal/logic/v1"
	"github.com/duynhlab/pkg/httpx"
)

// Cancellation observability: one counter, bounded results, answering "are
// users cancelling, and what happens when they try?".
var cancellationsCounter, _ = otel.Meter("order-service").Int64Counter("order.cancellations.total",
	metric.WithDescription("Cancel-order requests by result"))

const (
	cancelResultAccepted           = "accepted"
	cancelResultReplayed           = "replayed"
	cancelResultRejectedState      = "rejected_state"
	cancelResultRejectedDispatched = "rejected_dispatched"
)

func recordCancellation(ctx context.Context, result string) {
	cancellationsCounter.Add(ctx, 1, metric.WithAttributes(
		attribute.String("result", result)))
}

// shipmentPrecheckTimeout bounds the UX pre-check; the workflow re-checks
// authoritatively, so an unreachable shipping service must not block a
// cancel here.
const shipmentPrecheckTimeout = 2 * time.Second

// CancelOrder opens a cancellation episode for the caller's own order.
// POST /order/v1/private/orders/:id/cancel — empty body (the reason is fixed
// CUSTOMER_REQUEST; nothing free-form may reach the audit trail).
//
//	202 {order_id, status:"cancelling"}  — episode opened
//	200 {order_id, status}               — idempotent replay (already cancelling/cancelled)
//	409 ORDER_NOT_CANCELLABLE            — the FSM refuses (pending/failed/manual_review)
//	409 SHIPMENT_ALREADY_DISPATCHED      — the UX pre-check saw a dispatched shipment
//	404                                  — not the caller's order
func (h *OrderHandler) CancelOrder(c *gin.Context) {
	ctx, span, zapLogger, userID, ok := h.beginAuthed(c, "CancelOrder")
	if !ok {
		return
	}
	defer span.End()
	orderID := c.Param("id")

	// Ownership FIRST: the pre-check below would otherwise leak the
	// existence and shipment state of another user's order as a 409 that
	// should have been a 404.
	if _, err := h.orderService.GetOrder(ctx, userID, orderID); err != nil {
		writeOrderLookupError(c, err)
		return
	}

	// Best-effort UX pre-check; the CancellationWorkflow's policy activity is
	// the authoritative gate. Soft-fails on an unreachable shipping service.
	if h.shippingClient != nil {
		checkCtx, cancel := context.WithTimeout(ctx, shipmentPrecheckTimeout)
		shipment, err := h.shippingClient.GetShipmentByOrderID(checkCtx, orderID)
		cancel()
		if err == nil && shipment != nil && !cancellableShipmentStatus(shipment.Status) {
			recordCancellation(ctx, cancelResultRejectedDispatched)
			httpx.RespondError(c, http.StatusConflict, httpx.CodeConflict,
				"Shipment already dispatched; cancellation needs a return")
			return
		}
	}

	outcome, err := h.orderService.CancelOrder(ctx, userID, orderID)
	switch {
	case errors.Is(err, logicv1.ErrOrderNotCancellable):
		recordCancellation(ctx, cancelResultRejectedState)
		httpx.RespondError(c, http.StatusConflict, httpx.CodeConflict, "Order is not cancellable")
		return
	case err != nil:
		writeOrderLookupError(c, err)
		return
	}

	if outcome.Replayed {
		recordCancellation(ctx, cancelResultReplayed)
		c.JSON(http.StatusOK, gin.H{"order_id": orderID, "status": outcome.Order.Status})
		return
	}

	// Inline start (common path); the dispatcher sweeps whatever this misses,
	// which is why a failure here is only logged — the outbox row is already
	// durable. Detached context: a client disconnect must not orphan the start.
	if cancellation.Ready(h.temporal) {
		err := cancellation.Start(context.WithoutCancel(ctx), h.temporal, h.taskQueue, cancellation.Input{
			OrderID: orderID,
			UserID:  userID,
			Total:   outcome.Order.Total,
			Epoch:   outcome.Epoch,
		})
		if err == nil || errors.Is(err, cancellation.ErrAlreadyStarted) {
			closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			if err := h.cancelCloser.CloseDispatchedForUser(closeCtx, userID, orderID, outcome.Epoch); err != nil {
				zapLogger.Warn("cancellation dispatched but row not closed; the sweeper replays it", zap.Error(err))
			}
			cancel()
		} else {
			zapLogger.Error("inline cancellation start failed; the dispatcher retries it", zap.Error(err))
		}
	}

	recordCancellation(ctx, cancelResultAccepted)
	c.JSON(http.StatusAccepted, gin.H{"order_id": orderID, "status": outcome.Order.Status})
}

// cancellableShipmentStatus mirrors the workflow's policy gate for the UX
// pre-check: pending / cancelled / absent still cancel.
func cancellableShipmentStatus(status string) bool {
	switch status {
	case "", "pending", "cancelled":
		return true
	default:
		return false
	}
}
