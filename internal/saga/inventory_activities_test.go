package saga

import (
	"context"
	"errors"
	"testing"

	"go.temporal.io/sdk/temporal"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"

	"github.com/duynhlab/pkg/grpcx"
	inventoryv1 "github.com/duynhlab/pkg/proto/inventory/v1"
	"google.golang.org/grpc/status"
	"time"
)

// stubInventoryClient embeds the generated client so only the RPCs under test
// need bodies; the rest are never called here.
type stubInventoryClient struct {
	inventoryv1.InventoryServiceClient
	reserveErr error
	releaseErr error
	commitErr  error

	gotReserve *inventoryv1.ReserveRequest
	gotRelease *inventoryv1.ReleaseRequest
	gotCommit  *inventoryv1.CommitRequest
}

func (s *stubInventoryClient) Reserve(_ context.Context, in *inventoryv1.ReserveRequest, _ ...grpc.CallOption) (*inventoryv1.ReserveResponse, error) {
	s.gotReserve = in
	if s.reserveErr != nil {
		return nil, s.reserveErr
	}
	return &inventoryv1.ReserveResponse{ReservationId: in.GetReservationId(), Status: inventoryv1.ReservationStatus_RESERVATION_STATUS_RESERVED}, nil
}

func (s *stubInventoryClient) Release(_ context.Context, in *inventoryv1.ReleaseRequest, _ ...grpc.CallOption) (*inventoryv1.ReleaseResponse, error) {
	s.gotRelease = in
	if s.releaseErr != nil {
		return nil, s.releaseErr
	}
	return &inventoryv1.ReleaseResponse{ReservationId: in.GetReservationId(), Status: inventoryv1.ReservationStatus_RESERVATION_STATUS_RELEASED}, nil
}

func (s *stubInventoryClient) Commit(_ context.Context, in *inventoryv1.CommitRequest, _ ...grpc.CallOption) (*inventoryv1.CommitResponse, error) {
	s.gotCommit = in
	if s.commitErr != nil {
		return nil, s.commitErr
	}
	return &inventoryv1.CommitResponse{ReservationId: in.GetReservationId(), Status: inventoryv1.ReservationStatus_RESERVATION_STATUS_COMMITTED}, nil
}

// reasonErr builds a platform error carrying a google.rpc.ErrorInfo reason,
// exactly as inventory-service emits them.
func reasonErr(c codes.Code, reason string) error {
	return grpcx.ErrorWithReason(c, reason, "test", nil)
}

func isNonRetryableWithType(t *testing.T, err error, wantType string) {
	t.Helper()
	var appErr *temporal.ApplicationError
	if !errors.As(err, &appErr) {
		t.Fatalf("want *temporal.ApplicationError, got %T: %v", err, err)
	}
	if !appErr.NonRetryable() {
		t.Errorf("error should be non-retryable: %v", appErr)
	}
	if appErr.Type() != wantType {
		t.Errorf("ApplicationError type = %q, want %q", appErr.Type(), wantType)
	}
}

func TestClassifyInventoryErr(t *testing.T) {
	tests := []struct {
		name         string
		err          error
		wantNil      bool
		nonRetryable bool
		wantType     string
	}{
		{"nil passes through", nil, true, false, ""},
		{"INSUFFICIENT_STOCK is business", reasonErr(codes.FailedPrecondition, grpcx.ReasonInsufficientStock), false, true, "INSUFFICIENT_STOCK"},
		{"IDEMPOTENCY_CONFLICT is business", reasonErr(codes.AlreadyExists, grpcx.ReasonIdempotencyConflict), false, true, "IDEMPOTENCY_CONFLICT"},
		{"INVALID_TRANSITION is business", reasonErr(codes.FailedPrecondition, grpcx.ReasonInvalidTransition), false, true, "INVALID_TRANSITION"},
		{"CONCURRENCY_CONFLICT is transient", reasonErr(codes.Aborted, grpcx.ReasonConcurrencyConflict), false, false, ""},
		{"DEPENDENCY_UNAVAILABLE is transient", reasonErr(codes.Unavailable, grpcx.ReasonDependencyUnavailable), false, false, ""},
		{"bare Unavailable (no reason) is transient", reasonErr(codes.Unavailable, ""), false, false, ""},
		{"bare InvalidArgument (no reason) is business, typed by code", reasonErr(codes.InvalidArgument, ""), false, true, "InvalidArgument"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyInventoryErr("op", "42", tt.err)
			if tt.wantNil {
				if got != nil {
					t.Fatalf("want nil, got %v", got)
				}
				return
			}
			if got == nil {
				t.Fatal("want error, got nil")
			}
			if tt.nonRetryable {
				isNonRetryableWithType(t, got, tt.wantType)
			} else {
				var appErr *temporal.ApplicationError
				if errors.As(got, &appErr) && appErr.NonRetryable() {
					t.Errorf("transient error must stay retryable, got non-retryable %v", appErr)
				}
			}
		})
	}
}

