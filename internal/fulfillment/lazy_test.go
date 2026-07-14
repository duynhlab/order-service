package fulfillment

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"go.temporal.io/sdk/client"
	"go.uber.org/zap"
)

// dialScript returns a dial func that fails `failures` times before handing
// out the given client, counting every attempt.
func dialScript(failures int, c client.Client, calls *atomic.Int32) func() (client.Client, error) {
	var n atomic.Int32
	return func() (client.Client, error) {
		calls.Add(1)
		if int(n.Add(1)) <= failures {
			return nil, errors.New("dial refused")
		}
		return c, nil
	}
}

// fakeTemporal is the minimal client.Client stand-in the lazy holder needs:
// only ExecuteWorkflow and Close are exercised.
type fakeTemporal struct {
	client.Client
	executed atomic.Int32
	closed   atomic.Int32
}

func (f *fakeTemporal) ExecuteWorkflow(ctx context.Context, options client.StartWorkflowOptions, workflow any, args ...any) (client.WorkflowRun, error) {
	f.executed.Add(1)
	return nil, nil
}

func (f *fakeTemporal) Close() { f.closed.Add(1) }

func TestLazyNotReadyBeforeFirstSuccessfulDial(t *testing.T) {
	var calls atomic.Int32
	inner := &fakeTemporal{}
	// Dial never succeeds within this test's window.
	l := NewLazy(dialScript(1000, inner, &calls), time.Hour, zap.NewNop())
	defer l.Close()

	if l.TemporalReady() {
		t.Fatal("TemporalReady = true before any successful dial")
	}
	_, err := l.ExecuteWorkflow(context.Background(), client.StartWorkflowOptions{}, nil)
	if !errors.Is(err, ErrTemporalUnavailable) {
		t.Fatalf("ExecuteWorkflow err = %v, want ErrTemporalUnavailable", err)
	}
	if inner.executed.Load() != 0 {
		t.Fatal("inner client was called while not ready")
	}
}

func TestLazyBecomesReadyAndDelegates(t *testing.T) {
	var calls atomic.Int32
	inner := &fakeTemporal{}
	// First attempt fails, second (background retry) succeeds.
	l := NewLazy(dialScript(1, inner, &calls), 10*time.Millisecond, zap.NewNop())
	defer l.Close()

	deadline := time.After(3 * time.Second)
	for !l.TemporalReady() {
		select {
		case <-deadline:
			t.Fatal("lazy client never became ready")
		case <-time.After(5 * time.Millisecond):
		}
	}
	if _, err := l.ExecuteWorkflow(context.Background(), client.StartWorkflowOptions{}, nil); err != nil {
		t.Fatalf("ExecuteWorkflow after ready: %v", err)
	}
	if inner.executed.Load() != 1 {
		t.Fatalf("inner executed = %d, want 1", inner.executed.Load())
	}
}

func TestLazySeededClientIsReadyImmediately(t *testing.T) {
	inner := &fakeTemporal{}
	l := NewLazySeeded(inner, zap.NewNop())
	defer l.Close()

	if !l.TemporalReady() {
		t.Fatal("seeded lazy client not ready")
	}
	if _, err := l.ExecuteWorkflow(context.Background(), client.StartWorkflowOptions{}, nil); err != nil {
		t.Fatalf("ExecuteWorkflow: %v", err)
	}
}

func TestLazyCloseStopsRetriesAndClosesClient(t *testing.T) {
	var calls atomic.Int32
	inner := &fakeTemporal{}
	l := NewLazy(dialScript(1, inner, &calls), 10*time.Millisecond, zap.NewNop())

	deadline := time.After(3 * time.Second)
	for !l.TemporalReady() {
		select {
		case <-deadline:
			t.Fatal("never ready")
		case <-time.After(5 * time.Millisecond):
		}
	}
	l.Close()
	if inner.closed.Load() != 1 {
		t.Fatalf("inner closed = %d, want 1", inner.closed.Load())
	}
}

func TestLazyCloseBeforeReadyStopsLoop(t *testing.T) {
	var calls atomic.Int32
	l := NewLazy(dialScript(1000, &fakeTemporal{}, &calls), 5*time.Millisecond, zap.NewNop())
	time.Sleep(20 * time.Millisecond)
	l.Close()
	n := calls.Load()
	time.Sleep(30 * time.Millisecond)
	if calls.Load() != n {
		t.Fatal("dial attempts continued after Close")
	}
}

func TestReady(t *testing.T) {
	if Ready(nil) {
		t.Fatal("Ready(nil) = true")
	}
	if !Ready(&fakeTemporal{}) {
		t.Fatal("Ready(plain starter) = false, want true (assumed ready)")
	}
	var calls atomic.Int32
	l := NewLazy(dialScript(1000, &fakeTemporal{}, &calls), time.Hour, zap.NewNop())
	defer l.Close()
	if Ready(l) {
		t.Fatal("Ready(unconnected lazy) = true")
	}
	seeded := NewLazySeeded(&fakeTemporal{}, zap.NewNop())
	defer seeded.Close()
	if !Ready(seeded) {
		t.Fatal("Ready(seeded lazy) = false")
	}
}

func TestLazyDoubleCloseAndNilReceiver(t *testing.T) {
	inner := &fakeTemporal{}
	l := NewLazySeeded(inner, zap.NewNop())
	l.Close()
	l.Close() // must not panic or double-close the client
	if inner.closed.Load() != 1 {
		t.Fatalf("inner closed = %d, want 1", inner.closed.Load())
	}

	var nilLazy *Lazy
	if nilLazy.TemporalReady() {
		t.Fatal("nil receiver TemporalReady = true")
	}
	if Ready(nilLazy) {
		t.Fatal("Ready(typed-nil *Lazy) = true")
	}
	if _, err := nilLazy.ExecuteWorkflow(context.Background(), client.StartWorkflowOptions{}, nil); !errors.Is(err, ErrTemporalUnavailable) {
		t.Fatalf("nil receiver ExecuteWorkflow err = %v", err)
	}
	nilLazy.Close() // must not panic
}

func TestLazyCloseDoesNotWaitOnHungDial(t *testing.T) {
	release := make(chan struct{})
	inner := &fakeTemporal{}
	dial := func() (client.Client, error) {
		<-release // simulate a blackholed endpoint
		return inner, nil
	}
	l := NewLazy(dial, time.Millisecond, zap.NewNop())

	closed := make(chan struct{})
	go func() { l.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("Close blocked on an in-flight dial")
	}

	// A late dial success must not leak the client: the abandoned goroutine
	// closes it.
	close(release)
	deadline := time.After(2 * time.Second)
	for inner.closed.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("late-dialed client was never closed")
		case <-time.After(10 * time.Millisecond):
		}
	}
}
