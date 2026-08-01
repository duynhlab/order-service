package domain

import (
	"errors"
	"fmt"
)

// OrderStatus is the order's commercial lifecycle vocabulary. It answers
// "what is commercially true" — money and promise-to-the-customer — and
// deliberately contains no downstream operational states: PAID, RESERVED,
// SHIPPING and friends belong to Payment/Inventory/Shipping, and UI progress
// reads the processing projection instead (RFC-0021 non-goal).
//
// Lowercase on the wire and in the database: `pending/confirmed/failed`
// predate this type, and extending the existing casing beats migrating every
// stored row and API consumer for a cosmetic change.
//
// The schema half of this contract (orders.version, the metadata columns,
// and order_status_history with UNIQUE(order_id, command_id)) ships in
// migration 000011; this package must not merge ahead of it.
type OrderStatus string

const (
	// OrderStatusPending — row committed, fulfillment saga in flight (or not
	// yet started after a crash; the start outbox owns that gap).
	OrderStatusPending OrderStatus = "pending"
	// OrderStatusConfirmed — the pivot succeeded; payment captured.
	OrderStatusConfirmed OrderStatus = "confirmed"
	// OrderStatusFailed — a pre-pivot step failed AND compensation converged
	// (or there were no side effects). Never used while side effects are
	// still unaccounted for — that is what manual_review exists for.
	OrderStatusFailed OrderStatus = "failed"
	// OrderStatusCancelling — cancellation accepted; the cancellation
	// workflow is unwinding shipment/payment/stock.
	OrderStatusCancelling OrderStatus = "cancelling"
	// OrderStatusCancelled — cancellation converged (refund/void, shipment
	// cancel, inventory disposition recorded).
	OrderStatusCancelled OrderStatus = "cancelled"
	// OrderStatusCompleted — the fulfillment workflow finished its tail
	// (commit + post-processing). Completion policy v1: "workflow done";
	// it upgrades when shipping grows real dispatch states.
	OrderStatusCompleted OrderStatus = "completed"
	// OrderStatusManualReview — an ambiguous or unconverged side effect
	// needs a human; only an operator command moves the order out.
	OrderStatusManualReview OrderStatus = "manual_review"
)

// transitions is the whole FSM. A transition absent here does not exist.
// Two non-obvious edges, both deliberate:
//
//   - completed → cancelling: the fulfillment tail completes an order
//     seconds after the pivot, so without this edge the cancellation window
//     would be those few seconds. The real gate is the cancellation policy
//     (shipment not dispatched), not the completed/confirmed distinction.
//   - manual_review → completed: an operator untangling a failed
//     cancellation of an already-fulfilled order must be able to put it
//     back exactly where it was; forcing it through confirmed would strand
//     it there (the fulfillment workflow that issues Complete has long
//     finished and never re-runs).
//   - confirmed → manual_review: the ambiguous-pivot seam. ConfirmOrder can
//     commit its write and lose the ack; the workflow then takes the
//     compensation branch (refund, cancel shipment, release stock) against
//     a row that says confirmed. FailOrder is rightly refused there, and
//     without this edge the order would be stranded asserting a
//     confirmation its side effects no longer back — parking it for a
//     human is the only honest terminal.
var transitions = map[OrderStatus][]OrderStatus{
	OrderStatusPending:      {OrderStatusConfirmed, OrderStatusFailed, OrderStatusManualReview},
	OrderStatusConfirmed:    {OrderStatusCancelling, OrderStatusCompleted, OrderStatusManualReview},
	OrderStatusCompleted:    {OrderStatusCancelling},
	OrderStatusCancelling:   {OrderStatusCancelled, OrderStatusManualReview},
	OrderStatusManualReview: {OrderStatusConfirmed, OrderStatusFailed, OrderStatusCancelled, OrderStatusCompleted},
	OrderStatusFailed:       nil,
	OrderStatusCancelled:    nil,
}

// CanTransition reports whether the FSM permits from → to.
func CanTransition(from, to OrderStatus) bool {
	for _, next := range transitions[from] {
		if next == to {
			return true
		}
	}
	return false
}

