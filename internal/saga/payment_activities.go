package saga

import (
	"context"
	"fmt"
	"strconv"

	paymentv1 "github.com/duynhlab/pkg/proto/payment/v1"
	"go.temporal.io/sdk/temporal"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	// paymentCurrency is the fixed settlement currency (USD in v1).
	paymentCurrency = "USD"
	// demoPaymentToken is the fallback when the workflow input carries no
	// payment method (API-created orders, older clients).
	demoPaymentToken = "tok_visa"

	// statusPaymentAuthorized is the only Authorize status the saga proceeds on;
	// any other (failed decline, or an unexpected pending/expired/empty) is a
	// non-retryable rejection.
	statusPaymentAuthorized = "authorized"

	// refundReasonCompensation is the reason recorded on a saga-driven refund.
	refundReasonCompensation = "order fulfillment compensation"

	// Shared error message / reason-type strings for the payment activities.
	msgInvalidOrderID      = "invalid order id"
	msgInvalidUserID       = "invalid user id"
	msgPaymentClientNil    = "payment client not configured"
	reasonInvalidOrderID   = "InvalidOrderID"
	reasonInvalidUserID    = "InvalidUserID"
	reasonPaymentRejected  = "PaymentRejected"
	reasonPaymentClientNil = "PaymentClientNil"
)

// ensurePaymentClient guards against a nil payment client. The worker dials
// payment unconditionally, so this is defense-in-depth: it fails fast and
// non-retryably instead of panicking on a nil-interface deref.
func (a *Activities) ensurePaymentClient() error {
	if a.Payment == nil {
		return temporal.NewNonRetryableApplicationError(msgPaymentClientNil, reasonPaymentClientNil, nil)
	}
	return nil
}

// AuthorizePayment places the pre-pivot authorization hold for the order.
// Idempotent by order_id at the payment service (so Temporal retries are safe).
// A provider decline comes back as a normal response with status="failed" (not
// a gRPC error); the saga treats it as a non-retryable business rejection so it
// fails fast and compensates.
func (a *Activities) AuthorizePayment(ctx context.Context, orderID, userID string, amountMinor int64, paymentMethod string) error {
	if err := a.ensurePaymentClient(); err != nil {
		return err
	}
	oid, err := parsePositiveID(orderID)
	if err != nil {
		return temporal.NewNonRetryableApplicationError(msgInvalidOrderID, reasonInvalidOrderID, err)
	}
	uid, err := parsePositiveID(userID)
	if err != nil {
		return temporal.NewNonRetryableApplicationError(msgInvalidUserID, reasonInvalidUserID, err)
	}
	if paymentMethod == "" {
		paymentMethod = demoPaymentToken
	}
	resp, err := a.Payment.Authorize(ctx, &paymentv1.AuthorizeRequest{
		OrderId:       oid,
		UserId:        uid,
		AmountMinor:   amountMinor,
		Currency:      paymentCurrency,
		PaymentMethod: paymentMethod,
	})
	if err != nil {
		recordPaymentActivity(ctx, payOpAuthorize, paymentActivityResult(err))
		return mapPaymentErr("authorize payment for order "+orderID, err)
	}
	// Whitelist: only "authorized" proceeds. A decline ("failed") or any
	// unexpected status is a non-retryable rejection so the saga compensates.
	if resp.GetPayment().GetStatus() != statusPaymentAuthorized {
		recordPaymentActivity(ctx, payOpAuthorize, resultDeclined)
		return temporal.NewNonRetryableApplicationError("payment not authorized", "PaymentDeclined",
			fmt.Errorf("order %s status=%q decline_code=%s", orderID, resp.GetPayment().GetStatus(), resp.GetPayment().GetDeclineCode()))
	}
	recordPaymentActivity(ctx, payOpAuthorize, resultOK)
	return nil
}

