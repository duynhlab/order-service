package v1

import (
	"context"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"

	"github.com/duynhlab/order-service/internal/core/domain"
	"github.com/duynhlab/pkg/authmw"
	"github.com/duynhlab/pkg/httpmw"
	"github.com/duynhlab/pkg/httpx"
)

// The protected Backoffice surface (RFC-0023, ADR-047/050/051): the
// cross-customer order list, the operator case view, and the one privileged
// write this service exposes — the `manual_review` resolve. Every owner-scoped
// customer query bakes user_id into its SQL, so these ride the
// explicitly-unscoped repository paths, and the staff-realm verifier plus the
// backoffice_admin role gate are what authorize that width.
//
// WHY THE RESOLVE IS TRUSTED (ADR-051). The order parks in manual_review
// because a side effect is unaccounted for, and settling it — a refund, a
// stock release, a shipment cancellation — happens OUTSIDE this service, often
// outside the platform. This endpoint therefore cannot verify that the world
// now matches the target status, and does not pretend to: it validates what it
// genuinely owns (the target is one of four, the version the operator read is
// still current, the command has not already been applied) and records who
// decided, which unaccounted effect they settled, and why, in the same
// transaction as the transition. The control is the audit trail plus a case
// view that puts payment, reservation and shipment in front of the operator
// before they choose — deliberately NOT a cross-service veto, which would make
// the endpoint unavailable during exactly the incidents that fill this queue.

// backofficeRole is the staff-realm role every protected route requires.
const backofficeRole = "backoffice_admin"

// Resolve observability: one counter answering the two questions on-call has
// about this command — is the parked queue actually being worked, and are
// operators being REFUSED (a stale portal, a target the FSM rejects)? Attributes
// stay bounded: target and reason are closed vocabularies, result is one of
// three. No alert keys on this — an operator resolving is the system working as
// designed, and the backlog gauge already alerts when the queue does not drain.
var resolvesCounter, _ = otel.Meter("order-service").Int64Counter("order.operator.resolve.total",
	metric.WithDescription("manual_review resolve commands by target, reason and result"))

const (
	resolveResultApplied  = "applied"
	resolveResultReplayed = "replayed"
	resolveResultRejected = "rejected"
)

func (h *OrderHandler) recordResolve(ctx context.Context, target, reason, result string) {
	// A rejected command may carry a target or reason the operator invented, so
	// only record values the domain recognises — this counter must not become
	// the one place unbounded request input reaches the metrics backend.
	if !domain.KnownStatus(domain.OrderStatus(target)) {
		target = "invalid"
	}
	if !domain.KnownReason(domain.ReasonCode(reason)) {
		reason = "invalid"
	}
	resolvesCounter.Add(ctx, 1, metric.WithAttributes(
		attribute.String("target", target),
		attribute.String("reason", reason),
		attribute.String("result", result)))
}

// validOrderStatuses mirrors the order FSM's stored vocabulary — used only
// to reject typo filters, not to gate transitions (reads don't transition).
var validOrderStatuses = map[string]bool{
	"pending": true, "processing": true, "confirmed": true, "completed": true,
	"cancelling": true, "cancelled": true, "manual_review": true,
}

// RegisterProtectedRoutes mounts the Backoffice group with the real guard
// chain. Split from mountProtected so tests can inject fakes.
func RegisterProtectedRoutes(r *gin.Engine, h *OrderHandler, staffVerifier *authmw.Verifier) {
	h.mountProtected(r, authmw.MiddlewareJWT(staffVerifier), authmw.MiddlewareRequireRole(backofficeRole))
}

func (h *OrderHandler) mountProtected(r *gin.Engine, authMW ...gin.HandlerFunc) {
	protected := r.Group("/order/v1/protected")
	protected.Use(authMW...)
	{
		protected.GET("/orders", h.ListAllOrders)
		protected.GET("/orders/:id", h.GetOrderCase)
		protected.POST("/orders/:id/resolve", h.ResolveManualReview)
	}
}

// ListAllOrders serves GET /orders?status=&page=&page_size=.
func (h *OrderHandler) ListAllOrders(c *gin.Context) {
	page, pageSize := httpx.ParsePage(c)

	status := c.Query("status")
	if status != "" && !validOrderStatuses[status] {
		httpx.RespondError(c, http.StatusBadRequest, httpx.CodeValidation,
			"unknown status filter")
		return
	}

	items, total, err := h.orderService.ListAllOrders(c.Request.Context(), status, pageSize, httpx.Offset(page, pageSize))
	if err != nil {
		httpx.RespondError(c, http.StatusInternalServerError, httpx.CodeInternal, errInternal)
		return
	}
	c.JSON(http.StatusOK, httpx.NewPaginated(items, page, pageSize, total))
}

