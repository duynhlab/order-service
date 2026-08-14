package v1

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/duynhlab/order-service/internal/core/domain"
	"github.com/duynhlab/pkg/authmw"
	"github.com/duynhlab/pkg/httpx"
)

// Protected Backoffice reads (RFC-0023 slice A, ADR-047/050): the
// cross-customer order list and case view. Every owner-scoped customer query
// bakes user_id into its SQL, so these ride the explicitly-unscoped
// repository paths — and the staff-realm verifier plus the backoffice_admin
// role gate are what authorize that width. The order `manual_review`
// RESOLUTION stays a Future command (own safety review); these are reads.

// backofficeRole is the staff-realm role every protected route requires.
const backofficeRole = "backoffice_admin"

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
		httpx.RespondError(c, http.StatusInternalServerError, httpx.CodeInternal, "Internal server error")
		return
	}
	c.JSON(http.StatusOK, httpx.NewPaginated(items, page, pageSize, total))
}

// GetOrderCase serves GET /orders/:id — the operator case view (order +
// items + owner subject; enrichment fan-out stays with the customer detail
// path, which soft-fails per dependency).
func (h *OrderHandler) GetOrderCase(c *gin.Context) {
	order, err := h.orderService.GetOrderUnscoped(c.Request.Context(), c.Param("id"))
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			httpx.RespondError(c, http.StatusNotFound, httpx.CodeNotFound, "Order not found")
			return
		}
		httpx.RespondError(c, http.StatusInternalServerError, httpx.CodeInternal, "Internal server error")
		return
	}
	c.JSON(http.StatusOK, order)
}
