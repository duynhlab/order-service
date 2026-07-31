package cancellation

import (
	"context"
	"errors"
	"time"

	"go.uber.org/zap"

	"github.com/duynhlab/order-service/internal/core/domain"
)

// Dispatcher defaults — deliberately the fulfillment dispatcher's numbers;
// the cancellation outbox is its lean sibling.
const (
	DefaultPollInterval = 15 * time.Second
	DefaultBatchSize    = 50
	DefaultLease        = time.Minute
	// DefaultMaxAttempts caps retries before the row goes FAILED. Unlike the
	// fulfillment outbox there is no money-age hazard behind the cap — the
	// order simply stays `cancelling` and the stuck-cancelling gauge pages a
	// human — so the cap only bounds pointless retry noise.
	DefaultMaxAttempts = 60
)

// Dispatcher sweeps the cancellation outbox for episodes whose inline start
// did not land. Much simpler than the fulfillment dispatcher on purpose: no
// payment token to guard, no row-age money hazard, and no participant
// resolution (the workflow reads every external state server-side).
type Dispatcher struct {
	outbox  domain.CancellationRequestStore
	orders  domain.OrderLoader
	starter Starter

	taskQueue    string
	pollInterval time.Duration
	batchSize    int
	lease        time.Duration
	maxAttempts  int

	timeNow func() time.Time
	log     *zap.Logger
}

// NewDispatcher wires a cancellation dispatcher with the defaults.
func NewDispatcher(outbox domain.CancellationRequestStore, orders domain.OrderLoader,
	starter Starter, taskQueue string, log *zap.Logger) *Dispatcher {
	return &Dispatcher{
		outbox:       outbox,
		orders:       orders,
		starter:      starter,
		taskQueue:    taskQueue,
		pollInterval: DefaultPollInterval,
		batchSize:    DefaultBatchSize,
		lease:        DefaultLease,
		maxAttempts:  DefaultMaxAttempts,
		timeNow:      time.Now,
		log:          log,
	}
}

// Run sweeps until the context ends. Sweeps immediately on start: the most
// likely reason a row waits is the crash that restarted this process.
func (d *Dispatcher) Run(ctx context.Context) {
	ticker := time.NewTicker(d.pollInterval)
	defer ticker.Stop()

	d.log.Info("cancellation dispatcher running",
		zap.Duration("poll_interval", d.pollInterval), zap.Int("batch_size", d.batchSize))

	if err := d.Sweep(ctx); err != nil && !errors.Is(err, context.Canceled) {
		d.log.Error("initial cancellation-outbox sweep failed", zap.Error(err))
	}
	for {
		select {
		case <-ctx.Done():
			d.log.Info("cancellation dispatcher stopped")
			return
		case <-ticker.C:
			if err := d.Sweep(ctx); err != nil && !errors.Is(err, context.Canceled) {
				d.log.Error("cancellation-outbox sweep failed", zap.Error(err))
			}
		}
	}
}

// Sweep claims one batch and dispatches each row. Exported so tests and the
// e2e can drive exactly one pass.
func (d *Dispatcher) Sweep(ctx context.Context) error {
	claimed, err := d.outbox.ClaimDue(ctx, d.batchSize, d.lease)
	if err != nil {
		return err
	}
	for _, req := range claimed {
		d.dispatch(ctx, req)
	}
	return nil
}

// dispatch starts one episode's workflow and settles its row.
func (d *Dispatcher) dispatch(ctx context.Context, req domain.CancellationRequest) {
	order, err := d.orders.LoadForFulfillment(ctx, req.OrderID)
	if err != nil {
		// A missing order row under a live outbox row is an invariant breach
		// (FK ON DELETE CASCADE), so every error here is treated as
		// transient and retried.
		recordCancellationDispatch(ctx, resultError)
		d.retryOrFail(ctx, req, "ORDER_READ_FAILED")
		return
	}

	err = Start(ctx, d.starter, d.taskQueue, Input{
		OrderID: req.OrderID,
		UserID:  order.UserID,
		Total:   order.Total,
		Epoch:   req.Epoch,
	})
	switch {
	case err == nil, errors.Is(err, ErrAlreadyStarted):
		// Already-started is the inline path having won (or a previous sweep
		// whose MarkDispatched was lost) — the workflow exists, close the row.
		recordCancellationDispatch(ctx, resultDispatched)
		if err := d.outbox.MarkDispatched(ctx, req.OrderID, req.Epoch); err != nil {
			d.log.Warn("could not close a dispatched cancellation row; the next sweep replays it",
				zap.String("order_id", req.OrderID), zap.Error(err))
		}
	default:
		recordCancellationDispatch(ctx, resultError)
		d.log.Error("cancellation workflow start failed",
			zap.String("order_id", req.OrderID), zap.Error(err))
		d.retryOrFail(ctx, req, "START_FAILED")
	}
}

// retryOrFail schedules the next attempt, or parks the row at the cap. The
// order stays `cancelling` either way — the stuck-cancelling gauge owns the
// escalation to a human.
func (d *Dispatcher) retryOrFail(ctx context.Context, req domain.CancellationRequest, code string) {
	if req.Attempts >= d.maxAttempts {
		recordCancellationDispatch(ctx, resultFailed)
		d.log.Error("cancellation start attempts exhausted; row is now a worklist item",
			zap.String("order_id", req.OrderID), zap.Int("attempts", req.Attempts))
		if err := d.outbox.MarkFailed(ctx, req.OrderID, req.Epoch, code); err != nil {
			d.log.Warn("could not fail a cancellation row", zap.String("order_id", req.OrderID), zap.Error(err))
		}
		return
	}
	next := d.timeNow().Add(backoffFor(req.Attempts))
	if err := d.outbox.Reschedule(ctx, req.OrderID, req.Epoch, next, code); err != nil {
		d.log.Warn("could not reschedule a cancellation row; the lease expiry re-queues it",
			zap.String("order_id", req.OrderID), zap.Error(err))
	}
}

// backoffFor mirrors the fulfillment dispatcher: exponential from 15s,
// capped at 5 minutes.
func backoffFor(attempts int) time.Duration {
	d := 15 * time.Second
	for i := 1; i < attempts && d < 5*time.Minute; i++ {
		d *= 2
	}
	if d > 5*time.Minute {
		d = 5 * time.Minute
	}
	return d
}
