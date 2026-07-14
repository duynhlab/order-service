package fulfillment

import (
	"context"
	"errors"
	"sync"
	"time"

	"go.temporal.io/sdk/client"
	"go.uber.org/zap"
)

// ErrTemporalUnavailable reports that no Temporal connection exists yet. It
// maps to the same caller behavior as the old nil-client guards: the gRPC
// adapter answers Unavailable (after the idempotent insert, exactly like the
// old guard — replays converge), the web handler leaves the order pending
// and logs.
var ErrTemporalUnavailable = errors.New("temporal client not connected")

// Lazy is a Starter that keeps dialing Temporal in the background until it
// succeeds. The old startup flow gave up after a finite retry budget and ran
// the process with a nil client forever — an order pod that raced Temporal
// at bring-up answered every CreateOrder with Unavailable until a human
// restarted it. Lazy makes that startup race self-healing (an established
// connection that later breaks is still the SDK's reconnect job, as before)
// while preserving the not-ready contract via TemporalReady.
type Lazy struct {
	logger *zap.Logger

	mu     sync.RWMutex
	client client.Client

	stop     chan struct{}
	stopOnce sync.Once
	done     chan struct{}
}

// NewLazy returns a Lazy that dials in the background, waiting interval
// between failed attempts, until one dial succeeds. dial is injected for
// testability (production passes a temporalx.Dial closure).
func NewLazy(dial func() (client.Client, error), interval time.Duration, logger *zap.Logger) *Lazy {
	l := &Lazy{logger: logger, stop: make(chan struct{}), done: make(chan struct{})}
	go l.redial(dial, interval)
	return l
}

// NewLazySeeded returns an already-connected Lazy (the startup dial
// succeeded); no background loop runs.
func NewLazySeeded(c client.Client, logger *zap.Logger) *Lazy {
	l := &Lazy{logger: logger, client: c, stop: make(chan struct{}), done: make(chan struct{})}
	close(l.done)
	return l
}

func (l *Lazy) redial(dial func() (client.Client, error), interval time.Duration) {
	defer close(l.done)
	for attempt := 1; ; attempt++ {
		// Run the dial in its own goroutine so Close never waits on an
		// unresponsive endpoint (the dial itself takes no context). An
		// abandoned dial that succeeds late closes its own client.
		type result struct {
			c   client.Client
			err error
		}
		ch := make(chan result, 1)
		go func() {
			c, err := dial()
			ch <- result{c, err}
		}()

		select {
		case <-l.stop:
			go func() {
				if r := <-ch; r.err == nil && r.c != nil {
					r.c.Close()
				}
			}()
			return
		case r := <-ch:
			if r.err == nil {
				l.mu.Lock()
				l.client = r.c
				l.mu.Unlock()
				l.logger.Info("Temporal connected by background redial", zap.Int("attempt", attempt))
				return
			}
			// The first attempts are worth a warning; after that one line
			// per interval forever is just pager noise.
			if attempt <= 3 || attempt%20 == 0 {
				l.logger.Warn("Temporal background redial failed", zap.Int("attempt", attempt), zap.Error(r.err))
			}
		}

		// Pace AFTER the failed attempt so interval is a floor between
		// dials, not a ticker that queues up during a slow one.
		select {
		case <-l.stop:
			return
		case <-time.After(interval):
		}
	}
}

// TemporalReady reports whether a connection exists. Transport guards use it
// where they used to nil-check the client. Safe on a nil receiver (reports
// not-ready) so a future nil *Lazy can never panic a request path.
func (l *Lazy) TemporalReady() bool {
	if l == nil {
		return false
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.client != nil
}

// ExecuteWorkflow delegates to the connected client, or returns
// ErrTemporalUnavailable while the background dial is still trying.
func (l *Lazy) ExecuteWorkflow(ctx context.Context, options client.StartWorkflowOptions, workflow any, args ...any) (client.WorkflowRun, error) {
	if l == nil {
		return nil, ErrTemporalUnavailable
	}
	l.mu.RLock()
	c := l.client
	l.mu.RUnlock()
	if c == nil {
		return nil, ErrTemporalUnavailable
	}
	return c.ExecuteWorkflow(ctx, options, workflow, args...)
}

// Close stops the background loop (without waiting on an in-flight dial) and
// closes the connection if one was established. Safe to call more than once
// and on a nil receiver.
func (l *Lazy) Close() {
	if l == nil {
		return
	}
	l.stopOnce.Do(func() { close(l.stop) })
	<-l.done
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.client != nil {
		l.client.Close()
		l.client = nil
	}
}

// Ready reports whether a Starter can start workflows right now. A nil
// Starter is not ready; Starters without a readiness signal (bare clients,
// test fakes) are assumed ready; a Lazy answers with its connection state.
func Ready(s Starter) bool {
	if s == nil {
		return false
	}
	if rc, ok := s.(interface{ TemporalReady() bool }); ok {
		return rc.TemporalReady()
	}
	return true
}
