// Package fulfillment is the single place the order-fulfillment saga is
// started from. Both transports delegate here (web keeps its own logging
// semantics; gRPC adds idempotent-kickoff semantics) so the load-bearing
// details — the saga input mapping, the detached 5-second start context, the
// workflow-id dedup scheme — cannot drift between them (RFC-0015 P2,
// homelab ADR-018).
package fulfillment

import (
	"context"
	"errors"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"

	"github.com/duynhlab/order-service/internal/core/domain"
	"github.com/duynhlab/order-service/internal/saga"
)

// Starter starts a Temporal workflow. *client.Client satisfies it; kept as an
// interface so both transports stay testable with a fake.
type Starter interface {
	ExecuteWorkflow(ctx context.Context, options client.StartWorkflowOptions, workflow any, args ...any) (client.WorkflowRun, error)
}

// ErrAlreadyStarted reports that a fulfillment workflow for this order id
// already exists (open — any reuse policy; or closed within the namespace
// retention when RejectDuplicate is used). Callers decide what that means:
// the gRPC adapter treats it as success (the saga already happened), the web
// handler logs it like any other start failure (pre-P2 behavior preserved).
var ErrAlreadyStarted = errors.New("fulfillment workflow already started")

// startTimeout bounds the workflow start call, detached from the caller's
// request context so a client disconnect cannot cancel the start.
const startTimeout = 5 * time.Second

// Options tunes the start semantics per transport.
type Options struct {
	// ReusePolicy: zero (UNSPECIFIED) keeps the server default
	// (AllowDuplicate) — the web handler's pre-P2 behavior. The gRPC adapter
	// passes RejectDuplicate so a replayed CreateOrder can never re-run a
	// closed saga within the namespace retention (7 days on this platform —
	// homelab kubernetes/infra/configs/temporal/namespace.yaml; the belt to
	// this brace is the caller's order-status gate).
	// Semantics: https://docs.temporal.io/workflow-execution/workflowid-runid
	ReusePolicy enumspb.WorkflowIdReusePolicy
}

// Start kicks off OrderFulfillmentWorkflow for a committed order. It builds
// the saga input exactly as the web handler always has: no bearer token (the
// saga's cart-clear uses the tokenless internal route), paymentMethod carried
// separately from the order (it is never persisted). Returns
// ErrAlreadyStarted when a workflow with this order's id already exists;
// any other error is the raw start failure. t must be non-nil.
func Start(ctx context.Context, t Starter, taskQueue string, order *domain.Order, paymentMethod string, opts Options) error {
	items := make([]saga.ReserveItem, len(order.Items))
	for i, it := range order.Items {
		items[i] = saga.ReserveItem{ProductID: it.ProductID, Quantity: it.Quantity}
	}
	input := saga.OrderFulfillmentInput{
		OrderID:       order.ID,
		UserID:        order.UserID,
		Total:         order.Total,
		Items:         items,
		PaymentMethod: paymentMethod,
	}

	startCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), startTimeout)
	defer cancel()
	_, err := t.ExecuteWorkflow(startCtx, client.StartWorkflowOptions{
		ID:                    saga.WorkflowID(order.ID),
		TaskQueue:             taskQueue,
		WorkflowIDReusePolicy: opts.ReusePolicy,
	}, saga.OrderFulfillmentWorkflow, input)
	if err != nil {
		var already *serviceerror.WorkflowExecutionAlreadyStarted
		if errors.As(err, &already) {
			return ErrAlreadyStarted
		}
		return err
	}
	return nil
}