// OrderCaseResponse is the operator case view: the order, the version the
// resolve command must echo back, the same soft-fail enrichment blocks the
// customer detail path uses, and the full transition history.
//
// The order is EMBEDDED rather than nested under an `order` key, so every field
// this route already returned keeps its exact place and the portal's existing
// rendering is untouched; everything else is additive. (The customer
// /details response nests instead — it was shaped that way from the start.)
//
// `version` has to be named here because domain.Order deliberately never
// serializes it: it is server-internal on every customer path, and this is the
// only audience with a reason to see it — the resolve command echoes it back.
//
// `degraded` carries the same distinction as the customer response: a block
// that is genuinely absent (no shipment yet, no reservation on the product
// path) is omitted and NOT listed; a block whose fetch FAILED is omitted AND
// listed. An operator deciding what to resolve must be able to tell "there is
// no shipment" from "we could not ask about the shipment".
type OrderCaseResponse struct {
	*domain.Order

	Version       int64                       `json:"version"`
	Shipment      *Shipment                   `json:"shipment,omitempty"`
	Payment       *PaymentInfo                `json:"payment,omitempty"`
	Processing    *ProcessingBlock            `json:"processing,omitempty"`
	Inventory     *InventoryBlock             `json:"inventory,omitempty"`
	StatusHistory []domain.StatusHistoryEntry `json:"status_history"`
	Degraded      []string                    `json:"degraded,omitempty"`
}

// GetOrderCase serves GET /orders/:id — the operator case view. It fans out to
// payment, inventory and shipping so the three external truths an operator has
// to reconcile sit on the page where they act (ADR-051), and it returns the
// transition history because that trail is the control on the resolve command.
//
// Every enrichment is soft-fail: this route must keep answering while the
// services it reports on are the ones having the incident.
func (h *OrderHandler) GetOrderCase(c *gin.Context) {
	ctx := c.Request.Context()
	span := trace.SpanFromContext(ctx)
	zapLogger := httpmw.LoggerFrom(c)
	orderID := c.Param("id")

	order, err := h.orderService.GetOrderUnscoped(ctx, orderID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			httpx.RespondError(c, http.StatusNotFound, httpx.CodeNotFound, "Order not found")
			return
		}
		_ = c.Error(err)
		httpx.RespondError(c, http.StatusInternalServerError, httpx.CodeInternal, errInternal)
		return
	}

	var degraded []string
	shipment, degraded := h.fetchShipmentBlock(ctx, span, zapLogger, orderID, degraded)
	payment, degraded := h.fetchPaymentBlock(ctx, span, zapLogger, orderID, degraded)
	inventory, degraded := h.fetchInventoryBlock(ctx, span, zapLogger, orderID, degraded)
	processing, degraded := h.fetchProcessingBlock(ctx, span, zapLogger, orderID, degraded)

	// The history is soft-fail too, and for the same reason as the rest: a case
	// view that 500s because one read failed sends the operator back to psql.
	// An empty list must never be mistaken for "nothing ever happened", which
	// is what listing it in `degraded` prevents.
	history := []domain.StatusHistoryEntry{}
	if h.history != nil {
		rows, histErr := h.history.ListStatusHistory(ctx, orderID)
		if histErr != nil {
			zapLogger.Warn("Could not read the status history", zap.Error(histErr), zap.String("order_id", orderID))
			degraded = append(degraded, "status_history")
		} else if rows != nil {
			history = rows
		}
	}

	c.JSON(http.StatusOK, OrderCaseResponse{
		Order:         order,
		Version:       order.Version, //nolint:staticcheck // the embedded field is json:"-"; this names it for this audience
		Shipment:      shipment,
		Payment:       payment,
		Processing:    processing,
		Inventory:     inventory,
		StatusHistory: history,
		Degraded:      degraded,
	})
}

// resolveRequest is the operator's decision. Note the two things that are NOT
// here: the actor (taken from the verified token — a body-supplied subject
// would let one operator sign another's name) and the command id (derived from
// the order, version and target, so a retried request replays instead of
// transitioning twice).
type resolveRequest struct {
	// Target is where the order goes: confirmed | failed | cancelled |
	// completed. The domain owns the closed set.
	Target string `json:"target" binding:"required"`
	// Version is the order version the operator read. It is the optimistic
	// token AND the command-id epoch: resolving from a stale case view loses
	// the guarded update instead of overwriting someone else's decision.
	Version int64 `json:"version" binding:"required,min=1"`
	// Reason says WHICH unaccounted effect was settled, from the bounded
	// resolution vocabulary.
	Reason string `json:"reason" binding:"required"`
	// Note is the human record. Mandatory: the reason says what class of thing
	// was settled, the note says what the operator actually checked.
	Note string `json:"note" binding:"required"`
}

