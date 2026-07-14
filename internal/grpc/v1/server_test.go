package v1

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"

	"github.com/duynhlab/order-service/internal/fulfillment"
	"github.com/duynhlab/order-service/internal/saga"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	orderv1 "github.com/duynhlab/pkg/proto/order/v1"

	"github.com/duynhlab/order-service/internal/core/domain"
)

// --- doubles ---

type fakeOrderCreator struct {
	existing    *domain.Order // GetByIdempotencyKey hit
	lookupErr   error
	created     *domain.Order
	createErr   error
	gotReq      *domain.CreateOrderRequest
	createCalls int
}

func (f *fakeOrderCreator) CreateOrder(_ context.Context, req domain.CreateOrderRequest) (*domain.Order, error) {
	f.createCalls++
	f.gotReq = &req
	if f.createErr != nil {
		return nil, f.createErr
	}
	if f.created != nil {
		return f.created, nil
	}
	return &domain.Order{ID: "42", UserID: req.UserID, Status: "pending", Total: 6498,
		Items: []domain.OrderItem{{ProductID: "1", Quantity: 2, Price: 2999}}}, nil
}

func (f *fakeOrderCreator) GetByIdempotencyKey(_ context.Context, _, _ string) (*domain.Order, error) {
	return f.existing, f.lookupErr
}

type fakeStarter struct {
	calls    int
	err      error
	gotOpts  client.StartWorkflowOptions
	gotInput []any
}

func (f *fakeStarter) ExecuteWorkflow(_ context.Context, options client.StartWorkflowOptions, _ any, args ...any) (client.WorkflowRun, error) {
	f.calls++
	f.gotOpts = options
	f.gotInput = args
	return nil, f.err
}

func validReq() *orderv1.CreateOrderRequest {
	return &orderv1.CreateOrderRequest{
		UserId: "7",
		Items: []*orderv1.OrderItem{
			{ProductId: "1", ProductName: "Wireless Mouse", Quantity: 2, UnitPriceMinor: 2999},
		},
		PaymentMethod:  "tok_visa_ok",
		IdempotencyKey: "checkout:sess-1:key-1",
	}
}

func newServer(svc *fakeOrderCreator, st *fakeStarter) *Server {
	return NewServer(svc, st, "order-fulfillment")
}

// --- happy path ---

func TestCreateOrder_FreshOrderStartsSagaWithDedup(t *testing.T) {
	svc := &fakeOrderCreator{}
	st := &fakeStarter{}

	resp, err := newServer(svc, st).CreateOrder(context.Background(), validReq())
	if err != nil {
		t.Fatalf("CreateOrder() error = %v", err)
	}
	if resp.OrderId != "42" || resp.Status != "pending" {
		t.Errorf("resp = %+v, want order 42 pending", resp)
	}
	if svc.gotReq.IdempotencyKey != "checkout:sess-1:key-1" || svc.gotReq.UserID != "7" {
		t.Errorf("logic req = %+v, want key + user threaded", svc.gotReq)
	}
	if svc.gotReq.Items[0].Price != 2999 {
		t.Errorf("item price = %d, want 2999 minor units untouched", svc.gotReq.Items[0].Price)
	}
	if st.calls != 1 {
		t.Fatalf("saga starts = %d, want 1", st.calls)
	}
	if st.gotOpts.ID != "order-fulfillment-42" || st.gotOpts.TaskQueue != "order-fulfillment" {
		t.Errorf("start opts = %+v, want dedup id order-fulfillment-42", st.gotOpts)
	}
	if st.gotOpts.WorkflowIDReusePolicy != enumspb.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE {
		t.Errorf("reuse policy = %v, want REJECT_DUPLICATE", st.gotOpts.WorkflowIDReusePolicy)
	}
}

// --- idempotency ---

func TestCreateOrder_ReplayHitReturnsExistingWithoutSecondCreate(t *testing.T) {
	svc := &fakeOrderCreator{existing: &domain.Order{ID: "42", Status: "pending",
		Items: []domain.OrderItem{{ProductID: "1", Quantity: 2, Price: 2999}}, Total: 5998}}
	st := &fakeStarter{err: &serviceerror.WorkflowExecutionAlreadyStarted{}}

	resp, err := newServer(svc, st).CreateOrder(context.Background(), validReq())
	if err != nil {
		t.Fatalf("replay error = %v, want success (AlreadyStarted = saga already happened)", err)
	}
	if resp.OrderId != "42" || svc.createCalls != 0 {
		t.Errorf("resp=%+v createCalls=%d, want existing order and no second create", resp, svc.createCalls)
	}
	if st.calls != 1 {
		t.Errorf("kickoff attempts = %d, want 1 (idempotent heal attempt on replay)", st.calls)
	}
}

