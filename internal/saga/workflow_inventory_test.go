package saga

import (
	"errors"
	"strings"
	"testing"

	"github.com/duynhlab/order-service/internal/core/domain"
	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"

	"github.com/duynhlab/pkg/grpcx"
)

// inventoryInput is testInput() routed at the inventory participant, the way
// fulfillment.Start stamps it for a new order once the flag is flipped.
func inventoryInput() OrderFulfillmentInput {
	in := testInput()
	in.StockParticipant = ParticipantInventory
	return in
}

// stubPrePivot mocks every activity up to and including the pivot as succeeding,
// so a test only has to override the step it cares about.
func stubPrePivotProduct(env *testsuite.TestWorkflowEnvironment) {
	var a *Activities
	env.OnActivity(a.AuthorizePayment, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.ReserveStock, mock.Anything, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.CreateShipment, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.CapturePayment, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.ConfirmOrder, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.Complete, mock.Anything, mock.Anything).Return(nil)
}

func stubPostPivot(env *testsuite.TestWorkflowEnvironment) {
	var a *Activities
	env.OnActivity(a.SendNotification, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.SendReceipt, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.ClearCart, mock.Anything, mock.Anything).Return(nil)
}

// An input with no participant is what every history recorded before this change
// carries. It must keep running the product path exactly as before, and must
// never touch inventory — including the post-pivot commit, which did not exist.
func TestOrderFulfillmentWorkflow_EmptyParticipantStaysOnProduct(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()

	stubPrePivotProduct(env)
	stubPostPivot(env)

	in := testInput()
	if in.StockParticipant != "" {
		t.Fatalf("testInput() must carry no participant, got %q", in.StockParticipant)
	}
	env.ExecuteWorkflow(OrderFulfillmentWorkflow, in)

	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("product path must succeed, got %v", err)
	}
	env.AssertCalled(t, "ReserveStock", mock.Anything, mock.Anything, mock.Anything)
	env.AssertNotCalled(t, "ReserveInventory", mock.Anything, mock.Anything, mock.Anything)
	env.AssertNotCalled(t, "CommitInventory", mock.Anything, mock.Anything)
}

func TestOrderFulfillmentWorkflow_InventoryParticipantReservesAndCommits(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	var a *Activities

	env.OnActivity(a.AuthorizePayment, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.ReserveInventory, mock.Anything, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.CreateShipment, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.CapturePayment, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.ConfirmOrder, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.Complete, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.CommitInventory, mock.Anything, mock.Anything).Return(nil)
	stubPostPivot(env)

	env.ExecuteWorkflow(OrderFulfillmentWorkflow, inventoryInput())

	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("inventory path must succeed, got %v", err)
	}
	env.AssertCalled(t, "ReserveInventory", mock.Anything, mock.Anything, mock.Anything)
	env.AssertCalled(t, "CommitInventory", mock.Anything, mock.Anything)
	// The product stock surface must be untouched on this path — that is the
	// whole point of the migration.
	env.AssertNotCalled(t, "ReserveStock", mock.Anything, mock.Anything, mock.Anything)
	env.AssertNotCalled(t, "ReleaseStock", mock.Anything, mock.Anything, mock.Anything)
}

