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
func stubPostPivot(env *testsuite.TestWorkflowEnvironment) {
	var a *Activities
	env.OnActivity(a.SendNotification, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.SendReceipt, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.ClearCart, mock.Anything, mock.Anything).Return(nil)
}

// Two ways to express a negative, and neither of them is env.AssertNotCalled on
// its own.
//
// The test environment passes its OWN dummy T to the underlying testify mock
// (sdk@v1.45.0 internal/workflow_testsuite.go), and the two are combined with
// `&&` — so the dummy short-circuits and the real T is never touched. A violated
// negative is handed back as `false` and recorded nowhere. Verified by probe:
// AssertNotCalled returned false while t.Failed() stayed false. Every bare
// AssertNotCalled in this package was decoration.
//
// The RETURN VALUE is sound, though, so:
//
//   - assertNotCalled — checks that value. Use it when the test already stubs the
//     activity, because the assertion can only see calls the mock RECORDED.
//   - refuseActivity — registers the activity with a body that fails the test on
//     entry. Use it when the test does NOT otherwise stub the activity (an
//     unstubbed call is not a recorded call, so assertNotCalled would pass), and
//     when failing at the moment of the call is more useful than after the fact.
func refuseActivity(t *testing.T, env *testsuite.TestWorkflowEnvironment, why string,
	activity interface{}, args ...interface{}) {
	t.Helper()
	env.OnActivity(activity, args...).Run(func(mock.Arguments) {
		t.Errorf("activity was called but %s", why)
	}).Return(nil)
}

// assertNotCalled fails the test when a recorded activity was called.
func assertNotCalled(t *testing.T, env *testsuite.TestWorkflowEnvironment,
	activity string, args ...interface{}) {
	t.Helper()
	if !env.AssertNotCalled(t, activity, args...) {
		t.Errorf("activity %s was called; this path must not reach it", activity)
	}
}

