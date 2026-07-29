package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.temporal.io/api/workflowservice/v1"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/duynhlab/order-service/config"
	"github.com/duynhlab/order-service/internal/core/domain"
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
	tc, err := dialTemporalRetry(cfg, zap.NewNop(), 2, backoff)
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
			core, logs := observer.New(zap.InfoLevel)
			cfg := &config.Config{ReconcilerEnabled: tc.enabled}

			stop := startInventoryReconciler(cfg, zap.New(core), stubReconcileStore{},
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

	stop := startInventoryReconciler(cfg, zap.NewNop(), stubReconcileStore{}, stubInventory{}, stubDescriber{})

	done := make(chan struct{})
	go func() { stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(reconcilerDrainGrace + reconcilerStopTimeout + time.Second):
		t.Fatal("stop() did not return within its own budgets")
	}
}
