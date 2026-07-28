package saga

import (
	"context"
	"time"

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
//  4. Is stock reservation failing, and is it out-of-stock or infra?  → stock_reservation
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
	stockReservationCounter, _ = meter.Int64Counter("order.stock_reservation.total",
		metric.WithDescription("Order-side ReserveStock activity outcomes by result"))
	inventoryCommitLag, _ = meter.Float64Histogram("order.inventory.commit_lag",
		metric.WithDescription("Seconds between the ConfirmOrder pivot and CommitInventory settling"),
		metric.WithUnit("s"))
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

// Stock reservation results (bounded). This is the SAGA's (order-side) view of
// the ReserveStock activity outcome, distinct from product-service's own
// server-side product_stock_reservations_total counter.
const (
	resultReserved     = "reserved"     // stock reserved
	resultInsufficient = "insufficient" // out of stock (non-retryable business rejection)
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

// recordCommitLag records how long the post-pivot CommitInventory took to
// settle, measured from the ConfirmOrder pivot. This is the RFC-0021 P3 write-
// path SLI for orders that DID settle.
//
// It deliberately does not claim to show a backlog building: the sample is
// emitted only once the commit settles, so a commit still retrying contributes
// nothing, and during an inventory outage this series goes quiet rather than
// climbing. What bounds the silence is commitActivityOptions'
// ScheduleToCloseTimeout — every commit settles within it, as ok or failed — and
// what detects the accumulation live is the reconciler's
// confirmed-but-RESERVED backlog gauge (RFC-0021 P3, separate change). Alerting
// on this histogram alone would miss exactly the incident it looks like it
// covers.
//
// Time comes from workflow.Now, not the wall clock, so it is deterministic
// across replay; the emit itself is guarded by !workflow.IsReplaying like the
// other workflow-side instruments. result is bounded to ok|failed — a failed
// commit past the pivot is the invariant breach the write-migration alerts fire
// on, and keeping it on the same instrument means one query answers both "how
// slow" and "how often broken".
func recordCommitLag(ctx workflow.Context, lag time.Duration, err error) {
	if workflow.IsReplaying(ctx) {
		return
	}
	inventoryCommitLag.Record(context.Background(), lag.Seconds(), metric.WithAttributes(
		attribute.String("result", compResult(err))))
}

// recordStockReservation counts one order-side stock-reserve activity outcome
// (ReserveStock on the product path, ReserveInventory on the inventory path —
// same series across the RFC-0021 migration, split by a 2-value participant
// label. Without that label the rollout is unobservable: you cannot tell what
// fraction of new sagas took the inventory path, whether its error rate differs
// from product's, or whether the flag flip took effect at all. Two values is not
// a cardinality problem.) Emitted from the activity, which
// runs once per attempt outside workflow replay, so no IsReplaying guard is
// needed. reserved / insufficient / rejected are terminal and fire once
// (rejected = a non-retryable business rejection other than insufficient stock,
// e.g. IDEMPOTENCY_CONFLICT — a real defect signal); a transient "error" is
// re-driven by Temporal's retry policy and counted per attempt — a health
// signal, mirroring recordPaymentActivity.
func recordStockReservation(ctx context.Context, participant Participant, result string) {
	stockReservationCounter.Add(ctx, 1, metric.WithAttributes(
		attribute.String("participant", string(participant)),
		attribute.String("result", result)))
}
