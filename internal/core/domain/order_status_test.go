package domain

import (
	"errors"
	"strings"
	"testing"
)

// allStatuses spans the whole vocabulary so the matrix tests below cover
// every from/to pair — a mutated transition table cannot survive them.
var allStatuses = []OrderStatus{
	OrderStatusPending,
	OrderStatusConfirmed,
	OrderStatusFailed,
	OrderStatusCancelling,
	OrderStatusCancelled,
	OrderStatusCompleted,
	OrderStatusManualReview,
}

// allowed is the FSM spelled out pair-by-pair, independently of the
// production map, so the test fails if either side drifts.
var allowed = map[OrderStatus]map[OrderStatus]bool{
	OrderStatusPending: {
		OrderStatusConfirmed:    true,
		OrderStatusFailed:       true,
		OrderStatusManualReview: true,
	},
	OrderStatusConfirmed: {
		OrderStatusCancelling: true,
		OrderStatusCompleted:  true,
	},
	OrderStatusCompleted: {
		OrderStatusCancelling: true,
	},
	OrderStatusCancelling: {
		OrderStatusCancelled:    true,
		OrderStatusManualReview: true,
	},
	OrderStatusManualReview: {
		OrderStatusConfirmed: true,
		OrderStatusFailed:    true,
		OrderStatusCancelled: true,
		OrderStatusCompleted: true,
	},
}

// The matrix test is only exhaustive if allStatuses and the production map
// actually cover each other — pin that, so adding a status to one without
// the other cannot pass silently.
func TestVocabularyAndMatrixAgree(t *testing.T) {
	if len(transitions) != len(allStatuses) {
		t.Fatalf("transitions has %d statuses, test vocabulary has %d", len(transitions), len(allStatuses))
	}
	inVocab := map[OrderStatus]bool{}
	for _, s := range allStatuses {
		inVocab[s] = true
		if _, ok := transitions[s]; !ok {
			t.Errorf("status %s missing from transitions", s)
		}
	}
	for from, tos := range transitions {
		if !inVocab[from] {
			t.Errorf("transitions has status %s outside the test vocabulary", from)
		}
		for _, to := range tos {
			if !inVocab[to] {
				t.Errorf("edge %s → %s targets a status outside the test vocabulary", from, to)
			}
		}
	}
}

func TestCanTransition_FullMatrix(t *testing.T) {
	for _, from := range allStatuses {
		for _, to := range allStatuses {
			want := allowed[from][to]
			if got := CanTransition(from, to); got != want {
				t.Errorf("CanTransition(%s, %s) = %v, want %v", from, to, got, want)
			}
		}
	}
}

func TestCanTransition_UnknownStatuses(t *testing.T) {
	if CanTransition("shipped", OrderStatusConfirmed) {
		t.Error("unknown from-status must not transition")
	}
	if CanTransition(OrderStatusPending, "shipped") {
		t.Error("transition into an unknown status must be denied")
	}
	if CanTransition(OrderStatusPending, OrderStatusPending) {
		t.Error("self-transition is not an edge")
	}
}

// actorAllowed is the actor×edge matrix spelled out independently.
var actorAllowed = map[ActorType]map[OrderStatus]map[OrderStatus]bool{
	ActorWorkflow: {
		OrderStatusPending: {
			OrderStatusConfirmed:    true,
			OrderStatusFailed:       true,
			OrderStatusManualReview: true,
		},
		OrderStatusConfirmed: {OrderStatusCompleted: true},
		OrderStatusCancelling: {
			OrderStatusCancelled:    true,
			OrderStatusManualReview: true,
		},
	},
	ActorUser: {
		OrderStatusConfirmed: {OrderStatusCancelling: true},
		OrderStatusCompleted: {OrderStatusCancelling: true},
	},
	ActorOperator: {
		OrderStatusManualReview: {
			OrderStatusConfirmed: true,
			OrderStatusFailed:    true,
			OrderStatusCancelled: true,
			OrderStatusCompleted: true,
		},
	},
	ActorSystem: {
		OrderStatusPending: {OrderStatusFailed: true},
	},
}

