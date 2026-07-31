package saga

import (
	"context"
	"fmt"
	"strconv"

	inventoryv1 "github.com/duynhlab/pkg/proto/inventory/v1"
	paymentv1 "github.com/duynhlab/pkg/proto/payment/v1"
	shippingv1 "github.com/duynhlab/pkg/proto/shipping/v1"
	"go.temporal.io/sdk/temporal"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/duynhlab/order-service/internal/core/domain"
)

// ReleaseReasonCancellation is the movement-ledger reason for stock returned
// by a customer cancellation (fits inventory's ^[A-Za-z0-9_.-]{0,64}$).
const ReleaseReasonCancellation ReleaseReason = "ORDER_CANCELLED"

// reasonShipmentDispatched is the non-retryable verdict of the cancellation
// policy: the shipment already left, so unwinding is a Return problem
// (explicitly out of scope in v1), not a cancellation.
const reasonShipmentDispatched = "ShipmentDispatched"

// Shipment statuses that still allow cancellation. As-built nothing drives a
// shipment past `pending` (no carrier integration), so the gate is always
// open today; it exists so the policy tightens by itself when shipping grows
// dispatch states.
func shipmentCancellable(shipmentStatus string) bool {
	switch shipmentStatus {
	case "", "pending", "cancelled":
		return true
	default:
		return false
	}
}

// CheckCancellationPolicy is the authoritative half of the cancellation
// gate (the HTTP handler runs a best-effort copy for UX). It refuses,
// non-retryably, once the shipment has left.
func (a *Activities) CheckCancellationPolicy(ctx context.Context, orderID string) error {
	resp, err := a.Shipping.GetShipmentByOrder(ctx, &shippingv1.GetShipmentByOrderRequest{
		OrderId: orderID,
	})
	if err != nil {
		if status.Code(err) == codes.NotFound {
			// No shipment was ever created — nothing blocks cancellation.
			return nil
		}
		return fmt.Errorf("read shipment for order %s: %w", orderID, err)
	}
	if s := resp.GetShipment().GetStatus(); !shipmentCancellable(s) {
		return temporal.NewNonRetryableApplicationError(
			fmt.Sprintf("order %s shipment is %s", orderID, s), reasonShipmentDispatched, nil)
	}
	return nil
}

// PaymentState is what the cancellation workflow needs to unwind money:
// the current status and the amounts that decide how much is still out.
type PaymentState struct {
	Status        string
	AmountMinor   int64
	RefundedMinor int64
}

// GetPaymentState reads the payment's current state (zero value when no
// payment exists). The amounts matter as much as the status: a partially
// refunded payment keeps status=captured, and refunding the full total
// against it would be rejected by the ledger's sum guard — the workflow
// must refund the REMAINDER.
func (a *Activities) GetPaymentState(ctx context.Context, orderID string) (PaymentState, error) {
	id, err := strconv.ParseInt(orderID, 10, 64)
	if err != nil {
		return PaymentState{}, temporal.NewNonRetryableApplicationError(msgInvalidOrderID, reasonInvalidOrderID, err)
	}
	resp, err := a.Payment.GetPayment(ctx, &paymentv1.GetPaymentRequest{OrderId: id})
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return PaymentState{}, nil
		}
		return PaymentState{}, fmt.Errorf("read payment for order %s: %w", orderID, err)
	}
	pay := resp.GetPayment()
	return PaymentState{
		Status:        pay.GetStatus(),
		AmountMinor:   pay.GetAmountMinor(),
		RefundedMinor: pay.GetRefundedMinor(),
	}, nil
}

// GetReservationState reads the inventory reservation's status ("" when none
// exists — the product path, or a reserve that never landed).
func (a *Activities) GetReservationState(ctx context.Context, orderID string) (string, error) {
	resp, err := a.Inventory.GetReservation(ctx, &inventoryv1.GetReservationRequest{
		ReservationId: orderID,
	})
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return "", nil
		}
		return "", fmt.Errorf("read reservation for order %s: %w", orderID, err)
	}
	return resp.GetReservation().GetStatus().String(), nil
}

// CompleteCancellation closes a converged episode: cancelling → cancelled.
func (a *Activities) CompleteCancellation(ctx context.Context, orderID string, epoch int64) error {
	cmd, err := domain.NewCompleteCancellationCommand(orderID, epoch)
	if err != nil {
		return temporal.NewNonRetryableApplicationError(
			"complete cancellation "+orderID, reasonOrderTransitionRefused, err)
	}
	return applyOrderCommand(ctx, a.Orders, cmd)
}

// CancelManualReview parks a cancelling order whose unwind did not converge
// (the episode-scoped form of MarkManualReview).
func (a *Activities) CancelManualReview(ctx context.Context, orderID string, reason domain.ReasonCode, epoch int64) error {
	cmd, err := domain.NewCancelManualReviewCommand(orderID, reason, epoch)
	if err != nil {
		return temporal.NewNonRetryableApplicationError(
			"park cancellation "+orderID, reasonOrderTransitionRefused, err)
	}
	return applyOrderCommand(ctx, a.Orders, cmd)
}
