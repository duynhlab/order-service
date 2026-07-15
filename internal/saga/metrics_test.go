package saga

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/mock"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.temporal.io/sdk/testsuite"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	paymentv1 "github.com/duynhlab/pkg/proto/payment/v1"
)

// testReader is the process-wide ManualReader wired as the global MeterProvider.
// The OTel global provider is first-wins, so it is installed exactly once for
// the whole saga test binary (TestMain). The metrics.go instruments are created
// at package init via the global meter and are upgraded to forward here once the
// provider is set. The reader is cumulative and shared with every other saga
// test that runs the workflow, so assertions read a before/after DELTA rather
// than an absolute value.
var testReader *sdkmetric.ManualReader

func TestMain(m *testing.M) {
	testReader = sdkmetric.NewManualReader()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(testReader)))
	os.Exit(m.Run())
}

// counterValue returns the cumulative value of the named counter for the exact
// attribute set, or 0 if no such series exists yet.
func counterValue(t *testing.T, name string, want map[string]string) int64 {
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
				return sumValue(t, md, want)
			}
		}
	}
	return 0
}

// sumValue returns the value of the data point in md matching want, or 0.
func sumValue(t *testing.T, md metricdata.Metrics, want map[string]string) int64 {
	t.Helper()
	sum, ok := md.Data.(metricdata.Sum[int64])
	if !ok {
		t.Fatalf("metric %s is not Sum[int64]", md.Name)
	}
	for _, dp := range sum.DataPoints {
		if attrsMatch(dp.Attributes, want) {
			return dp.Value
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

const (
	metricOutcome      = "order.saga.outcome.total"
	metricCompensation = "order.saga.compensation.total"
	metricPaymentActy  = "order.payment.activity.total"
)

// assertDelta runs fn and asserts the named counter/label series moved by
// exactly want — the exactly-once property under the real (test) workflow
// engine or a single activity call.
func assertDelta(t *testing.T, name string, labels map[string]string, want int64, fn func()) {
	t.Helper()
	before := counterValue(t, name, labels)
	fn()
	got := counterValue(t, name, labels) - before
	if got != want {
		t.Fatalf("%s %v: delta = %d, want %d", name, labels, got, want)
	}
}

// TestMetrics_HappyPath_ConfirmedExactlyOnce proves the confirmed terminal
// outcome is recorded exactly once and no compensation fires on the happy path.
func TestMetrics_HappyPath_ConfirmedExactlyOnce(t *testing.T) {
	failOrderBefore := counterValue(t, metricCompensation, map[string]string{"step": compFailOrder, "result": resultOK})
	run := func() {
		var ts testsuite.WorkflowTestSuite
		env := ts.NewTestWorkflowEnvironment()
		var a *Activities
		env.OnActivity(a.AuthorizePayment, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)
		env.OnActivity(a.ReserveStock, mock.Anything, mock.Anything, mock.Anything).Return(nil)
		env.OnActivity(a.CreateShipment, mock.Anything, mock.Anything).Return(nil)
		env.OnActivity(a.CapturePayment, mock.Anything, mock.Anything).Return(nil)
		env.OnActivity(a.ConfirmOrder, mock.Anything, mock.Anything).Return(nil)
		env.OnActivity(a.SendNotification, mock.Anything, mock.Anything).Return(nil)
		env.OnActivity(a.SendReceipt, mock.Anything, mock.Anything).Return(nil)
		env.OnActivity(a.ClearCart, mock.Anything, mock.Anything).Return(nil)
		env.ExecuteWorkflow(OrderFulfillmentWorkflow, testInput())
		if err := env.GetWorkflowError(); err != nil {
			t.Fatalf("workflow error = %v", err)
		}
	}
	// One workflow run → the single terminal branch fires once (not per replay).
	assertDelta(t, metricOutcome, map[string]string{"outcome": outcomeConfirmed}, 1, run)
	// No compensation on the happy path.
	if d := counterValue(t, metricCompensation, map[string]string{"step": compFailOrder, "result": resultOK}) - failOrderBefore; d != 0 {
		t.Errorf("fail_order compensation must not fire on the happy path, delta = %d", d)
	}
}

// TestMetrics_CaptureFails_FailedWithVoidCompensation covers a pre-capture
// failure: outcome=failed and the reverse compensation chain, each step ok and
// counted once. The money was never captured, so no refund is counted.
func TestMetrics_CaptureFails_FailedWithVoidCompensation(t *testing.T) {
	run := func() {
		var ts testsuite.WorkflowTestSuite
		env := ts.NewTestWorkflowEnvironment()
		var a *Activities
		env.OnActivity(a.AuthorizePayment, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)
		env.OnActivity(a.ReserveStock, mock.Anything, mock.Anything, mock.Anything).Return(nil)
		env.OnActivity(a.CreateShipment, mock.Anything, mock.Anything).Return(nil)
		env.OnActivity(a.CapturePayment, mock.Anything, mock.Anything).Return(nonRetryable("capture failed"))
		env.OnActivity(a.CancelShipment, mock.Anything, mock.Anything).Return(nil)
		env.OnActivity(a.ReleaseStock, mock.Anything, mock.Anything, mock.Anything).Return(nil)
		env.OnActivity(a.VoidPayment, mock.Anything, mock.Anything).Return(nil)
		env.OnActivity(a.FailOrder, mock.Anything, mock.Anything).Return(nil)
		env.ExecuteWorkflow(OrderFulfillmentWorkflow, testInput())
		if env.GetWorkflowError() == nil {
			t.Fatal("expected workflow error")
		}
	}
	before := map[string]int64{
		"outcome_failed":  counterValue(t, metricOutcome, map[string]string{"outcome": outcomeFailed}),
		"void":            counterValue(t, metricCompensation, map[string]string{"step": compVoidPayment, "result": resultOK}),
		"release":         counterValue(t, metricCompensation, map[string]string{"step": compReleaseStock, "result": resultOK}),
		"cancel":          counterValue(t, metricCompensation, map[string]string{"step": compCancelShipment, "result": resultOK}),
		"fail_order":      counterValue(t, metricCompensation, map[string]string{"step": compFailOrder, "result": resultOK}),
		"refund":          counterValue(t, metricCompensation, map[string]string{"step": compRefundPayment, "result": resultOK}),
		"outcome_confirm": counterValue(t, metricOutcome, map[string]string{"outcome": outcomeConfirmed}),
	}
	run()
	checks := []struct {
		key, name string
		labels    map[string]string
		want      int64
	}{
		{"outcome_failed", metricOutcome, map[string]string{"outcome": outcomeFailed}, 1},
		{"void", metricCompensation, map[string]string{"step": compVoidPayment, "result": resultOK}, 1},
		{"release", metricCompensation, map[string]string{"step": compReleaseStock, "result": resultOK}, 1},
		{"cancel", metricCompensation, map[string]string{"step": compCancelShipment, "result": resultOK}, 1},
		{"fail_order", metricCompensation, map[string]string{"step": compFailOrder, "result": resultOK}, 1},
		{"refund", metricCompensation, map[string]string{"step": compRefundPayment, "result": resultOK}, 0},
		{"outcome_confirm", metricOutcome, map[string]string{"outcome": outcomeConfirmed}, 0},
	}
	for _, c := range checks {
		if d := counterValue(t, c.name, c.labels) - before[c.key]; d != c.want {
			t.Errorf("%s %v: delta = %d, want %d", c.name, c.labels, d, c.want)
		}
	}
}

// TestMetrics_ConfirmFails_CompensatedWithRefund covers a post-capture failure:
// outcome=compensated and a refund_payment compensation (not a void).
func TestMetrics_ConfirmFails_CompensatedWithRefund(t *testing.T) {
	run := func() {
		var ts testsuite.WorkflowTestSuite
		env := ts.NewTestWorkflowEnvironment()
		var a *Activities
		env.OnActivity(a.AuthorizePayment, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)
		env.OnActivity(a.ReserveStock, mock.Anything, mock.Anything, mock.Anything).Return(nil)
		env.OnActivity(a.CreateShipment, mock.Anything, mock.Anything).Return(nil)
		env.OnActivity(a.CapturePayment, mock.Anything, mock.Anything).Return(nil)
		env.OnActivity(a.ConfirmOrder, mock.Anything, mock.Anything).Return(nonRetryable("confirm failed"))
		env.OnActivity(a.RefundPayment, mock.Anything, mock.Anything, mock.Anything).Return(nil)
		env.OnActivity(a.SendRefundNotification, mock.Anything, mock.Anything).Return(nil)
		env.OnActivity(a.CancelShipment, mock.Anything, mock.Anything).Return(nil)
		env.OnActivity(a.ReleaseStock, mock.Anything, mock.Anything, mock.Anything).Return(nil)
		env.OnActivity(a.FailOrder, mock.Anything, mock.Anything).Return(nil)
		env.ExecuteWorkflow(OrderFulfillmentWorkflow, testInput())
		if env.GetWorkflowError() == nil {
			t.Fatal("expected workflow error")
		}
	}
	voidBefore := counterValue(t, metricCompensation, map[string]string{"step": compVoidPayment, "result": resultOK})
	refundBefore := counterValue(t, metricCompensation, map[string]string{"step": compRefundPayment, "result": resultOK})
	assertDelta(t, metricOutcome, map[string]string{"outcome": outcomeCompensated}, 1, run)
	// A post-capture failure refunds exactly once and never voids.
	if d := counterValue(t, metricCompensation, map[string]string{"step": compRefundPayment, "result": resultOK}) - refundBefore; d != 1 {
		t.Errorf("refund_payment delta = %d, want 1", d)
	}
	if d := counterValue(t, metricCompensation, map[string]string{"step": compVoidPayment, "result": resultOK}) - voidBefore; d != 0 {
		t.Errorf("void_payment must not fire on a post-capture failure, delta = %d", d)
	}
}

// TestMetrics_PaymentActivity_Labels asserts each op/result label combination
// the payment activities can emit, and that a single call counts exactly once.
func TestMetrics_PaymentActivity_Labels(t *testing.T) {
	ctx := context.Background()

	assertDelta(t, metricPaymentActy, map[string]string{"op": payOpAuthorize, "result": resultOK}, 1, func() {
		a := &Activities{Payment: &stubPaymentClient{authResp: authorizedResp()}}
		if err := a.AuthorizePayment(ctx, "42", "7", 2550, ""); err != nil {
			t.Fatalf("authorize ok: %v", err)
		}
	})
	assertDelta(t, metricPaymentActy, map[string]string{"op": payOpAuthorize, "result": resultDeclined}, 1, func() {
		a := &Activities{Payment: &stubPaymentClient{authResp: &paymentv1.AuthorizeResponse{
			Payment: &paymentv1.Payment{Status: "failed", DeclineCode: "insufficient_funds"}}}}
		_ = a.AuthorizePayment(ctx, "42", "7", 2550, "")
	})
	assertDelta(t, metricPaymentActy, map[string]string{"op": payOpAuthorize, "result": resultRejected}, 1, func() {
		a := &Activities{Payment: &stubPaymentClient{authErr: status.Error(codes.InvalidArgument, "x")}}
		_ = a.AuthorizePayment(ctx, "42", "7", 2550, "")
	})
	assertDelta(t, metricPaymentActy, map[string]string{"op": payOpAuthorize, "result": resultError}, 1, func() {
		a := &Activities{Payment: &stubPaymentClient{authErr: status.Error(codes.Unavailable, "x")}}
		_ = a.AuthorizePayment(ctx, "42", "7", 2550, "")
	})
	assertDelta(t, metricPaymentActy, map[string]string{"op": payOpCapture, "result": resultOK}, 1, func() {
		a := &Activities{Payment: &stubPaymentClient{}}
		if err := a.CapturePayment(ctx, "42"); err != nil {
			t.Fatalf("capture ok: %v", err)
		}
	})
	assertDelta(t, metricPaymentActy, map[string]string{"op": payOpVoid, "result": resultOK}, 1, func() {
		a := &Activities{Payment: &stubPaymentClient{}}
		if err := a.VoidPayment(ctx, "42"); err != nil {
			t.Fatalf("void ok: %v", err)
		}
	})
	assertDelta(t, metricPaymentActy, map[string]string{"op": payOpRefund, "result": resultRejected}, 1, func() {
		a := &Activities{Payment: &stubPaymentClient{refundErr: status.Error(codes.NotFound, "x")}}
		_ = a.RefundPayment(ctx, "42", 2550)
	})
}
