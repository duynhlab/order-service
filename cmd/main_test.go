package main

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"go.temporal.io/api/workflowservice/v1"

	"github.com/duynhlab/order-service/config"
	"github.com/duynhlab/order-service/internal/core/domain"
	"github.com/duynhlab/pkg/logger/slogx"
	inventoryv1 "github.com/duynhlab/pkg/proto/inventory/v1"
)

// TestDialTemporalRetry_ExhaustsBudgetAndReturnsError pins the retry contract:
// an unreachable Temporal burns the whole attempt budget (observable as at
// least one backoff sleep) and surfaces the last dial error instead of
// panicking or hanging.
func TestDialTemporalRetry_ExhaustsBudgetAndReturnsError(t *testing.T) {
	cfg := &config.Config{}
	cfg.Temporal.HostPort = "127.0.0.1:1" // nothing listens on port 1
	cfg.Temporal.Namespace = "mop"

	backoff := 10 * time.Millisecond
	start := time.Now()
	tc, err := dialTemporalRetry(cfg, slogx.New(slogx.Config{Stdout: io.Discard}), 2, backoff)
	if err == nil {
		tc.Close()
		t.Fatal("expected an error dialing an unreachable Temporal, got nil")
	}
	if elapsed := time.Since(start); elapsed < backoff {
		t.Errorf("elapsed %v < backoff %v — retry loop did not back off between attempts", elapsed, backoff)
	}
}

// --- inventory reconciler wiring (RFC-0021 P3) ---

// stubReconcileStore is an empty backlog: the wiring tests care about whether the
// loop runs and whether shutdown returns, not about repairs.
type stubReconcileStore struct{}

func (stubReconcileStore) ListForReconcile(context.Context, time.Duration, time.Duration, int) ([]domain.ReconcileCandidate, error) {
	return nil, nil
}

func (stubReconcileStore) CountUnreconciled(context.Context, time.Duration) (int, error) {
	return 0, nil
}

func (stubReconcileStore) MarkReconciled(context.Context, string) error              { return nil }
func (stubReconcileStore) MarkReconcileBreach(context.Context, string, string) error { return nil }
func (stubReconcileStore) CountOrdersInStatus(context.Context, string, time.Duration) (int, error) {
	return 0, nil
}

// stubInventory satisfies the client without implementing anything: a disabled or
// idle reconciler never calls it.
type stubInventory struct {
	inventoryv1.InventoryServiceClient
}

// stubDescriber is the Temporal half, equally unused here.
type stubDescriber struct{}

func (stubDescriber) DescribeWorkflowExecution(context.Context, string, string) (*workflowservice.DescribeWorkflowExecutionResponse, error) {
	return nil, errors.New("not used")
}

// ORDER_RECONCILER_ENABLED must actually decide whether the loop runs, not merely
// log about it. A flag that silently does nothing is worse than no flag: the
// operator believes the reconciler is off (or on) and it is not.
//
// Asserted through the loop's own startup line rather than through a scan: the
// first scan is a full DefaultInterval away, so "no scan in 20ms" is true whether
// the flag works or not — a test that would pass against a flag wired to nothing.
func TestStartInventoryReconciler_KillSwitchDecidesWhetherTheLoopRuns(t *testing.T) {
	const runningLine = "inventory reconciler running"

	for _, tc := range []struct {
		name    string
		enabled bool
	}{
		{"disabled", false},
		{"enabled", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obsLog, logs := newObserver("info")
			cfg := &config.Config{ReconcilerEnabled: tc.enabled}

			stop := startInventoryReconciler(cfg, obsLog, stubReconcileStore{},
				stubInventory{}, stubDescriber{})
			t.Cleanup(stop)

			// The loop logs from its goroutine, so give it a moment to start.
			deadline := time.Now().Add(time.Second)
			for logs.FilterMessageSnippet(runningLine).Len() == 0 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}

			running := logs.FilterMessageSnippet(runningLine).Len() > 0
			if running != tc.enabled {
				t.Errorf("reconciler loop running = %v, want %v", running, tc.enabled)
			}
			if warned := logs.FilterMessageSnippet("DISABLED").Len() > 0; warned == tc.enabled {
				t.Errorf("DISABLED warning logged = %v, want %v — an operator reads startup logs to know", warned, !tc.enabled)
			}
		})
	}
}

// The stop function must RETURN — a shutdown that blocks on a background loop
// holds up the pod until Kubernetes SIGKILLs it, and the deferred cleanups
// (database pool, health server) never run.
func TestStartInventoryReconciler_StopReturnsPromptly(t *testing.T) {
	cfg := &config.Config{ReconcilerEnabled: true}

	stop := startInventoryReconciler(cfg, slogx.New(slogx.Config{Stdout: io.Discard}), stubReconcileStore{}, stubInventory{}, stubDescriber{})

	done := make(chan struct{})
	go func() { stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(reconcilerDrainGrace + reconcilerStopTimeout + time.Second):
		t.Fatal("stop() did not return within its own budgets")
	}
}

// TestShutdownTelemetry_FreshBoundedContext pins the #1359 contract: the
// telemetry flush gets a live context with its own deadline, independent of
// however much of the HTTP drain budget was spent before it.
func TestShutdownTelemetry_FreshBoundedContext(t *testing.T) {
	const timeout = 5 * time.Second
	before := time.Now()
	called := false
	err := shutdownTelemetry(func(ctx context.Context) error {
		called = true
		if ctx.Err() != nil {
			t.Fatalf("shutdown context already done on entry: %v", ctx.Err())
		}
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Fatal("shutdown context has no deadline")
		}
		if remaining := time.Until(deadline); remaining <= 0 || remaining > timeout {
			t.Fatalf("deadline %v from now, want within (0, %v]", remaining, timeout)
		}
		if deadline.Before(before) {
			t.Fatalf("deadline %v is before the call started %v", deadline, before)
		}
		return nil
	}, timeout)
	if err != nil {
		t.Fatalf("shutdownTelemetry returned %v, want nil", err)
	}
	if !called {
		t.Fatal("shutdown func was not called")
	}
}

// TestShutdownTelemetry_ReturnsExporterError pins that an exporter failure is
// surfaced to the caller rather than swallowed.
func TestShutdownTelemetry_ReturnsExporterError(t *testing.T) {
	want := errors.New("exporter unavailable")
	err := shutdownTelemetry(func(context.Context) error { return want }, time.Second)
	if !errors.Is(err, want) {
		t.Fatalf("shutdownTelemetry returned %v, want %v", err, want)
	}
}
