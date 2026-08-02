package saga

import (
	"testing"

	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"

	"github.com/duynhlab/order-service/internal/core/domain"
	"github.com/duynhlab/pkg/grpcx"
)

func cancelInput() CancellationInput {
	return CancellationInput{OrderID: "42", UserID: "7", Total: 2500, Epoch: 3}
}

// newCancelEnv mocks the steps every cancellation shares; individual tests
// override the branch under study.
func newCancelEnv() (*testsuite.TestWorkflowEnvironment, *Activities) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	var a *Activities
	env.OnActivity(a.RecordProcessingStage, mock.Anything, mock.Anything).Return(nil)
	return env, a
}

func TestCancellationWorkflow_CapturedRefundsAndReleases(t *testing.T) {
	env, a := newCancelEnv()
	env.OnActivity(a.CheckCancellationPolicy, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.CancelShipment, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.GetPaymentState, mock.Anything, mock.Anything).Return(PaymentState{Status: "captured", AmountMinor: 2500}, nil)
	env.OnActivity(a.RefundPayment, mock.Anything, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.SendRefundNotification, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.GetReservationState, mock.Anything, mock.Anything).Return("RESERVATION_STATUS_RESERVED", nil)
	env.OnActivity(a.ReleaseInventory, mock.Anything, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.CompleteCancellation, mock.Anything, mock.Anything, mock.Anything).Return(nil)

	env.ExecuteWorkflow(CancellationWorkflow, cancelInput())

	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error = %v", err)
	}
	env.AssertCalled(t, "RefundPayment", mock.Anything, "42", int64(2500))
	env.AssertCalled(t, "ReleaseInventory", mock.Anything, "42", ReleaseReasonCancellation)
	env.AssertCalled(t, "CompleteCancellation", mock.Anything, "42", int64(3))
	env.AssertNotCalled(t, "VoidPayment", mock.Anything, mock.Anything)
	env.AssertNotCalled(t, "CancelManualReview", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

func TestCancellationWorkflow_AuthorizedVoids_CommittedSkipsRestock(t *testing.T) {
	env, a := newCancelEnv()
	env.OnActivity(a.CheckCancellationPolicy, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.CancelShipment, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.GetPaymentState, mock.Anything, mock.Anything).Return(PaymentState{Status: "authorized", AmountMinor: 2500}, nil)
	env.OnActivity(a.VoidPayment, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.GetReservationState, mock.Anything, mock.Anything).Return("RESERVATION_STATUS_COMMITTED", nil)
	env.OnActivity(a.CompleteCancellation, mock.Anything, mock.Anything, mock.Anything).Return(nil)

	env.ExecuteWorkflow(CancellationWorkflow, cancelInput())

	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error = %v", err)
	}
	env.AssertCalled(t, "VoidPayment", mock.Anything, "42")
	// v1 accepted shrinkage: COMMITTED stock stays committed.
	env.AssertNotCalled(t, "ReleaseInventory", mock.Anything, mock.Anything, mock.Anything)
	env.AssertNotCalled(t, "RefundPayment", mock.Anything, mock.Anything, mock.Anything)
	env.AssertCalled(t, "CompleteCancellation", mock.Anything, "42", int64(3))
}

// A product-path order simply has no reservation: skip, still cancelled.
func TestCancellationWorkflow_NoReservation_SkipsInventory(t *testing.T) {
	env, a := newCancelEnv()
	env.OnActivity(a.CheckCancellationPolicy, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.CancelShipment, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.GetPaymentState, mock.Anything, mock.Anything).Return(PaymentState{Status: "refunded", AmountMinor: 2500, RefundedMinor: 2500}, nil)
	env.OnActivity(a.GetReservationState, mock.Anything, mock.Anything).Return("", nil)
	env.OnActivity(a.CompleteCancellation, mock.Anything, mock.Anything, mock.Anything).Return(nil)

	env.ExecuteWorkflow(CancellationWorkflow, cancelInput())

	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error = %v", err)
	}
	env.AssertNotCalled(t, "ReleaseInventory", mock.Anything, mock.Anything, mock.Anything)
	env.AssertNotCalled(t, "RefundPayment", mock.Anything, mock.Anything, mock.Anything)
}

