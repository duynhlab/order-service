// Package outboxgauge builds the three state gauges every table-backed outbox
// in this service exposes: rows pending, rows that gave up, and the age of the
// oldest pending row. Each outbox keeps its own callback — they differ on what
// a failed read publishes — so only the instruments are shared.
package outboxgauge

import "go.opentelemetry.io/otel/metric"

// Names are the metric names and descriptions of one outbox's gauges.
type Names struct {
	Pending, PendingDesc string
	Failed, FailedDesc   string
	Oldest, OldestDesc   string
}

// Gauges are the created instruments, ready for a RegisterCallback.
type Gauges struct {
	Pending metric.Int64ObservableGauge
	Failed  metric.Int64ObservableGauge
	Oldest  metric.Float64ObservableGauge
}

// New creates the three gauges. Row counts carry the {row} annotation unit and
// the age is in seconds, as the metrics contract requires a unit on every
// instrument.
func New(m metric.Meter, n Names) (Gauges, error) {
	var g Gauges
	var err error
	if g.Pending, err = m.Int64ObservableGauge(n.Pending,
		metric.WithDescription(n.PendingDesc), metric.WithUnit("{row}")); err != nil {
		return Gauges{}, err
	}
	if g.Failed, err = m.Int64ObservableGauge(n.Failed,
		metric.WithDescription(n.FailedDesc), metric.WithUnit("{row}")); err != nil {
		return Gauges{}, err
	}
	if g.Oldest, err = m.Float64ObservableGauge(n.Oldest,
		metric.WithDescription(n.OldestDesc), metric.WithUnit("s")); err != nil {
		return Gauges{}, err
	}
	return g, nil
}
