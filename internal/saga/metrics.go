package saga

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.temporal.io/sdk/workflow"
)

// Business metrics for the order-fulfillment saga, answering the on-call
// questions that matter for order money-movement:
//  1. What fraction of sagas end confirmed vs failed vs compensated? → outcome
//  2. Which compensation steps run, and do they succeed?             → compensation
//  3. Are the order's payment calls being declined/rejected?         → payment.activity
//
// Instruments ride the global OTel MeterProvider that obsx.SetupObservability
// installs (RFC-0014 OTLP pipeline → collector → VictoriaMetrics). Before that
// setup the global provider is a no-op, so package-init here is safe. Names are
// OTel-style; the collector renders them as order_saga_outcome_total,
// order_saga_compensation_total, order_payment_activity_total.
//
// Labels are bounded to enumerable domain values (RFC-0017 D-9): no order/user
// ids, no payment tokens, no decline text, no amounts.
//
// Semantics & delivery (these are best-effort rate/trend KPIs, not a ledger —
// the CNPG ledger and the saga's Temporal history are authoritative for money):
//   - Workflow-emitted counters (outcome, compensation) are guarded by
//     !workflow.IsReplaying, so ordinary history replay after a worker restart
//     never re-counts. A worker crash in the narrow window between the emit and
//     the next recorded command, or an activity retry after a lost completion,
//     can rarely double-count — acceptable for a rate signal.
//   - outcome=failed|compensated records which terminal PATH the saga took
//     (pre- vs post-capture failure), NOT that every compensation succeeded. A
//     failed void/refund still counts as failed/compensated here; the
//     stuck-money signal is compensation{result=failed}, which must be alerted
//     on separately.
var (
	meter = otel.Meter("order-service")

	sagaOutcomeCounter, _ = meter.Int64Counter("order.saga.outcome.total",
		metric.WithDescription("Order-fulfillment saga terminal outcomes"))
	sagaCompensationCounter, _ = meter.Int64Counter("order.saga.compensation.total",
		metric.WithDescription("Saga compensation steps by step and result"))
	paymentActivityCounter, _ = meter.Int64Counter("order.payment.activity.total",
		metric.WithDescription("Order-side payment activity calls by operation and result"))
)

// Saga terminal outcomes (bounded).
const (
	outcomeConfirmed   = "confirmed"   // ConfirmOrder pivot succeeded
	outcomeFailed      = "failed"      // failed before capture (money voided, never captured)
	outcomeCompensated = "compensated" // failed after capture (captured money refunded)
)

// Compensation step names (bounded).
const (
	compVoidPayment    = "void_payment"
	compRefundPayment  = "refund_payment"
	compReleaseStock   = "release_stock"
	compCancelShipment = "cancel_shipment"
	compFailOrder      = "fail_order"
)

// Payment activity operations (bounded).
const (
	payOpAuthorize = "authorize"
	payOpCapture   = "capture"
	payOpVoid      = "void"
	payOpRefund    = "refund"
)

// Shared result labels (bounded). compensation uses ok|failed; payment.activity
// uses ok|declined|rejected|error.
const (
	resultOK       = "ok"
	resultFailed   = "failed"
	resultDeclined = "declined"
	resultRejected = "rejected"
	resultError    = "error"
)

// recordSagaOutcome counts one saga terminal outcome. Called from the workflow
// at the single terminal branch reached per execution, guarded by
// !workflow.IsReplaying so a history replay after a worker restart never
// re-counts. Best-effort: a crash between the emit and the workflow task
// completing loses the count (not double-counts) — the accepted tradeoff for
// workflow-side observability.
func recordSagaOutcome(ctx workflow.Context, outcome string) {
	if workflow.IsReplaying(ctx) {
		return
	}
	sagaOutcomeCounter.Add(context.Background(), 1, metric.WithAttributes(
		attribute.String("outcome", outcome)))
}

// recordCompensation counts one compensation step and its result. Same
// replay-guard rationale as recordSagaOutcome — one increment per real
// compensation run, not per replay.
func recordCompensation(ctx workflow.Context, step, result string) {
	if workflow.IsReplaying(ctx) {
		return
	}
	sagaCompensationCounter.Add(context.Background(), 1, metric.WithAttributes(
		attribute.String("step", step),
		attribute.String("result", result)))
}

// compResult maps a compensation activity error to its bounded result label.
func compResult(err error) string {
	if err != nil {
		return resultFailed
	}
	return resultOK
}

// recordPaymentActivity counts one order-side payment activity outcome. Called
// from the payment activities, which run once per attempt outside workflow
// replay. Terminal outcomes (ok/declined/rejected) fire once because the
// activity is not retried after them; a transient "error" is re-driven by
// Temporal's retry policy and so is counted per attempt — a health signal, not
// a per-order count.
func recordPaymentActivity(ctx context.Context, op, result string) {
	paymentActivityCounter.Add(ctx, 1, metric.WithAttributes(
		attribute.String("op", op),
		attribute.String("result", result)))
}
