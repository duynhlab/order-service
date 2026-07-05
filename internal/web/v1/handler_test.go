package v1

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/duynhlab/order-service/internal/core/domain"
	logicv1 "github.com/duynhlab/order-service/internal/logic/v1"
	"github.com/gin-gonic/gin"
)

func init() { gin.SetMode(gin.TestMode) }

// mockOrderRepo is a configurable domain.OrderRepository double for web tests.
type mockOrderRepo struct {
	list      []domain.Order
	total     int
	listErr   error
	countErr  error
	findErr   error
	order     *domain.Order
	idemOrder *domain.Order
	idemErr   error
}

func (m *mockOrderRepo) FindByID(_ context.Context, _, _ string) (*domain.Order, error) {
	return m.order, m.findErr
}
func (m *mockOrderRepo) FindByUserID(_ context.Context, _ string, _, _ int) ([]domain.Order, error) {
	return m.list, m.listErr
}
func (m *mockOrderRepo) CountByUserID(_ context.Context, _ string) (int, error) {
	return m.total, m.countErr
}
func (m *mockOrderRepo) FindByIdempotencyKey(_ context.Context, _, _ string) (*domain.Order, error) {
	return m.idemOrder, m.idemErr
}

// stubShipment implements shipmentFetcher for aggregation tests.
type stubShipment struct {
	shipment *Shipment
	err      error
}

func (s stubShipment) GetShipmentByOrderID(context.Context, string) (*Shipment, error) {
	return s.shipment, s.err
}
func (m *mockOrderRepo) Create(_ context.Context, _ *domain.Order) error   { return nil }
func (m *mockOrderRepo) UpdateStatus(_ context.Context, _, _ string) error { return nil }
func (m *mockOrderRepo) CreateWithTx(_ context.Context, _ domain.Transaction, _ *domain.Order) error {
	return nil
}

func newHandler(repo domain.OrderRepository) *OrderHandler {
	return NewOrderHandler(logicv1.NewOrderService(repo, nil), nil, nil, nil, "", false, nil)
}

func newCtx(method, target, userID string, params gin.Params) (*gin.Context, *httptest.ResponseRecorder) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(method, target, nil)
	if userID != "" {
		c.Set("user_id", userID)
	}
	c.Params = params
	return c, rec
}

// decode returns the parsed JSON body and the "code" field (if any).
func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON body %q: %v", rec.Body.String(), err)
	}
	return body
}

