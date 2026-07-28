package reconcile

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Observability for the inventory reconciler (RFC-0021 P3).
//
// The on-call questions:
//
//  1. Is anything inconsistent RIGHT NOW that the reconciler could not fix?
//     -> order_reconciler_backlog. This is the alert: it should be zero, and a
//     non-zero value that persists means stock is held or consumed against an
//     order in the wrong state. A repair that succeeds never shows here.
//  2. Is the reconciler doing work, and what kind?
//     -> order_reconciler_repairs_total{action}
//
// A steady stream of committed/released repairs is itself a signal worth
// watching: the saga is supposed to handle those, so the reconciler earning its
// keep every minute means something upstream is failing regularly.
var (
	meter = otel.Meter("order-service")

	repairCounter, _ = meter.Int64Counter("order.reconciler.repairs.total",
		metric.WithDescription("Inventory reconciler actions by kind"))
)

// recordRepair counts one action. action is one of the bounded Action* values.
func recordRepair(ctx context.Context, action string) {
	repairCounter.Add(ctx, 1, metric.WithAttributes(
		attribute.String("action", action)))
}

// RegisterGauge exposes the last pass's unresolved count.
//
// Observable rather than incremented, and read from the reconciler's own last
// result rather than the database: the inconsistency is only knowable by asking
// inventory, so there is no query a callback could run instead. Storing the
// pass result and reporting it keeps the gauge honest about WHEN it was measured
// — one interval old at most.
//
// The returned Registration must be kept by tests; production ignores it.
func (r *Reconciler) RegisterGauge() (metric.Registration, error) {
	backlog, err := meter.Int64ObservableGauge("order.reconciler.backlog",
		metric.WithDescription("Inconsistencies the last reconciler pass could not resolve"))
	if err != nil {
		return nil, err
	}
	return meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		o.ObserveInt64(backlog, r.backlog.Load())
		return nil
	}, backlog)
}