// Each pre-pivot failure point releases with its own bounded reason code, so the
// inventory movement ledger records WHY stock came back.
func TestOrderFulfillmentWorkflow_InventoryReleaseReasonPerFailurePoint(t *testing.T) {
	tests := []struct {
		name       string
		failStep   string
		wantReason ReleaseReason
	}{
		{"shipment", "CreateShipment", ReleaseReasonShipmentFailed},
		{"capture", "CapturePayment", ReleaseReasonCaptureFailed},
		{"confirm", "ConfirmOrder", ReleaseReasonConfirmFailed},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var ts testsuite.WorkflowTestSuite
			env := ts.NewTestWorkflowEnvironment()
			var a *Activities

			env.OnActivity(a.AuthorizePayment, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)
			env.OnActivity(a.ReserveInventory, mock.Anything, mock.Anything, mock.Anything).Return(nil)
			env.OnActivity(a.ReleaseInventory, mock.Anything, mock.Anything, mock.Anything).Return(nil)
			env.OnActivity(a.VoidPayment, mock.Anything, mock.Anything).Return(nil)
			env.OnActivity(a.RefundPayment, mock.Anything, mock.Anything, mock.Anything).Return(nil)
			env.OnActivity(a.SendRefundNotification, mock.Anything, mock.Anything).Return(nil)
			env.OnActivity(a.CancelShipment, mock.Anything, mock.Anything).Return(nil)
			env.OnActivity(a.FailOrder, mock.Anything, mock.Anything, mock.Anything).Return(nil)

			failed := nonRetryable(tt.failStep + " down")
			switch tt.failStep {
			case "CreateShipment":
				env.OnActivity(a.CreateShipment, mock.Anything, mock.Anything).Return(failed)
			case "CapturePayment":
				env.OnActivity(a.CreateShipment, mock.Anything, mock.Anything).Return(nil)
				env.OnActivity(a.CapturePayment, mock.Anything, mock.Anything).Return(failed)
			case "ConfirmOrder":
				env.OnActivity(a.CreateShipment, mock.Anything, mock.Anything).Return(nil)
				env.OnActivity(a.CapturePayment, mock.Anything, mock.Anything).Return(nil)
				env.OnActivity(a.ConfirmOrder, mock.Anything, mock.Anything).Return(failed)
			}

			env.ExecuteWorkflow(OrderFulfillmentWorkflow, inventoryInput())

			if env.GetWorkflowError() == nil {
				t.Fatalf("expected a workflow error when %s fails", tt.failStep)
			}
			env.AssertCalled(t, "ReleaseInventory", mock.Anything, mock.Anything, tt.wantReason)
			env.AssertNotCalled(t, "ReleaseStock", mock.Anything, mock.Anything, mock.Anything)
			env.AssertNotCalled(t, "CommitInventory", mock.Anything, mock.Anything)
		})
	}
}

// After the pivot the order IS confirmed. A commit that comes back with a
// business rejection is an invariant breach, not a reason to roll the order
// back: the money is captured and the customer has been told. The workflow must
// stay successful and leave the repair to the reconciler.
func TestOrderFulfillmentWorkflow_CommitBreachDoesNotFailConfirmedOrder(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	var a *Activities

	env.OnActivity(a.AuthorizePayment, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.ReserveInventory, mock.Anything, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.CreateShipment, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.CapturePayment, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.ConfirmOrder, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.Complete, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.CommitInventory, mock.Anything, mock.Anything).Return(nonRetryable("INVALID_TRANSITION"))
	stubPostPivot(env)

	env.ExecuteWorkflow(OrderFulfillmentWorkflow, inventoryInput())

	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("a commit breach must not fail a confirmed order, got %v", err)
	}
	// No compensation, no rollback — and the best-effort tail still runs.
	env.AssertNotCalled(t, "FailOrder", mock.Anything, mock.Anything, mock.Anything)
	env.AssertNotCalled(t, "ReleaseInventory", mock.Anything, mock.Anything, mock.Anything)
	env.AssertNotCalled(t, "RefundPayment", mock.Anything, mock.Anything, mock.Anything)
	env.AssertCalled(t, "SendNotification", mock.Anything, mock.Anything)
}

// INSUFFICIENT_STOCK is a definite "nothing was taken" verdict, so releasing
// would be a pointless RPC on every out-of-stock checkout. Only the payment hold
// compensates. Contrast with the ambiguous case below.
func TestOrderFulfillmentWorkflow_InsufficientStockReleasesNothing(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	var a *Activities

	env.OnActivity(a.AuthorizePayment, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.ReserveInventory, mock.Anything, mock.Anything, mock.Anything).Return(
		temporal.NewNonRetryableApplicationError("out of stock", grpcx.ReasonInsufficientStock, nil))
	env.OnActivity(a.VoidPayment, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.FailOrder, mock.Anything, mock.Anything, mock.Anything).Return(nil)

	env.ExecuteWorkflow(OrderFulfillmentWorkflow, inventoryInput())

	if env.GetWorkflowError() == nil {
		t.Fatal("expected a workflow error when the reservation is rejected")
	}
	env.AssertCalled(t, "VoidPayment", mock.Anything, mock.Anything)
	// The bounded reason must survive to the terminal write: out-of-stock is
	// INSUFFICIENT_STOCK, never the generic INVENTORY_UNAVAILABLE.
	env.AssertCalled(t, "FailOrder", mock.Anything, "42", domain.ReasonInsufficientStock)
	env.AssertNotCalled(t, "ReleaseInventory", mock.Anything, mock.Anything, mock.Anything)
	env.AssertNotCalled(t, "CreateShipment", mock.Anything, mock.Anything)
	env.AssertNotCalled(t, "CommitInventory", mock.Anything, mock.Anything)
}

