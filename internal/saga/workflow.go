// Package saga implements the Temporal order-fulfillment workflow and its
// activities. The workflow is started by the web layer right after the order row
// commits (status "pending") and drives fulfillment durably:
//
//	AuthorizePayment -> ReserveStock -> CreateShipment -> CapturePayment ->
//	ConfirmOrder (pivot) -> SendNotification -> ClearCart
//
// The stock steps route to one of two participants, chosen per workflow by
// OrderFulfillmentInput.StockParticipant (RFC-0021 P3, homelab ADR-027/ADR-030):
// product-service (ReserveStock/ReleaseStock) or inventory-service
// (ReserveInventory/ReleaseInventory + a post-pivot CommitInventory). Both
// branches stay until RFC-0021 phase 4 retires the product surface.
//
// Steps before the pivot compensate in reverse on failure (ReleaseStock /
// CancelShipment, and VoidPayment before capture / RefundPayment after) and the
// order is marked "failed". Once ConfirmOrder succeeds the order is "confirmed";
// the remaining steps are best-effort and never roll the order back. See
// homelab/docs/api/temporal-order-fulfillment.md.
package saga

import (
	"errors"
	"fmt"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/duynhlab/order-service/internal/core/domain"
	"github.com/duynhlab/pkg/grpcx"
)

// TaskQueue is the Temporal task queue the order worker polls.
const TaskQueue = "order-fulfillment"

// WorkflowID returns the dedup workflow ID for an order's fulfillment.
func WorkflowID(orderID string) string { return "order-fulfillment-" + orderID }

// Participant names the service that owns a saga's stock writes. A distinct type
// rather than a bare string so it cannot be transposed with the other strings it
// travels next to (task queue, order id) at a call site.
type Participant string

// Stock participants — the ORDER_STOCK_PARTICIPANT enum values, stamped into the
// workflow input at start (see internal/fulfillment).
const (
	ParticipantProduct   Participant = "product"
	ParticipantInventory Participant = "inventory"
)

// ReserveItem is a product/quantity pair for the ReserveStock step.
type ReserveItem struct {
	ProductID string
	Quantity  int
}

// NotifyInput is the payload for the SendNotification step.
type NotifyInput struct {
	OrderID string
	UserID  string
	Total   int64 // minor units
}

// OrderFulfillmentInput is the workflow input. The best-effort cart-clear step
// uses UserID against cart's internal (NetworkPolicy-fenced) endpoint, so no
// bearer token is carried in the workflow input/history.
type OrderFulfillmentInput struct {
	OrderID string
	UserID  string
	Total   int64 // minor units
	Items   []ReserveItem

	// PaymentMethod is the checkout's opaque payment token. Empty = the
	// authorize activity falls back to its demo token (API-created orders,
	// older clients).
	PaymentMethod string

	// StockParticipant pins which service handles this saga's stock writes for
	// its whole lifetime. Empty means product — every history recorded before
	// RFC-0021 P3 carries no value, and those sagas must keep reserving,
	// releasing and (not) committing exactly where they started.
	//
	// It is stamped ONCE at start from the ORDER_STOCK_PARTICIPANT flag and read
	// only from the input afterwards. The worker never reads the flag, so
	// reverting it redirects new sagas only: a saga that reserved in inventory
	// always compensates and commits in inventory, never half in each.
	StockParticipant Participant
}

