package v1

import (
	"context"
	"testing"

	paymentv1 "github.com/duynhlab/pkg/proto/payment/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// stubPaymentSvcClient fakes paymentv1.PaymentServiceClient so the gRPC mapping
// can be tested without a server. Only GetPayment is used by the fetcher.
type stubPaymentSvcClient struct {
	paymentv1.PaymentServiceClient
	resp       *paymentv1.GetPaymentResponse
	err        error
	gotOrderID int64
}

func (s *stubPaymentSvcClient) GetPayment(_ context.Context, in *paymentv1.GetPaymentRequest, _ ...grpc.CallOption) (*paymentv1.GetPaymentResponse, error) {
	s.gotOrderID = in.GetOrderId()
	return s.resp, s.err
}

func TestNewPaymentGRPCClient(t *testing.T) {
	if NewPaymentGRPCClient(nil) == nil {
		t.Fatal("NewPaymentGRPCClient returned nil")
	}
}

func TestPaymentGRPCClient_GetPaymentByOrderID(t *testing.T) {
	t.Run("captured maps to dollars", func(t *testing.T) {
		stub := &stubPaymentSvcClient{resp: &paymentv1.GetPaymentResponse{Payment: &paymentv1.Payment{
			Status: "captured", AmountMinor: 2550, Currency: "USD"}}}
		got, err := (&PaymentGRPCClient{client: stub}).GetPaymentByOrderID(context.Background(), 42)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if stub.gotOrderID != 42 {
			t.Fatalf("must forward order id 42, got %d", stub.gotOrderID)
		}
		if got.Status != "captured" || got.Amount != 25.50 || got.Currency != "USD" || got.Refunded != 0 {
			t.Fatalf("info = %+v", got)
		}
	})
	t.Run("partial refund derives partially_refunded", func(t *testing.T) {
		stub := &stubPaymentSvcClient{resp: &paymentv1.GetPaymentResponse{Payment: &paymentv1.Payment{
			Status: "captured", AmountMinor: 2550, RefundedMinor: 500, Currency: "USD"}}}
		got, err := (&PaymentGRPCClient{client: stub}).GetPaymentByOrderID(context.Background(), 42)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got.Status != "partially_refunded" || got.Refunded != 5.00 {
			t.Fatalf("partial refund must be derived, got %+v", got)
		}
	})
	t.Run("fully refunded keeps stored status", func(t *testing.T) {
		stub := &stubPaymentSvcClient{resp: &paymentv1.GetPaymentResponse{Payment: &paymentv1.Payment{
			Status: "refunded", AmountMinor: 2550, RefundedMinor: 2550}}}
		got, _ := (&PaymentGRPCClient{client: stub}).GetPaymentByOrderID(context.Background(), 42)
		if got.Status != "refunded" {
			t.Fatalf("status = %q, want refunded", got.Status)
		}
	})
	t.Run("declined carries the code", func(t *testing.T) {
		stub := &stubPaymentSvcClient{resp: &paymentv1.GetPaymentResponse{Payment: &paymentv1.Payment{
			Status: "failed", DeclineCode: "insufficient_funds"}}}
		got, _ := (&PaymentGRPCClient{client: stub}).GetPaymentByOrderID(context.Background(), 42)
		if got.Status != "failed" || got.DeclineCode != "insufficient_funds" {
			t.Fatalf("info = %+v", got)
		}
	})
	t.Run("NotFound means no payment yet", func(t *testing.T) {
		stub := &stubPaymentSvcClient{err: status.Error(codes.NotFound, "payment not found")}
		got, err := (&PaymentGRPCClient{client: stub}).GetPaymentByOrderID(context.Background(), 42)
		if err != nil || got != nil {
			t.Fatalf("NotFound must be (nil, nil), got %+v %v", got, err)
		}
	})
	t.Run("other errors surface", func(t *testing.T) {
		stub := &stubPaymentSvcClient{err: status.Error(codes.Unavailable, "down")}
		if _, err := (&PaymentGRPCClient{client: stub}).GetPaymentByOrderID(context.Background(), 42); err == nil {
			t.Fatal("transport errors must surface (the aggregation soft-fails them)")
		}
	})
	t.Run("nil payment in response means no payment", func(t *testing.T) {
		stub := &stubPaymentSvcClient{resp: &paymentv1.GetPaymentResponse{}}
		got, err := (&PaymentGRPCClient{client: stub}).GetPaymentByOrderID(context.Background(), 42)
		if err != nil || got != nil {
			t.Fatalf("nil payment must be (nil, nil), got %+v %v", got, err)
		}
	})
}
