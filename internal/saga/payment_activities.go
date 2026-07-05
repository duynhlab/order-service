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
	// paymentCurrency and demoPaymentToken are placeholders used while payment
	// runs behind PAYMENT_ENABLED: there is no checkout UI collecting a real
	// payment method until the frontend phase, so the saga authorizes with a
	// fixed demo token. Replaced by real checkout data when checkout lands.
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

// ensurePaymentClient guards against a nil payment client — e.g. a PAYMENT_ENABLED
// skew where a worker ran a payment-enabled workflow without a dialed client.
// Fails fast and non-retryably instead of panicking on a nil-interface deref.
// (The worker now dials payment unconditionally, so this is defense-in-depth.)
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
		return mapPaymentErr("authorize payment for order "+orderID, err)
	}
	// Whitelist: only "authorized" proceeds. A decline ("failed") or any
	// unexpected status is a non-retryable rejection so the saga compensates.
	if resp.GetPayment().GetStatus() != statusPaymentAuthorized {
		return temporal.NewNonRetryableApplicationError("payment not authorized", "PaymentDeclined",
			fmt.Errorf("order %s status=%q decline_code=%s", orderID, resp.GetPayment().GetStatus(), resp.GetPayment().GetDeclineCode()))
	}
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
		return mapPaymentErr("capture payment for order "+orderID, err)
	}
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
		return mapPaymentErr("void payment for order "+orderID, err)
	}
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
		return mapPaymentErr("refund payment for order "+orderID, err)
	}
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

// mapPaymentErr maps a payment gRPC error to a saga activity error. Business and
// programming rejections (invalid argument, not found, invalid state, conflict)
// are non-retryable so the saga fails fast; transient codes (aborted lock,
// unavailable, internal) stay retryable for Temporal's retry policy.
func mapPaymentErr(msg string, err error) error {
	// The default arm intentionally treats every other gRPC code as retryable.
	switch status.Code(err) { //nolint:exhaustive // default covers the remaining codes
	case codes.InvalidArgument, codes.NotFound, codes.FailedPrecondition, codes.AlreadyExists,
		codes.Unauthenticated, codes.PermissionDenied, codes.Unimplemented:
		return temporal.NewNonRetryableApplicationError(msg, reasonPaymentRejected, err)
	default:
		return fmt.Errorf("%s: %w", msg, err)
	}
}
