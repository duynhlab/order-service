// Package sweeploop is the shared ticker loop of the outbox dispatchers
// (fulfillment start, cancellation start). One home for the loop so the two
// dispatchers cannot drift on the parts that must behave identically:
// sweep-immediately-on-start (the most likely reason a row waits is the
// crash that restarted the process) and a context-bounded ticker.
package sweeploop

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/duynhlab/pkg/logger/slogx"
)

// Run sweeps immediately, then on every tick, until ctx ends. name labels
// the log lines; sweep errors are logged and the loop continues.
func Run(ctx context.Context, log *slogx.Logger, name string, interval time.Duration,
	batchSize int, sweep func(context.Context) error) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	log.Info(ctx, name+" running",
		slog.Duration("poll_interval", interval), slog.Int("batch_size", batchSize))

	if err := sweep(ctx); err != nil && !errors.Is(err, context.Canceled) {
		log.Error(ctx, "initial "+name+" sweep failed", slogx.Err(err))
	}
	for {
		select {
		case <-ctx.Done():
			log.Info(ctx, name+" stopped")
			return
		case <-ticker.C:
			if err := sweep(ctx); err != nil && !errors.Is(err, context.Canceled) {
				log.Error(ctx, name+" sweep failed", slogx.Err(err))
			}
		}
	}
}