func TestCreateOrder_ReplayOfZombiePendingOrderHealsSaga(t *testing.T) {
	// Crash-recovery: order row exists (pending) but no saga ever started.
	// The replay's kickoff attempt must actually start it (fresh start, no error).
	svc := &fakeOrderCreator{existing: &domain.Order{ID: "42", Status: "pending",
		Items: []domain.OrderItem{{ProductID: "1", Quantity: 2, Price: 2999}}, Total: 5998}}
	st := &fakeStarter{}

	if _, err := newServer(svc, st).CreateOrder(context.Background(), validReq()); err != nil {
		t.Fatalf("heal error = %v", err)
	}
	if st.calls != 1 {
		t.Errorf("kickoff attempts = %d, want 1 (zombie healed)", st.calls)
	}
}

func TestCreateOrder_CompletedOrderReplayNeverRestartsSaga(t *testing.T) {
	// Status gate: an old idempotency key replayed after the Temporal
	// retention window must NOT re-run the saga on a confirmed order
	// (double-charge guard — doubt-cycle finding).
	for _, status := range []string{"confirmed", "failed"} {
		svc := &fakeOrderCreator{existing: &domain.Order{ID: "42", Status: status,
			Items: []domain.OrderItem{{ProductID: "1", Quantity: 2, Price: 2999}}, Total: 5998}}
		st := &fakeStarter{}

		resp, err := newServer(svc, st).CreateOrder(context.Background(), validReq())
		if err != nil {
			t.Fatalf("%s replay error = %v", status, err)
		}
		if resp.Status != status {
			t.Errorf("status = %s, want %s echoed", resp.Status, status)
		}
		if st.calls != 0 {
			t.Errorf("%s: kickoff attempts = %d, want 0 (status gate)", status, st.calls)
		}
	}
}

func TestCreateOrder_LookupErrorIsOpaqueInternal(t *testing.T) {
	svc := &fakeOrderCreator{lookupErr: errors.New("pq: connection to 10.0.0.9 failed")}

	_, err := newServer(svc, &fakeStarter{}).CreateOrder(context.Background(), validReq())
	st := status.Convert(err)
	if st.Code() != codes.Internal || strings.Contains(st.Message(), "10.0.0.9") {
		t.Errorf("got (%v, %q), want opaque Internal (no error-as-miss)", st.Code(), st.Message())
	}
}

// --- kickoff failure semantics ---

func TestCreateOrder_KickoffFailureIsUnavailableSoCallerRetries(t *testing.T) {
	// Temporal down on the fresh path: succeeding silently would strand a
	// zombie forever (the machine caller never retries success). Unavailable
	// tells checkout's idempotent retry to come back — the replay path heals.
	svc := &fakeOrderCreator{}
	st := &fakeStarter{err: errors.New("dial temporal:7233: connection refused")}

	_, err := newServer(svc, st).CreateOrder(context.Background(), validReq())
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("code = %v, want Unavailable", status.Code(err))
	}
	if strings.Contains(status.Convert(err).Message(), "7233") {
		t.Errorf("message leaks internals: %q", status.Convert(err).Message())
	}
}

// --- validation ---