// The product token still MEANS product, and this build must refuse it rather
// than quietly run the inventory branch. Remapping it would release stock
// inventory never reserved and orphan the hold at product-service — the
// invisible-hold split the participant pinning exists to prevent. An empty value
// is the same case: every history carrying one predates the participant column,
// so product is what it actually ran.
//
// The refusal is asserted through the workflow, not through participant()
// directly: what matters is that the saga refuses BEFORE it authorizes money or
// touches stock, and only executing it can show that. Under the SDK's
// BlockWorkflow policy a live worker keeps retrying the failing task, so such a
// saga stalls visibly; the test environment surfaces the same panic as a
// workflow error. Unreachable in practice — pinning keeps those histories on a
// build that still has the branch — which is exactly why it needs a test.
func TestWorkflow_RefusesAProductParticipant(t *testing.T) {
	for _, participant := range []Participant{ParticipantProduct, ""} {
		t.Run("participant="+string(participant), func(t *testing.T) {
			var ts testsuite.WorkflowTestSuite
			env := ts.NewTestWorkflowEnvironment()
			var a *Activities

			refuseActivity(t, env, "a refused saga must authorize no money",
				a.AuthorizePayment, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
			refuseActivity(t, env, "a refused saga must touch no stock",
				a.ReserveInventory, mock.Anything, mock.Anything, mock.Anything)

			in := testInput()
			in.StockParticipant = participant
			env.ExecuteWorkflow(OrderFulfillmentWorkflow, in)

			err := env.GetWorkflowError()
			if err == nil {
				t.Fatal("this build ran a saga pinned to the product-service stock branch")
			}
			if !strings.Contains(err.Error(), "product-service") {
				t.Errorf("error %q does not name the removed branch", err.Error())
			}
		})
	}
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
	// A saga that succeeds end to end never gives stock back.
	refuseActivity(t, env, "a fully successful saga must not release stock",
		a.ReleaseInventory, mock.Anything, mock.Anything, mock.Anything)

	env.ExecuteWorkflow(OrderFulfillmentWorkflow, inventoryInput())

	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("inventory path must succeed, got %v", err)
	}
	env.AssertCalled(t, "ReserveInventory", mock.Anything, mock.Anything, mock.Anything)
	env.AssertCalled(t, "CommitInventory", mock.Anything, mock.Anything)
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
			env.OnActivity(a.RefundPayment, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)
			env.OnActivity(a.SendRefundNotification, mock.Anything, mock.Anything).Return(nil)
			env.OnActivity(a.CancelShipment, mock.Anything, mock.Anything).Return(nil)
			env.OnActivity(a.FailOrder, mock.Anything, mock.Anything, mock.Anything).Return(nil)
			// Pre-pivot failures release; committing stock for an order that
			// failed would strand the goods with no order to ship them against.
			refuseActivity(t, env, "a pre-pivot failure must not commit stock",
				a.CommitInventory, mock.Anything, mock.Anything)

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

	// Negatives are registered, not asserted afterwards (see refuseActivity).
	refuseActivity(t, env, "a confirmed order must not be failed by a commit breach",
		a.FailOrder, mock.Anything, mock.Anything, mock.Anything)
	refuseActivity(t, env, "stock for a confirmed order must not be given back",
		a.ReleaseInventory, mock.Anything, mock.Anything, mock.Anything)
	refuseActivity(t, env, "a commit breach must not refund a confirmed order",
		a.RefundPayment, mock.Anything, mock.Anything, mock.Anything)

	env.ExecuteWorkflow(OrderFulfillmentWorkflow, inventoryInput())

	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("a commit breach must not fail a confirmed order, got %v", err)
	}
	// No compensation, no rollback — and the best-effort tail still runs.
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

	// Negatives are registered, not asserted afterwards (see refuseActivity).
	refuseActivity(t, env, "INSUFFICIENT_STOCK took nothing, so there is no hold to release",
		a.ReleaseInventory, mock.Anything, mock.Anything, mock.Anything)
	refuseActivity(t, env, "a failed reserve must not ship",
		a.CreateShipment, mock.Anything, mock.Anything)
	refuseActivity(t, env, "a failed reserve has nothing to commit",
		a.CommitInventory, mock.Anything, mock.Anything)

	env.ExecuteWorkflow(OrderFulfillmentWorkflow, inventoryInput())

	if env.GetWorkflowError() == nil {
		t.Fatal("expected a workflow error when the reservation is rejected")
	}
	env.AssertCalled(t, "VoidPayment", mock.Anything, mock.Anything)
	// The bounded reason must survive to the terminal write: out-of-stock is
	// INSUFFICIENT_STOCK, never the generic INVENTORY_UNAVAILABLE.
	env.AssertCalled(t, "FailOrder", mock.Anything, "42", domain.ReasonInsufficientStock)
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

	// Negatives are registered, not asserted afterwards (see refuseActivity).
	refuseActivity(t, env, "a confirmed order must not be failed by a commit that never landed",
		a.FailOrder, mock.Anything, mock.Anything, mock.Anything)

	env.ExecuteWorkflow(OrderFulfillmentWorkflow, inventoryInput())

	if !env.IsWorkflowCompleted() {
		t.Fatal("workflow did not complete — the commit retried without an elapsed bound")
	}
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("a commit that never succeeds must not fail a confirmed order, got %v", err)
	}
}

// A token this build does not recognise must stall the workflow rather than be
// guessed at. Guessing product for a saga whose stock is held in inventory would
// release at product-service — stock it never reserved.
func TestOrderFulfillmentWorkflow_UnknownParticipantStalls(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()

	var a *Activities
	// Nothing may have been attempted: no money, no stock.
	refuseActivity(t, env, "an unrecognised token must authorize no money",
		a.AuthorizePayment, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	refuseActivity(t, env, "an unrecognised token must touch no stock",
		a.ReserveInventory, mock.Anything, mock.Anything, mock.Anything)

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
}