// The release/commit race: Release is refused because the fulfillment tail
// committed first — re-read and take the COMMITTED branch.
func TestCancellationWorkflow_ReleaseRacesCommit_Rereads(t *testing.T) {
	env, a := newCancelEnv()
	env.OnActivity(a.CheckCancellationPolicy, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.CancelShipment, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.GetPaymentState, mock.Anything, mock.Anything).Return(PaymentState{Status: "captured", AmountMinor: 2500}, nil)
	env.OnActivity(a.RefundPayment, mock.Anything, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.SendRefundNotification, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.GetReservationState, mock.Anything, mock.Anything).Return("RESERVATION_STATUS_RESERVED", nil).Once()
	env.OnActivity(a.ReleaseInventory, mock.Anything, mock.Anything, mock.Anything).Return(
		temporal.NewNonRetryableApplicationError("committed", grpcx.ReasonInvalidTransition, nil))
	env.OnActivity(a.GetReservationState, mock.Anything, mock.Anything).Return("RESERVATION_STATUS_COMMITTED", nil).Once()
	env.OnActivity(a.CompleteCancellation, mock.Anything, mock.Anything, mock.Anything).Return(nil)

	env.ExecuteWorkflow(CancellationWorkflow, cancelInput())

	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error = %v", err)
	}
	env.AssertCalled(t, "CompleteCancellation", mock.Anything, "42", int64(3))
	env.AssertNotCalled(t, "CancelManualReview", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

// A dispatched shipment is a Return problem: park, never lie about a
// cancellation that cannot physically happen.
func TestCancellationWorkflow_PolicyRefused_Parks(t *testing.T) {
	env, a := newCancelEnv()
	env.OnActivity(a.CheckCancellationPolicy, mock.Anything, mock.Anything).Return(
		temporal.NewNonRetryableApplicationError("shipment is in_transit", reasonShipmentDispatched, nil))
	env.OnActivity(a.CancelManualReview, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)

	env.ExecuteWorkflow(CancellationWorkflow, cancelInput())

	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("the park is the workflow's success path: %v", err)
	}
	env.AssertCalled(t, "CancelManualReview", mock.Anything, "42", domain.ReasonCompensationIncomplete, int64(3))
	env.AssertNotCalled(t, "CancelShipment", mock.Anything, mock.Anything)
	env.AssertNotCalled(t, "CompleteCancellation", mock.Anything, mock.Anything, mock.Anything)
}

// TestCancellationWorkflow_UnknownPaymentOutcome_ParksWithoutTouchingMoney is the
// fail-closed rule. Payment-service can now say "an operation was attempted and
// the provider never answered" (RFC-0021 phase 6). Cancelling on top of that
// would settle the order while the money is unaccounted for — the customer sees
// `cancelled` and nobody ever returns the funds. Before this, `processing` fell
// into the "nothing to move" arm and did exactly that.
func TestCancellationWorkflow_UnknownPaymentOutcome_ParksWithoutTouchingMoney(t *testing.T) {
	env, a := newCancelEnv()
	env.OnActivity(a.CheckCancellationPolicy, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.CancelShipment, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.GetPaymentState, mock.Anything, mock.Anything).Return(
		PaymentState{Status: "processing", AmountMinor: 2500}, nil)
	env.OnActivity(a.CancelManualReview, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)

	env.ExecuteWorkflow(CancellationWorkflow, cancelInput())

	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("the park is the workflow's success path: %v", err)
	}
	env.AssertCalled(t, "CancelManualReview", mock.Anything, "42", domain.ReasonCompensationIncomplete, int64(3))
	// Neither money operation may be attempted against an unknown outcome:
	// refunding might pay twice, voiding might release a hold that is gone.
	env.AssertNotCalled(t, "RefundPayment", mock.Anything, mock.Anything, mock.Anything)
	env.AssertNotCalled(t, "VoidPayment", mock.Anything, mock.Anything)
	env.AssertNotCalled(t, "CompleteCancellation", mock.Anything, mock.Anything, mock.Anything)
}