func TestActorAllowed_FullMatrix(t *testing.T) {
	actors := []ActorType{ActorSystem, ActorWorkflow, ActorUser, ActorOperator}
	for _, actor := range actors {
		for _, from := range allStatuses {
			for _, to := range allStatuses {
				want := actorAllowed[actor][from][to]
				if got := ActorAllowed(actor, from, to); got != want {
					t.Errorf("ActorAllowed(%s, %s, %s) = %v, want %v", actor, from, to, got, want)
				}
				// Every actor edge must also be an FSM edge.
				if want && !CanTransition(from, to) {
					t.Errorf("actor edge %s: %s → %s is not an FSM edge", actor, from, to)
				}
			}
		}
	}
	if ActorAllowed("INTRUDER", OrderStatusConfirmed, OrderStatusCancelling) {
		t.Error("unknown actor must never be allowed")
	}
}

func TestTerminalStatus(t *testing.T) {
	terminal := map[OrderStatus]bool{
		OrderStatusFailed:    true,
		OrderStatusCancelled: true,
	}
	for _, s := range allStatuses {
		if got := TerminalStatus(s); got != terminal[s] {
			t.Errorf("TerminalStatus(%s) = %v, want %v", s, got, terminal[s])
		}
	}
	// completed keeps its cancelling edge — the one non-obvious case.
	if TerminalStatus(OrderStatusCompleted) {
		t.Error("completed must not be FSM-terminal: completed → cancelling exists")
	}
	// A corrupt/legacy status must look like work, never like settled.
	for _, s := range []OrderStatus{"shipped", "processing", "", "PENDING"} {
		if TerminalStatus(s) {
			t.Errorf("TerminalStatus(%q) must be false for unknown statuses", s)
		}
	}
}

func TestKnownStatus(t *testing.T) {
	for _, s := range allStatuses {
		if !KnownStatus(s) {
			t.Errorf("KnownStatus(%s) = false", s)
		}
	}
	for _, s := range []OrderStatus{"shipped", "processing", "", "PENDING"} {
		if KnownStatus(s) {
			t.Errorf("KnownStatus(%q) must be false", s)
		}
	}
}

// The exact strings are the database/wire contract — pin them as literals so
// a renamed constant cannot slide through the constant-based matrix tests.
func TestVocabularyLiterals(t *testing.T) {
	statuses := map[OrderStatus]string{
		OrderStatusPending:      "pending",
		OrderStatusConfirmed:    "confirmed",
		OrderStatusFailed:       "failed",
		OrderStatusCancelling:   "cancelling",
		OrderStatusCancelled:    "cancelled",
		OrderStatusCompleted:    "completed",
		OrderStatusManualReview: "manual_review",
	}
	for s, want := range statuses {
		if string(s) != want {
			t.Errorf("status literal %q, want %q", s, want)
		}
	}
	actors := map[ActorType]string{
		ActorSystem:   "SYSTEM",
		ActorWorkflow: "WORKFLOW",
		ActorUser:     "USER",
		ActorOperator: "OPERATOR",
	}
	for a, want := range actors {
		if string(a) != want {
			t.Errorf("actor literal %q, want %q", a, want)
		}
	}
	reasons := map[ReasonCode]string{
		ReasonPaymentDeclined:        "PAYMENT_DECLINED",
		ReasonPaymentOutcomeUnknown:  "PAYMENT_OUTCOME_UNKNOWN",
		ReasonInventoryUnavailable:   "INVENTORY_UNAVAILABLE",
		ReasonInsufficientStock:      "INSUFFICIENT_STOCK",
		ReasonShipmentUnavailable:    "SHIPMENT_UNAVAILABLE",
		ReasonConfirmationFailed:     "CONFIRMATION_FAILED",
		ReasonCompensationIncomplete: "COMPENSATION_INCOMPLETE",
		ReasonWorkflowStartFailed:    "WORKFLOW_START_FAILED",
		ReasonCustomerRequest:        "CUSTOMER_REQUEST",
		ReasonOperatorResolved:       "OPERATOR_RESOLVED",
	}
	for r, want := range reasons {
		if string(r) != want {
			t.Errorf("reason literal %q, want %q", r, want)
		}
	}
	// Closure: the authority set contains exactly these reasons.
	if len(knownReasons) != len(reasons) {
		t.Errorf("knownReasons has %d entries, test pins %d", len(knownReasons), len(reasons))
	}
}

