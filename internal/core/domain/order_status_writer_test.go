package domain

import (
	"errors"
	"testing"
)

func TestStatusCommandValidate_ConstructorOutputsPass(t *testing.T) {
	mustOK := func(cmd StatusCommand, err error) StatusCommand {
		t.Helper()
		if err != nil {
			t.Fatalf("constructor: %v", err)
		}
		return cmd
	}
	confirm := mustOK(NewConfirmCommand("42"))
	fail := mustOK(NewFailCommand("42", ReasonPaymentDeclined))
	sysFail := mustOK(NewSystemFailCommand("42", ReasonWorkflowStartFailed))
	complete := mustOK(NewCompleteCommand("42"))
	review := mustOK(NewMarkManualReviewCommand("42", ReasonCompensationIncomplete))
	cancelReview := mustOK(NewCancelManualReviewCommand("42", ReasonCompensationIncomplete, 5))
	cancel := mustOK(NewRequestCancellationCommand("42", "7", 5))
	cancelDone := mustOK(NewCompleteCancellationCommand("42", 5))
	for _, cmd := range []StatusCommand{confirm, fail, sysFail, complete, review, cancelReview, cancel, cancelDone} {
		if err := cmd.Validate(); err != nil {
			t.Errorf("constructor output rejected: %+v: %v", cmd, err)
		}
	}
	for _, target := range []OrderStatus{OrderStatusConfirmed, OrderStatusFailed, OrderStatusCancelled, OrderStatusCompleted} {
		cmd := mustOK(NewResolveManualReviewCommand("42", target, ReasonRefundedManually, "ops-1", "checked provider", 7))
		if err := cmd.Validate(); err != nil {
			t.Errorf("resolve to %s rejected: %v", target, err)
		}
	}
}

func TestStatusCommandValidate_RefusesForgeries(t *testing.T) {
	base := func() StatusCommand {
		cmd, err := NewFailCommand("42", ReasonPaymentDeclined)
		if err != nil {
			t.Fatalf("constructor: %v", err)
		}
		return cmd
	}

	cases := map[string]StatusCommand{
		"free-form command id": func() StatusCommand {
			c := base()
			c.CommandID = "whatever i want"
			return c
		}(),
		"stolen confirm id on a fail": func() StatusCommand {
			c := base()
			c.CommandID = "confirm:42"
			return c
		}(),
		"id for a different order": func() StatusCommand {
			c := base()
			c.CommandID = "fail:43"
			return c
		}(),
		"unknown reason": func() StatusCommand {
			c := base()
			c.Reason = "pq: connection refused"
			return c
		}(),
		"unknown target": func() StatusCommand {
			c := base()
			c.To = "shipped"
			c.CommandID = "fail:42"
			return c
		}(),
		"unknown actor": func() StatusCommand {
			c := base()
			c.ActorType = "INTRUDER"
			return c
		}(),
		"non-canonical order id": {
			OrderID: "042", CommandID: "fail:042", To: OrderStatusFailed,
			Reason: ReasonPaymentDeclined, ActorType: ActorWorkflow,
		},
		"empty command id": func() StatusCommand {
			c := base()
			c.CommandID = ""
			return c
		}(),
	}
	for name, cmd := range cases {
		if err := cmd.Validate(); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("%s: got %v, want ErrInvalidInput", name, err)
		}
	}

	// The verb grammar is per-destination: a cancel id cannot land anywhere
	// but cancelling.
	cmd := base()
	cmd.CommandID = "cancel:42:v5"
	if err := cmd.Validate(); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("cancel id on a fail: got %v, want ErrInvalidInput", err)
	}
}
