package walletgraph

import (
	"context"
	"sync"
	"testing"
	"time"
)

type fakeRows struct{ rows []CoTradeRow }

func (f *fakeRows) ListCoTradeRows(_ context.Context, _ time.Duration, _ int) ([]CoTradeRow, error) {
	return f.rows, nil
}

type fakeSink struct {
	mu    sync.Mutex
	edges []Edge
}

func (s *fakeSink) UpsertEdges(_ context.Context, e []Edge) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.edges = append(s.edges, e...)
	return nil
}

func newCfg() Config {
	return Config{
		Enabled: true, Interval: time.Hour, CoTradeWindow: time.Hour,
		MinSharedEvents: 3, BatchSize: 10000, EdgeVersion: 1,
	}
}

func TestComputeCoTradeEdges_SharedAtLeastMin(t *testing.T) {
	now := time.Now()
	rows := []CoTradeRow{
		{Wallet: "A", EventSlug: "ev1", Side: "BUY", At: now},
		{Wallet: "A", EventSlug: "ev2", Side: "BUY", At: now},
		{Wallet: "A", EventSlug: "ev3", Side: "BUY", At: now},
		{Wallet: "B", EventSlug: "ev1", Side: "BUY", At: now},
		{Wallet: "B", EventSlug: "ev2", Side: "BUY", At: now},
		{Wallet: "B", EventSlug: "ev3", Side: "BUY", At: now},
		{Wallet: "C", EventSlug: "ev1", Side: "BUY", At: now},
		// A & B share all 3 → edge fires.
		// C only shares 1 with A → no edge.
	}
	edges := computeCoTradeEdges(rows, 3, 1, now)
	if got, want := len(edges), 1; got != want {
		t.Fatalf("expected exactly 1 edge: got %d (%+v)", got, edges)
	}
	if edges[0].WalletA != "A" || edges[0].WalletB != "B" {
		t.Fatalf("expected sorted pair A/B; got %+v", edges[0])
	}
	if edges[0].CoEventsCount != 3 {
		t.Fatalf("expected co_events=3; got %d", edges[0].CoEventsCount)
	}
	if edges[0].SimilarityScore != 1.0 {
		t.Fatalf("expected similarity=1.0; got %f", edges[0].SimilarityScore)
	}
}

func TestComputeCoTradeEdges_DifferentSidesDoNotEdge(t *testing.T) {
	now := time.Now()
	rows := []CoTradeRow{
		{Wallet: "A", EventSlug: "ev1", Side: "BUY"},
		{Wallet: "A", EventSlug: "ev2", Side: "BUY"},
		{Wallet: "A", EventSlug: "ev3", Side: "BUY"},
		{Wallet: "B", EventSlug: "ev1", Side: "SELL"},
		{Wallet: "B", EventSlug: "ev2", Side: "SELL"},
		{Wallet: "B", EventSlug: "ev3", Side: "SELL"},
	}
	edges := computeCoTradeEdges(rows, 3, 1, now)
	if len(edges) != 0 {
		t.Fatalf("opposite-side wallets must not pair; got %+v", edges)
	}
}

func TestTick_PersistsViaSink(t *testing.T) {
	now := time.Now()
	rows := []CoTradeRow{
		{Wallet: "A", EventSlug: "ev1", Side: "BUY"},
		{Wallet: "A", EventSlug: "ev2", Side: "BUY"},
		{Wallet: "A", EventSlug: "ev3", Side: "BUY"},
		{Wallet: "B", EventSlug: "ev1", Side: "BUY"},
		{Wallet: "B", EventSlug: "ev2", Side: "BUY"},
		{Wallet: "B", EventSlug: "ev3", Side: "BUY"},
	}
	sink := &fakeSink{}
	w := New(newCfg(), &fakeRows{rows: rows}, nil, sink, nil, nil).WithClock(func() time.Time { return now })
	w.Tick(context.Background())
	if len(sink.edges) != 1 {
		t.Fatalf("expected 1 edge persisted; got %d", len(sink.edges))
	}
}

func TestTick_DepsMissingNoOp(t *testing.T) {
	w := New(newCfg(), nil, nil, nil, nil, nil)
	w.Tick(context.Background())
}
