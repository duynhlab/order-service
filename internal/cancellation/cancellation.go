// Package cancellation starts and sweeps the order-cancellation workflow
// (RFC-0021 P5). It is the fulfillment start seam's lean sibling: the same
// outbox durability argument, minus the payment-token and row-age money
// hazards — every cancellation activity is idempotent and the terminal
// write is CAS'd, so a late or duplicate start is harmless.
package cancellation

import (
	"context"
	"errors"
	"fmt"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"

	"github.com/duynhlab/order-service/internal/saga"
)

// Starter is the slice of the Temporal client Start needs.
type Starter interface {
	ExecuteWorkflow(ctx context.Context, options client.StartWorkflowOptions,
		workflow interface{}, args ...interface{}) (client.WorkflowRun, error)
}

// ErrAlreadyStarted reports that this episode's workflow already exists —
// the caller's start was a replay, not a failure.
var ErrAlreadyStarted = errors.New("cancellation workflow already started")

// startTimeout bounds one start attempt, detached from the request context
// so a client disconnect cannot orphan a half-started episode.
const startTimeout = 5 * time.Second

// Input mirrors saga.CancellationInput at the seam.
type Input struct {
	OrderID string
	UserID  string
	Total   int64
	Epoch   int64
}

// Ready reports whether the Temporal client exists (mirrors fulfillment.Ready).
func Ready(t Starter) bool { return t != nil }

// Start begins one cancellation episode's workflow. REJECT_DUPLICATE +
// error-when-already-started, for the same reason as the fulfillment seam:
// the dispatcher must be able to tell a refused start from a fresh one.
func Start(ctx context.Context, t Starter, taskQueue string, in Input) error {
	startCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), startTimeout)
	defer cancel()
	_, err := t.ExecuteWorkflow(startCtx, client.StartWorkflowOptions{
		ID:                                       saga.CancellationWorkflowID(in.OrderID, in.Epoch),
		TaskQueue:                                taskQueue,
		WorkflowIDReusePolicy:                    enumspb.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE,
		WorkflowExecutionErrorWhenAlreadyStarted: true,
	}, saga.CancellationWorkflow, saga.CancellationInput{
		OrderID: in.OrderID,
		UserID:  in.UserID,
		Total:   in.Total,
		Epoch:   in.Epoch,
	})
	if err != nil {
		var already *serviceerror.WorkflowExecutionAlreadyStarted
		if errors.As(err, &already) {
			return fmt.Errorf("%w: %w", ErrAlreadyStarted, err)
		}
		return err
	}
	return nil
}
