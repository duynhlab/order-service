package domain

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// OrderStatusWriter is the ONLY status-write surface of the order aggregate.
//
// It is deliberately a separate interface from OrderRepository (the
// OrderLoader precedent): the request path holds OrderRepository, and a
// convenient generic status write sitting there is exactly the hazard this
// phase removes. Code that transitions orders asks for this interface by
// name and hands it a validated StatusCommand — there is no SetStatus.
//
// The implementation applies the command in one transaction, under a row
// lock: replay detection by (order_id, command_id), FSM + actor validation
// against the CURRENT row state, the history insert, and the guarded
// version-bumping UPDATE either all happen or none do.
type OrderStatusWriter interface {
	// ApplyStatusCommand applies cmd or replays its recorded outcome.
	// replayed=true means the command had already been applied and the call
	// changed nothing — callers audit-logging operator actions must record
	// the second touch themselves. Errors: ErrNotFound (no such order),
	// ErrInvalidTransition (FSM or actor matrix refuses, from the CURRENT
	// state), ErrIdempotencyConflict (command id replayed with a different
	// outcome), ErrConcurrencyConflict (retryable race), ErrInvalidInput
	// (malformed command that escaped the constructors).
	ApplyStatusCommand(ctx context.Context, cmd StatusCommand) (replayed bool, err error)
}

// StatusHistoryEntry is one recorded transition, as the operator case view
// reads it. Every field is a value the writer committed in the same
// transaction as the transition itself, so this is the audit trail — not a
// best-effort log that could be missing a row.
type StatusHistoryEntry struct {
	FromStatus string    `json:"from_status"`
	ToStatus   string    `json:"to_status"`
	ReasonCode string    `json:"reason_code,omitempty"`
	ActorType  string    `json:"actor_type"`
	ActorID    string    `json:"actor_id,omitempty"`
	Note       string    `json:"note,omitempty"`
	CommandID  string    `json:"command_id"`
	CreatedAt  time.Time `json:"created_at"`
}

// StatusHistoryReader reads an order's transitions, newest first.
//
// ADR-051 makes this trail the only control on a trusted operator command, so
// it has to be readable by the surface the operator acts on — an audit nobody
// can see is not a control. *repository.PostgresOrderRepository satisfies it.
type StatusHistoryReader interface {
	ListStatusHistory(ctx context.Context, orderID string) ([]StatusHistoryEntry, error)
}

// commandVerbs is the id grammar per destination: which builder verbs may
// mint a command that lands on To. The repository re-checks this so a
// literal-constructed command with a stolen or free-form id is refused even
// though StatusCommand fields are exported (they cross the Temporal payload
// boundary and cannot be sealed).
var commandVerbs = map[OrderStatus][]string{
	OrderStatusConfirmed:    {"confirm", "resolve"},
	OrderStatusFailed:       {"fail", "resolve"},
	OrderStatusCompleted:    {"complete", "resolve"},
	OrderStatusManualReview: {"manual-review"},
	OrderStatusCancelling:   {"cancel"},
	OrderStatusCancelled:    {"cancel-complete", "resolve"},
}

// Validate re-checks everything a constructor guarantees, for commands that
// arrived over a payload boundary or were (wrongly) built as literals.
func (c StatusCommand) Validate() error {
	if err := requireOrderID("status", c.OrderID); err != nil {
		return err
	}
	if !KnownStatus(c.To) {
		return fmt.Errorf("status command for order %s: %w: unknown target %q", c.OrderID, ErrInvalidInput, c.To)
	}
	switch c.ActorType {
	case ActorSystem, ActorWorkflow, ActorUser, ActorOperator:
	default:
		return fmt.Errorf("status command for order %s: %w: unknown actor %q", c.OrderID, ErrInvalidInput, c.ActorType)
	}
	if c.Reason != "" && !KnownReason(c.Reason) {
		return fmt.Errorf("status command for order %s: %w: unknown reason %q", c.OrderID, ErrInvalidInput, c.Reason)
	}
	if len(c.ActorID) > maxActorIDLen {
		return fmt.Errorf("status command for order %s: %w: actor id too long", c.OrderID, ErrInvalidInput)
	}
	if c.CommandID == "" || len(c.CommandID) > 255 {
		return fmt.Errorf("status command for order %s: %w: bad command id", c.OrderID, ErrInvalidInput)
	}
	for _, verb := range commandVerbs[c.To] {
		prefix := verb + ":" + c.OrderID
		if c.CommandID == prefix || strings.HasPrefix(c.CommandID, prefix+":") {
			return nil
		}
	}
	return fmt.Errorf("status command for order %s: %w: command id %q does not match target %q",
		c.OrderID, ErrInvalidInput, c.CommandID, c.To)
}
