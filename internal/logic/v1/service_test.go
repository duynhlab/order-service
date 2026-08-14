package v1

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/duynhlab/order-service/internal/core/domain"
)

// errBoom is a generic non-sentinel infrastructure error used to exercise
// error-propagation paths.
var errBoom = errors.New("boom")

// MockTransaction records whether Commit/Rollback were called and can be
// configured to fail on commit.
type MockTransaction struct {
	commitErr      error
	commitCalled   bool
	rollbackCalled bool
}

func (m *MockTransaction) Commit(ctx context.Context) error {
	m.commitCalled = true
	return m.commitErr
}

func (m *MockTransaction) Rollback(ctx context.Context) error {
	m.rollbackCalled = true
	return nil
}

// MockTransactionManager hands out a configurable transaction and can be made
// to fail on Begin.
type MockTransactionManager struct {
	tx       *MockTransaction
	beginErr error
}

func (m *MockTransactionManager) Begin(ctx context.Context) (domain.Transaction, error) {
	if m.beginErr != nil {
		return nil, m.beginErr
	}
	if m.tx == nil {
		m.tx = &MockTransaction{}
	}
	return m.tx, nil
}

// MockOrderRepository is a fully configurable repository double; each method
// delegates to its *Func field when set, otherwise returns a benign default.
type MockOrderRepository struct {
	findByIDFunc             func(ctx context.Context, userID, id string) (*domain.Order, error)
	findByUserIDFunc         func(ctx context.Context, userID string, limit, offset int) ([]domain.Order, error)
	countByUserIDFunc        func(ctx context.Context, userID string) (int, error)
	findByIdempotencyKeyFunc func(ctx context.Context, userID, key string) (*domain.Order, error)
	createFunc               func(ctx context.Context, order *domain.Order) error
	createWithTxFunc         func(ctx context.Context, tx domain.Transaction, order *domain.Order) error
}

func (m *MockOrderRepository) FindByID(ctx context.Context, userID, id string) (*domain.Order, error) {
	if m.findByIDFunc != nil {
		return m.findByIDFunc(ctx, userID, id)
	}
	return nil, nil
}

func (m *MockOrderRepository) FindByUserID(ctx context.Context, userID string, limit, offset int) ([]domain.Order, error) {
	if m.findByUserIDFunc != nil {
		return m.findByUserIDFunc(ctx, userID, limit, offset)
	}
	return nil, nil
}

func (m *MockOrderRepository) CountByUserID(ctx context.Context, userID string) (int, error) {
	if m.countByUserIDFunc != nil {
		return m.countByUserIDFunc(ctx, userID)
	}
	return 0, nil
}

func (m *MockOrderRepository) FindByIdempotencyKey(ctx context.Context, userID, key string) (*domain.Order, error) {
	if m.findByIdempotencyKeyFunc != nil {
		return m.findByIdempotencyKeyFunc(ctx, userID, key)
	}
	return nil, domain.ErrNotFound
}

func (m *MockOrderRepository) Create(ctx context.Context, order *domain.Order) error {
	if m.createFunc != nil {
		return m.createFunc(ctx, order)
	}
	return nil
}

func (m *MockOrderRepository) CreateWithTx(ctx context.Context, tx domain.Transaction, order *domain.Order) error {
	if m.createWithTxFunc != nil {
		return m.createWithTxFunc(ctx, tx, order)
	}
	return nil
}

func (m *MockOrderRepository) ListAll(_ context.Context, _ string, _, _ int) ([]domain.Order, int, error) {
	return nil, 0, nil
}

func (m *MockOrderRepository) FindByIDUnscoped(_ context.Context, _ string) (*domain.Order, error) {
	return nil, domain.ErrNotFound
}

