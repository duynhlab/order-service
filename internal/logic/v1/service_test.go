package v1

import (
	"context"
	"errors"
	"testing"

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
	findByUserIDFunc         func(ctx context.Context, userID string) ([]domain.Order, error)
	findByIdempotencyKeyFunc func(ctx context.Context, userID, key string) (*domain.Order, error)
	createFunc               func(ctx context.Context, order *domain.Order) error
	updateStatusFunc         func(ctx context.Context, id, status string) error
	createWithTxFunc         func(ctx context.Context, tx domain.Transaction, order *domain.Order) error
}

func (m *MockOrderRepository) FindByID(ctx context.Context, userID, id string) (*domain.Order, error) {
	if m.findByIDFunc != nil {
		return m.findByIDFunc(ctx, userID, id)
	}
	return nil, nil
}

func (m *MockOrderRepository) FindByUserID(ctx context.Context, userID string) ([]domain.Order, error) {
	if m.findByUserIDFunc != nil {
		return m.findByUserIDFunc(ctx, userID)
	}
	return nil, nil
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

func (m *MockOrderRepository) UpdateStatus(ctx context.Context, id, status string) error {
	if m.updateStatusFunc != nil {
		return m.updateStatusFunc(ctx, id, status)
	}
	return nil
}

func (m *MockOrderRepository) CreateWithTx(ctx context.Context, tx domain.Transaction, order *domain.Order) error {
	if m.createWithTxFunc != nil {
		return m.createWithTxFunc(ctx, tx, order)
	}
	return nil
}

func TestCreateOrder(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name         string
		req          domain.CreateOrderRequest
		repo         *MockOrderRepository
		txMgr        *MockTransactionManager
		wantSubtotal float64
		wantErr      error // sentinel to match with errors.Is; nil means success
		wantCommit   bool  // whether the tx should have been committed
	}{
		{
			name: "success path enriches items and commits",
			req: domain.CreateOrderRequest{
				UserID:         "user1",
				IdempotencyKey: "key-1",
				Items: []domain.OrderItem{
					{ProductID: "p1", Quantity: 2, Price: 10.0}, // 20.0
					{ProductID: "p2", Quantity: 1, Price: 5.0},  // 5.0
				},
			},
			repo:         &MockOrderRepository{},
			txMgr:        &MockTransactionManager{},
			wantSubtotal: 25.0,
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
				Items:  []domain.OrderItem{{ProductID: "p1", Quantity: 0, Price: 10.0}},
			},
			repo:    &MockOrderRepository{},
			txMgr:   &MockTransactionManager{},
			wantErr: ErrInvalidOrder,
		},
		{
			name: "negative price is rejected",
			req: domain.CreateOrderRequest{
				UserID: "user1",
				Items:  []domain.OrderItem{{ProductID: "p1", Quantity: 1, Price: -1.0}},
			},
			repo:    &MockOrderRepository{},
			txMgr:   &MockTransactionManager{},
			wantErr: ErrInvalidOrder,
		},
		{
			name: "empty product id is rejected",
			req: domain.CreateOrderRequest{
				UserID: "user1",
				Items:  []domain.OrderItem{{ProductID: "", Quantity: 1, Price: 10.0}},
			},
			repo:    &MockOrderRepository{},
			txMgr:   &MockTransactionManager{},
			wantErr: ErrInvalidOrder,
		},
		{
			name: "Begin failure propagates",
			req: domain.CreateOrderRequest{
				UserID: "user1",
				Items:  []domain.OrderItem{{ProductID: "p1", Quantity: 1, Price: 10.0}},
			},
			repo:    &MockOrderRepository{},
			txMgr:   &MockTransactionManager{beginErr: errBoom},
			wantErr: errBoom,
		},
		{
			name: "CreateWithTx failure rolls back",
			req: domain.CreateOrderRequest{
				UserID: "user1",
				Items:  []domain.OrderItem{{ProductID: "p1", Quantity: 1, Price: 10.0}},
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
				Items:  []domain.OrderItem{{ProductID: "p1", Quantity: 1, Price: 10.0}},
			},
			repo:    &MockOrderRepository{},
			txMgr:   &MockTransactionManager{tx: &MockTransaction{commitErr: errBoom}},
			wantErr: errBoom,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			service := NewOrderService(tt.repo, tt.txMgr)

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
			if want := tt.wantSubtotal + 5.00; order.Total != want {
				t.Errorf("CreateOrder() total = %v, want %v", order.Total, want)
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

func TestCreateOrder_ProductNameFallback(t *testing.T) {
	ctx := context.Background()
	var captured *domain.Order
	repo := &MockOrderRepository{
		createWithTxFunc: func(ctx context.Context, tx domain.Transaction, order *domain.Order) error {
			captured = order
			return nil
		},
	}
	service := NewOrderService(repo, &MockTransactionManager{})

	_, err := service.CreateOrder(ctx, domain.CreateOrderRequest{
		UserID: "user1",
		Items: []domain.OrderItem{
			{ProductID: "p1", Quantity: 1, Price: 10.0},                        // no name -> fallback
			{ProductID: "p2", Quantity: 1, Price: 10.0, ProductName: "Widget"}, // keeps name
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
			service := NewOrderService(tt.repo, &MockTransactionManager{})
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
			findByUserIDFunc: func(ctx context.Context, userID string) ([]domain.Order, error) {
				return want, nil
			},
		}
		service := NewOrderService(repo, &MockTransactionManager{})
		got, err := service.ListOrders(ctx, "user1")
		if err != nil {
			t.Fatalf("ListOrders() unexpected error = %v", err)
		}
		if len(got) != len(want) {
			t.Errorf("ListOrders() len = %d, want %d", len(got), len(want))
		}
	})

	t.Run("propagates repository error", func(t *testing.T) {
		repo := &MockOrderRepository{
			findByUserIDFunc: func(ctx context.Context, userID string) ([]domain.Order, error) {
				return nil, errBoom
			},
		}
		service := NewOrderService(repo, &MockTransactionManager{})
		if _, err := service.ListOrders(ctx, "user1"); !errors.Is(err, errBoom) {
			t.Errorf("ListOrders() error = %v, want %v", err, errBoom)
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
			service := NewOrderService(tt.repo, &MockTransactionManager{})
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

func TestUpdateOrderStatus(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name    string
		repo    *MockOrderRepository
		wantErr error
	}{
		{
			name: "success",
			repo: &MockOrderRepository{},
		},
		{
			name: "not found maps to ErrOrderNotFound",
			repo: &MockOrderRepository{
				updateStatusFunc: func(ctx context.Context, id, status string) error {
					return domain.ErrNotFound
				},
			},
			wantErr: ErrOrderNotFound,
		},
		{
			name: "infrastructure error propagates",
			repo: &MockOrderRepository{
				updateStatusFunc: func(ctx context.Context, id, status string) error {
					return errBoom
				},
			},
			wantErr: errBoom,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			service := NewOrderService(tt.repo, &MockTransactionManager{})
			err := service.UpdateOrderStatus(ctx, "order-1", "shipped")

			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("UpdateOrderStatus() error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("UpdateOrderStatus() unexpected error = %v", err)
			}
		})
	}
}
