package cancellation

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.uber.org/zap"

	"github.com/duynhlab/order-service/internal/core/domain"
)

var testReader *sdkmetric.ManualReader

func init() {
	testReader = sdkmetric.NewManualReader()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(testReader)))
}

type statsOutbox struct {
	fakeOutbox
	stats domain.CancellationRequestStats
	err   error
}

func (s *statsOutbox) Stats(context.Context) (domain.CancellationRequestStats, error) {
	return s.stats, s.err
}

func gaugeValue(t *testing.T, name string) (float64, bool) {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := testReader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			switch g := m.Data.(type) {
			case metricdata.Gauge[int64]:
				for _, dp := range g.DataPoints {
					return float64(dp.Value), true
				}
			case metricdata.Gauge[float64]:
				for _, dp := range g.DataPoints {
					return dp.Value, true
				}
			}
		}
	}
	return 0, false
}

func TestRegisterOutboxGauges_ReadFromTheStore(t *testing.T) {
	store := &statsOutbox{stats: domain.CancellationRequestStats{
		Pending: 2, Failed: 1, OldestPendingAge: 90 * time.Second,
	}}
	reg, err := RegisterOutboxGauges(store, zap.NewNop())
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	t.Cleanup(func() { _ = reg.Unregister() })

	if got, ok := gaugeValue(t, "order.cancellation.outbox.pending"); !ok || got != 2 {
		t.Errorf("pending = %v ok=%v, want 2", got, ok)
	}
	if got, ok := gaugeValue(t, "order.cancellation.outbox.failed"); !ok || got != 1 {
		t.Errorf("failed = %v ok=%v, want 1", got, ok)
	}
	if got, ok := gaugeValue(t, "order.cancellation.outbox.oldest_pending_age"); !ok || got != 90 {
		t.Errorf("oldest = %v ok=%v, want 90s", got, ok)
	}
}

// The house rule for every table-backed gauge: a failed read publishes
// NOTHING (never zero) and must not fail the collection cycle.
func TestRegisterOutboxGauges_FailedReadPublishesNothing(t *testing.T) {
	store := &statsOutbox{err: errors.New("db down")}
	reg, err := RegisterOutboxGauges(store, zap.NewNop())
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	t.Cleanup(func() { _ = reg.Unregister() })

	if _, ok := gaugeValue(t, "order.cancellation.outbox.pending"); ok {
		t.Error("a failed read must publish no datapoint")
	}
}

// Run sweeps immediately and stops with its context.
func TestDispatcher_RunSweepsAndStops(t *testing.T) {
	outbox := &fakeOutbox{due: []domain.CancellationRequest{req("42", 5, 1)}}
	starter := &fakeStarter{}
	d := newTestDispatcher(outbox, &fakeLoader{}, starter)
	d.pollInterval = 5 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	d.Run(ctx) // returns when the context ends

	if len(starter.calls) == 0 {
		t.Fatal("the initial sweep must have dispatched the due row")
	}
}