func TestCreateOrder(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name         string
		req          domain.CreateOrderRequest
		repo         *MockOrderRepository
		txMgr        *MockTransactionManager
		wantSubtotal int64
		wantErr      error // sentinel to match with errors.Is; nil means success
		wantCommit   bool  // whether the tx should have been committed
	}{
		{
			name: "success path enriches items and commits",
			req: domain.CreateOrderRequest{
				UserID:         "user1",
				IdempotencyKey: "key-1",
				Items: []domain.OrderItem{
					{ProductID: "p1", Quantity: 2, Price: 10}, // 20
					{ProductID: "p2", Quantity: 1, Price: 5},  // 5
				},
			},
			repo:         &MockOrderRepository{},
			txMgr:        &MockTransactionManager{},
			wantSubtotal: 25,
			wantErr:      nil,
			wantCommit:   true,
		},
		{
			name: "empty cart returns ErrInvalidOrder",
			req: domain.CreateOrderRequest{
				UserID: "user1",
				Items:  []domain.OrderItem{},
			},
			repo:    &MockOrderRepository{},
			txMgr:   &MockTransactionManager{},
			wantErr: ErrInvalidOrder,
		},
		{
			name: "non-positive quantity is rejected",
			req: domain.CreateOrderRequest{
				UserID: "user1",
				Items:  []domain.OrderItem{{ProductID: "p1", Quantity: 0, Price: 10}},
			},
			repo:    &MockOrderRepository{},
			txMgr:   &MockTransactionManager{},
			wantErr: ErrInvalidOrder,
		},
		{
			name: "negative price is rejected",
			req: domain.CreateOrderRequest{
				UserID: "user1",
				Items:  []domain.OrderItem{{ProductID: "p1", Quantity: 1, Price: -1}},
			},
			repo:    &MockOrderRepository{},
			txMgr:   &MockTransactionManager{},
			wantErr: ErrInvalidOrder,
		},
		{
			name: "empty product id is rejected",
			req: domain.CreateOrderRequest{
				UserID: "user1",
				Items:  []domain.OrderItem{{ProductID: "", Quantity: 1, Price: 10}},
			},
			repo:    &MockOrderRepository{},
			txMgr:   &MockTransactionManager{},
			wantErr: ErrInvalidOrder,
		},
		{
			name: "Begin failure propagates",
			req: domain.CreateOrderRequest{
				UserID: "user1",
				Items:  []domain.OrderItem{{ProductID: "p1", Quantity: 1, Price: 10}},
			},
			repo:    &MockOrderRepository{},
			txMgr:   &MockTransactionManager{beginErr: errBoom},
			wantErr: errBoom,
		},
		{
			name: "CreateWithTx failure rolls back",
			req: domain.CreateOrderRequest{
				UserID: "user1",
				Items:  []domain.OrderItem{{ProductID: "p1", Quantity: 1, Price: 10}},
			},
			repo: &MockOrderRepository{
				createWithTxFunc: func(ctx context.Context, tx domain.Transaction, order *domain.Order) error {
					return errBoom
				},
			},
			txMgr:      &MockTransactionManager{},
			wantErr:    errBoom,
			wantCommit: false,
		},
		{
			name: "Commit failure propagates",
			req: domain.CreateOrderRequest{
				UserID: "user1",
				Items:  []domain.OrderItem{{ProductID: "p1", Quantity: 1, Price: 10}},
			},
			repo:    &MockOrderRepository{},
			txMgr:   &MockTransactionManager{tx: &MockTransaction{commitErr: errBoom}},
			wantErr: errBoom,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			service := NewOrderService(tt.repo, tt.txMgr, &stubStartRequests{}, &stubStartRequests{}, &stubProjection{}, &stubTxWriter{}, &stubCancellations{})

			order, err := service.CreateOrder(ctx, tt.req)

			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("CreateOrder() error = %v, want %v", err, tt.wantErr)
				}
				if order != nil {
					t.Errorf("CreateOrder() order = %v, want nil on error", order)
				}
				return
			}

			if err != nil {
				t.Fatalf("CreateOrder() unexpected error = %v", err)
			}
			if order == nil {
				t.Fatal("CreateOrder() order = nil, want non-nil")
			}
			if order.Subtotal != tt.wantSubtotal {
				t.Errorf("CreateOrder() subtotal = %v, want %v", order.Subtotal, tt.wantSubtotal)
			}
			// No totals in these requests, so total == subtotal (fee, tax,
			// and discount are the caller's since RFC-0021 P5).
			if order.Total != tt.wantSubtotal {
				t.Errorf("CreateOrder() total = %v, want %v", order.Total, tt.wantSubtotal)
			}
			if order.Status != "pending" {
				t.Errorf("CreateOrder() status = %q, want %q", order.Status, "pending")
			}
			if order.IdempotencyKey != tt.req.IdempotencyKey {
				t.Errorf("CreateOrder() idempotencyKey = %q, want %q", order.IdempotencyKey, tt.req.IdempotencyKey)
			}
			if tt.wantCommit && !tt.txMgr.tx.commitCalled {
				t.Error("CreateOrder() expected transaction commit, got none")
			}
		})
	}
}

