package saga

import (
	"context"
	"testing"

	paymentv1 "github.com/duynhlab/pkg/proto/payment/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type stubPaymentClient struct {
	paymentv1.PaymentServiceClient
	authResp  *paymentv1.AuthorizeResponse
	authErr   error
	capErr    error
	voidErr   error
	refundErr error

	gotAuth   *paymentv1.AuthorizeRequest
	gotRefund *paymentv1.RefundRequest
}

func (s *stubPaymentClient) Authorize(_ context.Context, in *paymentv1.AuthorizeRequest, _ ...grpc.CallOption) (*paymentv1.AuthorizeResponse, error) {
	s.gotAuth = in
	return s.authResp, s.authErr
}
func (s *stubPaymentClient) Capture(_ context.Context, _ *paymentv1.CaptureRequest, _ ...grpc.CallOption) (*paymentv1.CaptureResponse, error) {
	return &paymentv1.CaptureResponse{}, s.capErr
}
func (s *stubPaymentClient) Void(_ context.Context, _ *paymentv1.VoidRequest, _ ...grpc.CallOption) (*paymentv1.VoidResponse, error) {
	return &paymentv1.VoidResponse{}, s.voidErr
}
func (s *stubPaymentClient) Refund(_ context.Context, in *paymentv1.RefundRequest, _ ...grpc.CallOption) (*paymentv1.RefundResponse, error) {
	s.gotRefund = in
	return &paymentv1.RefundResponse{}, s.refundErr
}

func authorizedResp() *paymentv1.AuthorizeResponse {
	return &paymentv1.AuthorizeResponse{Payment: &paymentv1.Payment{Status: "authorized"}}
}

func TestAuthorizePayment_OK(t *testing.T) {
	p := &stubPaymentClient{authResp: authorizedResp()}
	a := &Activities{Payment: p}
	if err := a.AuthorizePayment(context.Background(), "42", "7", 2550); err != nil {
		t.Fatalf("authorize: %v", err)
	}
	if p.gotAuth.GetOrderId() != 42 || p.gotAuth.GetUserId() != 7 || p.gotAuth.GetAmountMinor() != 2550 {
		t.Fatalf("bad request %+v", p.gotAuth)
	}
	if p.gotAuth.GetCurrency() != paymentCurrency || p.gotAuth.GetPaymentMethod() != demoPaymentToken {
		t.Fatalf("currency/token = %q/%q", p.gotAuth.GetCurrency(), p.gotAuth.GetPaymentMethod())
	}
}

func TestPaymentActivities_NilClient(t *testing.T) {
	a := &Activities{} // Payment is nil (e.g. config skew)
	ctx := context.Background()
	checks := map[string]func() error{
		"authorize": func() error { return a.AuthorizePayment(ctx, "42", "7", 2550) },
		"capture":   func() error { return a.CapturePayment(ctx, "42") },
		"void":      func() error { return a.VoidPayment(ctx, "42") },
		"refund":    func() error { return a.RefundPayment(ctx, "42", 2550) },
	}
	for name, fn := range checks {
		if err := fn(); err == nil || !isNonRetryable(err) {
			t.Fatalf("%s with a nil payment client must fail non-retryably (not panic), got %v", name, err)
		}
	}
}

func TestAuthorizePayment_UnexpectedStatusRejected(t *testing.T) {
	p := &stubPaymentClient{authResp: &paymentv1.AuthorizeResponse{Payment: &paymentv1.Payment{Status: "pending"}}}
	a := &Activities{Payment: p}
	if err := a.AuthorizePayment(context.Background(), "42", "7", 2550); err == nil || !isNonRetryable(err) {
		t.Fatalf("a non-authorized status must be a non-retryable rejection, got %v", err)
	}
}

func TestAuthorizePayment_DeclineIsNonRetryable(t *testing.T) {
	p := &stubPaymentClient{authResp: &paymentv1.AuthorizeResponse{
		Payment: &paymentv1.Payment{Status: "failed", DeclineCode: "insufficient_funds"}}}
	a := &Activities{Payment: p}
	err := a.AuthorizePayment(context.Background(), "42", "7", 2550)
	if err == nil || !isNonRetryable(err) {
		t.Fatalf("decline must be a non-retryable error, got %v", err)
	}
}