// Any reserve failure that is NOT a definite "nothing was taken" verdict is
// ambiguous: the reservation may exist server-side with its response lost. v1
// reservations never auto-expire and the reconciler only looks at CONFIRMED
// orders, so skipping the release would hold that stock forever against a failed
// order.
func TestOrderFulfillmentWorkflow_AmbiguousReserveFailureReleases(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	var a *Activities

	env.OnActivity(a.AuthorizePayment, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)
	// A transient class that exhausted its budget — not INSUFFICIENT_STOCK.
	env.OnActivity(a.ReserveInventory, mock.Anything, mock.Anything, mock.Anything).Return(nonRetryable("inventory unavailable"))
	env.OnActivity(a.ReleaseInventory, mock.Anything, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.VoidPayment, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.FailOrder, mock.Anything, mock.Anything, mock.Anything).Return(nil)

	env.ExecuteWorkflow(OrderFulfillmentWorkflow, inventoryInput())

	if env.GetWorkflowError() == nil {
		t.Fatal("expected a workflow error when the reservation fails")
	}
	env.AssertCalled(t, "ReleaseInventory", mock.Anything, mock.Anything, ReleaseReasonReserveFailed)
	env.AssertCalled(t, "VoidPayment", mock.Anything, mock.Anything)
}

// A retryable commit failure must still SETTLE. Unlimited attempts without an
// elapsed bound would park the workflow here forever, and the breach report —
// metric and log — is only reached once the activity settles, so the platform
// would emit nothing at all while an order sat confirmed with stock RESERVED.
// The test environment auto-advances time, so ScheduleToCloseTimeout is what
// makes this terminate.
func TestOrderFulfillmentWorkflow_RetryableCommitFailureStillSettles(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	var a *Activities

	env.OnActivity(a.AuthorizePayment, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.ReserveInventory, mock.Anything, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.CreateShipment, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.CapturePayment, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.ConfirmOrder, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.Complete, mock.Anything, mock.Anything).Return(nil)
	stubPostPivot(env)
	// Retryable forever: only the elapsed bound can end this.
	env.OnActivity(a.CommitInventory, mock.Anything, mock.Anything).Return(errors.New("inventory unavailable"))

	env.ExecuteWorkflow(OrderFulfillmentWorkflow, inventoryInput())

	if !env.IsWorkflowCompleted() {
		t.Fatal("workflow did not complete — the commit retried without an elapsed bound")
	}
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("a commit that never succeeds must not fail a confirmed order, got %v", err)
	}
	env.AssertNotCalled(t, "FailOrder", mock.Anything, mock.Anything, mock.Anything)
}

// A token this build does not recognise must stall the workflow rather than be
// guessed at. Guessing product for a saga whose stock is held in inventory would
// release at product-service — stock it never reserved.
func TestOrderFulfillmentWorkflow_UnknownParticipantStalls(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()

	in := testInput()
	in.StockParticipant = Participant("inventory_regional")
	env.ExecuteWorkflow(OrderFulfillmentWorkflow, in)

	err := env.GetWorkflowError()
	if err == nil {
		t.Fatal("expected the workflow task to fail on an unknown participant")
	}
	if !strings.Contains(err.Error(), "unknown stock participant") {
		t.Errorf("error %q does not name the unknown participant", err.Error())
	}
	// Nothing may have been attempted: no money, no stock.
	env.AssertNotCalled(t, "AuthorizePayment", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	env.AssertNotCalled(t, "ReserveStock", mock.Anything, mock.Anything, mock.Anything)
	env.AssertNotCalled(t, "ReserveInventory", mock.Anything, mock.Anything, mock.Anything)
}
