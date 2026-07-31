package v1

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/duynhlab/order-service/internal/core/domain"
	logicv1 "github.com/duynhlab/order-service/internal/logic/v1"
	inventoryv1 "github.com/duynhlab/pkg/proto/inventory/v1"
	"go.temporal.io/sdk/client"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Cancel-path stubs: the logic service is real; only its seams are faked.

type stubStatusTxWriter struct {
	err  error
	cmds []domain.StatusCommand
}

func (s *stubStatusTxWriter) ApplyStatusCommandWithTx(_ context.Context, _ domain.Transaction, cmd domain.StatusCommand) (bool, error) {
	if s.err != nil {
		return false, s.err
	}
	s.cmds = append(s.cmds, cmd)
	return false, nil
}

type stubCancelStore struct {
	armed  []int64
	closed []int64
}

func (s *stubCancelStore) ArmWithTx(_ context.Context, _ domain.Transaction, _ string, epoch int64) error {
	s.armed = append(s.armed, epoch)
	return nil
}
func (s *stubCancelStore) MarkDispatched(context.Context, string, int64) error { return nil }
func (s *stubCancelStore) ClaimDue(context.Context, int, time.Duration) ([]domain.CancellationRequest, error) {
	return nil, nil
}
func (s *stubCancelStore) Reschedule(context.Context, string, int64, time.Time, string) error {
	return nil
}
func (s *stubCancelStore) MarkFailed(context.Context, string, int64, string) error { return nil }
func (s *stubCancelStore) Stats(context.Context) (domain.CancellationRequestStats, error) {
	return domain.CancellationRequestStats{}, nil
}
func (s *stubCancelStore) CloseDispatchedForUser(_ context.Context, _, _ string, epoch int64) error {
	s.closed = append(s.closed, epoch)
	return nil
}

func cancelHandler(order *domain.Order, findErr error, ship shipmentFetcher) (*OrderHandler, *stubStatusTxWriter, *stubCancelStore) {
	repo := &mockOrderRepo{order: order, findErr: findErr}
	tw := &stubStatusTxWriter{}
	store := &stubCancelStore{}
	svc := logicv1.NewOrderService(repo, stubTxManager{}, &stubOutbox{}, &stubOutbox{}, noopProjection{}, tw, store)
	// No Temporal starter: the inline start is skipped (the outbox row is
	// durable and the dispatcher owns it) — the handler must still 202.
	h := NewOrderHandler(svc, nil, ship, nil, "order-fulfillment", nil, "product", store, nil, nil)
	return h, tw, store
}

func cancelCtx(userID string) (*gin.Context, *httptest.ResponseRecorder) {
	return newCtx(http.MethodPost, "/order/v1/private/orders/42/cancel", userID,
		gin.Params{{Key: "id", Value: "42"}})
}

func TestCancelOrderHandler(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("202: opens the episode, arms the outbox", func(t *testing.T) {
		h, tw, store := cancelHandler(&domain.Order{ID: "42", UserID: "7", Status: "confirmed", Total: 2500, Version: 4}, nil, nil)
		c, rec := cancelCtx("7")
		h.CancelOrder(c)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("status = %d body=%s, want 202", rec.Code, rec.Body.String())
		}
		if len(tw.cmds) != 1 || tw.cmds[0].CommandID != "cancel:42:v4" {
			t.Errorf("commands = %+v", tw.cmds)
		}
		if len(store.armed) != 1 || store.armed[0] != 4 {
			t.Errorf("armed = %+v", store.armed)
		}
	})

	t.Run("200: already cancelling replays without writes", func(t *testing.T) {
		h, tw, store := cancelHandler(&domain.Order{ID: "42", UserID: "7", Status: "cancelling", Version: 5}, nil, nil)
		c, rec := cancelCtx("7")
		h.CancelOrder(c)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		if len(tw.cmds) != 0 || len(store.armed) != 0 {
			t.Error("replay must write nothing")
		}
	})

	t.Run("409: pending refuses", func(t *testing.T) {
		h, _, _ := cancelHandler(&domain.Order{ID: "42", UserID: "7", Status: "pending", Version: 1}, nil, nil)
		c, rec := cancelCtx("7")
		h.CancelOrder(c)
		if rec.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409", rec.Code)
		}
	})

	t.Run("409: dispatched shipment refuses at the pre-check", func(t *testing.T) {
		h, tw, _ := cancelHandler(&domain.Order{ID: "42", UserID: "7", Status: "confirmed", Version: 4}, nil,
			stubShipment{shipment: &Shipment{Status: "in_transit"}})
		c, rec := cancelCtx("7")
		h.CancelOrder(c)
		if rec.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409", rec.Code)
		}
		if len(tw.cmds) != 0 {
			t.Error("the pre-check must refuse before any write")
		}
	})

	t.Run("pre-check soft-fails when shipping is dark", func(t *testing.T) {
		h, tw, _ := cancelHandler(&domain.Order{ID: "42", UserID: "7", Status: "confirmed", Version: 4}, nil,
			stubShipment{err: context.DeadlineExceeded})
		c, rec := cancelCtx("7")
		h.CancelOrder(c)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want 202 — the workflow re-checks authoritatively", rec.Code)
		}
		if len(tw.cmds) != 1 {
			t.Error("expected the episode to open")
		}
	})

	t.Run("404: not the caller's order — before the shipment pre-check leaks anything", func(t *testing.T) {
		h, _, _ := cancelHandler(nil, domain.ErrNotFound,
			stubShipment{shipment: &Shipment{Status: "in_transit"}})
		c, rec := cancelCtx("999")
		h.CancelOrder(c)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404 (never a shipment-state 409 for a stranger's order)", rec.Code)
		}
	})

	t.Run("401: no user", func(t *testing.T) {
		h, _, _ := cancelHandler(&domain.Order{ID: "42", UserID: "7", Status: "confirmed"}, nil, nil)
		c, rec := cancelCtx("")
		h.CancelOrder(c)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	})
}

