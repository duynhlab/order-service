package v1

import (
	"context"
	"fmt"

	paymentv1 "github.com/duynhlab/pkg/proto/payment/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/duynhlab/order-service/internal/core/domain"
)

// PaymentInfo is the payment slice of the aggregated order-details response.
// Money renders as dollars, matching the rest of the HTTP contract (the
// internal gRPC carries minor units).
type PaymentInfo struct {
	// Status is the display status. A partial refund keeps the stored status
	// "captured" on the payment service, so this derives "partially_refunded"
	// when refunds have been applied but don't cover the full amount — the same
	// derivation payment's own HTTP surface documents.
	Status      string  `json:"status"`
	Amount      float64 `json:"amount"`
	Refunded    float64 `json:"refunded,omitempty"`
	Currency    string  `json:"currency"`
	DeclineCode string  `json:"decline_code,omitempty"`
}

// Payment display statuses this client cares about; the rest pass through
// verbatim from the payment service.
const (
	paymentStatusCaptured          = "captured"
	paymentStatusPartiallyRefunded = "partially_refunded"
)

// PaymentGRPCClient fetches an order's payment snapshot from the payment
// service over gRPC (payment.v1.GetPayment) for the order-details aggregation.
type PaymentGRPCClient struct {
	client paymentv1.PaymentServiceClient
}

// NewPaymentGRPCClient wraps a gRPC connection (typically from grpcx.Dial).
func NewPaymentGRPCClient(conn *grpc.ClientConn) *PaymentGRPCClient {
	return &PaymentGRPCClient{client: paymentv1.NewPaymentServiceClient(conn)}
}

// GetPaymentByOrderID returns the order's payment, or (nil, nil) when the order
// has no payment yet (gRPC NOT_FOUND) — mirroring the shipment fetcher's
// no-shipment handling so the aggregation treats both enrichments identically.
// Owner-scoping happened before this call: the handler already verified the
// order belongs to the requesting user.
func (c *PaymentGRPCClient) GetPaymentByOrderID(ctx context.Context, orderID int64) (*PaymentInfo, error) {
	resp, err := c.client.GetPayment(ctx, &paymentv1.GetPaymentRequest{OrderId: orderID})
	if status.Code(err) == codes.NotFound {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("payment gRPC call failed: %w", err)
	}
	p := resp.GetPayment()
	if p == nil {
		return nil, nil
	}
	displayStatus := p.GetStatus()
	if displayStatus == paymentStatusCaptured && p.GetRefundedMinor() > 0 && p.GetRefundedMinor() < p.GetAmountMinor() {
		displayStatus = paymentStatusPartiallyRefunded
	}
	return &PaymentInfo{
		Status:      displayStatus,
		Amount:      domain.Dollars(p.GetAmountMinor()),
		Refunded:    domain.Dollars(p.GetRefundedMinor()),
		Currency:    p.GetCurrency(),
		DeclineCode: p.GetDeclineCode(),
	}, nil
}
