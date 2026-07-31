package saga

import (
	"errors"
	"fmt"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/duynhlab/order-service/internal/core/domain"
	"github.com/duynhlab/pkg/grpcx"
)

// CancellationWorkflowID returns the dedup workflow ID for one cancellation
// EPISODE. The epoch (the orders.version the server read when accepting the
// cancel) is part of the id: manual_review → confirmed → cancel-again is
// legal, and a version-free id would collide with the first episode under
// WorkflowIDReusePolicy and Temporal's dedup.
func CancellationWorkflowID(orderID string, epoch int64) string {
	return fmt.Sprintf("order-cancellation-%s-v%d", orderID, epoch)
}

// CancellationInput is the cancellation workflow's input.
type CancellationInput struct {
	OrderID string
	UserID  string
	// Total is the order total in minor units, for the refund.
	Total int64
	// Epoch namespaces this episode's command ids and workflow id.
	Epoch int64
}

// Reservation-status tokens as GetReservationState returns them (the
// inventory.v1 enum names).
const (
	resStatusReserved  = "RESERVATION_STATUS_RESERVED"
	resStatusCommitted = "RESERVATION_STATUS_COMMITTED"
)

// Bounded projection error tokens specific to cancellation.
const (
	errCodeShipmentDispatched = "SHIPMENT_DISPATCHED"
	stepCancelShipment        = "CANCEL_SHIPMENT"
	stepVoidPayment           = "VOID_PAYMENT"
	stepRefundPayment         = "REFUND_PAYMENT"
	stepReleaseInventory      = "RELEASE_INVENTORY"
	stepRestockSkipped        = "RESTOCK_SKIPPED"
	stepCompleteCancellation  = "COMPLETE_CANCELLATION"
)

// CancellationWorkflow unwinds a confirmed (or completed) order:
//
//	CheckCancellationPolicy → CancelShipment → Void|Refund by payment state →
//	inventory disposition (Release if RESERVED, recorded-skip if COMMITTED) →
//	CompleteCancellation (cancelling → cancelled)
//
// It is a separate workflow, not a signal branch in the fulfillment saga
// (RFC-0021 §5.11): the fulfillment workflow is long finished by the time a
// user may cancel, and bolting a selector onto it would complicate its
// determinism for nothing.
//
// The workflow always COMPLETES; the order's state carries the outcome. Any
// step that exhausts its retries — or a policy refusal — parks the order in
// manual_review(COMPENSATION_INCOMPLETE) for a human. Every activity is
// idempotent, so a duplicate start of the same episode replays harmlessly.
func CancellationWorkflow(ctx workflow.Context, in CancellationInput) error {
	ctx = workflow.WithActivityOptions(ctx, compensationActivityOptions())
	log := workflow.GetLogger(ctx)
	var a *Activities

	recordStage(ctx, domain.ProcessingUpdate{OrderID: in.OrderID, Stage: domain.StageCancelling})

	// 1. The authoritative policy gate. A dispatched shipment is a Return
	// problem (out of scope v1), so the order parks rather than lying about
	// a cancellation that cannot physically happen.
	if err := workflow.ExecuteActivity(ctx, a.CheckCancellationPolicy, in.OrderID).Get(ctx, nil); err != nil {
		log.Error("cancellation policy refused or unreadable; parking for manual review",
			"order_id", in.OrderID, "error", err)
		return parkCancellation(ctx, in, errorCodeForPolicy(err))
	}

	// 2. Cancel the shipment (idempotent no-op when none exists).
	if err := workflow.ExecuteActivity(ctx, a.CancelShipment, in.OrderID).Get(ctx, nil); err != nil {
		// Unavailability, not dispatch: shipping maps its failures to
		// retryable Internal, so exhausting here means the service was dark,
		// and the projection must not send the operator hunting for a
		// shipment that "left".
		log.Error("CancelShipment did not converge; parking for manual review",
			"order_id", in.OrderID, "error", err)
		return parkCancellation(ctx, in, string(domain.ReasonShipmentUnavailable))
	}
	recordStage(ctx, domain.ProcessingUpdate{OrderID: in.OrderID,
		Stage: domain.StageCancelling, LastStep: stepCancelShipment})

	// 3. Money: void an authorized hold, refund captured money, skip
	// anything already settled. The state is read server-side rather than
	// carried in the input so a late-starting episode acts on the truth.
	if err := unwindPayment(ctx, in); err != nil {
		log.Error("payment unwind did not converge; parking for manual review",
			"order_id", in.OrderID, "error", err)
		return parkCancellation(ctx, in, string(domain.ReasonPaymentOutcomeUnknown))
	}

	// 4. Stock disposition.
	if err := resolveInventoryDisposition(ctx, in); err != nil {
		log.Error("inventory disposition did not converge; parking for manual review",
			"order_id", in.OrderID, "error", err)
		return parkCancellation(ctx, in, string(domain.ReasonInventoryUnavailable))
	}

	// 5. Close the episode: cancelling → cancelled.
	if err := workflow.ExecuteActivity(ctx, a.CompleteCancellation, in.OrderID, in.Epoch).Get(ctx, nil); err != nil {
		log.Error("CompleteCancellation did not land; parking for manual review",
			"order_id", in.OrderID, "error", err)
		return parkCancellation(ctx, in, string(domain.ReasonCompensationIncomplete))
	}
	recordCancellationOutcome(ctx, cancellationOutcomeCancelled)
	recordStage(ctx, domain.ProcessingUpdate{OrderID: in.OrderID,
		Stage: domain.StageDone, LastStep: stepCompleteCancellation})
	return nil
}