// participant resolves the saga's pinned participant.
//
// An unrecognised token PANICS, which stalls the workflow instead of guessing.
// Defaulting to product would be the opposite of conservative: the enum grows on
// the inventory side (regions, warehouses), so a token this build does not know
// is far more likely to mean inventory than product. Guessing product for a saga
// whose stock is held in inventory would release at product-service — stock it
// never reserved — and orphan the inventory hold, which is precisely the split
// the pinning exists to prevent.
//
// A panic in workflow code fails the WORKFLOW TASK, not the workflow: the SDK's
// default WorkflowPanicPolicy is BlockWorkflow, which "just logs error but
// doesn't fail workflow" (sdk@v1.44.1/internal/worker.go:223). Temporal keeps
// retrying the task, so the saga stalls — loudly and visibly as a workflow with
// a failing task — until a build that understands the token serves the queue.
// Nothing is lost and nothing is corrupted.
func (in OrderFulfillmentInput) participant() Participant {
	switch in.StockParticipant {
	case "", ParticipantProduct:
		// Empty is every history recorded before RFC-0021 P3.
		return ParticipantProduct
	case ParticipantInventory:
		return ParticipantInventory
	default:
		panic(fmt.Sprintf("saga: unknown stock participant %q — this build cannot safely run this workflow", in.StockParticipant))
	}
}

// usesInventory reports whether this saga's stock writes go to inventory-service.
func (in OrderFulfillmentInput) usesInventory() bool {
	return in.participant() == ParticipantInventory
}

// activityOptions applies a bounded retry to every activity. Business
// rejections (e.g. insufficient stock) are returned as non-retryable
// application errors by the activity, so they fail fast instead of retrying.
func activityOptions() workflow.ActivityOptions {
	return workflow.ActivityOptions{
		StartToCloseTimeout: 30 * time.Second,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    time.Second,
			BackoffCoefficient: 2.0,
			MaximumInterval:    30 * time.Second,
			MaximumAttempts:    5,
		},
	}
}

// commitActivityOptions retries CommitInventory with unlimited ATTEMPTS but a
// bounded total ELAPSED time. Past the pivot the order is confirmed, the money is
// captured and the customer has been told, so the reservation must converge to
// COMMITTED — a handful of attempts would give up on a routine inventory
// restart.
//
// The elapsed bound is the load-bearing half, and unlimited attempts alone are
// not enough. grpcx classifies Internal/Unknown/Unavailable/ResourceExhausted/
// Aborted as RETRYABLE, and a panicking inventory handler surfaces as Internal
// with no reason at all. So a deterministic bug in Commit for one reservation is
// retryable forever: without ScheduleToCloseTimeout the activity never settles,
// the workflow parks here permanently, and the breach report below — metric AND
// log — is never reached. The platform would emit nothing while an order sits
// confirmed with stock still merely RESERVED. Thirty minutes rides out a normal
// inventory rollout while guaranteeing the workflow reaches its own reporting
// branch, after which the reconciler owns the repair.
//
// StartToCloseTimeout stays at the saga's usual 30s rather than being tightened:
// a shorter one turns an inventory latency regression into an amplifier, where
// every attempt is cancelled at the timeout and immediately re-issued against a
// dependency that is merely slow.
func commitActivityOptions() workflow.ActivityOptions {
	return workflow.ActivityOptions{
		StartToCloseTimeout:    30 * time.Second,
		ScheduleToCloseTimeout: 30 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    time.Second,
			BackoffCoefficient: 2.0,
			MaximumInterval:    time.Minute,
			MaximumAttempts:    0,
		},
	}
}

// compensationActivityOptions retries compensations harder than forward steps: a
// compensation that gives up leaves money held or stock reserved with nothing
// left to drive it, while a forward step that gives up simply fails the saga.
// Applied to EVERY compensation — void, refund, release, cancel-shipment and
// fail-order — not just the stock one; the money compensations are the ones the
// rationale is really about.
func compensationActivityOptions() workflow.ActivityOptions {
	return workflow.ActivityOptions{
		StartToCloseTimeout: 30 * time.Second,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    time.Second,
			BackoffCoefficient: 2.0,
			MaximumInterval:    time.Minute,
			MaximumAttempts:    10,
		},
	}
}

