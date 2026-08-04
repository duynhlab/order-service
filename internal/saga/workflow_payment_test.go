package saga

import (
	"github.com/duynhlab/pkg/grpcx"
	"testing"

	"github.com/duynhlab/order-service/internal/core/domain"
	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
)

func TestWorkflow_Payment_HappyPath(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	var a *Activities

	env.OnActivity(a.AuthorizePayment, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.ReserveInventory, mock.Anything, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.CreateShipment, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.CapturePayment, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.ConfirmOrder, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.Complete, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.SendNotification, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.SendReceipt, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.ClearCart, mock.Anything, mock.Anything).Return(nil)

	// Negatives are registered, not asserted afterwards (see refuseActivity).
	refuseActivity(t, env, "a successful saga voids nothing",
		a.VoidPayment, mock.Anything, mock.Anything)
	refuseActivity(t, env, "a successful saga refunds nothing",
		a.RefundPayment, mock.Anything, mock.Anything, mock.Anything)
	refuseActivity(t, env, "a successful saga fails no order",
		a.FailOrder, mock.Anything, mock.Anything, mock.Anything)

	env.ExecuteWorkflow(OrderFulfillmentWorkflow, testInput())

	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error = %v, want nil", err)
	}
	env.AssertCalled(t, "AuthorizePayment", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	env.AssertCalled(t, "CapturePayment", mock.Anything, mock.Anything)
	env.AssertCalled(t, "ConfirmOrder", mock.Anything, mock.Anything)
	// Payment captured → a receipt is sent (best-effort, post-pivot).
	env.AssertCalled(t, "SendReceipt", mock.Anything, mock.Anything)
}

