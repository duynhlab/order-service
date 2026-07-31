package cancellation

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.uber.org/zap"

	"github.com/duynhlab/order-service/internal/core/domain"
)

// Observability for the cancellation start path (RFC-0021 P5). The on-call
// questions mirror the fulfillment outbox's:
//  1. Are cancellation starts being dispatched, and how?  → dispatch counter
//  2. Is anything stuck waiting for a start?              → outbox gauges
//
// The order-level signal (stuck in `cancelling`) is the reconciler package's
// order_cancelling_backlog gauge; these cover the start machinery itself.
var (
	meter = otel.Meter("order-service")

	dispatchCounter, _ = meter.Int64Counter("order.cancellation.start_dispatch.total",
		metric.WithDescription("Cancellation-workflow start dispatches by result"))
)

// Bounded dispatch results.
const (
	resultDispatched = "dispatched"
	resultError      = "error"
	resultFailed     = "failed"
)

func recordCancellationDispatch(ctx context.Context, result string) {
	dispatchCounter.Add(ctx, 1, metric.WithAttributes(
		attribute.String("result", result)))
}

// RegisterOutboxGauges exposes the cancellation outbox's open state. Same
// design rules as every table-backed gauge in this service: registered in
// both processes, read from the table each collection cycle, and a failing
// read publishes NOTHING rather than zero or an SDK error.
func RegisterOutboxGauges(store domain.CancellationRequestStore, log *zap.Logger) (metric.Registration, error) {
	pending, err := meter.Int64ObservableGauge("order.cancellation.outbox.pending",
		metric.WithDescription("Cancellation starts not yet dispatched"))
	if err != nil {
		return nil, err
	}
	failed, err := meter.Int64ObservableGauge("order.cancellation.outbox.failed",
		metric.WithDescription("Cancellation starts that exhausted their attempts"))
	if err != nil {
		return nil, err
	}
	oldest, err := meter.Float64ObservableGauge("order.cancellation.outbox.oldest_pending_age",
		metric.WithDescription("Age in seconds of the oldest undispatched cancellation start"),
		metric.WithUnit("s"))
	if err != nil {
		return nil, err
	}
	return meter.RegisterCallback(func(ctx context.Context, o metric.Observer) error {
		stats, err := store.Stats(ctx)
		if err != nil {
			log.Warn("could not read the cancellation outbox; publishing no value for this cycle",
				zap.Error(err))
			return nil
		}
		o.ObserveInt64(pending, int64(stats.Pending))
		o.ObserveInt64(failed, int64(stats.Failed))
		o.ObserveFloat64(oldest, stats.OldestPendingAge.Seconds())
		return nil
	}, pending, failed, oldest)
}