func TestCreateOrder_ValidationRejects(t *testing.T) {
	long := strings.Repeat("x", 256)
	pan := "tok_4111111111111111"
	cases := map[string]func(r *orderv1.CreateOrderRequest){
		"empty idempotency_key":  func(r *orderv1.CreateOrderRequest) { r.IdempotencyKey = "" },
		"oversized key":          func(r *orderv1.CreateOrderRequest) { r.IdempotencyKey = strings.Repeat("k", 201) },
		"empty user_id":          func(r *orderv1.CreateOrderRequest) { r.UserId = "" },
		"non-numeric user_id":    func(r *orderv1.CreateOrderRequest) { r.UserId = "alice" },
		"no items":               func(r *orderv1.CreateOrderRequest) { r.Items = nil },
		"non-numeric product_id": func(r *orderv1.CreateOrderRequest) { r.Items[0].ProductId = "abc" },
		"product_id > int32":     func(r *orderv1.CreateOrderRequest) { r.Items[0].ProductId = "4111111111111111" },
		"oversized product_name": func(r *orderv1.CreateOrderRequest) { r.Items[0].ProductName = long },
		"zero quantity":          func(r *orderv1.CreateOrderRequest) { r.Items[0].Quantity = 0 },
		"huge quantity":          func(r *orderv1.CreateOrderRequest) { r.Items[0].Quantity = 10001 },
		"negative price":         func(r *orderv1.CreateOrderRequest) { r.Items[0].UnitPriceMinor = -1 },
		"absurd price":           func(r *orderv1.CreateOrderRequest) { r.Items[0].UnitPriceMinor = 1_000_000_000_001_00 },
		"PAN payment_method":     func(r *orderv1.CreateOrderRequest) { r.PaymentMethod = pan },
		"PAN in product_name":    func(r *orderv1.CreateOrderRequest) { r.Items[0].ProductName = "4111 1111 1111 1111" },
		"key bad alphabet":       func(r *orderv1.CreateOrderRequest) { r.IdempotencyKey = "key with spaces\n" },
	}
	for name, mutate := range cases {
		req := validReq()
		mutate(req)
		svc := &fakeOrderCreator{}
		_, err := newServer(svc, &fakeStarter{}).CreateOrder(context.Background(), req)
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: code = %v, want InvalidArgument", name, status.Code(err))
		}
		if svc.createCalls != 0 {
			t.Errorf("%s: create called despite invalid input", name)
		}
		if msg := status.Convert(err).Message(); strings.Contains(msg, pan) || strings.Contains(msg, long) {
			t.Errorf("%s: error message echoes input: %q", name, msg)
		}
	}
}

func TestCreateOrder_NilTemporalIsUnavailableNeverSuccess(t *testing.T) {
	// The doc-comment contract: answering success with no kickoff would
	// strand a permanent pending zombie (machine callers don't retry 200s).
	for name, svc := range map[string]*fakeOrderCreator{
		"fresh": {},
		"pending replay": {existing: &domain.Order{ID: "42", Status: "pending",
			Items: []domain.OrderItem{{ProductID: "1", Quantity: 2, Price: 2999}}, Total: 5998}},
	} {
		_, err := NewServer(svc, nil, "order-fulfillment").CreateOrder(context.Background(), validReq())
		if status.Code(err) != codes.Unavailable {
			t.Errorf("%s: code = %v, want Unavailable when Temporal client is nil", name, status.Code(err))
		}
	}
}

func TestCreateOrder_ZombieHealPassesWirePaymentMethod(t *testing.T) {
	svc := &fakeOrderCreator{existing: &domain.Order{ID: "42", Status: "pending",
		Items: []domain.OrderItem{{ProductID: "1", Quantity: 2, Price: 2999}}, Total: 5998}}
	st := &fakeStarter{}

	if _, err := newServer(svc, st).CreateOrder(context.Background(), validReq()); err != nil {
		t.Fatalf("heal: %v", err)
	}
	input, ok := st.gotInput[0].(saga.OrderFulfillmentInput)
	if !ok || input.PaymentMethod != "tok_visa_ok" {
		t.Errorf("saga input = %+v, want the WIRE payment method on the heal path", st.gotInput)
	}
}

func TestCreateOrder_KeyReuseWithDifferentBasketIsFailedPrecondition(t *testing.T) {
	// Fingerprint guard: same key, different items — a caller bug, never a
	// replay (would silently bind the wrong basket to the stored order).
	svc := &fakeOrderCreator{existing: &domain.Order{ID: "42", Status: "confirmed",
		Items: []domain.OrderItem{{ProductID: "1", Quantity: 1, Price: 9999}}}}

	_, err := newServer(svc, &fakeStarter{}).CreateOrder(context.Background(), validReq())
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("code = %v, want FailedPrecondition on payload mismatch", status.Code(err))
	}
}

func TestCreateOrder_TooManyItemsRejected(t *testing.T) {
	req := validReq()
	req.Items = nil
	for i := 0; i < 201; i++ {
		req.Items = append(req.Items, &orderv1.OrderItem{ProductId: "1", Quantity: 1, UnitPriceMinor: 1})
	}
	if _, err := newServer(&fakeOrderCreator{}, &fakeStarter{}).CreateOrder(context.Background(), req); status.Code(err) != codes.InvalidArgument {
		t.Errorf("code = %v, want InvalidArgument (item cap)", status.Code(err))
	}
}

func TestCreateOrder_EmptyPaymentMethodAllowed(t *testing.T) {
	req := validReq()
	req.PaymentMethod = "" // demo-token fallback downstream, same as REST
	if _, err := newServer(&fakeOrderCreator{}, &fakeStarter{}).CreateOrder(context.Background(), req); err != nil {
		t.Errorf("empty payment_method should be allowed (demo fallback): %v", err)
	}
}

