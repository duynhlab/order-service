package v1

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Business metric for order value at creation (RFC-0017 W2): the distribution of
// order totals, split by whether the total was demo-computed or checkout-quoted.
//
// The instrument rides the global OTel MeterProvider that
// obsx.SetupObservability installs (RFC-0014 OTLP pipeline → collector →
// VictoriaMetrics). Before that setup the global provider is a no-op, so
// package-init here is safe. The collector renders the name as
// order_value_minor.
//
// Labels are bounded (RFC-0017 D-9): totals_source has two enumerable values and
// carries no order/user ids; the amount rides in the histogram VALUE, never a
// label. Recorded exactly once per genuine order creation — never on an
// idempotent replay, which would double-count an already-recorded order.
var (
	meter = otel.Meter("order-service")

	orderValueHist, _ = meter.Int64Histogram("order.value.minor",
		metric.WithUnit("1"),
		metric.WithDescription("Order total at creation, in minor USD units"),
		// Money-scale buckets (cents): $5, $10, $25, $50, $100, $250, $500,
		// $1k, $2.5k, $5k, $10k. obsx installs no View for this instrument, so
		// without this hint the SDK's default (0,5,…,10000, ms-shaped) would put
		// almost every real order total in the top bucket — useless for AOV.
		metric.WithExplicitBucketBoundaries(500, 1000, 2500, 5000, 10000, 25000, 50000, 100000, 250000, 500000, 1000000))
)

// Order totals source (bounded): a demo-computed total (legacy REST path with
// the fixed demo shipping fee) vs a checkout-quoted total the shopper confirmed
// (RFC-0015 P4 machine caller supplies fee, tax, and discount).
const (
	totalsSourceDemo           = "demo"
	totalsSourceCheckoutQuoted = "checkout_quoted"
)

// recordOrderValue records one order's total on order.value.minor, tagged with
// whether the total was demo-computed or checkout-quoted.
func recordOrderValue(ctx context.Context, totalMinor int64, totalsProvided bool) {
	source := totalsSourceDemo
	if totalsProvided {
		source = totalsSourceCheckoutQuoted
	}
	orderValueHist.Record(ctx, totalMinor, metric.WithAttributes(
		attribute.String("totals_source", source)))
}