// errorCodeForPolicy distinguishes "the shipment left" from "shipping was
// unreadable" for the projection row an operator will read first.
func errorCodeForPolicy(err error) string {
	var appErr *temporal.ApplicationError
	if errors.As(err, &appErr) && appErr.Type() == reasonShipmentDispatched {
		return errCodeShipmentDispatched
	}
	return string(domain.ReasonShipmentUnavailable)
}

// unwindPayment returns the money by whatever the payment's current state
// requires. Refunds notify the customer best-effort.
func unwindPayment(ctx workflow.Context, in CancellationInput) error {
	var a *Activities
	log := workflow.GetLogger(ctx)

	var pay PaymentState
	if err := workflow.ExecuteActivity(ctx, a.GetPaymentState, in.OrderID).Get(ctx, &pay); err != nil {
		return err
	}

	switch pay.Status {
	case "authorized":
		if err := workflow.ExecuteActivity(ctx, a.VoidPayment, in.OrderID).Get(ctx, nil); err != nil {
			return err
		}
		recordStage(ctx, domain.ProcessingUpdate{OrderID: in.OrderID,
			Stage: domain.StageCancelling, LastStep: stepVoidPayment})
	case "captured":
		// Partial refunds keep status=captured; the ledger's sum guard
		// REJECTS a full-total refund against them, so the workflow refunds
		// what is still out. The amounts come from the payment read above —
		// recorded, so replay-deterministic — never from the input total.
		remaining := pay.AmountMinor - pay.RefundedMinor
		if remaining <= 0 {
			log.Info("payment already fully refunded", "order_id", in.OrderID)
			return nil
		}
		if err := workflow.ExecuteActivity(ctx, a.RefundPayment, in.OrderID, remaining).Get(ctx, nil); err != nil {
			return err
		}
		recordStage(ctx, domain.ProcessingUpdate{OrderID: in.OrderID,
			Stage: domain.StageCancelling, LastStep: stepRefundPayment})
		if err := workflow.ExecuteActivity(ctx, a.SendRefundNotification,
			NotifyInput{OrderID: in.OrderID, UserID: in.UserID, Total: remaining}).Get(ctx, nil); err != nil {
			log.Warn("SendRefundNotification failed (non-fatal)", "order_id", in.OrderID, "error", err)
		}
	default:
		// "", pending, voided, refunded, expired, failed — nothing to move.
		log.Info("payment needs no unwind", "order_id", in.OrderID, "payment_status", pay.Status)
	}
	return nil
}

