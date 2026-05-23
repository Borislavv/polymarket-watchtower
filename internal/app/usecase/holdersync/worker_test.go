package holdersync

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type fakeMarkets struct {
	cands []Candidate
	err   error
}

func (f *fakeMarkets) ListHolderSyncCandidates(_ context.Context, _ int, _ time.Duration) ([]Candidate, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.cands, nil
}

type fakeFetcher struct {
	rows    map[string][]HolderRow
	failOn  map[string]bool
	fetched int
	mu      sync.Mutex
}

func (f *fakeFetcher) FetchHolders(_ context.Context, cid, _ string, _ int) ([]HolderRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fetched++
	if f.failOn[cid] {
		return nil, errors.New("fetch failed")
	}
	return f.rows[cid], nil
}

type fakeSink struct {
	mu        sync.Mutex
	snapshots []Snapshot
}

func (s *fakeSink) UpsertSnapshot(_ context.Context, snap Snapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snapshots = append(s.snapshots, snap)
	return nil
}

func newCfg() Config {
	return Config{
		Enabled:      true,
		Interval:     time.Minute,
		MaxMarkets:   10,
		TopK:         25,
		FetchTimeout: 5 * time.Second,
		Concurrency:  2,
		StaleAfter:   time.Hour,
	}
}

func TestTick_PersistsSnapshots(t *testing.T) {
	markets := &fakeMarkets{cands: []Candidate{
		{ConditionID: "A", OutcomeToken: "Yes"},
		{ConditionID: "B", OutcomeToken: "No"},
	}}
	fetcher := &fakeFetcher{
		rows: map[string][]HolderRow{
			"A": {{Wallet: "0x1", Rank: 1, Shares: 100, PctOI: 0.05}},
			"B": {{Wallet: "0x2", Rank: 1, Shares: 50, PctOI: 0.02}},
		},
	}
	sink := &fakeSink{}
	w := New(newCfg(), markets, fetcher, sink, nil, nil)
	w.Tick(context.Background())
	if got, want := len(sink.snapshots), 2; got != want {
		t.Fatalf("snapshots: got %d want %d", got, want)
	}
}

func TestTick_FetchFailureDoesNotStopBatch(t *testing.T) {
	markets := &fakeMarkets{cands: []Candidate{
		{ConditionID: "A"}, {ConditionID: "B"},
	}}
	fetcher := &fakeFetcher{
		rows:   map[string][]HolderRow{"B": {{Wallet: "0x2", Rank: 1, Shares: 10}}},
		failOn: map[string]bool{"A": true},
	}
	sink := &fakeSink{}
	w := New(newCfg(), markets, fetcher, sink, nil, nil)
	w.Tick(context.Background())
	if got, want := len(sink.snapshots), 1; got != want {
		t.Fatalf("expected exactly 1 successful upsert; got %d", got)
	}
}

func TestTick_NoOpOnEmpty(t *testing.T) {
	markets := &fakeMarkets{cands: nil}
	fetcher := &fakeFetcher{}
	sink := &fakeSink{}
	w := New(newCfg(), markets, fetcher, sink, nil, nil)
	w.Tick(context.Background())
	if fetcher.fetched != 0 {
		t.Fatalf("must not fetch when no candidates")
	}
}

func TestTick_ListErrorBailsCleanly(t *testing.T) {
	markets := &fakeMarkets{err: errors.New("db down")}
	w := New(newCfg(), markets, &fakeFetcher{}, &fakeSink{}, nil, nil)
	w.Tick(context.Background()) // must not panic
}
