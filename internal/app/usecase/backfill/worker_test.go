package backfill

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/trade"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/vo"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/polymarket/dataapi"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/repository"
)

// --- fakes ----------------------------------------------------------------

type fakeMarketStore struct {
	mu            sync.Mutex
	candidates    []repository.Market
	beginErr      error
	resetCalls    int
	beginIDs      []int64
	completeCalls []completeCall
	failCalls     []failCall
	// listCalls captures every (limit, partialRetryAfter) the worker
	// asked for so the cooldown plumbing can be pinned by test.
	listCalls []listCall
}

type listCall struct {
	Limit             int32
	PartialRetryAfter time.Duration
}

type completeCall struct {
	ID     int64
	Status repository.BackfillStatus
	Oldest time.Time
	Newest time.Time
}

type failCall struct {
	ID     int64
	ErrMsg string
}

func (f *fakeMarketStore) ResetStaleRunning(_ context.Context, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resetCalls++
	return nil
}

func (f *fakeMarketStore) ListActiveForBackfill(_ context.Context, limit int32, partialRetryAfter time.Duration) ([]repository.Market, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCalls = append(f.listCalls, listCall{Limit: limit, PartialRetryAfter: partialRetryAfter})
	out := f.candidates
	f.candidates = nil
	return out, nil
}

func (f *fakeMarketStore) BeginBackfill(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.beginIDs = append(f.beginIDs, id)
	return f.beginErr
}

func (f *fakeMarketStore) CompleteBackfill(_ context.Context, id int64, s repository.BackfillStatus, oldest, newest time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.completeCalls = append(f.completeCalls, completeCall{ID: id, Status: s, Oldest: oldest, Newest: newest})
	return nil
}

func (f *fakeMarketStore) FailBackfill(_ context.Context, id int64, msg string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failCalls = append(f.failCalls, failCall{ID: id, ErrMsg: msg})
	return nil
}

type fakeTrades struct {
	mu       sync.Mutex
	batches  [][]repository.InsertTradeInput
	upserted int
}

func (f *fakeTrades) UpsertBatch(_ context.Context, rows []repository.InsertTradeInput) (repository.UpsertResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.batches = append(f.batches, rows)
	f.upserted += len(rows)
	return repository.UpsertResult{Requested: len(rows), Inserted: len(rows)}, nil
}

type fakeTraders struct {
	mu  sync.Mutex
	ids map[string]int64
}

func (f *fakeTraders) UpsertSeen(_ context.Context, wallets []string) ([]repository.Trader, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ids == nil {
		f.ids = make(map[string]int64)
	}
	out := make([]repository.Trader, 0, len(wallets))
	for _, w := range wallets {
		id, ok := f.ids[w]
		if !ok {
			id = int64(len(f.ids) + 1)
			f.ids[w] = id
		}
		out = append(out, repository.Trader{ID: id, WalletAddress: w})
	}
	return out, nil
}

type fakeClient struct {
	pages [][]trade.Trade
	cap   int // pages beyond this index return ErrOffsetCapExceeded
	err   error
	mu    sync.Mutex
	calls int
}

func (f *fakeClient) ListTradesPage(_ context.Context, _ vo.MarketID, offset, _ int) ([]trade.Trade, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	idx := offset / 500
	if idx >= len(f.pages) {
		if f.cap > 0 && idx >= f.cap {
			return nil, dataapi.ErrOffsetCapExceeded
		}
		return nil, nil // exhausted
	}
	return f.pages[idx], nil
}

func nopLogger() *zerolog.Logger {
	l := zerolog.Nop()
	return &l
}

func makePage(n int, start time.Time, wallets ...string) []trade.Trade {
	if len(wallets) == 0 {
		wallets = []string{"0xwhale"}
	}
	out := make([]trade.Trade, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, trade.Trade{
			ID:        fmt.Sprintf("tr-%d-%d", start.UnixNano(), i),
			Market:    "0xa",
			Token:     "tok",
			Side:      trade.SideBuy,
			Price:     0.5,
			Size:      100,
			Timestamp: start.Add(time.Duration(-i) * time.Minute),
			Taker:     wallets[i%len(wallets)],
		})
	}
	return out
}