// resolveResponse mirrors the platform's command shape (inventory's receipts
// are the precedent): 201 + applied:true when the transition landed, 200 +
// applied:false when this exact command had already been applied. The order is
// returned either way so the portal renders settled state without a refetch.
type resolveResponse struct {
	Order   *domain.Order `json:"order"`
	Applied bool          `json:"applied"`
}

// ResolveManualReview serves POST /orders/:id/resolve — the operator escape
// hatch out of manual_review (ADR-051), replacing the raw-SQL runbook step.
//
// The writer does the work that matters in one transaction under a row lock:
// replay check, FSM and actor-matrix validation against the CURRENT state, the
// history row, and the guarded version-bumping update. This handler's job is to
// turn a request into a validated command, take the actor from the token, and
// map the writer's vocabulary onto HTTP.
func (h *OrderHandler) ResolveManualReview(c *gin.Context) {
	ctx := c.Request.Context()
	zapLogger := httpmw.LoggerFrom(c)
	orderID := c.Param("id")

	// The actor is the token subject, never the body. authmw has already
	// verified it; an empty subject means the chain was misassembled, and a
	// privileged write must fail closed rather than record an anonymous actor.
	operator := c.GetString(authmw.CtxUserID)
	if operator == "" {
		zapLogger.Warn("ResolveManualReview: no subject on a verified request")
		httpx.RespondError(c, http.StatusUnauthorized, httpx.CodeUnauthorized, errAuthRequired)
		return
	}

	var req resolveRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, httpx.CodeValidation,
			"target, version, reason and note are all required")
		return
	}

	cmd, err := domain.NewResolveManualReviewCommand(orderID, domain.OrderStatus(req.Target),
		domain.ReasonCode(req.Reason), operator, req.Note, req.Version)
	if err != nil {
		h.recordResolve(ctx, req.Target, req.Reason, resolveResultRejected)
		respondResolveError(c, err)
		return
	}

	replayed, err := h.statusWriter.ApplyStatusCommand(ctx, cmd)
	if err != nil {
		h.recordResolve(ctx, req.Target, req.Reason, resolveResultRejected)
		respondResolveError(c, err)
		return
	}

	// Re-read so the response carries settled state rather than the operator's
	// stale copy. A read failure here does not undo a committed transition, so
	// the command still reports success — with the order omitted.
	order, readErr := h.orderService.GetOrderUnscoped(ctx, orderID)
	if readErr != nil {
		zapLogger.Warn("Resolve landed but the re-read failed",
			zap.Error(readErr), zap.String("order_id", orderID))
		order = nil
	}

	if replayed {
		h.recordResolve(ctx, req.Target, req.Reason, resolveResultReplayed)
		zapLogger.Info("Resolve replayed — nothing changed",
			zap.String("order_id", orderID), zap.String("command_id", cmd.CommandID),
			zap.String("operator", operator))
		c.JSON(http.StatusOK, resolveResponse{Order: order, Applied: false})
		return
	}

	h.recordResolve(ctx, req.Target, req.Reason, resolveResultApplied)
	zapLogger.Info("Order resolved out of manual_review",
		zap.String("order_id", orderID), zap.String("target", req.Target),
		zap.String("reason", req.Reason), zap.String("operator", operator),
		zap.String("command_id", cmd.CommandID))
	c.JSON(http.StatusCreated, resolveResponse{Order: order, Applied: true})
}

// respondResolveError maps the command vocabulary onto HTTP. Order matters:
// ErrInvalidTransition and ErrIdempotencyConflict are distinct 409s the portal
// phrases differently, and a stale version arrives as ErrConcurrencyConflict
// from the guarded update — for an operator that is not a race to retry
// silently but a signal that the case view they read is out of date.
func respondResolveError(c *gin.Context, err error) {
	_ = c.Error(err)
	switch {
	case errors.Is(err, domain.ErrNotFound):
		httpx.RespondError(c, http.StatusNotFound, httpx.CodeNotFound, "Order not found")
	case errors.Is(err, domain.ErrInvalidTransition):
		httpx.RespondError(c, http.StatusConflict, "INVALID_TRANSITION", err.Error())
	case errors.Is(err, domain.ErrIdempotencyConflict):
		httpx.RespondError(c, http.StatusConflict, httpx.CodeIdempotencyConflict,
			"this order and version were already resolved to a different status")
	case errors.Is(err, domain.ErrConcurrencyConflict):
		httpx.RespondError(c, http.StatusConflict, "VERSION_CONFLICT",
			"the order changed since it was read; reload the case and decide again")
	case errors.Is(err, domain.ErrInvalidInput):
		httpx.RespondError(c, http.StatusBadRequest, httpx.CodeValidation, err.Error())
	default:
		httpx.RespondError(c, http.StatusInternalServerError, httpx.CodeInternal, errInternal)
	}
}
