package v1

import (
	"context"
	"time"

	"github.com/duynhlab/order-service/internal/core/domain"
	logicv1 "github.com/duynhlab/order-service/internal/logic/v1"
	"github.com/duynhlab/order-service/internal/saga"
	"go.temporal.io/sdk/client"
)

// stubOutbox is the start-outbox seam for handler tests: startFulfillment
// closes the row after a successful start, so the handler needs a real service
// wired to something rather than a nil one.
type stubOutbox struct {
	marked              []string
	err                 error
	enqueuedParticipant string
}

func (s *stubOutbox) EnqueueWithTx(_ context.Context, _ domain.Transaction, _, _, participant string) error {
	s.enqueuedParticipant = participant
	return nil
}

func (s *stubOutbox) MarkDispatchedForUser(_ context.Context, _, orderID string) error {
	return s.MarkDispatched(context.Background(), orderID)
}

func (s *stubOutbox) MarkDispatched(_ context.Context, orderID string) error {
	s.marked = append(s.marked, orderID)
	return s.err
}

func (s *stubOutbox) ClaimDue(context.Context, int, time.Duration) ([]domain.FulfillmentStartRequest, error) {
	return nil, nil
}

func (s *stubOutbox) Reschedule(context.Context, string, time.Time, string) error { return nil }
func (s *stubOutbox) MarkFailed(context.Context, string, string) error            { return nil }

func (s *stubOutbox) Stats(context.Context) (domain.StartRequestStats, error) {
	return domain.StartRequestStats{}, nil
}

// newOutboxService builds the minimal OrderService startFulfillment needs: only
// the outbox is reachable from that path.
func newOutboxService(outbox *stubOutbox) *logicv1.OrderService {
	return logicv1.NewOrderService(nil, nil, outbox, outbox, noopProjection{}, nil, nil)
}

type stubStarter struct {
	called   bool
	gotID    string
	gotInput saga.OrderFulfillmentInput
	err      error
}

func (s *stubStarter) ExecuteWorkflow(_ context.Context, opts client.StartWorkflowOptions, _ any, args ...any) (client.WorkflowRun, error) {
	s.called = true
	s.gotID = opts.ID
	if len(args) > 0 {
		if in, ok := args[0].(saga.OrderFulfillmentInput); ok {
			s.gotInput = in
		}
	}
	return nil, s.err
}

// stubTx / stubTxManager give the REST create path a transaction to commit, so
// the participant stamp can be observed where it actually lands: the outbox row.
type stubTx struct{}

func (stubTx) Commit(context.Context) error   { return nil }
func (stubTx) Rollback(context.Context) error { return nil }

type stubTxManager struct{}

func (stubTxManager) Begin(context.Context) (domain.Transaction, error) { return stubTx{}, nil }

// noopProjection satisfies domain.ProcessingProjector for wiring tests.
type noopProjection struct{}

func (noopProjection) UpsertProcessingStage(context.Context, domain.ProcessingUpdate) error {
	return nil
}
func (noopProjection) UpsertProcessingStageWithTx(context.Context, domain.Transaction, domain.ProcessingUpdate) error {
	return nil
}
