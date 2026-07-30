package fulfillment

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/duynhlab/order-service/internal/core/domain"
)

// Observability for the fulfillment start outbox (RFC-0021 P3).
//
// The on-call questions these exist to answer, in order:
//
//  1. Is any committed order missing its saga right now?
//     -> order_fulfillment_start_outbox_pending
//  2. How long has the oldest one been waiting? This, NOT the count, is what
//     alerts fire on: one order pending for 20 minutes is an incident, twenty
//     pending for two seconds during a Temporal restart is the system working.
//     -> order_fulfillment_start_outbox_oldest_age_seconds
//  3. Is anything stuck needing a human?
//     -> order_fulfillment_start_outbox_failed
//  4. What is the dispatcher actually doing?
//     -> order_fulfillment_start_dispatch_total{result}
//
// The first three are derived from database state, so they are OBSERVABLE
// (async) gauges read on collection rather than counters the code increments.
// A self-incremented gauge would drift the moment a process restarted or two
// dispatchers ran; the table is the truth and this reads it.
var (
	meter = otel.Meter("order-service")

	startDispatchCounter, _ = meter.Int64Counter("order.fulfillment.start_dispatch.total",
		metric.WithDescription("Outbox dispatch attempts by result"))

	startParticipantCounter, _ = meter.Int64Counter("order.fulfillment.start_participant.total",
		metric.WithDescription("Saga starts by resolved stock participant and where that value came from"))
)

// recordStartDispatch counts one dispatch outcome. result is one of the bounded
// Result* constants — never an error string.
func recordStartDispatch(ctx context.Context, result string) {
	startDispatchCounter.Add(ctx, 1, metric.WithAttributes(
		attribute.String("result", result)))
}

// recordStartParticipant counts the branch a start actually resolved, and why.
//
// The fifth on-call question, and the one the RFC-0021 cutover is steered by:
// which branch are sagas starting on, and is anything still answering from a
// record this build cannot use? Without it the resolution is invisible — after a
// flag flip the only way to tell which branch an order took is to read its
// Temporal history one order at a time.
//
// source="unrecognised" is the alert-worthy value: it means a row named something
// no build understands and the flag was used instead. source="absent" should decay
// to zero as pre-column orders age out; if it does not, rows are being written
// without a participant.
//
// Both labels come from closed sets (two participants, three sources), so this
// cannot grow cardinality.
func recordStartParticipant(ctx context.Context, participant string, source ParticipantSource) {
	startParticipantCounter.Add(ctx, 1, metric.WithAttributes(
		attribute.String("participant", participant),
		attribute.String("source", source.String())))
}

// String names a source for the metric label. Not a fmt.Stringer for display —
// these exact strings are the label values on-call queries by.
func (s ParticipantSource) String() string {
	switch s {
	case SourceRecorded:
		return "recorded"
	case SourceAbsent:
		return "absent"
	case SourceUnrecognised:
		return "unrecognised"
	}
	return "invalid"
}

// RegisterOutboxGauges wires the three state gauges to the outbox. It is called
// once at worker startup; the callback runs on every collection cycle.
//
// A failing read returns the error to the SDK rather than reporting a zero:
// zero-on-error is indistinguishable from "nothing pending", which is exactly
// the reading an operator must not be handed during a database problem.
// The returned Registration must be kept: an erroring callback poisons every
// later Collect() in the process, so a test that registers one has to be able to
// unregister it. Production ignores it (the callback lives as long as the
// process), but returning it is what makes the package's own tests
// order-independent — without it they failed roughly half the time under
// -shuffle=on.
func RegisterOutboxGauges(outbox domain.StartRequestRepository) (metric.Registration, error) {
	pending, err := meter.Int64ObservableGauge("order.fulfillment.start_outbox.pending",
		metric.WithDescription("Committed orders whose fulfillment start is still owed"))
	if err != nil {
		return nil, err
	}
	failed, err := meter.Int64ObservableGauge("order.fulfillment.start_outbox.failed",
		metric.WithDescription("Start requests that gave up and need a manual requeue"))
	if err != nil {
		return nil, err
	}
	oldest, err := meter.Float64ObservableGauge("order.fulfillment.start_outbox.oldest_age",
		metric.WithDescription("Age of the oldest pending start request"),
		metric.WithUnit("s"))
	if err != nil {
		return nil, err
	}

	return meter.RegisterCallback(func(ctx context.Context, o metric.Observer) error {
		stats, err := outbox.Stats(ctx)
		if err != nil {
			return err
		}
		o.ObserveInt64(pending, int64(stats.Pending))
		o.ObserveInt64(failed, int64(stats.Failed))
		o.ObserveFloat64(oldest, stats.OldestPendingAge.Seconds())
		return nil
	}, pending, failed, oldest)
}
