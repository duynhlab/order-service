package reconcile

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.uber.org/zap"

	"github.com/duynhlab/order-service/internal/core/domain"
)

// Observability for the inventory reconciler (RFC-0021 P3).
//
// The on-call questions:
//
//  1. Is anything inconsistent RIGHT NOW?
//     -> order_reconciler_backlog. It should be zero, and a non-zero value that
//     persists means stock is held or consumed against an order in the wrong
//     state. A repair that succeeds settles its row, so it never shows here.
//     UNWINDOWED on purpose — an unresolved breach must not age out of the number
//     an operator is watching.
//
//     NOTE: no alert, dashboard or runbook ships with this change. There is also
//     no supported way yet to CLEAR a breach once a human has resolved it — the
//     row's reconcile_breach_code and reconciled_at have to be set by hand. Both
//     gaps belong to the write-path alerts + RUNBOOK-007 change. The series
//     reaches VictoriaMetrics through the OTLP pipeline and is currently
//     UNALERTED — the write-path alerts land separately, and they need absent()
//     handling, because a database failure makes the gauge disappear rather than
//     go high.
//
//  2. Is the reconciler doing work, and what kind?
//     -> order_reconciler_repairs_total{action}
//
//  4. Is any order's stock branch out of step with what its row recorded?
//     -> order_reconciler_participant_disagreements_total{row_participant}
//     Flat zero once every start path resolves the branch from the order. It sees
//     only the half where a reservation exists — see
//     Reconciler.reportParticipantDisagreement for the other half.
//
// A steady stream of committed/released repairs is itself a signal worth
// watching: the saga is supposed to handle those, so the reconciler earning its
// keep every minute means something upstream is failing regularly.
var (
	meter = otel.Meter("order-service")

	repairCounter, _ = meter.Int64Counter("order.reconciler.repairs.total",
		metric.WithDescription("Inventory reconciler actions by kind"))

	// Truncation makes the backlog gauge a FLOOR rather than a count, so it needs
	// to be alertable rather than only greppable.
	truncatedCounter, _ = meter.Int64Counter("order.reconciler.passes.truncated.total",
		metric.WithDescription("Reconciler passes that hit their batch cap, so the window was not fully examined"))

	// The cutover's own health signal: orders holding a reservation their row does
	// not account for. It should be flat at zero, and any increase points at a saga
	// start that chose its branch from a flag rather than from the order.
	participantDisagreementCounter, _ = meter.Int64Counter("order.reconciler.participant_disagreements.total",
		metric.WithDescription("Orders holding an inventory reservation while not recorded as inventory-path"))
)

// recordTruncated counts one pass that did not see its whole window.
func recordTruncated(ctx context.Context) {
	truncatedCounter.Add(ctx, 1)
}

// recordParticipantDisagreement counts one order whose reservation and row do not
// agree about which service owns its stock.
//
// The label is normalised rather than passed through: the column is meant to hold
// a closed enum, but this metric fires precisely when something has gone wrong
// with it, and that is the worst moment to let an unexpected string become a new
// time series.
func recordParticipantDisagreement(ctx context.Context, rowParticipant string) {
	recorded := "other"
	switch rowParticipant {
	case "":
		recorded = "absent"
	case participantProduct:
		recorded = participantProduct
	}
	participantDisagreementCounter.Add(ctx, 1, metric.WithAttributes(
		attribute.String("row_participant", recorded)))
}

// recordRepair counts one action. action is one of the bounded Action* values.
func recordRepair(ctx context.Context, action string) {
	repairCounter.Add(ctx, 1, metric.WithAttributes(
		attribute.String("action", action)))
}

// RegisterBacklogGauge exposes how many terminal orders are still unsettled.
//
// A FUNCTION of the store, not a method on Reconciler, because the backlog is a
// query and has no dependency on the repair machinery. That is what lets it be
// registered in BOTH processes — the same reasoning cmd/main.go applies to the
// outbox gauges. The reconciler lives in the worker, and the worker exits when
// Temporal is unreachable; if the backlog only reported from there, the situations
// where stranded stock accumulates (worker down, or ORDER_RECONCILER_ENABLED=false
// during an incident) would be exactly the situations with no signal. Two
// reporters is not double-counting: it is the same table read twice.
//
// Read from the TABLE on every collection cycle, not from the last pass's
// result. That distinction is the whole point: a value carried in memory sticks
// at its old reading when a pass fails, reads zero after a restart until the
// first pass completes, can publish a false high if a pass is interrupted at
// shutdown, and conflates "inventory was unreachable" with "something is
// inconsistent". A count of unsettled rows has none of those failure modes.
//
// A failing read publishes NOTHING and logs, rather than reporting zero or
// returning the error to the SDK. Both alternatives are worse:
//
//   - Zero means "everything agrees", the one thing an operator must not be told
//     during a database problem.
//   - Returning the error takes down every OTHER metric too. PeriodicReader does
//     `err := r.Collect(ctx, rm); if err == nil { export }`
//     (sdk/metric@v1.44.0/periodic_reader.go:252), so ONE failing callback
//     discards the entire ResourceMetrics for that cycle — saga outcomes, outbox
//     gauges, runtime metrics, all of it. Every series would go absent at once, so
//     `absent(order_reconciler_backlog)` could no longer tell a database problem
//     from a dead pod, which is the exact distinction this gauge supports.
//
// Publishing nothing makes THIS series absent and leaves the others intact.
//
// The returned Registration must be kept by the caller: a callback that outlives
// its database pool queries a closed pool on the next collection cycle.
func RegisterBacklogGauge(store domain.ReconcileStore, log *zap.Logger) (metric.Registration, error) {
	backlog, err := meter.Int64ObservableGauge("order.reconciler.backlog",
		metric.WithDescription("Terminal orders whose stock has not been confirmed to agree with their outcome"))
	if err != nil {
		return nil, err
	}
	return meter.RegisterCallback(func(ctx context.Context, o metric.Observer) error {
		n, err := store.CountUnreconciled(ctx, DefaultSettleDelay)
		if err != nil {
			log.Warn("could not read the reconciler backlog; publishing no value for this cycle",
				zap.Error(err))
			return nil
		}
		o.ObserveInt64(backlog, int64(n))
		return nil
	}, backlog)
}
