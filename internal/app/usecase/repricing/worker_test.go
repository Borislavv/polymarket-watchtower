package repricing

import (
	"context"
	"sync"
	"testing"
	"time"
)

type fakeTriggers struct{ triggers []Trigger }

func (f *fakeTriggers) ListNewTriggers(_ context.Context, _ time.Duration, _ int) ([]Trigger, error) {
	return f.triggers, nil
}

type fakeOpen struct{ rows []OpenWindow }

func (f *fakeOpen) ListOpenWindows(_ context.Context, _ time.Time, _ int) ([]OpenWindow, error) {
	return f.rows, nil
}

type fakeSampler struct {
	target   float64
	targetOK bool
	peer     float64
	peerN    int
}

func (f *fakeSampler) SampleTarget(_ context.Context, _ string) (float64, bool, error) {
	return f.target, f.targetOK, nil
}
func (f *fakeSampler) SamplePeerMedian(_ context.Context, _, _ string) (float64, int, error) {
	return f.peer, f.peerN, nil
}

type fakeSink struct {
	mu     sync.Mutex
	opened []Trigger
	closed []closedRow
}
type closedRow struct {
	id       int64
	observed float64
	peer     float64
	lag      float64
	status   string
}

func (s *fakeSink) OpenWindow(_ context.Context, t Trigger, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.opened = append(s.opened, t)
	return nil
}
func (s *fakeSink) CloseWindow(_ context.Context, id int64, observed, peer, lag float64, status string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = append(s.closed, closedRow{id, observed, peer, lag, status})
	return nil
}

func newCfg() Config {
	return Config{
		Enabled: true, Interval: time.Minute, OpenLookback: 15 * time.Minute,
		MaxOpenWindows: 100, CloseAfter: 2 * time.Hour,
	}
}

func TestTick_OpensNewWindowsAndClosesDue(t *testing.T) {
	now := time.Date(2026, 5, 22, 12, 0, 0, 0, time.UTC)
	triggers := &fakeTriggers{triggers: []Trigger{{
		ConditionID: "A", EventSlug: "ny-mayor",
		Kind:     TriggerAnnotation,
		OpenedAt: now, BaselinePrice: 0.50,
	}}}
	open := &fakeOpen{rows: []OpenWindow{{
		ID: 7, ConditionID: "B", EventSlug: "ny-mayor",
		BaselinePrice: 0.40, ClosesAt: now,
	}}}
	sampler := &fakeSampler{target: 0.42, targetOK: true, peer: 0.50, peerN: 3}
	sink := &fakeSink{}
	w := New(newCfg(), triggers, open, sampler, sink, nil, nil).WithClock(func() time.Time { return now })
	w.Tick(context.Background())

	if got, want := len(sink.opened), 1; got != want {
		t.Fatalf("opened: got %d want %d", got, want)
	}
	if got, want := len(sink.closed), 1; got != want {
		t.Fatalf("closed: got %d want %d", got, want)
	}
	row := sink.closed[0]
	if row.status != "closed_lag_detected" {
		t.Fatalf("expected lag detected (observed=0.02 peer=0.10): %+v", row)
	}
}

func TestTick_TargetSampleMissBailsStale(t *testing.T) {
	now := time.Now()
	open := &fakeOpen{rows: []OpenWindow{{ID: 7, ConditionID: "A", BaselinePrice: 0.4, ClosesAt: now}}}
	sampler := &fakeSampler{targetOK: false}
	sink := &fakeSink{}
	w := New(newCfg(), &fakeTriggers{}, open, sampler, sink, nil, nil)
	w.Tick(context.Background())
	// v11.9: target sample miss → stale_missing_price (not blocked).
	if got, want := len(sink.closed), 1; got != want || sink.closed[0].status != "stale_missing_price" {
		t.Fatalf("expected one stale_missing_price row: %+v", sink.closed)
	}
}

func TestTick_PeerSampleMissBailsStale(t *testing.T) {
	now := time.Now()
	open := &fakeOpen{rows: []OpenWindow{{ID: 8, ConditionID: "A", BaselinePrice: 0.4, ClosesAt: now}}}
	// Target found, peer missing → stale_missing_peers.
	sampler := &fakeSampler{target: 0.45, targetOK: true, peerN: 0}
	sink := &fakeSink{}
	w := New(newCfg(), &fakeTriggers{}, open, sampler, sink, nil, nil)
	w.Tick(context.Background())
	if got, want := len(sink.closed), 1; got != want || sink.closed[0].status != "stale_missing_peers" {
		t.Fatalf("expected one stale_missing_peers row: %+v", sink.closed)
	}
}

func TestTick_DepsMissingNoOp(t *testing.T) {
	w := New(newCfg(), nil, nil, nil, nil, nil, nil)
	w.Tick(context.Background())
}
