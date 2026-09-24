package saga

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"go.temporal.io/sdk/activity"
	sdklog "go.temporal.io/sdk/log"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"

	"github.com/duynhlab/pkg/logger/slogx"

	"github.com/duynhlab/order-service/internal/core/domain"
)

// lockedBuffer serializes writes from the activity/workflow goroutines.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) events(t *testing.T) []map[string]any {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(l.b.String()), "\n") {
		var m map[string]any
		if line != "" && json.Unmarshal([]byte(line), &m) == nil && m["event"] != nil {
			out = append(out, m)
		}
	}
	return out
}

// captureDefault installs a facade writing into a buffer as the process
// default (activities log through slogx.FromContext) and restores it after.
func captureDefault(t *testing.T) (*slogx.Logger, *lockedBuffer) {
	t.Helper()
	buf := &lockedBuffer{}
	l := slogx.New(slogx.Config{Level: "debug", Stdout: buf})
	prev := slogx.FromContext(t.Context())
	slogx.SetDefault(l)
	t.Cleanup(func() { slogx.SetDefault(prev) })
	return l, buf
}

// Activity-side events fire once per APPLIED transition: a replayed command
// (a Temporal retry after the write landed) writes nothing.
func TestActivityEvents_OncePerAppliedTransition(t *testing.T) {
	cases := []struct {
		name  string
		run   func(a *Activities) any
		args  []any
		event string
		check func(e map[string]any) bool
	}{
		{"confirm", func(a *Activities) any { return a.ConfirmOrder }, []any{"42"}, "order.confirmed",
			func(e map[string]any) bool { return e["order.id"] == "42" }},
		{"manual review", func(a *Activities) any { return a.MarkManualReview }, []any{"42", domain.ReasonCompensationIncomplete},
			"order.manual_review.entered", func(e map[string]any) bool { return e["reason"] == "COMPENSATION_INCOMPLETE" && e["level"] == "warn" }},
		{"cancelled", func(a *Activities) any { return a.CompleteCancellation }, []any{"42", int64(3)}, "order.cancelled",
			func(e map[string]any) bool { return e["order.epoch"] == float64(3) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, buf := captureDefault(t)
			if err := runActivity(t, tc.run(&Activities{Orders: &stubOrders{}}), tc.args...); err != nil {
				t.Fatalf("applied: %v", err)
			}
			if err := runActivity(t, tc.run(&Activities{Orders: &stubOrders{replayed: true}}), tc.args...); err != nil {
				t.Fatalf("replayed: %v", err)
			}
			ev := buf.events(t)
			if len(ev) != 1 {
				t.Fatalf("events = %d, want exactly one (the replayed command writes none)", len(ev))
			}
			if ev[0]["event"] != tc.event || !tc.check(ev[0]) {
				t.Errorf("event = %v", ev[0])
			}
		})
	}
}

// The workflow-side events go through the SDK logger; their attributes are the
// catalog's.
func TestWorkflowEvents(t *testing.T) {
	buf := &lockedBuffer{}
	facade := slogx.New(slogx.Config{Level: "debug", Stdout: buf})
	var ts testsuite.WorkflowTestSuite
	ts.SetLogger(sdklog.NewStructuredLogger(facade.Slog()))
	env := ts.NewTestWorkflowEnvironment()
	wf := func(ctx workflow.Context) error {
		compensationDone(ctx, "42", compVoidPayment, nil)
		compensationDone(ctx, "42", compRefundPayment, errors.New("provider down"))
		sagaFailed(ctx, "42", domain.ReasonPaymentDeclined, outcomeCompensated)
		retryExhausted(ctx, "42", "completion", errors.New("gave up"))
		return nil
	}
	env.RegisterWorkflowWithOptions(wf, workflow.RegisterOptions{Name: "events"})
	env.ExecuteWorkflow("events")
	ev := buf.events(t)
	if len(ev) != 4 {
		t.Fatalf("events = %d, want 4", len(ev))
	}
	if e := ev[0]; e["event"] != "order.compensation.completed" || e["compensation.step"] != "void_payment" ||
		e["outcome"] != "ok" || e["error.type"] != nil || e["level"] != "info" {
		t.Errorf("ok step = %v", e)
	}
	if e := ev[1]; e["outcome"] != "failed" || e["error.type"] == nil || e["level"] != "warn" {
		t.Errorf("failed step = %v", e)
	}
	if e := ev[2]; e["event"] != "order.failed" || e["reason"] != "PAYMENT_DECLINED" || e["outcome"] != "compensated" {
		t.Errorf("saga failed = %v", e)
	}
	if e := ev[3]; e["event"] != "order.retry.exhausted" || e["operation"] != "completion" || e["level"] != "error" {
		t.Errorf("retry exhausted = %v", e)
	}
}

// A failed step reaches workflow code as *temporal.ActivityError; its
// error.type must be the bounded application-error type, not the wrapper's Go
// type, or every failure reads the same.
func TestCompensationEvent_ErrorTypeFromActivityError(t *testing.T) {
	buf := &lockedBuffer{}
	facade := slogx.New(slogx.Config{Level: "debug", Stdout: buf})
	var ts testsuite.WorkflowTestSuite
	ts.SetLogger(sdklog.NewStructuredLogger(facade.Slog()))
	env := ts.NewTestWorkflowEnvironment()
	failing := func(context.Context) error {
		return temporal.NewNonRetryableApplicationError("refused", reasonOrderTransitionRefused, nil)
	}
	env.RegisterActivityWithOptions(failing, activity.RegisterOptions{Name: "failing"})
	wf := func(ctx workflow.Context) error {
		ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{StartToCloseTimeout: time.Second})
		err := workflow.ExecuteActivity(ctx, "failing").Get(ctx, nil)
		compensationDone(ctx, "42", compVoidPayment, err)
		return nil
	}
	env.RegisterWorkflowWithOptions(wf, workflow.RegisterOptions{Name: "wf"})
	env.ExecuteWorkflow("wf")
	ev := buf.events(t)
	if len(ev) != 1 || ev[0]["error.type"] != reasonOrderTransitionRefused {
		t.Errorf("events = %v, want error.type %q", ev, reasonOrderTransitionRefused)
	}
}