// OrderFulfillmentWorkflow orchestrates the order-fulfillment saga.
func OrderFulfillmentWorkflow(ctx workflow.Context, in OrderFulfillmentInput) error {
	ctx = workflow.WithActivityOptions(ctx, activityOptions())
	log := workflow.GetLogger(ctx)

	// Resolve the participant FIRST. An unrecognised token must stall the saga
	// before any money is authorized or any stock is touched — discovering it at
	// the reserve step would leave a payment hold behind on a workflow that then
	// blocks indefinitely. The value is re-derived where needed; the call here
	// exists for its early panic.
	_ = in.participant()

	// Nil receiver: ExecuteActivity only needs the method's identity; Temporal
	// invokes the registered *Activities instance at execution time.
	var a *Activities

	// Step 0 — authorize the payment hold (pre-pivot). Nothing to compensate yet.
	if err := workflow.ExecuteActivity(ctx, a.AuthorizePayment, in.OrderID, in.UserID, in.Total, in.PaymentMethod).Get(ctx, nil); err != nil {
		log.Error("AuthorizePayment failed; marking order failed", "order_id", in.OrderID, "error", err)
		reason := paymentFailReason(err)
		recordStage(ctx, domain.ProcessingUpdate{OrderID: in.OrderID,
			Stage: domain.StageCompensating, LastErrorCode: string(reason)})
		// A decline is a definitive "no hold exists". Anything else is
		// ambiguous — the authorize may have committed with its response lost
		// — so a void runs before `failed` may be asserted. Void is
		// idempotent and voiding a nonexistent hold is a tolerable no-op.
		var voidErr error
		if reason != domain.ReasonPaymentDeclined {
			voidErr = voidPayment(ctx, in.OrderID)
		}
		if termErr := finishFailed(ctx, in, reason, outcomeFailed, voidErr); termErr != nil {
			return termErr
		}
		return fmt.Errorf("authorize payment: %w", err)
	}

	recordStage(ctx, domain.ProcessingUpdate{OrderID: in.OrderID,
		Stage: domain.StagePaymentAuthorized, LastStep: stepAuthorizePayment})

	if err := runPrePivotSteps(ctx, in); err != nil {
		return err
	}

	// Step 4 (pivot) — confirm the order. Payment is already captured, so the
	// compensation is a refund (not a void): cancel shipment + release stock.
	if err := workflow.ExecuteActivity(ctx, a.ConfirmOrder, in.OrderID).Get(ctx, nil); err != nil {
		log.Error("ConfirmOrder failed; compensating", "order_id", in.OrderID, "error", err)
		recordStage(ctx, domain.ProcessingUpdate{OrderID: in.OrderID,
			Stage: domain.StageCompensating, LastErrorCode: string(domain.ReasonConfirmationFailed)})
		refundErr := refundPayment(ctx, in)
		shipErr := cancelShipment(ctx, in.OrderID)
		relErr := releaseStock(ctx, in, ReleaseReasonConfirmFailed)
		if termErr := finishFailed(ctx, in, domain.ReasonConfirmationFailed, outcomeCompensated, refundErr, shipErr, relErr); termErr != nil {
			return termErr
		}
		return fmt.Errorf("confirm order: %w", err)
	}
	recordSagaOutcome(ctx, outcomeConfirmed)
	recordStage(ctx, domain.ProcessingUpdate{OrderID: in.OrderID,
		Stage: domain.StageOrderConfirmed, LastStep: stepConfirmOrder})
	runFulfillmentTail(ctx, in, workflow.Now(ctx))

	return nil
}