// actorEdges is the FSM's second dimension: which actor class may drive
// which edge. The repository checks it under the same lock as
// CanTransition, so a handler bug that routes a user request into an
// operator-only edge is caught at the write, not just by constructor
// convention.
var actorEdges = map[ActorType]map[OrderStatus][]OrderStatus{
	ActorWorkflow: {
		OrderStatusPending:    {OrderStatusConfirmed, OrderStatusFailed, OrderStatusManualReview},
		OrderStatusConfirmed:  {OrderStatusCompleted, OrderStatusManualReview},
		OrderStatusCancelling: {OrderStatusCancelled, OrderStatusManualReview},
	},
	ActorUser: {
		OrderStatusConfirmed: {OrderStatusCancelling},
		OrderStatusCompleted: {OrderStatusCancelling},
	},
	ActorOperator: {
		OrderStatusManualReview: {OrderStatusConfirmed, OrderStatusFailed, OrderStatusCancelled, OrderStatusCompleted},
	},
	// SYSTEM is the dispatcher's give-up path (WORKFLOW_START_FAILED):
	// no workflow ever ran, so the order fails by system decision. Not
	// wired yet; the edge exists so wiring it is not a domain change.
	ActorSystem: {
		OrderStatusPending: {OrderStatusFailed},
	},
}

// ActorAllowed reports whether the FSM permits actor to drive from → to.
// It implies CanTransition: every actor edge is also a transition edge.
func ActorAllowed(actor ActorType, from, to OrderStatus) bool {
	for _, next := range actorEdges[actor][from] {
		if next == to {
			return true
		}
	}
	return false
}

// TerminalStatus reports whether a KNOWN status has no outgoing edges.
// Unknown values return false: a corrupt or legacy status must look like
// work for a human, never like something settled a reconciler may skip.
// Note this is the FSM notion of terminal; the reconciler's "settled" set
// is a different, per-query decision (completed counts as settled there
// even though it still has the cancelling edge).
func TerminalStatus(s OrderStatus) bool {
	return KnownStatus(s) && len(transitions[s]) == 0
}

// KnownStatus reports whether s is part of the vocabulary at all.
func KnownStatus(s OrderStatus) bool {
	_, ok := transitions[s]
	return ok
}

// ActorType records who issued a status command. A closed set — the audit
// trail is sorted and alerted by it.
type ActorType string

const (
	ActorSystem   ActorType = "SYSTEM"
	ActorWorkflow ActorType = "WORKFLOW"
	ActorUser     ActorType = "USER"
	ActorOperator ActorType = "OPERATOR"
)

// ReasonCode is the bounded reason vocabulary written to
// orders.failure_code / cancellation_reason / manual_review_reason and
// order_status_history.reason_code. knownReasons below is the authority the
// constructors enforce; the columns are plain VARCHAR so adding a code
// never needs a migration. Raw error text stays in logs, never here.
type ReasonCode string

const (
	ReasonPaymentDeclined        ReasonCode = "PAYMENT_DECLINED"
	ReasonPaymentOutcomeUnknown  ReasonCode = "PAYMENT_OUTCOME_UNKNOWN"
	ReasonInventoryUnavailable   ReasonCode = "INVENTORY_UNAVAILABLE"
	ReasonInsufficientStock      ReasonCode = "INSUFFICIENT_STOCK"
	ReasonShipmentUnavailable    ReasonCode = "SHIPMENT_UNAVAILABLE"
	ReasonConfirmationFailed     ReasonCode = "CONFIRMATION_FAILED"
	ReasonCompensationIncomplete ReasonCode = "COMPENSATION_INCOMPLETE"
	ReasonWorkflowStartFailed    ReasonCode = "WORKFLOW_START_FAILED"
	// ReasonCustomerRequest is the (only, v1) cancellation reason.
	ReasonCustomerRequest ReasonCode = "CUSTOMER_REQUEST"
	// ReasonOperatorResolved marks a manual_review exit; the human context
	// lives in the command's Note.
	ReasonOperatorResolved ReasonCode = "OPERATOR_RESOLVED"
)