// A refund that exhausts its retries parks the order with money still out.
func TestCancellationWorkflow_RefundExhausted_Parks(t *testing.T) {
	env, a := newCancelEnv()
	env.OnActivity(a.CheckCancellationPolicy, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.CancelShipment, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.GetPaymentState, mock.Anything, mock.Anything).Return(PaymentState{Status: "captured", AmountMinor: 2500}, nil)
	env.OnActivity(a.RefundPayment, mock.Anything, mock.Anything, mock.Anything).Return(nonRetryable("provider gone"))
	env.OnActivity(a.CancelManualReview, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)

	env.ExecuteWorkflow(CancellationWorkflow, cancelInput())

	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("the park is the workflow's success path: %v", err)
	}
	env.AssertCalled(t, "CancelManualReview", mock.Anything, "42", domain.ReasonCompensationIncomplete, int64(3))
	env.AssertNotCalled(t, "CompleteCancellation", mock.Anything, mock.Anything, mock.Anything)
}

// If even the park cannot land, the workflow fails and the order stays
// `cancelling` — the stuck-cancelling gauge owns the escalation.
func TestCancellationWorkflow_ParkExhausted_FailsWorkflow(t *testing.T) {
	env, a := newCancelEnv()
	env.OnActivity(a.CheckCancellationPolicy, mock.Anything, mock.Anything).Return(nonRetryable("shipping dark"))
	env.OnActivity(a.CancelManualReview, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nonRetryable("db refuses"))

	env.ExecuteWorkflow(CancellationWorkflow, cancelInput())

	if env.GetWorkflowError() == nil {
		t.Fatal("expected the workflow to fail when no terminal state can land")
	}
}

// A partially refunded payment keeps status=captured; the workflow must
// refund the REMAINDER — the ledger's sum guard rejects the full total.
func TestCancellationWorkflow_PartialRefund_RefundsRemainder(t *testing.T) {
	env, a := newCancelEnv()
	env.OnActivity(a.CheckCancellationPolicy, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.CancelShipment, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.GetPaymentState, mock.Anything, mock.Anything).Return(
		PaymentState{Status: "captured", AmountMinor: 2500, RefundedMinor: 1000}, nil)
	env.OnActivity(a.RefundPayment, mock.Anything, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.SendRefundNotification, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.GetReservationState, mock.Anything, mock.Anything).Return("", nil)
	env.OnActivity(a.CompleteCancellation, mock.Anything, mock.Anything, mock.Anything).Return(nil)

	env.ExecuteWorkflow(CancellationWorkflow, cancelInput())

	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error = %v", err)
	}
	env.AssertCalled(t, "RefundPayment", mock.Anything, "42", int64(1500))
}

// A refund the provider declined (surfaced non-retryably by the activity)
// must PARK the order — never cancel it and tell the customer money moved.
func TestCancellationWorkflow_RefundDeclined_Parks(t *testing.T) {
	env, a := newCancelEnv()
	env.OnActivity(a.CheckCancellationPolicy, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.CancelShipment, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.GetPaymentState, mock.Anything, mock.Anything).Return(
		PaymentState{Status: "captured", AmountMinor: 2500}, nil)
	env.OnActivity(a.RefundPayment, mock.Anything, mock.Anything, mock.Anything).Return(
		temporal.NewNonRetryableApplicationError("refund did not succeed", "RefundNotSucceeded", nil))
	env.OnActivity(a.CancelManualReview, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)

	env.ExecuteWorkflow(CancellationWorkflow, cancelInput())

	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("the park is the workflow's success path: %v", err)
	}
	env.AssertNotCalled(t, "SendRefundNotification", mock.Anything, mock.Anything)
	env.AssertNotCalled(t, "CompleteCancellation", mock.Anything, mock.Anything, mock.Anything)
	env.AssertCalled(t, "CancelManualReview", mock.Anything, "42", domain.ReasonCompensationIncomplete, int64(3))
}