// --- tests ----------------------------------------------------------------

func TestWorker_CompletesWhenLastPageIsShort(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	ms := &fakeMarketStore{candidates: []repository.Market{{ID: 1, ConditionID: "0xa"}}}
	tr := &fakeTrades{}
	td := &fakeTraders{}
	cl := &fakeClient{pages: [][]trade.Trade{
		makePage(500, now),
		makePage(500, now.Add(-9*time.Hour)),
		makePage(123, now.Add(-18*time.Hour)), // short → complete
	}}
	w := New(Config{PageSize: 500, Concurrency: 1, BatchSize: 1, Clock: func() time.Time { return now }},
		ms, tr, td, cl, nil, nopLogger())

	w.Tick(context.Background())

	if len(ms.completeCalls) != 1 {
		t.Fatalf("complete calls: %+v", ms.completeCalls)
	}
	if ms.completeCalls[0].Status != repository.BackfillCompleted {
		t.Errorf("status: %s", ms.completeCalls[0].Status)
	}
	if len(ms.failCalls) != 0 {
		t.Errorf("unexpected fail calls: %+v", ms.failCalls)
	}
	if tr.upserted != 500+500+123 {
		t.Errorf("upserted: %d want 1123", tr.upserted)
	}
}

func TestWorker_PartialAPILimitOnOffsetCap(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	ms := &fakeMarketStore{candidates: []repository.Market{{ID: 7, ConditionID: "0xa"}}}
	tr := &fakeTrades{}
	td := &fakeTraders{}
	// 7 full pages of 500 → 3500 rows total, then offset would be 3500 > 3000 → cap.
	pages := make([][]trade.Trade, 7)
	for i := range pages {
		pages[i] = makePage(500, now.Add(time.Duration(-i)*9*time.Hour))
	}
	cl := &fakeClient{pages: pages, cap: 7}
	w := New(Config{PageSize: 500, Concurrency: 1, BatchSize: 1, Clock: func() time.Time { return now }},
		ms, tr, td, cl, nil, nopLogger())

	w.Tick(context.Background())

	if len(ms.completeCalls) != 1 || ms.completeCalls[0].Status != repository.BackfillPartialAPILimit {
		t.Fatalf("expected partial_api_limit, got %+v", ms.completeCalls)
	}
}

func TestWorker_FailsOnUpstreamError(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	ms := &fakeMarketStore{candidates: []repository.Market{{ID: 1, ConditionID: "0xa"}}}
	tr := &fakeTrades{}
	td := &fakeTraders{}
	cl := &fakeClient{err: errors.New("upstream 500")}
	w := New(Config{Concurrency: 1, BatchSize: 1, Clock: func() time.Time { return now }},
		ms, tr, td, cl, nil, nopLogger())

	w.Tick(context.Background())

	if len(ms.failCalls) != 1 {
		t.Fatalf("expected 1 fail call, got %+v", ms.failCalls)
	}
	if len(ms.completeCalls) != 0 {
		t.Errorf("must not complete on failure: %+v", ms.completeCalls)
	}
}

func TestWorker_ContextCancellationLeavesRunning(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	ms := &fakeMarketStore{candidates: []repository.Market{{ID: 1, ConditionID: "0xa"}}}
	cl := &fakeClient{err: context.Canceled}
	w := New(Config{Concurrency: 1, BatchSize: 1, Clock: func() time.Time { return now }},
		ms, &fakeTrades{}, &fakeTraders{}, cl, nil, nopLogger())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w.Tick(ctx)

	// Cancelled mid-run must NOT mark failed (ResetStaleRunning recovers it).
	if len(ms.failCalls) != 0 {
		t.Errorf("cancellation should not fail the market, got %+v", ms.failCalls)
	}
	if len(ms.completeCalls) != 0 {
		t.Errorf("cancellation should not complete the market, got %+v", ms.completeCalls)
	}
}

