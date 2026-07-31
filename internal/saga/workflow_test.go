package saga

import (
	"strings"
	"testing"

	"github.com/duynhlab/order-service/internal/core/domain"
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
	env.OnActivity(a.CancelShipment, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.ReleaseStock, mock.Anything, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.VoidPayment, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.FailOrder, mock.Anything, mock.Anything, mock.Anything).Return(nil)

	env.ExecuteWorkflow(OrderFulfillmentWorkflow, testInput())

	if env.GetWorkflowError() == nil {
		t.Fatal("expected workflow error when CreateShipment fails")
	}
	// Reserve + authorize happened, so both compensate (release stock, void
	// the still-uncaptured hold); the possibly-created shipment is cancelled
	// (idempotent no-op if the create never landed); order failed with the
	// shipment reason. Not captured → no refund.
	env.AssertCalled(t, "CancelShipment", mock.Anything, mock.Anything)
	env.AssertCalled(t, "ReleaseStock", mock.Anything, mock.Anything, mock.Anything)
	env.AssertCalled(t, "VoidPayment", mock.Anything, mock.Anything)
	env.AssertCalled(t, "FailOrder", mock.Anything, "42", domain.ReasonShipmentUnavailable)
	env.AssertNotCalled(t, "CapturePayment", mock.Anything, mock.Anything)
	env.AssertNotCalled(t, "ConfirmOrder", mock.Anything, mock.Anything)
	// Contract's other half: a product-path compensation must never reach
	// inventory. This is the assertion that catches a future refactor inverting
	// the branch in releaseStock.
	env.AssertNotCalled(t, "ReleaseInventory", mock.Anything, mock.Anything, mock.Anything)
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
	env.OnActivity(a.Complete, mock.Anything, mock.Anything).Return(nil)
	// All post-pivot steps fail, but the order is already confirmed.
	env.OnActivity(a.SendNotification, mock.Anything, mock.Anything).Return(nonRetryable("smtp down"))
	env.OnActivity(a.SendReceipt, mock.Anything, mock.Anything).Return(nonRetryable("smtp down"))
	env.OnActivity(a.ClearCart, mock.Anything, mock.Anything).Return(nonRetryable("cart down"))

	env.ExecuteWorkflow(OrderFulfillmentWorkflow, testInput())

	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("post-pivot failures must not fail the workflow, got %v", err)
	}
	env.AssertNotCalled(t, "FailOrder", mock.Anything, mock.Anything, mock.Anything)
}

// RFC-0021 P5: the terminal-bookkeeping contract. `failed` asserts that
// compensation CONVERGED; when a compensation exhausts its retries the order
// parks in manual_review instead, and FailOrder is never asserted.

func TestWorkflow_CompensationExhaustion_ParksManualReview(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	var a *Activities

	env.OnActivity(a.AuthorizePayment, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.ReserveStock, mock.Anything, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.CreateShipment, mock.Anything, mock.Anything).Return(nonRetryable("carrier down"))
	env.OnActivity(a.ReleaseStock, mock.Anything, mock.Anything, mock.Anything).Return(nil)
	// The void never lands: side effects are unaccounted for.
	env.OnActivity(a.VoidPayment, mock.Anything, mock.Anything).Return(nonRetryable("payment gone"))
	env.OnActivity(a.MarkManualReview, mock.Anything, mock.Anything, mock.Anything).Return(nil)

	env.ExecuteWorkflow(OrderFulfillmentWorkflow, testInput())

	if env.GetWorkflowError() == nil {
		t.Fatal("expected workflow error")
	}
	env.AssertCalled(t, "MarkManualReview", mock.Anything, "42", domain.ReasonCompensationIncomplete)
	env.AssertNotCalled(t, "FailOrder", mock.Anything, mock.Anything, mock.Anything)
}

func TestWorkflow_FailOrderExhaustion_ParksManualReview(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	var a *Activities

	// Authorize fails: no compensations owed, so the terminal write is
	// FailOrder — which itself cannot land.
	env.OnActivity(a.AuthorizePayment, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nonRetryable("declined"))
	env.OnActivity(a.FailOrder, mock.Anything, mock.Anything, mock.Anything).Return(nonRetryable("db refuses"))
	env.OnActivity(a.MarkManualReview, mock.Anything, mock.Anything, mock.Anything).Return(nil)

	env.ExecuteWorkflow(OrderFulfillmentWorkflow, testInput())

	if env.GetWorkflowError() == nil {
		t.Fatal("expected workflow error")
	}
	env.AssertCalled(t, "MarkManualReview", mock.Anything, "42", domain.ReasonCompensationIncomplete)
}

func TestWorkflow_ManualReviewExhaustion_FailsTheWorkflow(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	var a *Activities

	env.OnActivity(a.AuthorizePayment, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nonRetryable("declined"))
	env.OnActivity(a.FailOrder, mock.Anything, mock.Anything, mock.Anything).Return(nonRetryable("db refuses"))
	env.OnActivity(a.MarkManualReview, mock.Anything, mock.Anything, mock.Anything).Return(nonRetryable("db still refuses"))

	env.ExecuteWorkflow(OrderFulfillmentWorkflow, testInput())

	err := env.GetWorkflowError()
	if err == nil {
		t.Fatal("expected the workflow to fail when no terminal state can land")
	}
	// The order is left pending on purpose — visible via workflow-failure
	// alerting and pending-age, never silently mislabeled.
	if !strings.Contains(err.Error(), "terminal bookkeeping") {
		t.Errorf("workflow error = %v, want the terminal-bookkeeping failure", err)
	}
}

// The bounded reason recorded on failure comes from the recorded activity
// error, deterministically: a real provider decline carries the
// PaymentDeclined application-error type.
func TestWorkflow_FailureReasons_DeriveFromRecordedErrors(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	var a *Activities

	env.OnActivity(a.AuthorizePayment, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(temporal.NewNonRetryableApplicationError("payment not authorized", "PaymentDeclined", nil))
	env.OnActivity(a.FailOrder, mock.Anything, mock.Anything, mock.Anything).Return(nil)

	env.ExecuteWorkflow(OrderFulfillmentWorkflow, testInput())

	env.AssertCalled(t, "FailOrder", mock.Anything, "42", domain.ReasonPaymentDeclined)
}

// The happy tail now ends in Complete (confirmed → completed) — after the
// best-effort steps and (on the inventory path) after CommitInventory.
func TestWorkflow_HappyTail_Completes(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	var a *Activities

	env.OnActivity(a.AuthorizePayment, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.ReserveStock, mock.Anything, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.CreateShipment, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.CapturePayment, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.ConfirmOrder, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.SendNotification, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.SendReceipt, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.ClearCart, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.Complete, mock.Anything, mock.Anything).Return(nil)

	env.ExecuteWorkflow(OrderFulfillmentWorkflow, testInput())

	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error = %v", err)
	}
	env.AssertCalled(t, "Complete", mock.Anything, "42")
}

// A Complete that never lands must not fail a fulfilled order.
func TestWorkflow_CompleteFailure_IsBestEffort(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	var a *Activities

	env.OnActivity(a.AuthorizePayment, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.ReserveStock, mock.Anything, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.CreateShipment, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.CapturePayment, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.ConfirmOrder, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.SendNotification, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.SendReceipt, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.ClearCart, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.Complete, mock.Anything, mock.Anything).Return(nonRetryable("db refuses"))

	env.ExecuteWorkflow(OrderFulfillmentWorkflow, testInput())

	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("a failed Complete must not fail the workflow: %v", err)
	}
}