func TestListOrders_Success(t *testing.T) {
	repo := &mockOrderRepo{list: []domain.Order{{ID: "1"}, {ID: "2"}}, total: 2}
	c, rec := newCtx(http.MethodGet, "/order/v1/private/orders?page=1&page_size=5", "user1", nil)

	newHandler(repo).ListOrders(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := decode(t, rec)
	if body["total_items"].(float64) != 2 {
		t.Errorf("total_items = %v, want 2", body["total_items"])
	}
	if items, ok := body["items"].([]any); !ok || len(items) != 2 {
		t.Errorf("items = %v, want length 2", body["items"])
	}
}

func TestListOrders_Unauthorized(t *testing.T) {
	c, rec := newCtx(http.MethodGet, "/order/v1/private/orders", "", nil)
	newHandler(&mockOrderRepo{}).ListOrders(c)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if code := decode(t, rec)["code"]; code != "UNAUTHORIZED" {
		t.Errorf("code = %v, want UNAUTHORIZED", code)
	}
}

func TestListOrders_ServiceError(t *testing.T) {
	repo := &mockOrderRepo{countErr: context.DeadlineExceeded}
	c, rec := newCtx(http.MethodGet, "/order/v1/private/orders", "user1", nil)
	newHandler(repo).ListOrders(c)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if code := decode(t, rec)["code"]; code != "INTERNAL_ERROR" {
		t.Errorf("code = %v, want INTERNAL_ERROR", code)
	}
}

func TestGetOrder_Success(t *testing.T) {
	repo := &mockOrderRepo{order: &domain.Order{ID: "1", UserID: "user1"}}
	c, rec := newCtx(http.MethodGet, "/order/v1/private/orders/1", "user1", gin.Params{{Key: "id", Value: "1"}})
	newHandler(repo).GetOrder(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestGetOrder_NotFound(t *testing.T) {
	repo := &mockOrderRepo{findErr: domain.ErrNotFound}
	c, rec := newCtx(http.MethodGet, "/order/v1/private/orders/9", "user1", gin.Params{{Key: "id", Value: "9"}})
	newHandler(repo).GetOrder(c)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if code := decode(t, rec)["code"]; code != "NOT_FOUND" {
		t.Errorf("code = %v, want NOT_FOUND", code)
	}
}

func TestGetOrder_Unauthorized(t *testing.T) {
	c, rec := newCtx(http.MethodGet, "/order/v1/private/orders/1", "", gin.Params{{Key: "id", Value: "1"}})
	newHandler(&mockOrderRepo{}).GetOrder(c)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestGetOrderDetails_Success(t *testing.T) {
	repo := &mockOrderRepo{order: &domain.Order{ID: "1", UserID: "user1"}}
	c, rec := newCtx(http.MethodGet, "/order/v1/private/orders/1/details", "user1", gin.Params{{Key: "id", Value: "1"}})
	newHandler(repo).GetOrderDetails(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if _, ok := decode(t, rec)["order"]; !ok {
		t.Errorf("response missing order field: %s", rec.Body.String())
	}
}

func TestGetOrderDetails_NotFound(t *testing.T) {
	repo := &mockOrderRepo{findErr: domain.ErrNotFound}
	c, rec := newCtx(http.MethodGet, "/order/v1/private/orders/9/details", "user1", gin.Params{{Key: "id", Value: "9"}})
	newHandler(repo).GetOrderDetails(c)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if code := decode(t, rec)["code"]; code != "NOT_FOUND" {
		t.Errorf("code = %v, want NOT_FOUND", code)
	}
}

func TestGetOrderDetails_WithShipment(t *testing.T) {
	repo := &mockOrderRepo{order: &domain.Order{ID: "1", UserID: "user1"}}
	ship := stubShipment{shipment: &Shipment{ID: 1, Status: "shipped"}}
	h := NewOrderHandler(logicv1.NewOrderService(repo, nil), nil, ship, nil, "", false, nil)
	c, rec := newCtx(http.MethodGet, "/order/v1/private/orders/1/details", "user1", gin.Params{{Key: "id", Value: "1"}})
	h.GetOrderDetails(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if _, ok := decode(t, rec)["shipment"]; !ok {
		t.Errorf("expected shipment in response: %s", rec.Body.String())
	}
}

func TestGetOrderDetails_ShipmentError(t *testing.T) {
	repo := &mockOrderRepo{order: &domain.Order{ID: "1", UserID: "user1"}}
	ship := stubShipment{err: context.DeadlineExceeded}
	h := NewOrderHandler(logicv1.NewOrderService(repo, nil), nil, ship, nil, "", false, nil)
	c, rec := newCtx(http.MethodGet, "/order/v1/private/orders/1/details", "user1", gin.Params{{Key: "id", Value: "1"}})
	h.GetOrderDetails(c)

	// Shipment is optional — a fetch error must not fail the request.
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (shipment soft-fail)", rec.Code)
	}
}

func TestCreateOrder_BadJSON(t *testing.T) {
	c, rec := ctxWithBody(http.MethodPost, "/order/v1/private/orders", "user1", "{", nil)
	newHandler(&mockOrderRepo{}).CreateOrder(c)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if code := decode(t, rec)["code"]; code != "VALIDATION_ERROR" {
		t.Errorf("code = %v, want VALIDATION_ERROR", code)
	}
}

func TestCreateOrder_Unauthorized(t *testing.T) {
	c, rec := ctxWithBody(http.MethodPost, "/order/v1/private/orders", "", "{}", nil)
	newHandler(&mockOrderRepo{}).CreateOrder(c)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestCreateOrder_IdempotentReplayHit(t *testing.T) {
	repo := &mockOrderRepo{idemOrder: &domain.Order{ID: "7"}}
	c, rec := ctxWithBody(http.MethodPost, "/order/v1/private/orders", "user1", "{}",
		map[string]string{"Idempotency-Key": "k1"})
	newHandler(repo).CreateOrder(c)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (replay)", rec.Code)
	}
}

func TestCreateOrder_IdempotentLookupError(t *testing.T) {
	repo := &mockOrderRepo{idemErr: context.DeadlineExceeded}
	c, rec := ctxWithBody(http.MethodPost, "/order/v1/private/orders", "user1", "{}",
		map[string]string{"Idempotency-Key": "k1"})
	newHandler(repo).CreateOrder(c)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

func TestCreateOrder_NilCartClient(t *testing.T) {
	// With no cart client configured, CreateOrder can't source items → 500.
	c, rec := ctxWithBody(http.MethodPost, "/order/v1/private/orders", "user1", "{}", nil)
	newHandler(&mockOrderRepo{}).CreateOrder(c)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (no cart client)", rec.Code)
	}
}

func TestCreateOrder_CartEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"items":[]}`))
	}))
	defer srv.Close()
	h := NewOrderHandler(logicv1.NewOrderService(&mockOrderRepo{}, nil), NewCartClient(srv.URL), nil, nil, "", false, nil)
	c, rec := ctxWithBody(http.MethodPost, "/order/v1/private/orders", "user1", "{}", nil)
	h.CreateOrder(c)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (cart empty)", rec.Code)
	}
}

func TestCreateOrder_CartReadError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	h := NewOrderHandler(logicv1.NewOrderService(&mockOrderRepo{}, nil), NewCartClient(srv.URL), nil, nil, "", false, nil)
	c, rec := ctxWithBody(http.MethodPost, "/order/v1/private/orders", "user1", "{}", nil)
	h.CreateOrder(c)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (cart read error)", rec.Code)
	}
}

func ctxWithBody(method, target, userID, body string, hdr map[string]string) (*gin.Context, *httptest.ResponseRecorder) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(method, target, strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		c.Request.Header.Set(k, v)
	}
	if userID != "" {
		c.Set("user_id", userID)
	}
	return c, rec
}

// stubPayment is a PaymentFetcher double for the details enrichment.
type stubPayment struct {
	info *PaymentInfo
	err  error
}

func (s stubPayment) GetPaymentByOrderID(context.Context, int64) (*PaymentInfo, error) {
	return s.info, s.err
}

func TestGetOrderDetails_WithPayment(t *testing.T) {
	repo := &mockOrderRepo{order: &domain.Order{ID: "1", UserID: "user1"}}
	pay := stubPayment{info: &PaymentInfo{Status: "captured", Amount: 25.50, Currency: "USD"}}
	h := NewOrderHandler(logicv1.NewOrderService(repo, nil), nil, nil, nil, "", true, pay)
	c, rec := newCtx(http.MethodGet, "/order/v1/private/orders/1/details", "user1", gin.Params{{Key: "id", Value: "1"}})
	h.GetOrderDetails(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := decode(t, rec)
	p, ok := body["payment"].(map[string]any)
	if !ok {
		t.Fatalf("response missing payment field: %s", rec.Body.String())
	}
	if p["status"] != "captured" || p["amount"] != 25.50 {
		t.Fatalf("payment = %+v", p)
	}
}

func TestGetOrderDetails_PaymentSoftFail(t *testing.T) {
	repo := &mockOrderRepo{order: &domain.Order{ID: "1", UserID: "user1"}}
	pay := stubPayment{err: errors.New("payment down")}
	h := NewOrderHandler(logicv1.NewOrderService(repo, nil), nil, nil, nil, "", true, pay)
	c, rec := newCtx(http.MethodGet, "/order/v1/private/orders/1/details", "user1", gin.Params{{Key: "id", Value: "1"}})
	h.GetOrderDetails(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("payment failure must not fail the details response: %d", rec.Code)
	}
	if _, ok := decode(t, rec)["payment"]; ok {
		t.Fatalf("failed enrichment must omit the payment field: %s", rec.Body.String())
	}
}

func TestGetOrderDetails_PaymentDisabled(t *testing.T) {
	repo := &mockOrderRepo{order: &domain.Order{ID: "1", UserID: "user1"}}
	// paymentEnabled=false → the fetcher must not be consulted even when set.
	h := NewOrderHandler(logicv1.NewOrderService(repo, nil), nil, nil, nil, "", false, stubPayment{info: &PaymentInfo{Status: "captured"}})
	c, rec := newCtx(http.MethodGet, "/order/v1/private/orders/1/details", "user1", gin.Params{{Key: "id", Value: "1"}})
	h.GetOrderDetails(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if _, ok := decode(t, rec)["payment"]; ok {
		t.Fatalf("disabled integration must omit the payment field: %s", rec.Body.String())
	}
}