var knownReasons = map[ReasonCode]bool{
	ReasonPaymentDeclined:        true,
	ReasonPaymentOutcomeUnknown:  true,
	ReasonInventoryUnavailable:   true,
	ReasonInsufficientStock:      true,
	ReasonShipmentUnavailable:    true,
	ReasonConfirmationFailed:     true,
	ReasonCompensationIncomplete: true,
	ReasonWorkflowStartFailed:    true,
	ReasonCustomerRequest:        true,
	ReasonOperatorResolved:       true,
}

// KnownReason reports whether r is part of the bounded vocabulary. The
// constructors refuse anything else, which is what keeps raw error text
// (and its unbounded length) out of the reason columns.
func KnownReason(r ReasonCode) bool { return knownReasons[r] }

// Status-command errors. The repository maps them to transport codes; in
// Temporal activities the first two are non-retryable (retrying cannot make
// an illegal transition legal), while concurrency conflicts retry.
var (
	ErrInvalidTransition   = errors.New("status transition not permitted")
	ErrIdempotencyConflict = errors.New("command id replayed with a different outcome")
	ErrConcurrencyConflict = errors.New("order changed concurrently")
)

// maxActorIDLen guards the actor_id VARCHAR(255) column: an oversized id
// must be a validation error at the constructor, not an insert failure in
// the middle of a money-bearing transaction.
const maxActorIDLen = 255

// StatusCommand is the only way a status write is expressed. The repository
// discovers the current status under lock and validates the transition AND
// the actor edge there, so the constructors are convenience + validation,
// not the security boundary. Fields stay exported because commands cross
// the Temporal payload boundary.
//
// CommandID is the idempotency anchor (UNIQUE(order_id, command_id)): a
// retried command replays its recorded outcome instead of applying twice.
// The id never contains free-form input — reasons live in the Reason field,
// not the id, so a reason that differs between two workflow attempts (a
// reset that fails differently) still replays instead of colliding. Two id
// regimes, by construction:
//
//   - Fulfillment-workflow commands (confirm/fail/complete/manual-review of
//     the pending phase, plus complete) are version-free — each is issued
//     by exactly one single-shot producer, and the id must be deterministic
//     across activity retries and workflow resets.
//   - USER/OPERATOR commands and everything inside a cancellation episode
//     carry an epoch: the orders.version the SERVER read from the row when
//     the episode was requested (never client-supplied — a stale epoch from
//     an old browser tab would replay a previous episode's outcome).
//     manual_review → confirmed → cancelling can legally happen more than
//     once, and a version-free id would make the second episode replay the
//     first one's outcome instead of transitioning.
type StatusCommand struct {
	OrderID   string
	CommandID string
	To        OrderStatus
	Reason    ReasonCode
	ActorType ActorType
	// ActorID is the user or operator id; empty for SYSTEM/WORKFLOW.
	ActorID string
	// Note is the human-authored audit note (required on ResolveManualReview).
	Note string
	// WorkflowID/RunID pin which execution applied the transition; set via
	// WithWorkflowIdentity by WORKFLOW actors, empty otherwise.
	WorkflowID string
	RunID      string
}

// WithWorkflowIdentity records which execution is applying the command.
func (c StatusCommand) WithWorkflowIdentity(workflowID, runID string) StatusCommand {
	c.WorkflowID = workflowID
	c.RunID = runID
	return c
}

// Command-id builders. Keep every format here so the vocabulary has one
// home; nothing free-form is ever interpolated.

func confirmCommandID(orderID string) string { return "confirm:" + orderID }

func completeCommandID(orderID string) string { return "complete:" + orderID }

func failCommandID(orderID string) string { return "fail:" + orderID }

func manualReviewCommandID(orderID string) string { return "manual-review:" + orderID }

// Epoch-carrying builders (cancellation episode + operator commands).

func cancelCommandID(orderID string, epoch int64) string {
	return fmt.Sprintf("cancel:%s:v%d", orderID, epoch)
}

func cancelCompleteCommandID(orderID string, epoch int64) string {
	return fmt.Sprintf("cancel-complete:%s:v%d", orderID, epoch)
}

func cancelManualReviewCommandID(orderID string, epoch int64) string {
	return fmt.Sprintf("manual-review:%s:v%d", orderID, epoch)
}

func resolveCommandID(orderID string, epoch int64, target OrderStatus) string {
	return fmt.Sprintf("resolve:%s:v%d:%s", orderID, epoch, target)
}