func TestKnownReason(t *testing.T) {
	for _, r := range []ReasonCode{
		ReasonPaymentDeclined, ReasonPaymentOutcomeUnknown,
		ReasonInventoryUnavailable, ReasonInsufficientStock,
		ReasonShipmentUnavailable, ReasonConfirmationFailed,
		ReasonCompensationIncomplete, ReasonWorkflowStartFailed,
		ReasonCustomerRequest, ReasonOperatorResolved,
	} {
		if !KnownReason(r) {
			t.Errorf("KnownReason(%s) = false", r)
		}
	}
	for _, r := range []ReasonCode{"", "pq: connection refused", "PAYMENT_DECLINED "} {
		if KnownReason(r) {
			t.Errorf("KnownReason(%q) must be false", r)
		}
	}
}

func TestCommandIDs_ExactFormats(t *testing.T) {
	cases := []struct{ got, want string }{
		{confirmCommandID("42"), "confirm:42"},
		{completeCommandID("42"), "complete:42"},
		{failCommandID("42"), "fail:42"},
		{manualReviewCommandID("42"), "manual-review:42"},
		{cancelCommandID("42", 5), "cancel:42:v5"},
		{cancelCompleteCommandID("42", 5), "cancel-complete:42:v5"},
		{cancelManualReviewCommandID("42", 5), "manual-review:42:v5"},
		{resolveCommandID("42", 7, OrderStatusFailed), "resolve:42:v7:failed"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("command id = %q, want %q", c.got, c.want)
		}
	}
}

// Every builder must mint a distinct id for the same order and epoch —
// UNIQUE(order_id, command_id) only protects transitions if two different
// commands can never share one string.
func TestCommandIDs_PairwiseDistinct(t *testing.T) {
	ids := []string{
		confirmCommandID("42"),
		completeCommandID("42"),
		failCommandID("42"),
		manualReviewCommandID("42"),
		cancelCommandID("42", 5),
		cancelCompleteCommandID("42", 5),
		cancelManualReviewCommandID("42", 5),
		resolveCommandID("42", 5, OrderStatusConfirmed),
		resolveCommandID("42", 5, OrderStatusFailed),
		resolveCommandID("42", 5, OrderStatusCancelled),
		resolveCommandID("42", 5, OrderStatusCompleted),
	}
	seen := map[string]int{}
	for i, id := range ids {
		if j, dup := seen[id]; dup {
			t.Errorf("builders %d and %d collide on %q", j, i, id)
		}
		seen[id] = i
	}
}

// Distinct epochs must mint distinct ids — the second-cancellation-episode
// guarantee the epoch exists for.
func TestCommandIDs_EpochSeparatesEpisodes(t *testing.T) {
	if cancelCommandID("42", 5) == cancelCommandID("42", 9) {
		t.Error("cancel ids across epochs must differ")
	}
	if cancelManualReviewCommandID("42", 0) == manualReviewCommandID("42") {
		t.Error("episode manual-review id must not collide with the pending-phase id")
	}
}

