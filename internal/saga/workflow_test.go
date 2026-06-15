package saga

import (
	"testing"

	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
)

func testInput() OrderFulfillmentInput {
	return OrderFulfillmentInput{
		OrderID:   "42",
		UserID:    "7",
		Total:     25.0,
		Items:     []ReserveItem{{ProductID: "1", Quantity: 2}},
		AuthToken: "Bearer token",
	}
}

// nonRetryable returns an error the workflow's retry policy won't retry, so
// failure-path tests run a single attempt.
func nonRetryable(msg string) error {
	return temporal.NewNonRetryableApplicationError(msg, "TestError", nil)
}

func TestOrderFulfillmentWorkflow_HappyPath(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	var a *Activities

	env.OnActivity(a.ReserveStock, mock.Anything, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.CreateShipment, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.ConfirmOrder, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.SendNotification, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.ClearCart, mock.Anything, mock.Anything).Return(nil)

	env.ExecuteWorkflow(OrderFulfillmentWorkflow, testInput())

	if !env.IsWorkflowCompleted() {
		t.Fatal("workflow did not complete")
	}
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error = %v, want nil", err)
	}
	env.AssertCalled(t, "ConfirmOrder", mock.Anything, mock.Anything)
	env.AssertNotCalled(t, "FailOrder", mock.Anything, mock.Anything)
	env.AssertNotCalled(t, "ReleaseStock", mock.Anything, mock.Anything, mock.Anything)
}

func TestOrderFulfillmentWorkflow_ReserveStockFails(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	var a *Activities

	env.OnActivity(a.ReserveStock, mock.Anything, mock.Anything, mock.Anything).Return(nonRetryable("insufficient stock"))
	env.OnActivity(a.FailOrder, mock.Anything, mock.Anything).Return(nil)

	env.ExecuteWorkflow(OrderFulfillmentWorkflow, testInput())

	if env.GetWorkflowError() == nil {
		t.Fatal("expected workflow error when ReserveStock fails")
	}
	// Nothing was reserved/shipped, so only the order is failed.
	env.AssertCalled(t, "FailOrder", mock.Anything, mock.Anything)
	env.AssertNotCalled(t, "ReleaseStock", mock.Anything, mock.Anything, mock.Anything)
	env.AssertNotCalled(t, "CreateShipment", mock.Anything, mock.Anything)
	env.AssertNotCalled(t, "ConfirmOrder", mock.Anything, mock.Anything)
}

func TestOrderFulfillmentWorkflow_CreateShipmentFails(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	var a *Activities

	env.OnActivity(a.ReserveStock, mock.Anything, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.CreateShipment, mock.Anything, mock.Anything).Return(nonRetryable("carrier down"))
	env.OnActivity(a.ReleaseStock, mock.Anything, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.FailOrder, mock.Anything, mock.Anything).Return(nil)

	env.ExecuteWorkflow(OrderFulfillmentWorkflow, testInput())

	if env.GetWorkflowError() == nil {
		t.Fatal("expected workflow error when CreateShipment fails")
	}
	// Reserve happened, so it must be compensated; order failed.
	env.AssertCalled(t, "ReleaseStock", mock.Anything, mock.Anything, mock.Anything)
	env.AssertCalled(t, "FailOrder", mock.Anything, mock.Anything)
	env.AssertNotCalled(t, "ConfirmOrder", mock.Anything, mock.Anything)
}

func TestOrderFulfillmentWorkflow_ConfirmOrderFails(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	var a *Activities

	env.OnActivity(a.ReserveStock, mock.Anything, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.CreateShipment, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.ConfirmOrder, mock.Anything, mock.Anything).Return(nonRetryable("db down"))
	env.OnActivity(a.CancelShipment, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.ReleaseStock, mock.Anything, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.FailOrder, mock.Anything, mock.Anything).Return(nil)

	env.ExecuteWorkflow(OrderFulfillmentWorkflow, testInput())

	if env.GetWorkflowError() == nil {
		t.Fatal("expected workflow error when ConfirmOrder fails")
	}
	// Both prior steps compensate in reverse; order failed.
	env.AssertCalled(t, "CancelShipment", mock.Anything, mock.Anything)
	env.AssertCalled(t, "ReleaseStock", mock.Anything, mock.Anything, mock.Anything)
	env.AssertCalled(t, "FailOrder", mock.Anything, mock.Anything)
}

func TestOrderFulfillmentWorkflow_PostPivotFailuresAreNonFatal(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	var a *Activities

	env.OnActivity(a.ReserveStock, mock.Anything, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.CreateShipment, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.ConfirmOrder, mock.Anything, mock.Anything).Return(nil)
	// Both post-pivot steps fail, but the order is already confirmed.
	env.OnActivity(a.SendNotification, mock.Anything, mock.Anything).Return(nonRetryable("smtp down"))
	env.OnActivity(a.ClearCart, mock.Anything, mock.Anything).Return(nonRetryable("cart down"))

	env.ExecuteWorkflow(OrderFulfillmentWorkflow, testInput())

	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("post-pivot failures must not fail the workflow, got %v", err)
	}
	env.AssertNotCalled(t, "FailOrder", mock.Anything, mock.Anything)
}