// Constructors. Each validates what the transition table requires beyond
// the from-state (which only the repository can check, under lock).

// requireOrderID accepts only the canonical decimal form of the SERIAL
// order id. Anything else ("042", "42:v5", free text) would mint a second
// command-id namespace for the same row — or a colliding one — and the ids
// must stay injective per order.
func requireOrderID(kind, orderID string) error {
	if orderID == "" || len(orderID) > 19 || (orderID[0] == '0' && orderID != "0") {
		return fmt.Errorf("%s command: %w: order id must be a canonical decimal", kind, ErrInvalidInput)
	}
	for _, c := range orderID {
		if c < '0' || c > '9' {
			return fmt.Errorf("%s command: %w: order id must be a canonical decimal", kind, ErrInvalidInput)
		}
	}
	return nil
}

func requireEpoch(kind, orderID string, epoch int64) error {
	if epoch < 0 {
		return fmt.Errorf("%s command for order %s: %w: negative epoch", kind, orderID, ErrInvalidInput)
	}
	return nil
}

func requireReason(kind, orderID string, reason ReasonCode) error {
	if !KnownReason(reason) {
		return fmt.Errorf("%s command for order %s: %w: unknown reason %q", kind, orderID, ErrInvalidInput, reason)
	}
	return nil
}

// NewConfirmCommand marks the pivot applied. Issued by the fulfillment
// workflow exactly once per order.
func NewConfirmCommand(orderID string) (StatusCommand, error) {
	if err := requireOrderID("confirm", orderID); err != nil {
		return StatusCommand{}, err
	}
	return StatusCommand{
		OrderID:   orderID,
		CommandID: confirmCommandID(orderID),
		To:        OrderStatusConfirmed,
		ActorType: ActorWorkflow,
	}, nil
}

// NewFailCommand records a failed order whose compensation converged. The
// reason rides in the payload, not the command id: a workflow reset that
// fails for a different reason must replay the recorded outcome, not mint
// a second fail command.
func NewFailCommand(orderID string, reason ReasonCode) (StatusCommand, error) {
	if err := requireOrderID("fail", orderID); err != nil {
		return StatusCommand{}, err
	}
	if err := requireReason("fail", orderID, reason); err != nil {
		return StatusCommand{}, err
	}
	return StatusCommand{
		OrderID:   orderID,
		CommandID: failCommandID(orderID),
		To:        OrderStatusFailed,
		Reason:    reason,
		ActorType: ActorWorkflow,
	}, nil
}

// NewSystemFailCommand is the dispatcher's give-up path: the workflow never
// started, so a SYSTEM decision fails the order. It shares failCommandID
// with the workflow form on purpose — pending→failed happens once, whichever
// side applies it first wins the history row, and the loser replays.
func NewSystemFailCommand(orderID string, reason ReasonCode) (StatusCommand, error) {
	cmd, err := NewFailCommand(orderID, reason)
	if err != nil {
		return StatusCommand{}, err
	}
	cmd.ActorType = ActorSystem
	return cmd, nil
}

// NewCompleteCommand records the fulfillment tail finishing. Single-shot:
// only the fulfillment workflow issues it, once; an operator putting an
// order back to completed goes through ResolveManualReview instead.
func NewCompleteCommand(orderID string) (StatusCommand, error) {
	if err := requireOrderID("complete", orderID); err != nil {
		return StatusCommand{}, err
	}
	return StatusCommand{
		OrderID:   orderID,
		CommandID: completeCommandID(orderID),
		To:        OrderStatusCompleted,
		ActorType: ActorWorkflow,
	}, nil
}

// NewMarkManualReviewCommand parks a PENDING order whose side effects are
// unaccounted for (fulfillment-workflow form; a cancellation episode uses
// NewCancelManualReviewCommand).
func NewMarkManualReviewCommand(orderID string, reason ReasonCode) (StatusCommand, error) {
	if err := requireOrderID("manual-review", orderID); err != nil {
		return StatusCommand{}, err
	}
	if err := requireReason("manual-review", orderID, reason); err != nil {
		return StatusCommand{}, err
	}
	return StatusCommand{
		OrderID:   orderID,
		CommandID: manualReviewCommandID(orderID),
		To:        OrderStatusManualReview,
		Reason:    reason,
		ActorType: ActorWorkflow,
	}, nil
}

