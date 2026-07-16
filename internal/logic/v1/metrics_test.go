package v1

import (
	"context"
	"os"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/duynhlab/order-service/internal/core/domain"
)

// testReader is the process-wide ManualReader wired as the global MeterProvider.
// The OTel global provider is first-wins, so it is installed exactly once for
// the whole logic-package test binary (TestMain). The metrics.go instrument is
// created at package init via the global meter and is upgraded to forward here
// once the provider is set. The reader is cumulative and shared with every
// CreateOrder test, so assertions read a before/after DELTA of the histogram's
// sample count rather than an absolute value.
var testReader *sdkmetric.ManualReader

func TestMain(m *testing.M) {
	testReader = sdkmetric.NewManualReader()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(testReader)))
	os.Exit(m.Run())
}

// histogramCount returns the cumulative sample count of the named histogram for
// the exact attribute set, or 0 if no such series exists yet.
func histogramCount(t *testing.T, name string, want map[string]string) uint64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := testReader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}
	for _, sm := range rm.ScopeMetrics {
		if sm.Scope.Name != "order-service" {
			continue
		}
		for _, md := range sm.Metrics {
			if md.Name == name {
				return histogramSampleCount(t, md, want)
			}
		}
	}
	return 0
}

// histogramSampleCount returns the sample count of the data point in md matching
// want, or 0.
func histogramSampleCount(t *testing.T, md metricdata.Metrics, want map[string]string) uint64 {
	t.Helper()
	hist, ok := md.Data.(metricdata.Histogram[int64])
	if !ok {
		t.Fatalf("metric %s is not Histogram[int64]", md.Name)
	}
	for _, dp := range hist.DataPoints {
		if attrsMatch(dp.Attributes, want) {
			return dp.Count
		}
	}
	return 0
}

// attrsMatch reports whether the data point's attribute set is exactly want —
// same keys, same values, no extras (so a label-set change is caught).
func attrsMatch(set attribute.Set, want map[string]string) bool {
	if set.Len() != len(want) {
		return false
	}
	for k, v := range want {
		got, ok := set.Value(attribute.Key(k))
		if !ok || got.AsString() != v {
			return false
		}
	}
	return true
}

// assertHistogramDelta runs fn and asserts the named histogram/label series
// gained exactly want samples — the exactly-once property per genuine creation.
func assertHistogramDelta(t *testing.T, name string, labels map[string]string, want uint64, fn func()) {
	t.Helper()
	before := histogramCount(t, name, labels)
	fn()
	if got := histogramCount(t, name, labels) - before; got != want {
		t.Fatalf("%s %v: sample delta = %d, want %d", name, labels, got, want)
	}
}

const metricOrderValue = "order.value.minor"

// TestCreateOrder_RecordsOrderValue asserts both totals_source label values are
// emitted (demo when the caller supplied no totals, checkout_quoted when it did)
// and that a single creation records exactly one sample.
func TestCreateOrder_RecordsOrderValue(t *testing.T) {
	ctx := context.Background()
	item := domain.OrderItem{ProductID: "p1", Quantity: 1, Price: 1000}

	assertHistogramDelta(t, metricOrderValue, map[string]string{"totals_source": totalsSourceDemo}, 1, func() {
		s := NewOrderService(&MockOrderRepository{}, &MockTransactionManager{})
		if _, err := s.CreateOrder(ctx, domain.CreateOrderRequest{
			UserID: "u1", IdempotencyKey: "k-demo", Items: []domain.OrderItem{item},
		}); err != nil {
			t.Fatalf("demo create: %v", err)
		}
	})

	assertHistogramDelta(t, metricOrderValue, map[string]string{"totals_source": totalsSourceCheckoutQuoted}, 1, func() {
		s := NewOrderService(&MockOrderRepository{}, &MockTransactionManager{})
		if _, err := s.CreateOrder(ctx, domain.CreateOrderRequest{
			UserID: "u1", IdempotencyKey: "k-quoted", TotalsProvided: true,
			ShippingFeeMinor: 200, TaxMinor: 50, DiscountMinor: 10,
			Items: []domain.OrderItem{item},
		}); err != nil {
			t.Fatalf("quoted create: %v", err)
		}
	})
}

// TestCreateOrder_ReplayDoesNotRecordValue proves the idempotent-replay path
// (unique-key conflict → re-fetch of an already-created order) records no new
// sample on either label, keeping the metric exactly-once per genuine creation.
func TestCreateOrder_ReplayDoesNotRecordValue(t *testing.T) {
	ctx := context.Background()
	existing := &domain.Order{ID: "existing-1", UserID: "u1", IdempotencyKey: "k-1", Status: "pending"}
	repo := &MockOrderRepository{
		createWithTxFunc: func(_ context.Context, _ domain.Transaction, _ *domain.Order) error {
			return domain.ErrConflict
		},
		findByIdempotencyKeyFunc: func(_ context.Context, _, _ string) (*domain.Order, error) {
			return existing, nil
		},
	}

	for _, source := range []string{totalsSourceDemo, totalsSourceCheckoutQuoted} {
		assertHistogramDelta(t, metricOrderValue, map[string]string{"totals_source": source}, 0, func() {
			s := NewOrderService(repo, &MockTransactionManager{})
			if _, err := s.CreateOrder(ctx, domain.CreateOrderRequest{
				UserID: "u1", IdempotencyKey: "k-1",
				Items: []domain.OrderItem{{ProductID: "p1", Quantity: 1, Price: 1000}},
			}); err != nil {
				t.Fatalf("replay create: %v", err)
			}
		})
	}
}
