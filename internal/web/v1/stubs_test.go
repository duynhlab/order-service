package v1

import (
	"context"
	"time"

	"github.com/duynhlab/order-service/internal/core/domain"
)

// stubOutbox is a no-op StartRequestRepository/Closer: the surviving web
// handlers never touch the fulfillment outbox, but NewOrderService requires
// non-nil seams.
type stubOutbox struct{}

func (s *stubOutbox) EnqueueWithTx(context.Context, domain.Transaction, string, string, string) error {
	return nil
}
func (s *stubOutbox) MarkDispatchedForUser(context.Context, string, string) error { return nil }
func (s *stubOutbox) MarkDispatched(context.Context, string) error                { return nil }
func (s *stubOutbox) ClaimDue(context.Context, int, time.Duration) ([]domain.FulfillmentStartRequest, error) {
	return nil, nil
}
func (s *stubOutbox) Reschedule(context.Context, string, time.Time, string) error { return nil }
func (s *stubOutbox) MarkFailed(context.Context, string, string) error            { return nil }
func (s *stubOutbox) Stats(context.Context) (domain.StartRequestStats, error) {
	return domain.StartRequestStats{}, nil
}

// stubTx / stubTxManager give the cancel path a transaction to commit (its
// CAS + outbox arm run in one transaction).
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
