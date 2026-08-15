package v1

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/duynhlab/order-service/internal/core/domain"
	logicv1 "github.com/duynhlab/order-service/internal/logic/v1"
	"github.com/duynhlab/pkg/authmw"
)

// unscopedRepo layers the protected read surface over the shared web mock.
type unscopedRepo struct {
	mockOrderRepo
	items   []domain.Order
	total   int
	listErr error
	getErr  error
	got     struct {
		status        string
		limit, offset int
	}
	byID map[string]*domain.Order
}

func (m *unscopedRepo) ListAll(_ context.Context, status string, limit, offset int) ([]domain.Order, int, error) {
	m.got.status, m.got.limit, m.got.offset = status, limit, offset
	return m.items, m.total, m.listErr
}

func (m *unscopedRepo) FindByIDUnscoped(_ context.Context, id string) (*domain.Order, error) {
	if m.getErr != nil {
		return nil, m.getErr
	}
	if o, ok := m.byID[id]; ok {
		return o, nil
	}
	return nil, domain.ErrNotFound
}

func protectedEngine(t *testing.T, repo domain.OrderRepository, roles ...string) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := NewOrderHandler(logicv1.NewOrderService(repo, nil, &stubOutbox{}, nil, noopProjection{}, nil, nil), nil, nil, "", nil, nil, nil, nil, nil, nil)
	h.mountProtected(r,
		func(c *gin.Context) {
			c.Set(authmw.CtxUserID, "d0e00000-0000-4000-8000-000000000001")
			c.Set(authmw.CtxRoles, roles)
			c.Next()
		},
		authmw.MiddlewareRequireRole(backofficeRole))
	return r
}

func TestProtectedOrdersRoleGate(t *testing.T) {
	r := protectedEngine(t, &unscopedRepo{}, "customer")
	for _, path := range []string{"/order/v1/protected/orders", "/order/v1/protected/orders/6"} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusForbidden {
			t.Fatalf("%s: want 403, got %d", path, w.Code)
		}
	}
}

func TestListAllOrders(t *testing.T) {
	repo := &unscopedRepo{
		items: []domain.Order{{ID: "6", UserID: "u-1", Status: "manual_review", Total: 25798}},
		total: 41,
	}
	r := protectedEngine(t, repo, backofficeRole)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/order/v1/protected/orders?status=manual_review&page=3&page_size=10", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	if repo.got.status != "manual_review" || repo.got.limit != 10 || repo.got.offset != 20 {
		t.Fatalf("filter/paging not forwarded: %+v", repo.got)
	}
	var resp struct {
		TotalItems int `json:"total_items"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.TotalItems != 41 {
		t.Fatalf("want total 41, got %d", resp.TotalItems)
	}

	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/order/v1/protected/orders?status=bogus", nil))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("bogus status: want 400, got %d", w.Code)
	}
}

func TestGetOrderCase(t *testing.T) {
	repo := &unscopedRepo{byID: map[string]*domain.Order{
		"6": {ID: "6", UserID: "u-1", Status: "completed", Items: []domain.OrderItem{{ProductID: "1", Quantity: 2}}},
	}}
	r := protectedEngine(t, repo, backofficeRole)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/order/v1/protected/orders/6", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	var order domain.Order
	_ = json.Unmarshal(w.Body.Bytes(), &order)
	if order.UserID != "u-1" || len(order.Items) != 1 {
		t.Fatalf("case view incomplete: %+v", order)
	}

	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/order/v1/protected/orders/999", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", w.Code)
	}
}

func TestRegisterProtectedRoutesRealChain(t *testing.T) {
	verifier, err := authmw.NewVerifier(authmw.Config{
		Issuer:   "http://localhost:8081/realms/duynhlab-staff",
		Audience: "duynhlab-platform",
	})
	if err != nil {
		t.Fatalf("verifier: %v", err)
	}
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := NewOrderHandler(logicv1.NewOrderService(&unscopedRepo{}, nil, &stubOutbox{}, nil, noopProjection{}, nil, nil), nil, nil, "", nil, nil, nil, nil, nil, nil)
	RegisterProtectedRoutes(r, h, verifier)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/order/v1/protected/orders", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("tokenless: want 401 from the real chain, got %d", w.Code)
	}
}

func TestProtectedOrdersErrorBranch(t *testing.T) {
	repo := &unscopedRepo{listErr: context.DeadlineExceeded}
	r := protectedEngine(t, repo, backofficeRole)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/order/v1/protected/orders", nil))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d", w.Code)
	}
}

func TestGetOrderCaseErrorBranch(t *testing.T) {
	r := protectedEngine(t, &unscopedRepo{getErr: context.DeadlineExceeded}, backofficeRole)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/order/v1/protected/orders/6", nil))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d", w.Code)
	}
}
