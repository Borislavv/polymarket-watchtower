package workerguard

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestGuard_OverlapSkipped pins the load-bearing contract: a second
// Run() call while the first is still running short-circuits with
// false instead of executing fn.
func TestGuard_OverlapSkipped(t *testing.T) {
	g := New("test", nil, nil)
	started := make(chan struct{})
	release := make(chan struct{})
	var firstRan, secondRan int32
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ok := g.Run(context.Background(), func(_ context.Context) {
			atomic.AddInt32(&firstRan, 1)
			close(started)
			<-release
		})
		if !ok {
			t.Errorf("first Run must succeed")
		}
	}()
	<-started
	ok := g.Run(context.Background(), func(_ context.Context) {
		atomic.AddInt32(&secondRan, 1)
	})
	if ok {
		t.Error("second Run must be skipped while first is in-flight")
	}
	close(release)
	wg.Wait()
	if atomic.LoadInt32(&firstRan) != 1 {
		t.Errorf("first fn must have run once; got %d", firstRan)
	}
	if atomic.LoadInt32(&secondRan) != 0 {
		t.Errorf("second fn must NOT have run; got %d", secondRan)
	}
}

// TestGuard_SequentialOK confirms the gate releases after Run
// returns — subsequent ticks proceed normally.
func TestGuard_SequentialOK(t *testing.T) {
	g := New("test", nil, nil)
	var n int32
	for i := 0; i < 5; i++ {
		ok := g.Run(context.Background(), func(_ context.Context) {
			atomic.AddInt32(&n, 1)
		})
		if !ok {
			t.Errorf("Run %d must succeed sequentially", i)
		}
	}
	if atomic.LoadInt32(&n) != 5 {
		t.Errorf("expected 5 runs; got %d", n)
	}
}

// TestGuard_RunWithTimeout pins the timeout enforcement: a long-
// running fn receives a context that cancels at the timeout.
func TestGuard_RunWithTimeout(t *testing.T) {
	g := New("test", nil, nil)
	var deadline time.Time
	ok := g.RunWithTimeout(context.Background(), 25*time.Millisecond, func(ctx context.Context) {
		d, _ := ctx.Deadline()
		deadline = d
		<-ctx.Done()
	})
	if !ok {
		t.Error("RunWithTimeout must execute fn")
	}
	if deadline.IsZero() {
		t.Error("fn should have seen a deadline")
	}
	if time.Until(deadline) > 25*time.Millisecond {
		t.Error("deadline should be tight")
	}
}

// TestGuard_PanicReleasesGate ensures a fn that panics still
// releases the gate so the next tick can run.
func TestGuard_PanicReleasesGate(t *testing.T) {
	g := New("test", nil, nil)
	func() {
		defer func() { _ = recover() }()
		g.Run(context.Background(), func(_ context.Context) {
			panic("boom")
		})
	}()
	// Second tick must succeed.
	ok := g.Run(context.Background(), func(_ context.Context) {})
	if !ok {
		t.Error("gate not released after panic")
	}
}