// --- /details expand (S5e): the degraded-vs-absent contract ---

type stubProcessing struct {
	state *domain.ProcessingState
	err   error
}

func (s *stubProcessing) ReadProcessingState(context.Context, string) (*domain.ProcessingState, error) {
	return s.state, s.err
}

type stubReservation struct {
	status string
	err    error
}

func (s *stubReservation) GetReservationStatus(context.Context, string) (string, error) {
	return s.status, s.err
}

func detailsHandler(ship shipmentFetcher, proc processingFetcher, inv ReservationFetcher) *OrderHandler {
	repo := &mockOrderRepo{order: &domain.Order{ID: "42", UserID: "7", Status: "confirmed", Total: 2500}}
	svc := logicv1.NewOrderService(repo, stubTxManager{}, &stubOutbox{}, &stubOutbox{}, noopProjection{}, nil, nil)
	return NewOrderHandler(svc, nil, ship, nil, "q", nil, "product", nil, proc, inv)
}

func TestGetOrderDetails_DegradedVsAbsent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	get := func(h *OrderHandler) (int, string) {
		c, rec := newCtx(http.MethodGet, "/order/v1/private/orders/42/details", "7",
			gin.Params{{Key: "id", Value: "42"}})
		h.GetOrderDetails(c)
		return rec.Code, rec.Body.String()
	}

	t.Run("failed fetches are DEGRADED", func(t *testing.T) {
		h := detailsHandler(
			stubShipment{err: context.DeadlineExceeded},
			&stubProcessing{err: context.DeadlineExceeded},
			&stubReservation{err: context.DeadlineExceeded},
		)
		code, body := get(h)
		if code != http.StatusOK {
			t.Fatalf("status = %d", code)
		}
		for _, token := range []string{"shipment", "inventory", "processing"} {
			if !strings.Contains(body, `"degraded"`) || !strings.Contains(body, `"`+token+`"`) {
				t.Errorf("degraded must list %q: %s", token, body)
			}
		}
	})

	t.Run("genuinely absent blocks are NOT degraded", func(t *testing.T) {
		h := detailsHandler(
			stubShipment{},                           // no shipment yet
			&stubProcessing{err: domain.ErrNotFound}, // pre-projection order
			&stubReservation{},                       // product path: no reservation
		)
		code, body := get(h)
		if code != http.StatusOK {
			t.Fatalf("status = %d", code)
		}
		if strings.Contains(body, "degraded") {
			t.Errorf("absence must not read as degradation: %s", body)
		}
		if strings.Contains(body, `"processing"`) || strings.Contains(body, `"inventory"`) {
			t.Errorf("absent blocks must be omitted: %s", body)
		}
	})

	t.Run("present blocks render", func(t *testing.T) {
		h := detailsHandler(
			stubShipment{shipment: &Shipment{Status: "pending"}},
			&stubProcessing{state: &domain.ProcessingState{Stage: domain.StageDone, UpdatedAt: "2026-07-31T00:00:00Z"}},
			&stubReservation{status: "committed"},
		)
		code, body := get(h)
		if code != http.StatusOK {
			t.Fatalf("status = %d", code)
		}
		for _, want := range []string{`"stage":"DONE"`, `"inventory":{"status":"committed"}`, `"shipment"`} {
			if !strings.Contains(body, want) {
				t.Errorf("body missing %s: %s", want, body)
			}
		}
		if strings.Contains(body, "degraded") {
			t.Errorf("nothing failed, nothing degrades: %s", body)
		}
	})
}

