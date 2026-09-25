package outboxgauge

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestNewCreatesThreeGaugesWithUnits(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	m := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("t")
	g, err := New(m, Names{
		Pending: "t.pending", PendingDesc: "p",
		Failed: "t.failed", FailedDesc: "f",
		Oldest: "t.oldest", OldestDesc: "o",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		o.ObserveInt64(g.Pending, 2)
		o.ObserveInt64(g.Failed, 1)
		o.ObserveFloat64(g.Oldest, 3.5)
		return nil
	}, g.Pending, g.Failed, g.Oldest); err != nil {
		t.Fatal(err)
	}
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	units := map[string]string{}
	for _, sm := range rm.ScopeMetrics {
		for _, md := range sm.Metrics {
			units[md.Name] = md.Unit
		}
	}
	want := map[string]string{"t.pending": "{row}", "t.failed": "{row}", "t.oldest": "s"}
	for name, unit := range want {
		if units[name] != unit {
			t.Errorf("%s: unit %q, want %q (all: %v)", name, units[name], unit, units)
		}
	}
}

func TestNewReportsAnInvalidName(t *testing.T) {
	m := sdkmetric.NewMeterProvider().Meter("t")
	// The SDK rejects a name that does not start with a letter.
	for _, n := range []Names{
		{Pending: "1bad", Failed: "t.f", Oldest: "t.o"},
		{Pending: "t.p", Failed: "1bad", Oldest: "t.o"},
		{Pending: "t.p", Failed: "t.f", Oldest: "1bad"},
	} {
		if _, err := New(m, n); err == nil {
			t.Errorf("New(%+v): want an error for the invalid name", n)
		}
	}
}