// CapturePayment captures the authorized hold, just before the confirm pivot.
func (a *Activities) CapturePayment(ctx context.Context, orderID string) error {
	if err := a.ensurePaymentClient(); err != nil {
		return err
	}
	oid, err := parsePositiveID(orderID)
	if err != nil {
		return temporal.NewNonRetryableApplicationError(msgInvalidOrderID, reasonInvalidOrderID, err)
	}
	if _, err := a.Payment.Capture(ctx, &paymentv1.CaptureRequest{OrderId: oid}); err != nil {
		recordPaymentActivity(ctx, payOpCapture, paymentActivityResult(err))
		return mapPaymentErr("capture payment for order "+orderID, err)
	}
	recordPaymentActivity(ctx, payOpCapture, resultOK)
	return nil
}

// VoidPayment releases an authorized-but-not-captured hold (compensation for
// AuthorizePayment when a pre-capture step fails). Idempotent at the service.
func (a *Activities) VoidPayment(ctx context.Context, orderID string) error {
	if err := a.ensurePaymentClient(); err != nil {
		return err
	}
	oid, err := parsePositiveID(orderID)
	if err != nil {
		return temporal.NewNonRetryableApplicationError(msgInvalidOrderID, reasonInvalidOrderID, err)
	}
	if _, err := a.Payment.Void(ctx, &paymentv1.VoidRequest{OrderId: oid}); err != nil {
		recordPaymentActivity(ctx, payOpVoid, paymentActivityResult(err))
		return mapPaymentErr("void payment for order "+orderID, err)
	}
	recordPaymentActivity(ctx, payOpVoid, resultOK)
	return nil
}

// RefundPayment returns captured money (compensation when a post-capture,
// pre-pivot step fails). Refunds the full order amount; idempotent at the service.
func (a *Activities) RefundPayment(ctx context.Context, orderID string, amountMinor int64) error {
	if err := a.ensurePaymentClient(); err != nil {
		return err
	}
	oid, err := parsePositiveID(orderID)
	if err != nil {
		return temporal.NewNonRetryableApplicationError(msgInvalidOrderID, reasonInvalidOrderID, err)
	}
	if _, err := a.Payment.Refund(ctx, &paymentv1.RefundRequest{
		OrderId:     oid,
		AmountMinor: amountMinor,
		Reason:      refundReasonCompensation,
	}); err != nil {
		recordPaymentActivity(ctx, payOpRefund, paymentActivityResult(err))
		return mapPaymentErr("refund payment for order "+orderID, err)
	}
	recordPaymentActivity(ctx, payOpRefund, resultOK)
	return nil
}

// parsePositiveID parses a positive numeric id (order/user) from its string form.
func parsePositiveID(s string) (int64, error) {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse id %q: %w", s, err)
	}
	if n <= 0 {
		return 0, fmt.Errorf("id must be positive, got %d", n)
	}
	return n, nil
}

// isPaymentRejection reports whether a payment gRPC error is a business or
// programming rejection (invalid argument, not found, invalid state, conflict)
// rather than a transient failure. It is the single classifier both
// mapPaymentErr (the retry decision) and the payment.activity metric label use,
// so the two can never drift.
func isPaymentRejection(err error) bool {
	// The default arm intentionally treats every other gRPC code as transient.
	switch status.Code(err) { //nolint:exhaustive // default covers the remaining codes
	case codes.InvalidArgument, codes.NotFound, codes.FailedPrecondition, codes.AlreadyExists,
		codes.Unauthenticated, codes.PermissionDenied, codes.Unimplemented:
		return true
	default:
		return false
	}
}

// mapPaymentErr maps a payment gRPC error to a saga activity error. Rejections
// are non-retryable so the saga fails fast; transient codes (aborted lock,
// unavailable, internal) stay retryable for Temporal's retry policy.
func mapPaymentErr(msg string, err error) error {
	if isPaymentRejection(err) {
		return temporal.NewNonRetryableApplicationError(msg, reasonPaymentRejected, err)
	}
	return fmt.Errorf("%s: %w", msg, err)
}

// paymentActivityResult classifies a payment gRPC error into the bounded
// payment.activity result label: "rejected" for a non-retryable rejection,
// "error" for a transient failure that Temporal will retry.
func paymentActivityResult(err error) string {
	if isPaymentRejection(err) {
		return resultRejected
	}
	return resultError
}