func TestWorker_ResetsStaleRunningEveryTick(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	ms := &fakeMarketStore{}
	w := New(Config{Concurrency: 1, BatchSize: 1, Clock: func() time.Time { return now }},
		ms, &fakeTrades{}, &fakeTraders{}, &fakeClient{}, nil, nopLogger())

	w.Tick(context.Background())
	w.Tick(context.Background())

	if ms.resetCalls != 2 {
		t.Errorf("reset calls: %d want 2", ms.resetCalls)
	}
}

// TestWorker_PartialRetryAfterPropagatedToStore pins the cooldown
// plumbing: the configured Config.PartialRetryAfter must reach the
// SQL claim query as the partial_retry_after argument. Without this,
// `partial_api_limit` markets would re-enter the claim pool every
// tick and burn API quota — the operator-reported symptom that drove
// migration 00014 + this knob.
func TestWorker_PartialRetryAfterPropagatedToStore(t *testing.T) {
	ms := &fakeMarketStore{}
	tr := &fakeTrades{}
	td := &fakeTraders{}
	cl := &fakeClient{}
	w := New(Config{
		Concurrency:       1,
		BatchSize:         4,
		PageSize:          500,
		PartialRetryAfter: 6 * time.Hour,
	}, ms, tr, td, cl, nil, nopLogger())

	w.Tick(context.Background())

	if len(ms.listCalls) != 1 {
		t.Fatalf("expected 1 list call, got %d", len(ms.listCalls))
	}
	if got := ms.listCalls[0].PartialRetryAfter; got != 6*time.Hour {
		t.Errorf("partial_retry_after: got %v want 6h", got)
	}
	if got := ms.listCalls[0].Limit; got != 4 {
		t.Errorf("limit: got %d want 4", got)
	}
}

// TestWorker_PartialRetryAfterDefaults pins the applyDefaults
// guarantee: zero/negative in the Config gets bumped to the
// production default (6h), so any caller that forgets to set it
// still gets cooldown.
func TestWorker_PartialRetryAfterDefaults(t *testing.T) {
	ms := &fakeMarketStore{}
	tr := &fakeTrades{}
	td := &fakeTraders{}
	cl := &fakeClient{}
	w := New(Config{}, ms, tr, td, cl, nil, nopLogger()) // zero cfg

	w.Tick(context.Background())

	if len(ms.listCalls) != 1 {
		t.Fatalf("expected 1 list call, got %d", len(ms.listCalls))
	}
	if got := ms.listCalls[0].PartialRetryAfter; got != 6*time.Hour {
		t.Errorf("default partial_retry_after: got %v want 6h", got)
	}
}

func TestWorker_PersistsTradersBeforeTrades(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	ms := &fakeMarketStore{candidates: []repository.Market{{ID: 1, ConditionID: "0xa"}}}
	tr := &fakeTrades{}
	td := &fakeTraders{}
	cl := &fakeClient{pages: [][]trade.Trade{makePage(3, now, "0xA", "0xB", "0xA")}}
	w := New(Config{Concurrency: 1, BatchSize: 1, PageSize: 500, Clock: func() time.Time { return now }},
		ms, tr, td, cl, nil, nopLogger())

	w.Tick(context.Background())

	if len(td.ids) != 2 {
		t.Errorf("unique wallets: %d want 2", len(td.ids))
	}
	if len(tr.batches) != 1 {
		t.Fatalf("batches: %d want 1", len(tr.batches))
	}
	for _, row := range tr.batches[0] {
		if row.TraderID == nil {
			t.Errorf("trader id not resolved for wallet %s", row.ExternalID)
		}
	}
}