// TestCreateOrder_ComposesCallerTotals pins the money math: the persisted
// order must carry the caller's fee/tax/discount and charge
// subtotal + fee + tax - discount. Guards the RFC-0015 P4 invariant that the
// charged total equals the session total the shopper confirmed — dropping a
// component here silently diverges the charge from the quote.
func TestCreateOrder_ComposesCallerTotals(t *testing.T) {
	ctx := context.Background()
	var created *domain.Order
	repo := &MockOrderRepository{
		createWithTxFunc: func(_ context.Context, _ domain.Transaction, o *domain.Order) error {
			created = o
			return nil
		},
	}
	s := NewOrderService(repo, &MockTransactionManager{}, &stubStartRequests{}, &stubStartRequests{}, &stubProjection{}, &stubTxWriter{}, &stubCancellations{})

	_, err := s.CreateOrder(ctx, domain.CreateOrderRequest{
		UserID: "u1", IdempotencyKey: "k-totals",
		ShippingFeeMinor: 300, TaxMinor: 504, DiscountMinor: 600,
		Items: []domain.OrderItem{{ProductID: "p1", Quantity: 2, Price: 2999}},
	})
	if err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	if created == nil {
		t.Fatal("CreateWithTx never saw the order")
	}
	if created.Subtotal != 5998 || created.Shipping != 300 || created.Tax != 504 || created.Discount != 600 {
		t.Errorf("components = subtotal %d fee %d tax %d discount %d, want 5998/300/504/600",
			created.Subtotal, created.Shipping, created.Tax, created.Discount)
	}
	if want := int64(5998 + 300 + 504 - 600); created.Total != want {
		t.Errorf("Total = %d, want %d (subtotal + fee + tax - discount)", created.Total, want)
	}
}

// TestCreateOrder_ConflictReplays verifies that when CreateWithTx hits the
// unique-key constraint (domain.ErrConflict) — a double-submit that raced past
// the pre-check — CreateOrder re-fetches by idempotency key and returns the
// existing order instead of surfacing a 500.
func TestCreateOrder_ConflictReplays(t *testing.T) {
	ctx := context.Background()
	existing := &domain.Order{ID: "existing-1", UserID: "user1", IdempotencyKey: "key-1", Status: "pending"}

	t.Run("conflict re-fetches and replays existing order", func(t *testing.T) {
		var lookupKey, lookupUser string
		repo := &MockOrderRepository{
			createWithTxFunc: func(ctx context.Context, tx domain.Transaction, order *domain.Order) error {
				return domain.ErrConflict
			},
			findByIdempotencyKeyFunc: func(ctx context.Context, userID, key string) (*domain.Order, error) {
				lookupUser, lookupKey = userID, key
				return existing, nil
			},
		}
		txMgr := &MockTransactionManager{}
		service := NewOrderService(repo, txMgr, &stubStartRequests{}, &stubStartRequests{}, &stubProjection{}, &stubTxWriter{}, &stubCancellations{})

		order, err := service.CreateOrder(ctx, domain.CreateOrderRequest{
			UserID:         "user1",
			IdempotencyKey: "key-1",
			Items:          []domain.OrderItem{{ProductID: "p1", Quantity: 1, Price: 10}},
		})
		if err != nil {
			t.Fatalf("CreateOrder() on conflict err = %v, want nil (replay)", err)
		}
		if order != existing {
			t.Errorf("CreateOrder() order = %v, want existing %v", order, existing)
		}
		if lookupUser != "user1" || lookupKey != "key-1" {
			t.Errorf("re-fetch used (user=%q,key=%q), want (user1,key-1)", lookupUser, lookupKey)
		}
		if txMgr.tx.commitCalled {
			t.Error("CreateOrder() committed on conflict, want no commit")
		}
	})

	t.Run("conflict re-fetch failure propagates", func(t *testing.T) {
		repo := &MockOrderRepository{
			createWithTxFunc: func(ctx context.Context, tx domain.Transaction, order *domain.Order) error {
				return domain.ErrConflict
			},
			findByIdempotencyKeyFunc: func(ctx context.Context, userID, key string) (*domain.Order, error) {
				return nil, errBoom
			},
		}
		service := NewOrderService(repo, &MockTransactionManager{}, &stubStartRequests{}, &stubStartRequests{}, &stubProjection{}, &stubTxWriter{}, &stubCancellations{})

		order, err := service.CreateOrder(ctx, domain.CreateOrderRequest{
			UserID:         "user1",
			IdempotencyKey: "key-1",
			Items:          []domain.OrderItem{{ProductID: "p1", Quantity: 1, Price: 10}},
		})
		if !errors.Is(err, errBoom) {
			t.Fatalf("CreateOrder() re-fetch err = %v, want errBoom", err)
		}
		if order != nil {
			t.Errorf("CreateOrder() order = %v, want nil on re-fetch failure", order)
		}
	})
}

