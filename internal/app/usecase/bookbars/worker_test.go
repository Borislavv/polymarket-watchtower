package bookbars

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/infra/polymarket/clob"
)

type fakeLister struct {
	cands []Candidate
	err   error
}

func (f *fakeLister) ListBookbarsCandidates(_ context.Context, _ int) ([]Candidate, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.cands, nil
}

type fakeFetcher struct {
	books   map[string]clob.Book
	batchEr bool
}

func (f *fakeFetcher) GetBook(_ context.Context, tok string) (clob.Book, error) {
	if b, ok := f.books[tok]; ok {
		return b, nil
	}
	return clob.Book{}, errors.New("not found")
}
func (f *fakeFetcher) GetBooks(_ context.Context, tokens []string) ([]clob.Book, error) {
	if f.batchEr {
		return nil, errors.New("batch boom")
	}
	out := make([]clob.Book, 0, len(tokens))
	for _, t := range tokens {
		if b, ok := f.books[t]; ok {
			out = append(out, b)
		}
	}
	return out, nil
}

type fakeSink struct {
	mu   sync.Mutex
	bars []Bar
}

func (s *fakeSink) UpsertBar(_ context.Context, b Bar) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bars = append(s.bars, b)
	return nil
}

func newCfg() Config {
	return Config{Enabled: true, Interval: 5 * time.Second, BarSeconds: 5, TopN: 3, MaxMarkets: 100, BatchSize: 10, FetchTimeout: 2 * time.Second}
}

func TestBuildBar_FullDepth(t *testing.T) {
	b := clob.Book{
		Market: "0xCID", AssetID: "TOK",
		Bids: []clob.Level{{Price: 0.49, Size: 100}, {Price: 0.48, Size: 200}, {Price: 0.40, Size: 50}},
		Asks: []clob.Level{{Price: 0.51, Size: 77}, {Price: 0.52, Size: 123}, {Price: 0.60, Size: 500}},
	}
	bar := buildBar(b, "0xCID", 5, 2, time.Date(2026, 5, 23, 0, 0, 0, 0, time.UTC))
	if bar.BestBid != 0.49 || bar.BestAsk != 0.51 {
		t.Fatalf("best bid/ask: %+v", bar)
	}
	if bar.MidPrice != 0.50 || bar.Spread < 0.019 || bar.Spread > 0.021 {
		t.Fatalf("mid/spread: %+v", bar)
	}
	// top-2 bid depth = 100 + 200 = 300; top-2 ask = 77 + 123 = 200.
	if bar.BidDepthTopN != 300 || bar.AskDepthTopN != 200 {
		t.Fatalf("topN depth: %+v", bar)
	}
	// imbalance = (300 - 200)/500 = 0.2
	if bar.DepthImbal < 0.19 || bar.DepthImbal > 0.21 {
		t.Fatalf("imbalance: %f", bar.DepthImbal)
	}
}

func TestBuildBar_EmptyLevelsHandled(t *testing.T) {
	b := clob.Book{AssetID: "TOK"}
	bar := buildBar(b, "X", 5, 5, time.Now())
	if bar.BestBid != 0 || bar.BestAsk != 0 || bar.MidPrice != 0 || bar.DepthImbal != 0 {
		t.Fatalf("empty book should yield zeros: %+v", bar)
	}
}

func TestTick_PersistsBars(t *testing.T) {
	now := time.Date(2026, 5, 23, 0, 0, 0, 0, time.UTC)
	lister := &fakeLister{cands: []Candidate{
		{ConditionID: "0xCID", Token: "TOK"},
		{ConditionID: "0xCID2", Token: "TOK2"},
	}}
	fetcher := &fakeFetcher{books: map[string]clob.Book{
		"TOK":  {Market: "0xCID", AssetID: "TOK", Bids: []clob.Level{{Price: 0.5, Size: 100}}, Asks: []clob.Level{{Price: 0.55, Size: 100}}},
		"TOK2": {Market: "0xCID2", AssetID: "TOK2", Bids: []clob.Level{{Price: 0.1, Size: 1000}}, Asks: []clob.Level{{Price: 0.2, Size: 800}}},
	}}
	sink := &fakeSink{}
	w := New(newCfg(), lister, fetcher, sink, nil, nil).WithClock(func() time.Time { return now })
	w.Tick(context.Background())
	if len(sink.bars) != 2 {
		t.Fatalf("bars persisted: got %d want 2", len(sink.bars))
	}
}

func TestTick_BatchFetchErrorContinues(t *testing.T) {
	lister := &fakeLister{cands: []Candidate{{ConditionID: "X", Token: "T"}}}
	fetcher := &fakeFetcher{batchEr: true}
	sink := &fakeSink{}
	w := New(newCfg(), lister, fetcher, sink, nil, nil)
	w.Tick(context.Background())
	if len(sink.bars) != 0 {
		t.Fatalf("batch failure must not write bars")
	}
}

func TestTick_DepsMissingNoOp(t *testing.T) {
	w := New(newCfg(), nil, nil, nil, nil, nil)
	w.Tick(context.Background())
}