func TestNewConfirmCommand(t *testing.T) {
	cmd, err := NewConfirmCommand("42")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cmd.To != OrderStatusConfirmed || cmd.ActorType != ActorWorkflow {
		t.Errorf("unexpected command: %+v", cmd)
	}
	if cmd.CommandID != "confirm:42" || cmd.OrderID != "42" {
		t.Errorf("unexpected ids: %+v", cmd)
	}
	if _, err := NewConfirmCommand(""); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("empty order id: got %v, want ErrInvalidInput", err)
	}
}

func TestNewFailCommand(t *testing.T) {
	cmd, err := NewFailCommand("42", ReasonInsufficientStock)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cmd.To != OrderStatusFailed || cmd.Reason != ReasonInsufficientStock {
		t.Errorf("unexpected command: %+v", cmd)
	}
	// The reason must NOT be part of the idempotency anchor.
	if strings.Contains(cmd.CommandID, string(ReasonInsufficientStock)) {
		t.Errorf("reason leaked into command id: %q", cmd.CommandID)
	}

	if _, err := NewFailCommand("42", ""); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("empty reason: got %v, want ErrInvalidInput", err)
	}
	if _, err := NewFailCommand("42", "pq: cannot connect"); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("unknown reason: got %v, want ErrInvalidInput", err)
	}
	if _, err := NewFailCommand("", ReasonPaymentDeclined); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("empty order id: got %v, want ErrInvalidInput", err)
	}
	// Non-canonical order ids must be refused — they would mint a second
	// command-id namespace for the same row.
	for _, id := range []string{"042", "42:v5", "abc", "-1", "12345678901234567890"} {
		if _, err := NewFailCommand(id, ReasonPaymentDeclined); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("order id %q: got %v, want ErrInvalidInput", id, err)
		}
	}
	// Field pinning: actor and exact id are part of the contract.
	if cmd.ActorType != ActorWorkflow || cmd.CommandID != "fail:42" {
		t.Errorf("unexpected actor/id: %+v", cmd)
	}
}

func TestNewSystemFailCommand(t *testing.T) {
	cmd, err := NewSystemFailCommand("42", ReasonWorkflowStartFailed)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Same id namespace as the workflow fail (pending→failed happens once;
	// first writer wins, the loser replays) but SYSTEM attribution.
	if cmd.CommandID != "fail:42" || cmd.ActorType != ActorSystem {
		t.Errorf("unexpected command: %+v", cmd)
	}
	if _, err := NewSystemFailCommand("42", "boom"); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("unknown reason: got %v, want ErrInvalidInput", err)
	}
}

func TestNewCompleteCommand(t *testing.T) {
	cmd, err := NewCompleteCommand("42")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cmd.To != OrderStatusCompleted || cmd.CommandID != "complete:42" {
		t.Errorf("unexpected command: %+v", cmd)
	}
	if _, err := NewCompleteCommand(""); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("empty order id: got %v, want ErrInvalidInput", err)
	}
}

func TestNewMarkManualReviewCommand(t *testing.T) {
	cmd, err := NewMarkManualReviewCommand("42", ReasonCompensationIncomplete)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cmd.CommandID != "manual-review:42" || cmd.To != OrderStatusManualReview {
		t.Errorf("unexpected command: %+v", cmd)
	}
	if _, err := NewMarkManualReviewCommand("42", "boom"); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("unknown reason: got %v, want ErrInvalidInput", err)
	}
}

func TestNewCancelManualReviewCommand(t *testing.T) {
	cmd, err := NewCancelManualReviewCommand("42", ReasonCompensationIncomplete, 5)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cmd.CommandID != "manual-review:42:v5" || cmd.To != OrderStatusManualReview ||
		cmd.ActorType != ActorWorkflow || cmd.Reason != ReasonCompensationIncomplete {
		t.Errorf("unexpected command: %+v", cmd)
	}
	// Epoch zero is a valid epoch.
	cmd, err = NewCancelManualReviewCommand("42", ReasonCompensationIncomplete, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cmd.CommandID != "manual-review:42:v0" {
		t.Errorf("epoch-0 id = %q", cmd.CommandID)
	}
	if _, err := NewCancelManualReviewCommand("42", ReasonCompensationIncomplete, -1); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("negative epoch: got %v, want ErrInvalidInput", err)
	}
}