// runFulfillmentTail is everything past the pivot: the order is confirmed,
// so every step here is best-effort — logged, counted where alertable, and
// never a reason to roll a confirmed order back.
func runFulfillmentTail(ctx workflow.Context, in OrderFulfillmentInput, confirmedAt time.Time) {
	var a *Activities
	log := workflow.GetLogger(ctx)

	recordStage(ctx, domain.ProcessingUpdate{OrderID: in.OrderID, Stage: domain.StagePostProcessing})

	if err := workflow.ExecuteActivity(ctx, a.SendNotification,
		NotifyInput{OrderID: in.OrderID, UserID: in.UserID, Total: in.Total}).Get(ctx, nil); err != nil {
		log.Warn("SendNotification failed (non-fatal)", "order_id", in.OrderID, "error", err)
	}

	// Payment receipt (best-effort) — money was captured before the pivot.
	if err := workflow.ExecuteActivity(ctx, a.SendReceipt,
		NotifyInput{OrderID: in.OrderID, UserID: in.UserID, Total: in.Total}).Get(ctx, nil); err != nil {
		log.Warn("SendReceipt failed (non-fatal)", "order_id", in.OrderID, "error", err)
	}

	if in.UserID != "" {
		if err := workflow.ExecuteActivity(ctx, a.ClearCart, in.UserID).Get(ctx, nil); err != nil {
			log.Warn("ClearCart failed (non-fatal)", "order_id", in.OrderID, "error", err)
		}
	}

	// Turn the reservation into a permanent decrement. Only on the
	// inventory path; the product path decremented at reserve time and has
	// nothing to commit.
	//
	// It runs AFTER the customer-visible tail, deliberately. Putting it in front
	// would have meant no confirmation email and an un-cleared cart for as long
	// as inventory was degraded — to the customer an apparently failed checkout,
	// so they check out again. That second order gets a new id, so the
	// workflow-id dedup does not stop it: a second authorize and a second
	// capture. A double charge is a far worse outcome than a confirmation email
	// that precedes an internal bookkeeping step, and the email is not lying
	// either — the order IS confirmed and paid. Availability is already protected
	// by the reservation itself (ATP excludes reserved stock), so an uncommitted
	// reservation cannot oversell; it is a stale row for the reconciler.
	if in.usesInventory() {
		// The stage asserts the commit SUCCEEDED — an uncommitted reservation
		// is the reconciler's business and must not read as done.
		if err := commitInventory(ctx, in, confirmedAt); err == nil {
			recordStage(ctx, domain.ProcessingUpdate{OrderID: in.OrderID,
				Stage: domain.StageInventoryCommitted, LastStep: stepCommitInventory})
		}
	}

	// Last step — confirmed → completed: the fulfillment tail ran. Completion
	// policy v1 is "workflow done"; it upgrades when shipping grows dispatch
	// states. Best-effort like the rest of the tail: a Complete that never
	// lands leaves the order confirmed, which the reconciler treats as
	// settled-if-stock-agrees — counted so it can be alerted, not silent.
	if completeOrder(ctx, in.OrderID) {
		recordStage(ctx, domain.ProcessingUpdate{OrderID: in.OrderID,
			Stage: domain.StageDone, LastStep: stepComplete})
	}
	// A refused Complete means the order moved on (cancelling) — the
	// cancellation workflow owns the projection from here; a failed one
	// leaves the last honest stage standing. Neither may claim DONE.
}