func TestReserveInventory(t *testing.T) {
	items := []ReserveItem{{ProductID: "1", Quantity: 2}, {ProductID: "5", Quantity: 1}}

	t.Run("maps items and ids", func(t *testing.T) {
		stub := &stubInventoryClient{}
		a := &Activities{Inventory: stub}
		if err := a.ReserveInventory(context.Background(), "42", items); err != nil {
			t.Fatalf("ReserveInventory: %v", err)
		}
		req := stub.gotReserve
		if req.GetReservationId() != "42" || req.GetOrderId() != "42" {
			t.Errorf("ids = %s/%s, want 42/42", req.GetReservationId(), req.GetOrderId())
		}
		if len(req.GetItems()) != 2 || req.GetItems()[0].GetSkuId() != "1" || req.GetItems()[0].GetQuantity() != 2 {
			t.Errorf("items mapped wrong: %+v", req.GetItems())
		}
		if req.GetRequestHash() != "" {
			t.Errorf("request_hash must be empty (server computes it), got %q", req.GetRequestHash())
		}
	})

	t.Run("insufficient stock is non-retryable", func(t *testing.T) {
		stub := &stubInventoryClient{reserveErr: reasonErr(codes.FailedPrecondition, grpcx.ReasonInsufficientStock)}
		a := &Activities{Inventory: stub}
		err := a.ReserveInventory(context.Background(), "42", items)
		isNonRetryableWithType(t, err, "INSUFFICIENT_STOCK")
	})

	t.Run("transient failure stays retryable", func(t *testing.T) {
		stub := &stubInventoryClient{reserveErr: reasonErr(codes.Unavailable, grpcx.ReasonDependencyUnavailable)}
		a := &Activities{Inventory: stub}
		err := a.ReserveInventory(context.Background(), "42", items)
		if err == nil {
			t.Fatal("want error")
		}
		var appErr *temporal.ApplicationError
		if errors.As(err, &appErr) && appErr.NonRetryable() {
			t.Errorf("transient reserve failure must be retryable, got %v", appErr)
		}
	})
}

func TestReleaseInventory(t *testing.T) {
	t.Run("passes bounded reason", func(t *testing.T) {
		stub := &stubInventoryClient{}
		a := &Activities{Inventory: stub}
		if err := a.ReleaseInventory(context.Background(), "42", ReleaseReasonCaptureFailed); err != nil {
			t.Fatalf("ReleaseInventory: %v", err)
		}
		if stub.gotRelease.GetReason() != "SAGA_CAPTURE_FAILED" {
			t.Errorf("reason = %q", stub.gotRelease.GetReason())
		}
	})

	t.Run("release after commit is a non-retryable breach", func(t *testing.T) {
		stub := &stubInventoryClient{releaseErr: reasonErr(codes.FailedPrecondition, grpcx.ReasonInvalidTransition)}
		a := &Activities{Inventory: stub}
		err := a.ReleaseInventory(context.Background(), "42", ReleaseReasonConfirmFailed)
		isNonRetryableWithType(t, err, "INVALID_TRANSITION")
	})
}

func TestCommitInventory(t *testing.T) {
	t.Run("commit converges", func(t *testing.T) {
		stub := &stubInventoryClient{}
		a := &Activities{Inventory: stub}
		if err := a.CommitInventory(context.Background(), "42"); err != nil {
			t.Fatalf("CommitInventory: %v", err)
		}
		if stub.gotCommit.GetReservationId() != "42" {
			t.Errorf("reservation id = %q", stub.gotCommit.GetReservationId())
		}
	})

	t.Run("transient failure stays retryable (unbounded retry converges)", func(t *testing.T) {
		stub := &stubInventoryClient{commitErr: reasonErr(codes.Unavailable, grpcx.ReasonDependencyUnavailable)}
		a := &Activities{Inventory: stub}
		err := a.CommitInventory(context.Background(), "42")
		if err == nil {
			t.Fatal("want error")
		}
		var appErr *temporal.ApplicationError
		if errors.As(err, &appErr) && appErr.NonRetryable() {
			t.Errorf("transient commit failure must be retryable, got %v", appErr)
		}
	})

	t.Run("commit after release is a non-retryable invariant breach", func(t *testing.T) {
		stub := &stubInventoryClient{commitErr: reasonErr(codes.FailedPrecondition, grpcx.ReasonInvalidTransition)}
		a := &Activities{Inventory: stub}
		err := a.CommitInventory(context.Background(), "42")
		isNonRetryableWithType(t, err, "INVALID_TRANSITION")
	})
}

