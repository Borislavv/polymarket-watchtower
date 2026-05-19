package detection

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/market"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/trade"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/vo"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/metrics"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/repository"
)

// fakeClaimer captures every transition the worker drives. It's the
// substitute for *repository.TradeRepository in tests.
type fakeClaimer struct {
	mu     sync.Mutex
	queue  []repository.PendingDetectionTrade
	served bool

	analyzed []int64
	skipped  []skipCall
	failed   []failedCall
	claimErr error

	lastClaim      time.Time
	resetCalls     int
	resetReturns   int64
	lastResetAfter time.Duration
}

type skipCall struct {
	id     int64
	reason string
}
type failedCall struct {
	id  int64
	msg string
}

func (f *fakeClaimer) ClaimUndetectedTrades(_ context.Context, _ string, _ int32, _ time.Duration) ([]repository.PendingDetectionTrade, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastClaim = time.Now()
	if f.claimErr != nil {
		return nil, f.claimErr
	}
	if f.served {
		return nil, nil
	}
	f.served = true
	return f.queue, nil
}

// ResetStaleDetectionClaims is the lease-reclaim primitive. Records
// the call so tests can verify the worker invokes it once per tick.
func (f *fakeClaimer) ResetStaleDetectionClaims(_ context.Context, after time.Duration) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resetCalls++
	f.lastResetAfter = after
	return f.resetReturns, nil
}
func (f *fakeClaimer) MarkDetectionAnalyzed(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.analyzed = append(f.analyzed, id)
	return nil
}
func (f *fakeClaimer) MarkDetectionSkipped(_ context.Context, id int64, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.skipped = append(f.skipped, skipCall{id, reason})
	return nil
}
func (f *fakeClaimer) MarkDetectionFailed(_ context.Context, id int64, msg string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failed = append(f.failed, failedCall{id, msg})
	return nil
}

// fakeCache returns markets keyed by condition_id.
type fakeCache struct{ m map[string]market.Market }

func (c *fakeCache) Get(id vo.MarketID) (market.Market, bool) {
	v, ok := c.m[string(id)]
	return v, ok
}

// fakeObserver records every Observe call.
type fakeObserver struct {
	mu    sync.Mutex
	seen  []trade.Trade
	panic bool
}

func (f *fakeObserver) Observe(_ context.Context, _ market.Market, t trade.Trade) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.panic {
		panic("boom")
	}
	f.seen = append(f.seen, t)
}

func nopLogger() *zerolog.Logger {
	l := zerolog.Nop()
	return &l
}

func newPending(id int64, marketCond string, traderID *int64, tradedAt time.Time) repository.PendingDetectionTrade {
	return repository.PendingDetectionTrade{
		ID:                id,
		MarketID:          1,
		MarketConditionID: marketCond,
		TraderID:          traderID,
		OutcomeToken:      "tok-yes",
		Side:              "BUY",
		Price:             0.1,
		SizeShares:        10000,
		NotionalUSD:       1000,
		TradedAt:          tradedAt,
	}
}

// TestRecentTradeIsObservedAndMarkedAnalyzed is the happy path: a
// trade younger than StaleThreshold goes through Observe and is
// stamped status=analyzed.
func TestRecentTradeIsObservedAndMarkedAnalyzed(t *testing.T) {
	now := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	traderID := int64(7)
	cl := &fakeClaimer{queue: []repository.PendingDetectionTrade{
		newPending(1, "0xa", &traderID, now.Add(-10*time.Minute)),
	}}
	cache := &fakeCache{m: map[string]market.Market{"0xa": {ID: "0xa", Slug: "us-pres"}}}
	obs := &fakeObserver{}
	w := New(Config{Workers: 1, ClaimLimit: 10, Interval: time.Hour,
		StaleThreshold: 2 * time.Hour, Clock: func() time.Time { return now }},
		cl, cache, obs, func(_ context.Context, id int64) string { return "0xwallet" }, metrics.New(), nopLogger())

	w.Tick(context.Background())

	if len(obs.seen) != 1 {
		t.Fatalf("expected 1 Observe, got %d", len(obs.seen))
	}
	if obs.seen[0].Taker != "0xwallet" {
		t.Errorf("Taker resolver wired wrong: %q", obs.seen[0].Taker)
	}
	if len(cl.analyzed) != 1 || cl.analyzed[0] != 1 {
		t.Fatalf("analyzed list: %v", cl.analyzed)
	}
	if len(cl.skipped) != 0 || len(cl.failed) != 0 {
		t.Errorf("no skipped/failed expected: skipped=%v failed=%v", cl.skipped, cl.failed)
	}
}