func TestNewRequestCancellationCommand(t *testing.T) {
	cmd, err := NewRequestCancellationCommand("42", "7", 5)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cmd.To != OrderStatusCancelling || cmd.ActorType != ActorUser || cmd.ActorID != "7" {
		t.Errorf("unexpected command: %+v", cmd)
	}
	if cmd.Reason != ReasonCustomerRequest {
		t.Errorf("reason = %q, want CUSTOMER_REQUEST", cmd.Reason)
	}

	if _, err := NewRequestCancellationCommand("42", "", 5); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("missing user: got %v, want ErrInvalidInput", err)
	}
	if _, err := NewRequestCancellationCommand("42", strings.Repeat("x", 256), 5); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("oversized user id: got %v, want ErrInvalidInput", err)
	}
	if _, err := NewRequestCancellationCommand("42", "7", -1); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("negative epoch: got %v, want ErrInvalidInput", err)
	}
}

func TestNewCompleteCancellationCommand(t *testing.T) {
	cmd, err := NewCompleteCancellationCommand("42", 5)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cmd.To != OrderStatusCancelled || cmd.CommandID != "cancel-complete:42:v5" {
		t.Errorf("unexpected command: %+v", cmd)
	}
	if _, err := NewCompleteCancellationCommand("42", -3); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("negative epoch: got %v, want ErrInvalidInput", err)
	}
}

func TestNewResolveManualReviewCommand(t *testing.T) {
	for _, target := range []OrderStatus{OrderStatusConfirmed, OrderStatusFailed, OrderStatusCancelled, OrderStatusCompleted} {
		cmd, err := NewResolveManualReviewCommand("42", target, "ops-1", "verified refund in provider", 7)
		if err != nil {
			t.Fatalf("target %s: unexpected error: %v", target, err)
		}
		if cmd.To != target || cmd.ActorType != ActorOperator {
			t.Errorf("unexpected command: %+v", cmd)
		}
		if cmd.Reason != ReasonOperatorResolved {
			t.Errorf("reason = %q, want OPERATOR_RESOLVED", cmd.Reason)
		}
	}

	// Targets outside the resolve set are refused.
	for _, target := range []OrderStatus{OrderStatusPending, OrderStatusCancelling, OrderStatusManualReview} {
		if _, err := NewResolveManualReviewCommand("42", target, "ops-1", "note", 7); !errors.Is(err, ErrInvalidTransition) {
			t.Errorf("target %s: got %v, want ErrInvalidTransition", target, err)
		}
	}

	if _, err := NewResolveManualReviewCommand("42", OrderStatusFailed, "", "note", 7); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("missing operator: got %v, want ErrInvalidInput", err)
	}
	if _, err := NewResolveManualReviewCommand("42", OrderStatusFailed, "ops-1", "", 7); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("missing note: got %v, want ErrInvalidInput", err)
	}
	if _, err := NewResolveManualReviewCommand("42", OrderStatusFailed, strings.Repeat("x", 256), "note", 7); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("oversized operator id: got %v, want ErrInvalidInput", err)
	}
	if _, err := NewResolveManualReviewCommand("42", OrderStatusFailed, "ops-1", "note", -1); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("negative epoch: got %v, want ErrInvalidInput", err)
	}
}

func TestWithWorkflowIdentity(t *testing.T) {
	cmd, err := NewConfirmCommand("42")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := cmd.WithWorkflowIdentity("order-fulfillment-42", "run-1")
	if got.WorkflowID != "order-fulfillment-42" || got.RunID != "run-1" {
		t.Errorf("identity not recorded: %+v", got)
	}
	if cmd.WorkflowID != "" {
		t.Error("WithWorkflowIdentity must not mutate the receiver")
	}
}
