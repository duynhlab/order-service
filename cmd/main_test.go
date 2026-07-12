package main

import (
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/duynhlab/order-service/config"
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