// TestOldTradeIsObservedButStampedSkipped pins the v6 contract: a
// trade older than StaleThreshold still flows through Observe (so the
// scorer's internal lag gate logs telemetry), but the row is stamped
// 'skipped' with reason too_old_for_live_alert so the DB tells the
// same truth and no Telegram message is produced.
func TestOldTradeIsObservedButStampedSkipped(t *testing.T) {
	now := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	cl := &fakeClaimer{queue: []repository.PendingDetectionTrade{
		newPending(2, "0xa", nil, now.Add(-3*time.Hour)), // 3h old, threshold is 2h
	}}
	cache := &fakeCache{m: map[string]market.Market{"0xa": {ID: "0xa"}}}
	obs := &fakeObserver{}
	w := New(Config{Workers: 1, ClaimLimit: 10, Interval: time.Hour,
		StaleThreshold: 2 * time.Hour, Clock: func() time.Time { return now }},
		cl, cache, obs, nil, metrics.New(), nopLogger())

	w.Tick(context.Background())

	if len(obs.seen) != 1 {
		t.Fatalf("Observe must still run for analytics even on old trades; got %d", len(obs.seen))
	}
	if len(cl.skipped) != 1 || cl.skipped[0].reason != SkipReasonTooOldForLiveAlert {
		t.Fatalf("expected one skipped/too_old_for_live_alert, got %v", cl.skipped)
	}
	if len(cl.analyzed) != 0 {
		t.Errorf("must not be marked analyzed when stamped skipped: %v", cl.analyzed)
	}
}

// TestUnknownMarketSkipped pins the safety belt: when discover hasn't
// caught up and the cache misses, the row is stamped 'skipped/market_unknown'
// so the worker doesn't busy-loop on the same trade.
func TestUnknownMarketSkipped(t *testing.T) {
	now := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	cl := &fakeClaimer{queue: []repository.PendingDetectionTrade{
		newPending(3, "0xunknown", nil, now.Add(-5*time.Minute)),
	}}
	cache := &fakeCache{m: map[string]market.Market{}} // empty → miss
	obs := &fakeObserver{}
	w := New(Config{Workers: 1, ClaimLimit: 10, Interval: time.Hour,
		StaleThreshold: 2 * time.Hour, Clock: func() time.Time { return now }},
		cl, cache, obs, nil, metrics.New(), nopLogger())

	w.Tick(context.Background())

	if len(obs.seen) != 0 {
		t.Fatalf("Observe must not run when market unknown; got %d", len(obs.seen))
	}
	if len(cl.skipped) != 1 || cl.skipped[0].reason != SkipReasonMarketUnknown {
		t.Fatalf("expected market_unknown skip, got %v", cl.skipped)
	}
}