// NewCancelManualReviewCommand parks a CANCELLING order whose cancellation
// did not converge. epoch namespaces the episode (see StatusCommand).
func NewCancelManualReviewCommand(orderID string, reason ReasonCode, epoch int64) (StatusCommand, error) {
	if err := requireOrderID("cancel-manual-review", orderID); err != nil {
		return StatusCommand{}, err
	}
	if err := requireReason("cancel-manual-review", orderID, reason); err != nil {
		return StatusCommand{}, err
	}
	if err := requireEpoch("cancel-manual-review", orderID, epoch); err != nil {
		return StatusCommand{}, err
	}
	return StatusCommand{
		OrderID:   orderID,
		CommandID: cancelManualReviewCommandID(orderID, epoch),
		To:        OrderStatusManualReview,
		Reason:    reason,
		ActorType: ActorWorkflow,
	}, nil
}

// NewRequestCancellationCommand opens a cancellation episode. epoch is the
// orders.version the HANDLER read from the row while serving this request;
// it must never come from the client.
func NewRequestCancellationCommand(orderID, userID string, epoch int64) (StatusCommand, error) {
	if err := requireOrderID("cancel", orderID); err != nil {
		return StatusCommand{}, err
	}
	if userID == "" || len(userID) > maxActorIDLen {
		return StatusCommand{}, fmt.Errorf("cancel command for order %s: %w: invalid user id", orderID, ErrInvalidInput)
	}
	if err := requireEpoch("cancel", orderID, epoch); err != nil {
		return StatusCommand{}, err
	}
	return StatusCommand{
		OrderID:   orderID,
		CommandID: cancelCommandID(orderID, epoch),
		To:        OrderStatusCancelling,
		Reason:    ReasonCustomerRequest,
		ActorType: ActorUser,
		ActorID:   userID,
	}, nil
}

// NewCompleteCancellationCommand closes a converged cancellation episode.
func NewCompleteCancellationCommand(orderID string, epoch int64) (StatusCommand, error) {
	if err := requireOrderID("cancel-complete", orderID); err != nil {
		return StatusCommand{}, err
	}
	if err := requireEpoch("cancel-complete", orderID, epoch); err != nil {
		return StatusCommand{}, err
	}
	return StatusCommand{
		OrderID:   orderID,
		CommandID: cancelCompleteCommandID(orderID, epoch),
		To:        OrderStatusCancelled,
		Reason:    ReasonCustomerRequest,
		ActorType: ActorWorkflow,
	}, nil
}

// resolveTargets are the only places an operator may move a parked order.
var resolveTargets = map[OrderStatus]bool{
	OrderStatusConfirmed: true,
	OrderStatusFailed:    true,
	OrderStatusCancelled: true,
	OrderStatusCompleted: true,
}

// NewResolveManualReviewCommand is the operator escape hatch out of
// manual_review. Identity and an audit note are mandatory — the whole point
// of the state is that a human decided something.
func NewResolveManualReviewCommand(orderID string, target OrderStatus, operatorID, note string, epoch int64) (StatusCommand, error) {
	if err := requireOrderID("resolve", orderID); err != nil {
		return StatusCommand{}, err
	}
	if !resolveTargets[target] {
		return StatusCommand{}, fmt.Errorf("resolve command for order %s: %w: target %q", orderID, ErrInvalidTransition, target)
	}
	if operatorID == "" || len(operatorID) > maxActorIDLen || note == "" {
		return StatusCommand{}, fmt.Errorf("resolve command for order %s: %w: operator identity and note required", orderID, ErrInvalidInput)
	}
	if err := requireEpoch("resolve", orderID, epoch); err != nil {
		return StatusCommand{}, err
	}
	return StatusCommand{
		OrderID:   orderID,
		CommandID: resolveCommandID(orderID, epoch, target),
		To:        target,
		Reason:    ReasonOperatorResolved,
		ActorType: ActorOperator,
		ActorID:   operatorID,
		Note:      note,
	}, nil
}