// runPrePivotSteps drives reserve → shipment → capture, compensating and
// finishing the order on failure. A nil return means the saga reached the
// pivot; a non-nil return is the workflow's terminal error.
func runPrePivotSteps(ctx workflow.Context, in OrderFulfillmentInput) error {
	var a *Activities
	log := workflow.GetLogger(ctx)
	participant := in.participant()

	// Step 1 — reserve stock at whichever participant this saga is pinned to.
	// Compensate: void the hold, and on the inventory path return an ambiguous
	// hold (see releaseAmbiguousReserve).
	if err := reserveStock(ctx, in); err != nil {
		log.Error("stock reservation failed; compensating", "order_id", in.OrderID,
			"participant", participant, "error", err)
		recordStage(ctx, domain.ProcessingUpdate{OrderID: in.OrderID,
			Stage: domain.StageCompensating, LastErrorCode: string(stockFailReason(err))})
		relErr := releaseAmbiguousReserve(ctx, in, err)
		voidErr := voidPayment(ctx, in.OrderID)
		if termErr := finishFailed(ctx, in, stockFailReason(err), outcomeFailed, relErr, voidErr); termErr != nil {
			return termErr
		}
		return fmt.Errorf("reserve stock: %w", err)
	}

	recordStage(ctx, domain.ProcessingUpdate{OrderID: in.OrderID,
		Stage: domain.StageInventoryReserved, LastStep: reserveStepToken(in)})

	// Step 2 — create shipment. Compensate: release stock + void the hold.
	if err := workflow.ExecuteActivity(ctx, a.CreateShipment, in.OrderID).Get(ctx, nil); err != nil {
		log.Error("CreateShipment failed; compensating", "order_id", in.OrderID, "error", err)
		recordStage(ctx, domain.ProcessingUpdate{OrderID: in.OrderID,
			Stage: domain.StageCompensating, LastErrorCode: string(domain.ReasonShipmentUnavailable)})
		// The failed create may have landed server-side with its response
		// lost; CancelShipment is an idempotent no-op when nothing exists,
		// so cancelling unconditionally is the cheap side of the bet.
		shipErr := cancelShipment(ctx, in.OrderID)
		relErr := releaseStock(ctx, in, ReleaseReasonShipmentFailed)
		voidErr := voidPayment(ctx, in.OrderID)
		if termErr := finishFailed(ctx, in, domain.ReasonShipmentUnavailable, outcomeFailed, shipErr, relErr, voidErr); termErr != nil {
			return termErr
		}
		return fmt.Errorf("create shipment: %w", err)
	}

	recordStage(ctx, domain.ProcessingUpdate{OrderID: in.OrderID,
		Stage: domain.StageShipmentPrepared, LastStep: stepCreateShipment})

	// Step 3 — capture the payment (immediately before the pivot). Still pre-pivot,
	// so compensate: cancel shipment + release stock + void the hold.
	if err := workflow.ExecuteActivity(ctx, a.CapturePayment, in.OrderID).Get(ctx, nil); err != nil {
		log.Error("CapturePayment failed; compensating", "order_id", in.OrderID, "error", err)
		recordStage(ctx, domain.ProcessingUpdate{OrderID: in.OrderID,
			Stage: domain.StageCompensating, LastErrorCode: string(paymentFailReason(err))})
		shipErr := cancelShipment(ctx, in.OrderID)
		relErr := releaseStock(ctx, in, ReleaseReasonCaptureFailed)
		voidErr := voidPayment(ctx, in.OrderID)
		if termErr := finishFailed(ctx, in, paymentFailReason(err), outcomeFailed, shipErr, relErr, voidErr); termErr != nil {
			return termErr
		}
		return fmt.Errorf("capture payment: %w", err)
	}

	recordStage(ctx, domain.ProcessingUpdate{OrderID: in.OrderID,
		Stage: domain.StagePaymentCaptured, LastStep: stepCapturePayment})

	return nil
}

// reserveStepToken names the reserve step the projection reports, per
// participant.
func reserveStepToken(in OrderFulfillmentInput) string {
	if in.usesInventory() {
		return stepReserveInventory
	}
	return stepReserveStock
}

// paymentFailReason maps a recorded payment-activity error to the bounded
// failure code. Deterministic on replay: it reads only the ApplicationError
// type recorded in history. A decline/rejection is a definite provider
// answer; anything else means retries ran out with the outcome unobserved.
func paymentFailReason(err error) domain.ReasonCode {
	var appErr *temporal.ApplicationError
	if errors.As(err, &appErr) {
		switch appErr.Type() {
		case "PaymentDeclined", reasonPaymentRejected:
			return domain.ReasonPaymentDeclined
		}
	}
	return domain.ReasonPaymentOutcomeUnknown
}

// stockFailReason distinguishes the business decline (out of stock) from an
// unavailable participant, on either stock path.
func stockFailReason(err error) domain.ReasonCode {
	var appErr *temporal.ApplicationError
	if errors.As(err, &appErr) {
		switch appErr.Type() {
		case grpcx.ReasonInsufficientStock, "InsufficientStock":
			return domain.ReasonInsufficientStock
		}
	}
	return domain.ReasonInventoryUnavailable
}

