package fulfillment

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/duynhlab/order-service/internal/core/domain"
)

// testReader is the process-wide ManualReader wired as the global MeterProvider.
// The instruments in metrics.go are created at package init through the global
// meter and are upgraded to forward here once the provider is installed, which
// is why this is done once in TestMain rather than per test.
var testReader *sdkmetric.ManualReader

func TestMain(m *testing.M) {
	testReader = sdkmetric.NewManualReader()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(testReader)))
	os.Exit(m.Run())
}

// statsOutbox reports fixed stats (or an error) so the gauge callback can be
// observed without a database.
type statsOutbox struct {
	*fakeOutbox
	stats domain.StartRequestStats
	err   error
}

func (s *statsOutbox) Stats(context.Context) (domain.StartRequestStats, error) {
	if s.err != nil {
		return domain.StartRequestStats{}, s.err
	}
	return s.stats, nil
}

func gaugeValues(t *testing.T) map[string]float64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := testReader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	out := map[string]float64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			switch data := m.Data.(type) {
			case metricdata.Gauge[int64]:
				for _, dp := range data.DataPoints {
					out[m.Name] = float64(dp.Value)
				}
			case metricdata.Gauge[float64]:
				for _, dp := range data.DataPoints {
					out[m.Name] = dp.Value
				}
			}
		}
	}
	return out
}

// The gauges must report what the TABLE says, because that is the whole reason
// they are observable callbacks rather than counters this code increments: a
// self-incremented gauge drifts the moment a process restarts.
func TestOutboxGauges_ReportTableState(t *testing.T) {
	outbox := &statsOutbox{
		fakeOutbox: newFakeOutbox(),
		stats: domain.StartRequestStats{
			Pending:          3,
			Failed:           1,
			OldestPendingAge: 90 * time.Second,
		},
	}
	reg, err := RegisterOutboxGauges(outbox)
	if err != nil {
		t.Fatalf("RegisterOutboxGauges() = %v", err)
	}
	// Unregister, or an erroring callback poisons every later Collect() in the
	// process and the package's tests become order-dependent.
	t.Cleanup(func() { _ = reg.Unregister() })

	got := gaugeValues(t)
	for name, want := range map[string]float64{
		"order.fulfillment.start_outbox.pending":    3,
		"order.fulfillment.start_outbox.failed":     1,
		"order.fulfillment.start_outbox.oldest_age": 90,
	} {
		if got[name] != want {
			t.Errorf("%s = %v, want %v", name, got[name], want)
		}
	}
}

// A failing read must surface as an error, NOT as zeros. Zero-on-error is
// indistinguishable from "nothing pending", which is exactly the reading an
// operator must not be handed during a database problem.
func TestOutboxGauges_ReadFailureDoesNotReportZero(t *testing.T) {
	outbox := &statsOutbox{fakeOutbox: newFakeOutbox(), err: errors.New("db down")}
	reg, err := RegisterOutboxGauges(outbox)
	if err != nil {
		t.Fatalf("RegisterOutboxGauges() = %v", err)
	}
	// Unregister, or an erroring callback poisons every later Collect() in the
	// process and the package's tests become order-dependent.
	t.Cleanup(func() { _ = reg.Unregister() })

	var rm metricdata.ResourceMetrics
	if err := testReader.Collect(context.Background(), &rm); err == nil {
		t.Fatal("Collect() = nil error while the outbox read fails; the gauges would read as 'nothing pending'")
	}
}