func TestCreateOrder_ProductNameFallback(t *testing.T) {
	ctx := context.Background()
	var captured *domain.Order
	repo := &MockOrderRepository{
		createWithTxFunc: func(ctx context.Context, tx domain.Transaction, order *domain.Order) error {
			captured = order
			return nil
		},
	}
	service := NewOrderService(repo, &MockTransactionManager{}, &stubStartRequests{}, &stubStartRequests{}, &stubProjection{}, &stubTxWriter{}, &stubCancellations{})

	_, err := service.CreateOrder(ctx, domain.CreateOrderRequest{
		UserID: "user1",
		Items: []domain.OrderItem{
			{ProductID: "p1", Quantity: 1, Price: 10},                        // no name -> fallback
			{ProductID: "p2", Quantity: 1, Price: 10, ProductName: "Widget"}, // keeps name
		},
	})
	if err != nil {
		t.Fatalf("CreateOrder() unexpected error = %v", err)
	}
	if captured == nil {
		t.Fatal("CreateWithTx was not called")
	}
	if captured.Items[0].ProductName != "Product p1" {
		t.Errorf("fallback name = %q, want %q", captured.Items[0].ProductName, "Product p1")
	}
	if captured.Items[1].ProductName != "Widget" {
		t.Errorf("provided name = %q, want %q", captured.Items[1].ProductName, "Widget")
	}
	if captured.Items[0].Subtotal != 10.0 {
		t.Errorf("item subtotal = %v, want %v", captured.Items[0].Subtotal, 10.0)
	}
}

func TestGetByIdempotencyKey(t *testing.T) {
	ctx := context.Background()
	existing := &domain.Order{ID: "order-1", UserID: "user1", IdempotencyKey: "key-1"}

	tests := []struct {
		name      string
		repo      *MockOrderRepository
		wantOrder *domain.Order
		wantErr   error
	}{
		{
			name: "existing order returned for same key",
			repo: &MockOrderRepository{
				findByIdempotencyKeyFunc: func(ctx context.Context, userID, key string) (*domain.Order, error) {
					return existing, nil
				},
			},
			wantOrder: existing,
		},
		{
			name: "not found maps to nil order and nil error",
			repo: &MockOrderRepository{
				findByIdempotencyKeyFunc: func(ctx context.Context, userID, key string) (*domain.Order, error) {
					return nil, domain.ErrNotFound
				},
			},
			wantOrder: nil,
		},
		{
			name: "infrastructure error propagates",
			repo: &MockOrderRepository{
				findByIdempotencyKeyFunc: func(ctx context.Context, userID, key string) (*domain.Order, error) {
					return nil, errBoom
				},
			},
			wantErr: errBoom,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			service := NewOrderService(tt.repo, &MockTransactionManager{}, &stubStartRequests{}, &stubStartRequests{}, &stubProjection{}, &stubTxWriter{}, &stubCancellations{})
			order, err := service.GetByIdempotencyKey(ctx, "user1", "key-1")

			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("GetByIdempotencyKey() error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("GetByIdempotencyKey() unexpected error = %v", err)
			}
			if order != tt.wantOrder {
				t.Errorf("GetByIdempotencyKey() order = %v, want %v", order, tt.wantOrder)
			}
		})
	}
}

func TestListOrders(t *testing.T) {
	ctx := context.Background()
	want := []domain.Order{{ID: "o1"}, {ID: "o2"}}

	t.Run("returns orders from repository", func(t *testing.T) {
		repo := &MockOrderRepository{
			findByUserIDFunc: func(ctx context.Context, userID string, limit, offset int) ([]domain.Order, error) {
				return want, nil
			},
			countByUserIDFunc: func(ctx context.Context, userID string) (int, error) {
				return len(want), nil
			},
		}
		service := NewOrderService(repo, &MockTransactionManager{}, &stubStartRequests{}, &stubStartRequests{}, &stubProjection{}, &stubTxWriter{}, &stubCancellations{})
		got, total, err := service.ListOrders(ctx, "user1", 20, 0)
		if err != nil {
			t.Fatalf("ListOrders() unexpected error = %v", err)
		}
		if len(got) != len(want) {
			t.Errorf("ListOrders() len = %d, want %d", len(got), len(want))
		}
		if total != len(want) {
			t.Errorf("ListOrders() total = %d, want %d", total, len(want))
		}
	})

	t.Run("propagates repository error", func(t *testing.T) {
		repo := &MockOrderRepository{
			findByUserIDFunc: func(ctx context.Context, userID string, limit, offset int) ([]domain.Order, error) {
				return nil, errBoom
			},
		}
		service := NewOrderService(repo, &MockTransactionManager{}, &stubStartRequests{}, &stubStartRequests{}, &stubProjection{}, &stubTxWriter{}, &stubCancellations{})
		if _, _, err := service.ListOrders(ctx, "user1", 20, 0); !errors.Is(err, errBoom) {
			t.Errorf("ListOrders() error = %v, want %v", err, errBoom)
		}
	})

	t.Run("propagates count error", func(t *testing.T) {
		repo := &MockOrderRepository{
			countByUserIDFunc: func(_ context.Context, _ string) (int, error) {
				return 0, errBoom
			},
		}
		service := NewOrderService(repo, &MockTransactionManager{}, &stubStartRequests{}, &stubStartRequests{}, &stubProjection{}, &stubTxWriter{}, &stubCancellations{})
		if _, _, err := service.ListOrders(ctx, "user1", 20, 0); !errors.Is(err, errBoom) {
			t.Errorf("ListOrders() count error = %v, want %v", err, errBoom)
		}
	})
}

