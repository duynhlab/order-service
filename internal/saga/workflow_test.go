package saga

import (
	"testing"

	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
)

func testInput() OrderFulfillmentInput {
	return OrderFulfillmentInput{
		OrderID: "42",
		UserID:  "7",
		Total:   25.0,
		Items:   []ReserveItem{{ProductID: "1", Quantity: 2}},
	}
}

// nonRetryable returns an error the workflow's retry policy won't retry, so
// failure-path tests run a single attempt.
func nonRetryable(msg string) error {
	return temporal.NewNonRetryableApplicationError(msg, "TestError", nil)
}

// The happy path and the reserve/capture/confirm failure paths (with their
// void/refund compensations) live in workflow_payment_test.go — payment is now
// an unconditional part of every saga run. These two cases cover the remaining
// branches: a mid-saga shipment failure and best-effort post-pivot failures.

func TestOrderFulfillmentWorkflow_CreateShipmentFails(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	var a *Activities

	env.OnActivity(a.AuthorizePayment, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.ReserveStock, mock.Anything, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.CreateShipment, mock.Anything, mock.Anything).Return(nonRetryable("carrier down"))
	env.OnActivity(a.ReleaseStock, mock.Anything, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.VoidPayment, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.FailOrder, mock.Anything, mock.Anything).Return(nil)

	env.ExecuteWorkflow(OrderFulfillmentWorkflow, testInput())

	if env.GetWorkflowError() == nil {
		t.Fatal("expected workflow error when CreateShipment fails")
	}
	// Reserve + authorize happened, so both compensate (release stock, void the
	// still-uncaptured hold); order failed. Not captured → no refund.
	env.AssertCalled(t, "ReleaseStock", mock.Anything, mock.Anything, mock.Anything)
	env.AssertCalled(t, "VoidPayment", mock.Anything, mock.Anything)
	env.AssertCalled(t, "FailOrder", mock.Anything, mock.Anything)
	env.AssertNotCalled(t, "CapturePayment", mock.Anything, mock.Anything)
	env.AssertNotCalled(t, "ConfirmOrder", mock.Anything, mock.Anything)
}

func TestOrderFulfillmentWorkflow_PostPivotFailuresAreNonFatal(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	var a *Activities

	env.OnActivity(a.AuthorizePayment, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.ReserveStock, mock.Anything, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.CreateShipment, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.CapturePayment, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.ConfirmOrder, mock.Anything, mock.Anything).Return(nil)
	// All post-pivot steps fail, but the order is already confirmed.
	env.OnActivity(a.SendNotification, mock.Anything, mock.Anything).Return(nonRetryable("smtp down"))
	env.OnActivity(a.SendReceipt, mock.Anything, mock.Anything).Return(nonRetryable("smtp down"))
	env.OnActivity(a.ClearCart, mock.Anything, mock.Anything).Return(nonRetryable("cart down"))

	env.ExecuteWorkflow(OrderFulfillmentWorkflow, testInput())

	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("post-pivot failures must not fail the workflow, got %v", err)
	}
	env.AssertNotCalled(t, "FailOrder", mock.Anything, mock.Anything)
}