// resolveInventoryDisposition unwinds stock per the reservation's CURRENT
// state, read from inventory rather than carried in the input: a
// product-path order simply has no reservation row (NOT_FOUND → skip), so
// the participant needs no plumbing here — and the product path's bare
// decrement is deliberately NOT incremented back, because without a
// reservation there is no proof the decrement ever landed and a blind
// release would mint phantom stock.
//
// v1 accepted shrinkage (RFC-0021 restock fork, option A): a COMMITTED
// reservation stays committed — inventory.v1 has no Return RPC, and faking
// one with Release would violate its FSM — so the skip is RECORDED on the
// projection instead. Adding a real Return later changes exactly this one
// branch.
func resolveInventoryDisposition(ctx workflow.Context, in CancellationInput) error {
	var a *Activities
	log := workflow.GetLogger(ctx)

	var resState string
	if err := workflow.ExecuteActivity(ctx, a.GetReservationState, in.OrderID).Get(ctx, &resState); err != nil {
		return err
	}

	switch resState {
	case resStatusReserved:
		err := workflow.ExecuteActivity(ctx, a.ReleaseInventory, in.OrderID, ReleaseReasonCancellation).Get(ctx, nil)
		if err == nil {
			recordStage(ctx, domain.ProcessingUpdate{OrderID: in.OrderID,
				Stage: domain.StageCancelling, LastStep: stepReleaseInventory})
			return nil
		}
		// The release lost a race with the fulfillment tail's commit:
		// re-read and take the COMMITTED branch instead of failing.
		var appErr *temporal.ApplicationError
		if errors.As(err, &appErr) && appErr.Type() == grpcx.ReasonInvalidTransition {
			log.Info("release raced the commit; re-reading the reservation", "order_id", in.OrderID)
			if err := workflow.ExecuteActivity(ctx, a.GetReservationState, in.OrderID).Get(ctx, &resState); err != nil {
				return err
			}
			if resState == resStatusCommitted {
				recordStage(ctx, domain.ProcessingUpdate{OrderID: in.OrderID,
					Stage: domain.StageCancelling, LastStep: stepRestockSkipped})
				return nil
			}
		}
		return err
	case resStatusCommitted:
		recordStage(ctx, domain.ProcessingUpdate{OrderID: in.OrderID,
			Stage: domain.StageCancelling, LastStep: stepRestockSkipped})
		return nil
	default:
		// "", RELEASED, EXPIRED — the stock is already back (or never left).
		log.Info("reservation needs no unwind", "order_id", in.OrderID, "reservation_status", resState)
		return nil
	}
}

// parkCancellation is the episode's terminal fallback: cancelling →
// manual_review. If even that cannot land, the workflow fails with the
// order left `cancelling` — the stuck-cancelling gauge owns the escalation.
func parkCancellation(ctx workflow.Context, in CancellationInput, errCode string) error {
	var a *Activities
	err := workflow.ExecuteActivity(ctx, a.CancelManualReview,
		in.OrderID, domain.ReasonCompensationIncomplete, in.Epoch).Get(ctx, nil)
	recordCompensation(ctx, compMarkManualReview, compResult(err))
	if err != nil {
		return fmt.Errorf("cancellation terminal bookkeeping for order %s did not land: %w", in.OrderID, err)
	}
	recordCancellationOutcome(ctx, cancellationOutcomeManualReview)
	// No LastStep: COALESCE keeps the last step that actually worked, next
	// to the specific error code an operator needs first.
	recordStage(ctx, domain.ProcessingUpdate{OrderID: in.OrderID,
		Stage: domain.StageManualReview, LastErrorCode: errCode})
	return nil
}