func TestGetOrder(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name    string
		repo    *MockOrderRepository
		wantErr error
	}{
		{
			name: "found",
			repo: &MockOrderRepository{
				findByIDFunc: func(ctx context.Context, userID, id string) (*domain.Order, error) {
					return &domain.Order{ID: id}, nil
				},
			},
		},
		{
			name: "not found maps to ErrOrderNotFound",
			repo: &MockOrderRepository{
				findByIDFunc: func(ctx context.Context, userID, id string) (*domain.Order, error) {
					return nil, domain.ErrNotFound
				},
			},
			wantErr: ErrOrderNotFound,
		},
		{
			name: "infrastructure error propagates",
			repo: &MockOrderRepository{
				findByIDFunc: func(ctx context.Context, userID, id string) (*domain.Order, error) {
					return nil, errBoom
				},
			},
			wantErr: errBoom,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			service := NewOrderService(tt.repo, &MockTransactionManager{}, &stubStartRequests{}, &stubStartRequests{}, &stubProjection{}, &stubTxWriter{}, &stubCancellations{})
			order, err := service.GetOrder(ctx, "user1", "order-1")

			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("GetOrder() error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("GetOrder() unexpected error = %v", err)
			}
			if order == nil {
				t.Error("GetOrder() order = nil, want non-nil")
			}
		})
	}
}

// stubStartRequests is the outbox seam for unit tests: it records what the
// service enqueued and can be made to fail, which is how the "a failed enqueue
// must fail the create" case is exercised without a database.
type stubStartRequests struct {
	enqueued            []string
	enqueuedToken       string
	enqueuedParticipant string
	enqueueErr          error
	dispatched          []string
}

func (s *stubStartRequests) EnqueueWithTx(_ context.Context, _ domain.Transaction, orderID, paymentMethod, participant string) error {
	if s.enqueueErr != nil {
		return s.enqueueErr
	}
	s.enqueued = append(s.enqueued, orderID)
	s.enqueuedToken = paymentMethod
	s.enqueuedParticipant = participant
	return nil
}

func (s *stubStartRequests) MarkDispatchedForUser(_ context.Context, _, orderID string) error {
	return s.MarkDispatched(context.Background(), orderID)
}

func (s *stubStartRequests) MarkDispatched(_ context.Context, orderID string) error {
	s.dispatched = append(s.dispatched, orderID)
	return nil
}

func (s *stubStartRequests) ClaimDue(_ context.Context, _ int, _ time.Duration) ([]domain.FulfillmentStartRequest, error) {
	return nil, nil
}

func (s *stubStartRequests) Reschedule(_ context.Context, _ string, _ time.Time, _ string) error {
	return nil
}

func (s *stubStartRequests) MarkFailed(_ context.Context, _, _ string) error { return nil }

func (s *stubStartRequests) Stats(_ context.Context) (domain.StartRequestStats, error) {
	return domain.StartRequestStats{}, nil
}

// The outbox row must be written with the order, carrying the payment token the
// dispatcher would otherwise be unable to rebuild.
func TestCreateOrder_EnqueuesTheStartRequestWithTheToken(t *testing.T) {
	ctx := context.Background()
	repo := &MockOrderRepository{
		createWithTxFunc: func(_ context.Context, _ domain.Transaction, order *domain.Order) error {
			order.ID = "77"
			return nil
		},
	}
	txMgr := &MockTransactionManager{}
	outbox := &stubStartRequests{}
	service := NewOrderService(repo, txMgr, outbox, outbox, &stubProjection{}, &stubTxWriter{}, &stubCancellations{})

	_, err := service.CreateOrder(ctx, domain.CreateOrderRequest{
		UserID:           "user1",
		Items:            []domain.OrderItem{{ProductID: "p1", Quantity: 1, Price: 10}},
		PaymentMethod:    "tok_visa_ok",
		StockParticipant: "inventory",
	})
	if err != nil {
		t.Fatalf("CreateOrder() = %v, want nil", err)
	}

	if len(outbox.enqueued) != 1 || outbox.enqueued[0] != "77" {
		t.Errorf("enqueued = %v, want exactly the created order id [77]", outbox.enqueued)
	}
	if outbox.enqueuedToken != "tok_visa_ok" {
		t.Errorf("enqueued token = %q, want the request's token — without it a retried start charges through the demo fallback", outbox.enqueuedToken)
	}
	// The participant has to reach the ROW, not just the workflow input. It is what
	// lets the reconciler tell a product-path order (no reservation is normal) from
	// a confirmed inventory-path one (no reservation is a lost write), and what the
	// dispatcher reads so a half-rolled-out cutover cannot start the saga on the
	// other participant than the row records.
	if outbox.enqueuedParticipant != "inventory" {
		t.Errorf("enqueued participant = %q, want inventory — the reconciler reads this column to judge a missing reservation",
			outbox.enqueuedParticipant)
	}
}