// finishFailed drives the terminal bookkeeping of a failed saga and returns
// nil once a terminal state landed on the row.
//
// The rule is §5.12 of the RFC: `failed` asserts that compensation CONVERGED
// (or there were no side effects). If any compensation exhausted its
// retries, or FailOrder itself cannot land, asserting `failed` would be a
// lie — the order parks in manual_review(COMPENSATION_INCOMPLETE) for a
// human instead. If even MarkManualReview cannot land, the error is
// returned and the workflow FAILS with the order still `pending`: visible
// through workflow-failure alerting and the pending-age signal, rather than
// quietly mislabeled.
func finishFailed(ctx workflow.Context, in OrderFulfillmentInput,
	reason domain.ReasonCode, outcome string, compensationErrs ...error) error {
	log := workflow.GetLogger(ctx)

	converged := true
	for _, err := range compensationErrs {
		if err != nil {
			converged = false
			break
		}
	}

	if converged {
		err := failOrder(ctx, in.OrderID, reason)
		if err == nil {
			recordSagaOutcome(ctx, outcome)
			// No LastStep: the COALESCE keeps the last BUSINESS step that
			// worked, which is what an operator wants to see next to the
			// failure reason.
			recordStage(ctx, domain.ProcessingUpdate{OrderID: in.OrderID,
				Stage: domain.StageDone, LastErrorCode: string(reason)})
			return nil
		}
		log.Error("FailOrder did not land; parking the order for manual review",
			"order_id", in.OrderID, "error", err)
	} else {
		for _, err := range compensationErrs {
			if err != nil {
				log.Error("a compensation did not converge; parking the order for manual review",
					"order_id", in.OrderID, "error", err)
				break
			}
		}
	}

	if err := markManualReview(ctx, in.OrderID, domain.ReasonCompensationIncomplete); err != nil {
		// Deliberately NO saga outcome here: the not-completing alert keys on
		// starts-without-outcomes, and an execution whose terminal write never
		// landed is exactly what it must keep firing for.
		log.Error("MarkManualReview did not land; failing the workflow",
			"order_id", in.OrderID, "error", err)
		return fmt.Errorf("terminal bookkeeping for order %s did not land: %w", in.OrderID, err)
	}
	recordSagaOutcome(ctx, outcomeManualReview)
	// The BUSINESS reason rides the projection: orders.manual_review_reason
	// already says COMPENSATION_INCOMPLETE, and the operator's first question
	// is what originally went wrong.
	recordStage(ctx, domain.ProcessingUpdate{OrderID: in.OrderID,
		Stage: domain.StageManualReview, LastErrorCode: string(reason)})
	return nil
}

// The compensation helpers below each run one compensation activity and record
// its outcome on order.saga.compensation.total. Extracting them keeps the
// workflow's terminal branches flat and single-purpose. Payment compensations
// (void/refund) log a terminal failure at Error — money may be held or not
// returned, an alertable reconcile-worthy event; stock/shipment/fail
// compensations stay silent (best-effort, unchanged from before instrumentation).

// The helpers take a nil *Activities receiver (the method-identity sentinel,
// exactly as the workflow does) rather than accepting it as a parameter, since
// it is always the same nil value.

// voidPayment releases an authorized-but-uncaptured hold. The error is
// returned as well as logged: an exhausted void means side effects are
// unaccounted for, which finishFailed turns into manual_review.
func voidPayment(ctx workflow.Context, orderID string) error {
	var a *Activities
	ctx = workflow.WithActivityOptions(ctx, compensationActivityOptions())
	err := workflow.ExecuteActivity(ctx, a.VoidPayment, orderID).Get(ctx, nil)
	recordCompensation(ctx, compVoidPayment, compResult(err))
	if err != nil {
		workflow.GetLogger(ctx).Error("VoidPayment compensation failed; authorized hold may remain",
			"order_id", orderID, "error", err)
	}
	return err
}