// TestPanicInObserveMarksFailedAndContinues pins the resilience
// contract: a panicking Observe must not kill the worker; the row is
// stamped 'failed' and the next row processes normally.
func TestPanicInObserveMarksFailedAndContinues(t *testing.T) {
	now := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	cl := &fakeClaimer{queue: []repository.PendingDetectionTrade{
		newPending(10, "0xa", nil, now.Add(-1*time.Minute)),
		newPending(11, "0xa", nil, now.Add(-1*time.Minute)),
	}}
	cache := &fakeCache{m: map[string]market.Market{"0xa": {ID: "0xa"}}}
	// observer panics on FIRST call, succeeds on rest. fakeObserver above
	// panics unconditionally when .panic=true, so build a counting variant.
	obs := &countingObserver{panicOn: 1}
	w := New(Config{Workers: 1, ClaimLimit: 10, Interval: time.Hour,
		StaleThreshold: 2 * time.Hour, Clock: func() time.Time { return now }},
		cl, cache, obs, nil, metrics.New(), nopLogger())

	w.Tick(context.Background())

	if len(cl.failed) != 1 {
		t.Fatalf("first row should be marked failed, got %v", cl.failed)
	}
	if len(cl.analyzed) != 1 {
		t.Fatalf("second row should still process after panic: %v", cl.analyzed)
	}
}

type countingObserver struct {
	mu      sync.Mutex
	calls   int
	panicOn int
}

func (o *countingObserver) Observe(_ context.Context, _ market.Market, _ trade.Trade) {
	o.mu.Lock()
	o.calls++
	c := o.calls
	o.mu.Unlock()
	if c == o.panicOn {
		panic("boom")
	}
}

// TestClaimErrorDoesNotKillWorker pins that a transient claim error
// is logged and the worker yields the goroutine — it does not panic
// or otherwise short-circuit future ticks.
func TestClaimErrorDoesNotKillWorker(t *testing.T) {
	cl := &fakeClaimer{claimErr: errors.New("db blip")}
	cache := &fakeCache{m: map[string]market.Market{}}
	obs := &fakeObserver{}
	w := New(Config{Workers: 1, ClaimLimit: 10, Interval: time.Hour,
		Clock: func() time.Time { return time.Now() }},
		cl, cache, obs, nil, metrics.New(), nopLogger())

	// Must return without panic.
	w.Tick(context.Background())
}

// TestStaleClaimReclaimRunsOncePerTick pins the v6 lease-hardening
// contract: every tick first calls ResetStaleDetectionClaims with the
// configured ClaimTTL, then proceeds with the claim. A crashed
// sibling's row therefore returns to the pool after ClaimTTL even
// without operator intervention.
func TestStaleClaimReclaimRunsOncePerTick(t *testing.T) {
	cl := &fakeClaimer{resetReturns: 3}
	cache := &fakeCache{m: map[string]market.Market{}}
	obs := &fakeObserver{}
	w := New(Config{Workers: 1, ClaimLimit: 10, Interval: time.Hour,
		ClaimTTL: 7 * time.Minute, WorkerID: "test-worker",
		Clock: func() time.Time { return time.Now() }},
		cl, cache, obs, nil, metrics.New(), nopLogger())
	w.Tick(context.Background())

	if cl.resetCalls != 1 {
		t.Fatalf("expected exactly 1 reset call per tick, got %d", cl.resetCalls)
	}
	if cl.lastResetAfter != 7*time.Minute {
		t.Errorf("reset TTL: got %v want 7m", cl.lastResetAfter)
	}
}

// TestEmptyQueueIsNoOp pins that an empty claim returns 0 trades and
// the worker yields without crash, mark calls, or Observe.
func TestEmptyQueueIsNoOp(t *testing.T) {
	cl := &fakeClaimer{queue: nil}
	cache := &fakeCache{m: map[string]market.Market{}}
	obs := &fakeObserver{}
	w := New(Config{Workers: 2, ClaimLimit: 10, Interval: time.Hour,
		Clock: func() time.Time { return time.Now() }},
		cl, cache, obs, nil, metrics.New(), nopLogger())

	w.Tick(context.Background())

	if len(obs.seen)+len(cl.analyzed)+len(cl.skipped)+len(cl.failed) != 0 {
		t.Errorf("nothing should fire on empty queue")
	}
}