// The enqueue is NOT best-effort. An order whose saga nothing remembers to start
// is worse than no order: the customer sees a created order that never
// progresses. So a failing enqueue must fail the create and roll the order back.
func TestCreateOrder_FailedEnqueueFailsTheCreate(t *testing.T) {
	ctx := context.Background()
	repo := &MockOrderRepository{
		createWithTxFunc: func(_ context.Context, _ domain.Transaction, order *domain.Order) error {
			order.ID = "77"
			return nil
		},
	}
	txMgr := &MockTransactionManager{}
	outbox := &stubStartRequests{enqueueErr: errors.New("outbox insert failed")}
	service := NewOrderService(repo, txMgr, outbox, outbox, &stubProjection{}, &stubTxWriter{}, &stubCancellations{})

	order, err := service.CreateOrder(ctx, domain.CreateOrderRequest{
		UserID: "user1",
		Items:  []domain.OrderItem{{ProductID: "p1", Quantity: 1, Price: 10}},
	})
	if err == nil {
		t.Fatal("CreateOrder() = nil error with a failing outbox; want the create to fail")
	}
	if order != nil {
		t.Errorf("CreateOrder() order = %v, want nil", order)
	}
	if txMgr.tx != nil && txMgr.tx.commitCalled {
		t.Error("transaction was committed despite the outbox failure — the order row would exist with no record that its saga is owed")
	}
	if txMgr.tx == nil || !txMgr.tx.rollbackCalled {
		t.Error("transaction was not rolled back after the outbox failure")
	}
}

// A replayed order carries whatever the repository read for it: the participant is
// selected alongside the order, in one snapshot, so the logic layer neither
// re-reads it nor re-decides it.
func TestGetByIdempotencyKey_PassesTheOrdersParticipantThrough(t *testing.T) {
	repo := &MockOrderRepository{
		findByIdempotencyKeyFunc: func(context.Context, string, string) (*domain.Order, error) {
			return &domain.Order{ID: "42", StockParticipant: "inventory"}, nil
		},
	}
	service := NewOrderService(repo, &MockTransactionManager{}, &stubStartRequests{}, &stubStartRequests{}, &stubProjection{}, &stubTxWriter{}, &stubCancellations{})

	order, err := service.GetByIdempotencyKey(context.Background(), "user1", "key-1")
	if err != nil {
		t.Fatalf("GetByIdempotencyKey() = %v", err)
	}
	if order.StockParticipant != "inventory" {
		t.Errorf("StockParticipant = %q, want %q — the caller starts the saga with this",
			order.StockParticipant, "inventory")
	}
}

func TestCreateOrder_ReplayCarriesTheOrdersParticipantNotTheRequests(t *testing.T) {
	repo := &MockOrderRepository{
		createWithTxFunc: func(context.Context, domain.Transaction, *domain.Order) error {
			return domain.ErrConflict
		},
		findByIdempotencyKeyFunc: func(context.Context, string, string) (*domain.Order, error) {
			return &domain.Order{ID: "42", StockParticipant: "inventory"}, nil
		},
	}
	service := NewOrderService(repo, &MockTransactionManager{}, &stubStartRequests{}, &stubStartRequests{}, &stubProjection{}, &stubTxWriter{}, &stubCancellations{})

	// This request wanted the product path; the order it replays is already on the
	// inventory one, and the order wins.
	order, err := service.CreateOrder(context.Background(), domain.CreateOrderRequest{
		UserID:           "user1",
		IdempotencyKey:   "key-1",
		StockParticipant: "product",
		Items:            []domain.OrderItem{{ProductID: "p1", Quantity: 1, Price: 10}},
	})
	if err != nil {
		t.Fatalf("CreateOrder() = %v", err)
	}
	if order.StockParticipant != "inventory" {
		t.Errorf("StockParticipant = %q, want %q — a replay must not re-decide the branch",
			order.StockParticipant, "inventory")
	}
}