func TestWorkflow_Payment_AuthorizeDeclined_NoCompensation(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	var a *Activities

	// A DECLINE is a definitive "no hold exists", so no void runs.
	env.OnActivity(a.AuthorizePayment, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(temporal.NewNonRetryableApplicationError("declined", "PaymentDeclined", nil))
	env.OnActivity(a.FailOrder, mock.Anything, mock.Anything, mock.Anything).Return(nil)

	// Negatives are registered, not asserted afterwards (see refuseActivity).
	refuseActivity(t, env, "a declined authorize left no hold to void",
		a.VoidPayment, mock.Anything, mock.Anything)
	refuseActivity(t, env, "no money means no stock is touched",
		a.ReserveInventory, mock.Anything, mock.Anything, mock.Anything)

	env.ExecuteWorkflow(OrderFulfillmentWorkflow, testInput())

	if env.GetWorkflowError() == nil {
		t.Fatal("expected error when AuthorizePayment fails")
	}
	env.AssertCalled(t, "FailOrder", mock.Anything, "42", domain.ReasonPaymentDeclined)
}

// An AMBIGUOUS authorize failure (timeout, transport — anything that is not
// a decline) may have left a hold behind, so a void runs before `failed`
// is asserted.
func TestWorkflow_Payment_AuthorizeAmbiguous_Voids(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	var a *Activities

	env.OnActivity(a.AuthorizePayment, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(nonRetryable("provider vanished"))
	env.OnActivity(a.VoidPayment, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.FailOrder, mock.Anything, mock.Anything, mock.Anything).Return(nil)

	env.ExecuteWorkflow(OrderFulfillmentWorkflow, testInput())

	if env.GetWorkflowError() == nil {
		t.Fatal("expected error when AuthorizePayment fails")
	}
	env.AssertCalled(t, "VoidPayment", mock.Anything, mock.Anything)
	env.AssertCalled(t, "FailOrder", mock.Anything, "42", domain.ReasonPaymentOutcomeUnknown)
}

func TestWorkflow_Payment_ReserveStockFails_Voids(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	var a *Activities

	env.OnActivity(a.AuthorizePayment, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)
	// INSUFFICIENT_STOCK, not a bare non-retryable: the reserve definitively took
	// nothing, so there is no hold to release. Any OTHER failure is ambiguous and
	// the saga would release first — a distinction the product path never had,
	// because a lost decrement is not a hold.
	env.OnActivity(a.ReserveInventory, mock.Anything, mock.Anything, mock.Anything).Return(
		temporal.NewNonRetryableApplicationError("no stock", grpcx.ReasonInsufficientStock, nil))
	env.OnActivity(a.VoidPayment, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.FailOrder, mock.Anything, mock.Anything, mock.Anything).Return(nil)

	// Negatives are registered, not asserted afterwards (see refuseActivity).
	refuseActivity(t, env, "INSUFFICIENT_STOCK took nothing, so there is no hold to release",
		a.ReleaseInventory, mock.Anything, mock.Anything, mock.Anything)
	refuseActivity(t, env, "an uncaptured hold is voided, never refunded",
		a.RefundPayment, mock.Anything, mock.Anything, mock.Anything)
	refuseActivity(t, env, "a failed reserve must not capture",
		a.CapturePayment, mock.Anything, mock.Anything)

	env.ExecuteWorkflow(OrderFulfillmentWorkflow, testInput())

	if env.GetWorkflowError() == nil {
		t.Fatal("expected error when the reserve fails")
	}
	// Authorized-not-captured → compensate with a void, not a refund.
	env.AssertCalled(t, "VoidPayment", mock.Anything, mock.Anything)
	env.AssertCalled(t, "FailOrder", mock.Anything, mock.Anything, mock.Anything)
}

func TestWorkflow_Payment_CaptureFails_Voids(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	var a *Activities

	env.OnActivity(a.AuthorizePayment, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.ReserveInventory, mock.Anything, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.CreateShipment, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.CapturePayment, mock.Anything, mock.Anything).Return(nonRetryable("capture failed"))
	env.OnActivity(a.CancelShipment, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.ReleaseInventory, mock.Anything, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.VoidPayment, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.FailOrder, mock.Anything, mock.Anything, mock.Anything).Return(nil)

	// Negatives are registered, not asserted afterwards (see refuseActivity).
	refuseActivity(t, env, "a capture that failed took no money to give back",
		a.RefundPayment, mock.Anything, mock.Anything, mock.Anything)

	env.ExecuteWorkflow(OrderFulfillmentWorkflow, testInput())

	if env.GetWorkflowError() == nil {
		t.Fatal("expected error when CapturePayment fails")
	}
	// Still authorized-not-captured → void; full reverse compensation.
	env.AssertCalled(t, "VoidPayment", mock.Anything, mock.Anything)
	env.AssertCalled(t, "CancelShipment", mock.Anything, mock.Anything)
	env.AssertCalled(t, "ReleaseInventory", mock.Anything, mock.Anything, mock.Anything)
	env.AssertCalled(t, "FailOrder", mock.Anything, "42", domain.ReasonPaymentOutcomeUnknown)
}

func TestWorkflow_Payment_ConfirmFails_Refunds(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	var a *Activities

	env.OnActivity(a.AuthorizePayment, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.ReserveInventory, mock.Anything, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.CreateShipment, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.CapturePayment, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.ConfirmOrder, mock.Anything, mock.Anything).Return(nonRetryable("confirm failed"))
	env.OnActivity(a.RefundPayment, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.SendRefundNotification, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.CancelShipment, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.ReleaseInventory, mock.Anything, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.FailOrder, mock.Anything, mock.Anything, mock.Anything).Return(nil)

	// Negatives are registered, not asserted afterwards (see refuseActivity).
	refuseActivity(t, env, "money already captured is refunded, not voided",
		a.VoidPayment, mock.Anything, mock.Anything)

	env.ExecuteWorkflow(OrderFulfillmentWorkflow, testInput())

	if env.GetWorkflowError() == nil {
		t.Fatal("expected error when ConfirmOrder fails")
	}
	// Money was captured → compensate with a refund, not a void, and the refund
	// is followed by a best-effort refund notification.
	// The compensation refund must carry its own identity, or it shares a key with
	// the cancellation remainder refund and the second one is rejected outright.
	env.AssertCalled(t, "RefundPayment", mock.Anything, mock.Anything, refundIDCompensation, mock.Anything)
	env.AssertCalled(t, "SendRefundNotification", mock.Anything, mock.Anything)
	env.AssertCalled(t, "CancelShipment", mock.Anything, mock.Anything)
	env.AssertCalled(t, "FailOrder", mock.Anything, mock.Anything, mock.Anything)
}