// refundPayment returns already-captured money and, on success, emails the
// customer (best-effort; the notification never blocks the compensation).
func refundPayment(ctx workflow.Context, in OrderFulfillmentInput) error {
	var a *Activities
	log := workflow.GetLogger(ctx)
	ctx = workflow.WithActivityOptions(ctx, compensationActivityOptions())
	err := workflow.ExecuteActivity(ctx, a.RefundPayment, in.OrderID, in.Total).Get(ctx, nil)
	recordCompensation(ctx, compRefundPayment, compResult(err))
	if err != nil {
		log.Error("RefundPayment compensation failed; captured money may not be returned",
			"order_id", in.OrderID, "error", err)
		return err
	}
	if err := workflow.ExecuteActivity(ctx, a.SendRefundNotification,
		NotifyInput{OrderID: in.OrderID, UserID: in.UserID, Total: in.Total}).Get(ctx, nil); err != nil {
		log.Warn("SendRefundNotification failed (non-fatal)", "order_id", in.OrderID, "error", err)
	}
	return nil
}

// reserveStock reserves at the saga's pinned participant. The two activities are
// deliberately DIFFERENT names rather than one activity repointed at a new
// backend: identical names would let an old history replay green while the side
// effects moved to another service mid-saga (homelab ADR-030).
func reserveStock(ctx workflow.Context, in OrderFulfillmentInput) error {
	var a *Activities
	if in.usesInventory() {
		return workflow.ExecuteActivity(ctx, a.ReserveInventory, in.OrderID, in.Items).Get(ctx, nil)
	}
	return workflow.ExecuteActivity(ctx, a.ReserveStock, in.OrderID, in.Items).Get(ctx, nil)
}

// releaseAmbiguousReserve returns a hold that MAY exist after a failed reserve.
//
// A reserve that ends in INSUFFICIENT_STOCK definitively took nothing, so
// releasing would be a no-op call on every out-of-stock checkout. Any other
// failure is ambiguous: the reservation can be committed server-side with its
// response lost (the pod is OOM-killed after the write), and the retry budget
// then runs out against a restarting service. v1 reservations never auto-expire
// — expires_at is observability-only and there is no reaper — and the reconciler
// is scoped to CONFIRMED orders, so nothing else would ever return that stock.
//
// Release is idempotent and releasing a reservation that does not exist is a
// tolerable non-retryable NOT_FOUND, so the cost of being wrong here is one RPC;
// the cost of skipping it is stock held forever against a failed order.
//
// Product path does nothing: its ReserveStock either decremented or did not, and
// a lost decrement is not a hold.
func releaseAmbiguousReserve(ctx workflow.Context, in OrderFulfillmentInput, reserveErr error) error {
	if !in.usesInventory() {
		return nil
	}
	var appErr *temporal.ApplicationError
	if errors.As(reserveErr, &appErr) && appErr.Type() == grpcx.ReasonInsufficientStock {
		return nil
	}
	return releaseStock(ctx, in, ReleaseReasonReserveFailed)
}

// releaseStock returns reserved stock to the participant that holds it
// (compensation for reserveStock). reason is one of the bounded
// ReleaseReason* codes and names the failure point, so inventory's movement
// ledger records why the stock came back; the product path has no reason
// parameter and ignores it.
//
// Compensations retry harder than forward steps — see
// compensationActivityOptions.
func releaseStock(ctx workflow.Context, in OrderFulfillmentInput, reason ReleaseReason) error {
	var a *Activities
	ctx = workflow.WithActivityOptions(ctx, compensationActivityOptions())
	var err error
	if in.usesInventory() {
		err = workflow.ExecuteActivity(ctx, a.ReleaseInventory, in.OrderID, reason).Get(ctx, nil)
	} else {
		err = workflow.ExecuteActivity(ctx, a.ReleaseStock, in.OrderID, in.Items).Get(ctx, nil)
	}
	recordCompensation(ctx, compReleaseStock, compResult(err))
	return err
}