// The fresh path stamps what it just enqueued: this transaction wrote the row, so
// the value in hand IS the row's, and reading it back would only add a round trip
// that can fail.
func TestCreateOrder_FreshOrderCarriesTheStampedParticipant(t *testing.T) {
	repo := &MockOrderRepository{
		createWithTxFunc: func(_ context.Context, _ domain.Transaction, order *domain.Order) error {
			order.ID = "77"
			return nil
		},
	}
	service := NewOrderService(repo, &MockTransactionManager{}, &stubStartRequests{}, &stubStartRequests{}, &stubProjection{}, &stubTxWriter{}, &stubCancellations{})

	order, err := service.CreateOrder(context.Background(), domain.CreateOrderRequest{
		UserID:           "user1",
		StockParticipant: "inventory",
		Items:            []domain.OrderItem{{ProductID: "p1", Quantity: 1, Price: 10}},
	})
	if err != nil {
		t.Fatalf("CreateOrder() = %v", err)
	}
	if order.StockParticipant != "inventory" {
		t.Errorf("StockParticipant = %q, want %q", order.StockParticipant, "inventory")
	}
}

// stubProjection satisfies domain.ProcessingProjector; the projection is
// best-effort so most tests only need it to exist.
type stubProjection struct {
	err    error
	seeded []domain.ProcessingUpdate
}

func (s *stubProjection) UpsertProcessingStage(context.Context, domain.ProcessingUpdate) error {
	return s.err
}
func (s *stubProjection) UpsertProcessingStageWithTx(_ context.Context, _ domain.Transaction, u domain.ProcessingUpdate) error {
	if s.err != nil {
		return s.err
	}
	s.seeded = append(s.seeded, u)
	return nil
}

// The ORDER_CREATED seed is the projection's one transactional write: it
// must happen on every genuine create, and its failure must fail the create
// (an order whose projection row can never exist would render bare forever,
// and the same transaction is the only place the row is guaranteed).
func TestCreateOrder_SeedsTheProjection(t *testing.T) {
	repo := &MockOrderRepository{}
	proj := &stubProjection{}
	service := NewOrderService(repo, &MockTransactionManager{}, &stubStartRequests{}, &stubStartRequests{}, proj, &stubTxWriter{}, &stubCancellations{})

	order, err := service.CreateOrder(context.Background(), domain.CreateOrderRequest{
		UserID: "7",
		Items:  []domain.OrderItem{{ProductID: "1", Quantity: 1, Price: 1000, Subtotal: 1000}},
	})
	if err != nil {
		t.Fatalf("CreateOrder = %v", err)
	}
	if len(proj.seeded) != 1 || proj.seeded[0].Stage != domain.StageOrderCreated || proj.seeded[0].OrderID != order.ID {
		t.Errorf("seeded = %+v, want one ORDER_CREATED for order %s", proj.seeded, order.ID)
	}
}

func TestCreateOrder_SeedFailureFailsTheCreate(t *testing.T) {
	repo := &MockOrderRepository{}
	proj := &stubProjection{err: errors.New("projection table missing")}
	service := NewOrderService(repo, &MockTransactionManager{}, &stubStartRequests{}, &stubStartRequests{}, proj, &stubTxWriter{}, &stubCancellations{})

	if _, err := service.CreateOrder(context.Background(), domain.CreateOrderRequest{
		UserID: "7",
		Items:  []domain.OrderItem{{ProductID: "1", Quantity: 1, Price: 1000, Subtotal: 1000}},
	}); err == nil {
		t.Fatal("a failed transactional seed must fail the create")
	}
}

// stubTxWriter / stubCancellations satisfy the cancel path's seams.
type stubTxWriter struct {
	err      error
	replayed bool
	cmds     []domain.StatusCommand
}

func (s *stubTxWriter) ApplyStatusCommandWithTx(_ context.Context, _ domain.Transaction, cmd domain.StatusCommand) (bool, error) {
	if s.err != nil {
		return false, s.err
	}
	s.cmds = append(s.cmds, cmd)
	return s.replayed, nil
}

type stubCancellations struct {
	err   error
	armed []int64
}

func (s *stubCancellations) ArmWithTx(_ context.Context, _ domain.Transaction, _ string, epoch int64) error {
	if s.err != nil {
		return s.err
	}
	s.armed = append(s.armed, epoch)
	return nil
}
func (s *stubCancellations) MarkDispatched(context.Context, string, int64) error { return nil }
func (s *stubCancellations) ClaimDue(context.Context, int, time.Duration) ([]domain.CancellationRequest, error) {
	return nil, nil
}
func (s *stubCancellations) Reschedule(context.Context, string, int64, time.Time, string) error {
	return nil
}
func (s *stubCancellations) MarkFailed(context.Context, string, int64, string) error { return nil }
func (s *stubCancellations) Stats(context.Context) (domain.CancellationRequestStats, error) {
	return domain.CancellationRequestStats{}, nil
}