func TestClassifyInventoryErr_CanceledStaysRetryable(t *testing.T) {
	// A canceled RPC (worker shutdown mid-call) must never become a permanent
	// failure — CommitInventory's convergence guarantee depends on it.
	got := classifyInventoryErr("commit inventory", "42", reasonErr(codes.Canceled, ""))
	if got == nil {
		t.Fatal("want error")
	}
	var appErr *temporal.ApplicationError
	if errors.As(got, &appErr) && appErr.NonRetryable() {
		t.Fatalf("Canceled must stay retryable, got non-retryable %v", appErr)
	}
}

func TestReserveInventory_BusinessRejectionCountsAsRejectedNotError(t *testing.T) {
	// IDEMPOTENCY_CONFLICT is deterministic — it must not pollute the transient
	// result=error bucket (Codex review finding).
	stub := &stubInventoryClient{reserveErr: reasonErr(codes.AlreadyExists, grpcx.ReasonIdempotencyConflict)}
	a := &Activities{Inventory: stub}
	err := a.ReserveInventory(context.Background(), "42", []ReserveItem{{ProductID: "1", Quantity: 1}})
	isNonRetryableWithType(t, err, "IDEMPOTENCY_CONFLICT")
	// Metric bucket assertion is structural: isNonRetryableApp drives the
	// switch; verify the helper agrees with the classification.
	if !isNonRetryableApp(err) {
		t.Error("classified business rejection must be non-retryable app error")
	}
}

func TestInventoryActivities_NilClientFailsFast(t *testing.T) {
	a := &Activities{} // Inventory nil
	if err := a.ReserveInventory(context.Background(), "42", nil); err == nil {
		t.Error("reserve: want typed error on nil client, not panic")
	}
	if err := a.ReleaseInventory(context.Background(), "42", ReleaseReasonConfirmFailed); err == nil {
		t.Error("release: want typed error on nil client")
	}
	if err := a.CommitInventory(context.Background(), "42"); err == nil {
		t.Error("commit: want typed error on nil client")
	}
}

// The GameDay fault hook: a non-zero CommitPause holds CommitInventory AFTER
// the Commit RPC succeeded (the G2b window), is inert at zero, and respects
// context cancellation so a worker shutdown never hangs on the pause.
func TestCommitInventory_FaultPause(t *testing.T) {
	t.Run("zero pause adds no delay", func(t *testing.T) {
		a := &Activities{Inventory: &stubInventoryClient{}}
		start := time.Now()
		if err := a.CommitInventory(context.Background(), "42"); err != nil {
			t.Fatalf("commit: %v", err)
		}
		if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
			t.Fatalf("zero pause took %v", elapsed)
		}
	})
	t.Run("pause holds after a successful commit", func(t *testing.T) {
		stub := &stubInventoryClient{}
		a := &Activities{Inventory: stub, CommitPause: 150 * time.Millisecond}
		start := time.Now()
		if err := a.CommitInventory(context.Background(), "42"); err != nil {
			t.Fatalf("commit: %v", err)
		}
		if elapsed := time.Since(start); elapsed < 150*time.Millisecond {
			t.Fatalf("pause not applied: %v", elapsed)
		}
		if stub.gotCommit == nil {
			t.Fatal("the pause must come AFTER the commit RPC, not instead of it")
		}
	})
	t.Run("pause does not delay a FAILED commit", func(t *testing.T) {
		a := &Activities{
			Inventory:   &stubInventoryClient{commitErr: status.Error(codes.Unavailable, "down")},
			CommitPause: time.Hour,
		}
		start := time.Now()
		if err := a.CommitInventory(context.Background(), "42"); err == nil {
			t.Fatal("expected the commit error through")
		}
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Fatalf("error path waited %v — the hook must only stretch the success window", elapsed)
		}
	})
	t.Run("cancellation cuts the pause short", func(t *testing.T) {
		a := &Activities{Inventory: &stubInventoryClient{}, CommitPause: time.Hour}
		ctx, cancel := context.WithCancel(context.Background())
		go func() { time.Sleep(50 * time.Millisecond); cancel() }()
		start := time.Now()
		err := a.CommitInventory(ctx, "42")
		if err == nil || time.Since(start) > 5*time.Second {
			t.Fatalf("want a prompt ctx error, got err=%v after %v", err, time.Since(start))
		}
	})
}