// commitInventory converts the reservation to COMMITTED. It cannot fail the
// order: the pivot already succeeded, so a failure here is an inconsistency to
// repair (the reconciler re-drives CONFIRMED-but-RESERVED orders), not a reason
// to roll back a confirmed order and refund a customer whose stock is fine.
//
// Reaching this with a failure at all means the unbounded retry gave up, which
// only happens on a non-retryable business rejection — INVALID_TRANSITION or
// NOT_FOUND after a successful reserve is an invariant breach, so it is counted
// and logged at Error for alerting rather than swallowed as best-effort noise.
func commitInventory(ctx workflow.Context, in OrderFulfillmentInput, confirmedAt time.Time) error {
	var a *Activities
	ctx = workflow.WithActivityOptions(ctx, commitActivityOptions())
	err := workflow.ExecuteActivity(ctx, a.CommitInventory, in.OrderID).Get(ctx, nil)
	recordCommitLag(ctx, workflow.Now(ctx).Sub(confirmedAt), err)
	if err != nil {
		workflow.GetLogger(ctx).Error("CommitInventory failed after the pivot; reservation left uncommitted on a confirmed order",
			"order_id", in.OrderID, "error", err)
	}
	return err
}

// cancelShipment cancels the order's shipment (compensation for CreateShipment).
func cancelShipment(ctx workflow.Context, orderID string) error {
	var a *Activities
	ctx = workflow.WithActivityOptions(ctx, compensationActivityOptions())
	err := workflow.ExecuteActivity(ctx, a.CancelShipment, orderID).Get(ctx, nil)
	recordCompensation(ctx, compCancelShipment, compResult(err))
	return err
}

// failOrder applies the terminal fail command with its bounded reason. The
// saga outcome is recorded by finishFailed, which alone knows whether the
// terminal state really landed.
func failOrder(ctx workflow.Context, orderID string, reason domain.ReasonCode) error {
	var a *Activities
	ctx = workflow.WithActivityOptions(ctx, compensationActivityOptions())
	err := workflow.ExecuteActivity(ctx, a.FailOrder, orderID, reason).Get(ctx, nil)
	recordCompensation(ctx, compFailOrder, compResult(err))
	return err
}

// markManualReview parks the order for a human — the last resort of a failed
// saga whose side effects are not fully accounted for.
func markManualReview(ctx workflow.Context, orderID string, reason domain.ReasonCode) error {
	var a *Activities
	ctx = workflow.WithActivityOptions(ctx, compensationActivityOptions())
	err := workflow.ExecuteActivity(ctx, a.MarkManualReview, orderID, reason).Get(ctx, nil)
	recordCompensation(ctx, compMarkManualReview, compResult(err))
	return err
}

// completionActivityOptions gives the Complete bookkeeping the same hard
// retry budget as a compensation — the value equality is coincidence of
// numbers, not of meaning, so it gets its own name.
func completionActivityOptions() workflow.ActivityOptions {
	return compensationActivityOptions()
}

// completeOrder closes the happy tail (confirmed → completed). Best-effort:
// a Complete that never lands leaves the order confirmed — reconciler-
// settled, customer-visible state unchanged — so it is counted and logged
// rather than failing a fulfilled order.
//
// One refusal is EXPECTED and not a failure: the user may legally cancel
// during the tail (confirmed → cancelling; the inventory commit alone can
// retry for up to 30 minutes), and the FSM then refuses cancelling →
// completed. That is the order moving on, not completion drift — logged at
// Info and deliberately not counted, or every legitimate tail-window
// cancellation would page.
func completeOrder(ctx workflow.Context, orderID string) bool {
	var a *Activities
	ctx = workflow.WithActivityOptions(ctx, completionActivityOptions())
	err := workflow.ExecuteActivity(ctx, a.Complete, orderID).Get(ctx, nil)
	if err == nil {
		return true
	}
	var appErr *temporal.ApplicationError
	if errors.As(err, &appErr) && appErr.Type() == reasonOrderTransitionRefused {
		workflow.GetLogger(ctx).Info("Complete skipped; the order already moved on (likely cancelling)",
			"order_id", orderID, "error", err)
		return false
	}
	recordCompleteFailure(ctx)
	workflow.GetLogger(ctx).Error("Complete did not land; order stays confirmed",
		"order_id", orderID, "error", err)
	return false
}