func TestCancelOrder(t *testing.T) {
	orderAt := func(status string, version int64) *MockOrderRepository {
		return &MockOrderRepository{findByIDFunc: func(_ context.Context, userID, id string) (*domain.Order, error) {
			return &domain.Order{ID: id, UserID: userID, Status: status, Total: 2500, Version: version}, nil
		}}
	}

	t.Run("confirmed order opens an episode: CAS + arm in one call", func(t *testing.T) {
		tw := &stubTxWriter{}
		canc := &stubCancellations{}
		svc := NewOrderService(orderAt("confirmed", 4), &MockTransactionManager{}, &stubStartRequests{}, &stubStartRequests{}, &stubProjection{}, tw, canc)

		out, err := svc.CancelOrder(context.Background(), "7", "42")
		if err != nil {
			t.Fatalf("CancelOrder = %v", err)
		}
		if out.Replayed || out.Epoch != 4 || out.Order.Status != "cancelling" {
			t.Errorf("outcome = %+v", out)
		}
		if len(tw.cmds) != 1 || tw.cmds[0].CommandID != "cancel:42:v4" || tw.cmds[0].ActorType != domain.ActorUser {
			t.Errorf("commands = %+v", tw.cmds)
		}
		if len(canc.armed) != 1 || canc.armed[0] != 4 {
			t.Errorf("armed = %+v, want the epoch", canc.armed)
		}
	})

	t.Run("completed orders are cancellable too (the merged-decision edge)", func(t *testing.T) {
		tw := &stubTxWriter{}
		svc := NewOrderService(orderAt("completed", 6), &MockTransactionManager{}, &stubStartRequests{}, &stubStartRequests{}, &stubProjection{}, tw, &stubCancellations{})
		if _, err := svc.CancelOrder(context.Background(), "7", "42"); err != nil {
			t.Fatalf("CancelOrder = %v", err)
		}
		if len(tw.cmds) != 1 {
			t.Errorf("commands = %+v", tw.cmds)
		}
	})

	t.Run("cancelling and cancelled replay idempotently, no writes", func(t *testing.T) {
		for _, status := range []string{"cancelling", "cancelled"} {
			tw := &stubTxWriter{}
			canc := &stubCancellations{}
			svc := NewOrderService(orderAt(status, 5), &MockTransactionManager{}, &stubStartRequests{}, &stubStartRequests{}, &stubProjection{}, tw, canc)
			out, err := svc.CancelOrder(context.Background(), "7", "42")
			if err != nil || !out.Replayed {
				t.Fatalf("%s: out=%+v err=%v, want replay", status, out, err)
			}
			if len(tw.cmds) != 0 || len(canc.armed) != 0 {
				t.Errorf("%s: replay must write nothing", status)
			}
		}
	})

	t.Run("pending, failed and manual_review refuse", func(t *testing.T) {
		for _, status := range []string{"pending", "failed", "manual_review"} {
			svc := NewOrderService(orderAt(status, 1), &MockTransactionManager{}, &stubStartRequests{}, &stubStartRequests{}, &stubProjection{}, &stubTxWriter{}, &stubCancellations{})
			if _, err := svc.CancelOrder(context.Background(), "7", "42"); !errors.Is(err, ErrOrderNotCancellable) {
				t.Errorf("%s: got %v, want ErrOrderNotCancellable", status, err)
			}
		}
	})

	t.Run("a lost CAS race reloads and answers from the truth", func(t *testing.T) {
		// The re-read sees the order already cancelling (another cancel won).
		reads := 0
		repo := &MockOrderRepository{findByIDFunc: func(_ context.Context, userID, id string) (*domain.Order, error) {
			reads++
			status := "confirmed"
			if reads > 1 {
				status = "cancelling"
			}
			return &domain.Order{ID: id, UserID: userID, Status: status, Version: 4}, nil
		}}
		tw := &stubTxWriter{err: domain.ErrConcurrencyConflict}
		svc := NewOrderService(repo, &MockTransactionManager{}, &stubStartRequests{}, &stubStartRequests{}, &stubProjection{}, tw, &stubCancellations{})
		out, err := svc.CancelOrder(context.Background(), "7", "42")
		if err != nil || !out.Replayed {
			t.Fatalf("out=%+v err=%v, want a replay answer", out, err)
		}
	})

	t.Run("missing orders are not found", func(t *testing.T) {
		repo := &MockOrderRepository{findByIDFunc: func(context.Context, string, string) (*domain.Order, error) {
			return nil, domain.ErrNotFound
		}}
		svc := NewOrderService(repo, &MockTransactionManager{}, &stubStartRequests{}, &stubStartRequests{}, &stubProjection{}, &stubTxWriter{}, &stubCancellations{})
		if _, err := svc.CancelOrder(context.Background(), "7", "42"); !errors.Is(err, ErrOrderNotFound) {
			t.Errorf("got %v, want ErrOrderNotFound", err)
		}
	})
}