// The inventory read client's status mapping and soft-miss semantics.
func TestInventoryGRPCClient_GetReservationStatus(t *testing.T) {
	t.Run("maps the enum to a lowercase token", func(t *testing.T) {
		c := &InventoryGRPCClient{client: &webInventoryStub{status: inventoryv1.ReservationStatus_RESERVATION_STATUS_COMMITTED}}
		got, err := c.GetReservationStatus(context.Background(), "42")
		if err != nil || got != "committed" {
			t.Fatalf("got %q err=%v", got, err)
		}
	})
	t.Run("NotFound is the empty state", func(t *testing.T) {
		c := &InventoryGRPCClient{client: &webInventoryStub{err: status.Error(codes.NotFound, "none")}}
		got, err := c.GetReservationStatus(context.Background(), "42")
		if err != nil || got != "" {
			t.Fatalf("got %q err=%v", got, err)
		}
	})
	t.Run("transport errors surface", func(t *testing.T) {
		c := &InventoryGRPCClient{client: &webInventoryStub{err: status.Error(codes.Unavailable, "down")}}
		if _, err := c.GetReservationStatus(context.Background(), "42"); err == nil {
			t.Fatal("want error")
		}
	})
}

type webInventoryStub struct {
	inventoryv1.InventoryServiceClient
	status inventoryv1.ReservationStatus
	err    error
}

func (s *webInventoryStub) GetReservation(context.Context, *inventoryv1.GetReservationRequest, ...grpc.CallOption) (*inventoryv1.GetReservationResponse, error) {
	if s.err != nil {
		return nil, s.err
	}
	return &inventoryv1.GetReservationResponse{
		Reservation: &inventoryv1.Reservation{Status: s.status},
	}, nil
}

// The inline start closes the outbox row with the episode's epoch; a start
// failure only logs (the row is durable, the dispatcher owns it).
func TestCancelOrderHandler_InlineStart(t *testing.T) {
	gin.SetMode(gin.TestMode)

	newH := func(startErr error) (*OrderHandler, *stubCancelStore, *webStarterStub) {
		repo := &mockOrderRepo{order: &domain.Order{ID: "42", UserID: "7", Status: "confirmed", Total: 2500, Version: 4}}
		tw := &stubStatusTxWriter{}
		store := &stubCancelStore{}
		starter := &webStarterStub{err: startErr}
		svc := logicv1.NewOrderService(repo, stubTxManager{}, &stubOutbox{}, &stubOutbox{}, noopProjection{}, tw, store)
		h := NewOrderHandler(svc, nil, nil, starter, "order-fulfillment", nil, "product", store, nil, nil)
		return h, store, starter
	}

	t.Run("start success closes the row with the epoch", func(t *testing.T) {
		h, store, starter := newH(nil)
		c, rec := cancelCtx("7")
		h.CancelOrder(c)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("status = %d", rec.Code)
		}
		if len(starter.calls) != 1 || starter.calls[0] != "order-cancellation-42-v4" {
			t.Errorf("starts = %+v", starter.calls)
		}
		if len(store.closed) != 1 || store.closed[0] != 4 {
			t.Errorf("closed = %+v, want the epoch", store.closed)
		}
	})

	t.Run("start failure still answers 202 and leaves the row open", func(t *testing.T) {
		h, store, _ := newH(context.DeadlineExceeded)
		c, rec := cancelCtx("7")
		h.CancelOrder(c)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("status = %d", rec.Code)
		}
		if len(store.closed) != 0 {
			t.Errorf("closed = %+v, want the dispatcher to own it", store.closed)
		}
	})
}

type webStarterStub struct {
	err   error
	calls []string
}

func (s *webStarterStub) ExecuteWorkflow(_ context.Context, opts client.StartWorkflowOptions,
	_ any, _ ...any) (client.WorkflowRun, error) {
	s.calls = append(s.calls, opts.ID)
	return nil, s.err
}
