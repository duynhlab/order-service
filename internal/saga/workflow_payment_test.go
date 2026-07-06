package saga

import (
	"testing"

	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/testsuite"
)

func TestWorkflow_Payment_HappyPath(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	var a *Activities

	env.OnActivity(a.AuthorizePayment, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.ReserveStock, mock.Anything, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.CreateShipment, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.CapturePayment, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.ConfirmOrder, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.SendNotification, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.ClearCart, mock.Anything, mock.Anything).Return(nil)

	env.ExecuteWorkflow(OrderFulfillmentWorkflow, testInput())

	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error = %v, want nil", err)
	}
	env.AssertCalled(t, "AuthorizePayment", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	env.AssertCalled(t, "CapturePayment", mock.Anything, mock.Anything)
	env.AssertCalled(t, "ConfirmOrder", mock.Anything, mock.Anything)
	env.AssertNotCalled(t, "VoidPayment", mock.Anything, mock.Anything)
	env.AssertNotCalled(t, "RefundPayment", mock.Anything, mock.Anything, mock.Anything)
	env.AssertNotCalled(t, "FailOrder", mock.Anything, mock.Anything)
}

func TestWorkflow_Payment_AuthorizeFails_NoCompensation(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	var a *Activities

	env.OnActivity(a.AuthorizePayment, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(nonRetryable("declined"))
	env.OnActivity(a.FailOrder, mock.Anything, mock.Anything).Return(nil)

	env.ExecuteWorkflow(OrderFulfillmentWorkflow, testInput())

	if env.GetWorkflowError() == nil {
		t.Fatal("expected error when AuthorizePayment fails")
	}
	// Nothing was authorized/reserved — only the order is failed, no void.
	env.AssertCalled(t, "FailOrder", mock.Anything, mock.Anything)
	env.AssertNotCalled(t, "VoidPayment", mock.Anything, mock.Anything)
	env.AssertNotCalled(t, "ReserveStock", mock.Anything, mock.Anything, mock.Anything)
}

func TestWorkflow_Payment_ReserveStockFails_Voids(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	var a *Activities

	env.OnActivity(a.AuthorizePayment, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.ReserveStock, mock.Anything, mock.Anything, mock.Anything).Return(nonRetryable("no stock"))
	env.OnActivity(a.VoidPayment, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.FailOrder, mock.Anything, mock.Anything).Return(nil)

	env.ExecuteWorkflow(OrderFulfillmentWorkflow, testInput())

	if env.GetWorkflowError() == nil {
		t.Fatal("expected error when ReserveStock fails")
	}
	// Authorized-not-captured → compensate with a void, not a refund.
	env.AssertCalled(t, "VoidPayment", mock.Anything, mock.Anything)
	env.AssertCalled(t, "FailOrder", mock.Anything, mock.Anything)
	env.AssertNotCalled(t, "RefundPayment", mock.Anything, mock.Anything, mock.Anything)
	env.AssertNotCalled(t, "CapturePayment", mock.Anything, mock.Anything)
}

func TestWorkflow_Payment_CaptureFails_Voids(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	var a *Activities

	env.OnActivity(a.AuthorizePayment, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.ReserveStock, mock.Anything, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.CreateShipment, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.CapturePayment, mock.Anything, mock.Anything).Return(nonRetryable("capture failed"))
	env.OnActivity(a.CancelShipment, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.ReleaseStock, mock.Anything, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.VoidPayment, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.FailOrder, mock.Anything, mock.Anything).Return(nil)

	env.ExecuteWorkflow(OrderFulfillmentWorkflow, testInput())

	if env.GetWorkflowError() == nil {
		t.Fatal("expected error when CapturePayment fails")
	}
	// Still authorized-not-captured → void; full reverse compensation.
	env.AssertCalled(t, "VoidPayment", mock.Anything, mock.Anything)
	env.AssertCalled(t, "CancelShipment", mock.Anything, mock.Anything)
	env.AssertCalled(t, "ReleaseStock", mock.Anything, mock.Anything, mock.Anything)
	env.AssertCalled(t, "FailOrder", mock.Anything, mock.Anything)
	env.AssertNotCalled(t, "RefundPayment", mock.Anything, mock.Anything, mock.Anything)
}

func TestWorkflow_Payment_ConfirmFails_Refunds(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	var a *Activities

	env.OnActivity(a.AuthorizePayment, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.ReserveStock, mock.Anything, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.CreateShipment, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.CapturePayment, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.ConfirmOrder, mock.Anything, mock.Anything).Return(nonRetryable("confirm failed"))
	env.OnActivity(a.RefundPayment, mock.Anything, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.CancelShipment, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.ReleaseStock, mock.Anything, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.FailOrder, mock.Anything, mock.Anything).Return(nil)

	env.ExecuteWorkflow(OrderFulfillmentWorkflow, testInput())

	if env.GetWorkflowError() == nil {
		t.Fatal("expected error when ConfirmOrder fails")
	}
	// Money was captured → compensate with a refund, not a void.
	env.AssertCalled(t, "RefundPayment", mock.Anything, mock.Anything, mock.Anything)
	env.AssertCalled(t, "CancelShipment", mock.Anything, mock.Anything)
	env.AssertCalled(t, "FailOrder", mock.Anything, mock.Anything)
	env.AssertNotCalled(t, "VoidPayment", mock.Anything, mock.Anything)
}