func TestCreateOrder_CallerTotalsComposeTheChargedTotal(t *testing.T) {
	// P4 (and the P3 gap it closed): the saga charges order.Total, so the
	// caller's quoted fee, tax, and discount must land in it — never the
	// legacy $5 demo shipping.
	svc := &fakeOrderCreator{}
	st := &fakeStarter{}
	req := validReq() // 2 × 2999 = 5998 subtotal
	req.ShippingFeeMinor = 300
	req.TaxMinor = 504
	req.DiscountMinor = 600

	if _, err := newServer(svc, st).CreateOrder(context.Background(), req); err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	if !svc.gotReq.TotalsProvided {
		t.Fatal("gRPC adapter must mark the totals as caller-provided")
	}
	if svc.gotReq.ShippingFeeMinor != 300 || svc.gotReq.TaxMinor != 504 || svc.gotReq.DiscountMinor != 600 {
		t.Errorf("components = %+v, want fee/tax/discount threaded", svc.gotReq)
	}
}

func TestCreateOrder_TotalsBoundsRejected(t *testing.T) {
	for name, mutate := range map[string]func(r *orderv1.CreateOrderRequest){
		"negative fee":      func(r *orderv1.CreateOrderRequest) { r.ShippingFeeMinor = -1 },
		"negative tax":      func(r *orderv1.CreateOrderRequest) { r.TaxMinor = -1 },
		"negative discount": func(r *orderv1.CreateOrderRequest) { r.DiscountMinor = -1 },
		"discount > total":  func(r *orderv1.CreateOrderRequest) { r.DiscountMinor = 1_000_000 },
		"fee over cap":      func(r *orderv1.CreateOrderRequest) { r.ShippingFeeMinor = 2_000_000_000_000 },
		"tax over cap":      func(r *orderv1.CreateOrderRequest) { r.TaxMinor = 2_000_000_000_000 },
	} {
		req := validReq()
		mutate(req)
		_, err := newServer(&fakeOrderCreator{}, &fakeStarter{}).CreateOrder(context.Background(), req)
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: code = %v, want InvalidArgument", name, status.Code(err))
		}
	}
}

func TestCreateOrder_KeyReuseWithDifferentDiscountRejected(t *testing.T) {
	svc := &fakeOrderCreator{existing: &domain.Order{ID: "42", Status: "confirmed",
		Items: []domain.OrderItem{{ProductID: "1", Quantity: 2, Price: 2999}}, Total: 5998}}
	req := validReq()
	req.DiscountMinor = 500 // same basket, different composed total

	_, err := newServer(svc, &fakeStarter{}).CreateOrder(context.Background(), req)
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("code = %v, want FailedPrecondition (total fingerprint)", status.Code(err))
	}
}

// The BUGS-6 zombie regression test: the same server instance must go from
// Unavailable (Temporal absent) to starting the saga once the lazy client's
// background redial connects — no pod restart.
func TestCreateOrder_LazyStarterHealsWithoutRestart(t *testing.T) {
	svc := &fakeOrderCreator{}
	fails := atomic.Int32{}
	fails.Store(1) // first dial fails, retry succeeds
	dial := func() (client.Client, error) {
		if fails.Add(-1) >= 0 {
			return nil, errors.New("temporal not up yet")
		}
		return temporalStub{}, nil
	}
	lz := fulfillment.NewLazy(dial, 10*time.Millisecond, zap.NewNop())
	defer lz.Close()
	srv := NewServer(svc, lz, "order-fulfillment")

	if _, err := srv.CreateOrder(context.Background(), validReq()); status.Code(err) != codes.Unavailable {
		t.Fatalf("before redial: code = %v, want Unavailable", status.Code(err))
	}

	deadline := time.After(3 * time.Second)
	for !lz.TemporalReady() {
		select {
		case <-deadline:
			t.Fatal("lazy starter never connected")
		case <-time.After(5 * time.Millisecond):
		}
	}
	resp, err := srv.CreateOrder(context.Background(), validReq())
	if err != nil {
		t.Fatalf("after redial: %v", err)
	}
	if resp.GetStatus() != "pending" {
		t.Fatalf("status = %q, want pending", resp.GetStatus())
	}
}

// temporalStub satisfies client.Client for the lazy-heal test; only
// ExecuteWorkflow and Close are reached.
type temporalStub struct{ client.Client }

func (temporalStub) ExecuteWorkflow(ctx context.Context, options client.StartWorkflowOptions, workflow any, args ...any) (client.WorkflowRun, error) {
	return nil, nil
}
func (temporalStub) Close() {}