func TestAuthorizePayment_ErrorMapping(t *testing.T) {
	tests := []struct {
		code     codes.Code
		nonRetry bool
	}{
		{codes.InvalidArgument, true},
		{codes.AlreadyExists, true},
		{codes.FailedPrecondition, true},
		{codes.Internal, false}, // transient → retryable
		{codes.Unavailable, false},
		{codes.Aborted, false}, // idempotency lock → retryable
	}
	for _, tt := range tests {
		p := &stubPaymentClient{authErr: status.Error(tt.code, "x")}
		a := &Activities{Payment: p}
		err := a.AuthorizePayment(context.Background(), "42", "7", 2550)
		if err == nil {
			t.Fatalf("%v: want error", tt.code)
		}
		if got := isNonRetryable(err); got != tt.nonRetry {
			t.Fatalf("%v: nonRetryable=%v, want %v", tt.code, got, tt.nonRetry)
		}
	}
}

func TestAuthorizePayment_InvalidIDs(t *testing.T) {
	a := &Activities{Payment: &stubPaymentClient{authResp: authorizedResp()}}
	for _, tc := range []struct{ order, user string }{
		{"0", "7"}, {"abc", "7"}, {"42", "0"}, {"42", "-1"}, {"42", ""},
	} {
		if err := a.AuthorizePayment(context.Background(), tc.order, tc.user, 2550); err == nil || !isNonRetryable(err) {
			t.Fatalf("order=%q user=%q: want non-retryable error, got %v", tc.order, tc.user, err)
		}
	}
}

func TestCapturePayment(t *testing.T) {
	if err := (&Activities{Payment: &stubPaymentClient{}}).CapturePayment(context.Background(), "42"); err != nil {
		t.Fatalf("capture ok: %v", err)
	}
	// FailedPrecondition (bad state) → non-retryable
	err := (&Activities{Payment: &stubPaymentClient{capErr: status.Error(codes.FailedPrecondition, "x")}}).
		CapturePayment(context.Background(), "42")
	if err == nil || !isNonRetryable(err) {
		t.Fatalf("capture FailedPrecondition must be non-retryable, got %v", err)
	}
	// Internal → retryable
	if err := (&Activities{Payment: &stubPaymentClient{capErr: status.Error(codes.Internal, "x")}}).
		CapturePayment(context.Background(), "42"); err == nil || isNonRetryable(err) {
		t.Fatalf("capture Internal must be retryable, got %v", err)
	}
	// invalid order id → non-retryable
	if err := (&Activities{Payment: &stubPaymentClient{}}).CapturePayment(context.Background(), "0"); err == nil || !isNonRetryable(err) {
		t.Fatalf("capture bad id must be non-retryable, got %v", err)
	}
}

func TestVoidPayment(t *testing.T) {
	if err := (&Activities{Payment: &stubPaymentClient{}}).VoidPayment(context.Background(), "42"); err != nil {
		t.Fatalf("void ok: %v", err)
	}
	if err := (&Activities{Payment: &stubPaymentClient{voidErr: status.Error(codes.Internal, "x")}}).
		VoidPayment(context.Background(), "42"); err == nil || isNonRetryable(err) {
		t.Fatalf("void Internal must be retryable, got %v", err)
	}
	if err := (&Activities{Payment: &stubPaymentClient{}}).VoidPayment(context.Background(), "0"); err == nil || !isNonRetryable(err) {
		t.Fatalf("void bad id must be non-retryable, got %v", err)
	}
}

func TestRefundPayment(t *testing.T) {
	p := &stubPaymentClient{}
	if err := (&Activities{Payment: p}).RefundPayment(context.Background(), "42", 2550); err != nil {
		t.Fatalf("refund ok: %v", err)
	}
	if p.gotRefund.GetOrderId() != 42 || p.gotRefund.GetAmountMinor() != 2550 {
		t.Fatalf("bad refund request %+v", p.gotRefund)
	}
	if err := (&Activities{Payment: &stubPaymentClient{refundErr: status.Error(codes.NotFound, "x")}}).
		RefundPayment(context.Background(), "42", 2550); err == nil || !isNonRetryable(err) {
		t.Fatalf("refund NotFound must be non-retryable, got %v", err)
	}
	if err := (&Activities{Payment: &stubPaymentClient{}}).RefundPayment(context.Background(), "0", 2550); err == nil || !isNonRetryable(err) {
		t.Fatalf("refund bad id must be non-retryable, got %v", err)
	}
}
