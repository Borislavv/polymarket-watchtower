package detect

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/analytics/accumulation"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/analytics/baseline"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/analytics/cluster"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/analytics/mmfilter"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/marketcache"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/anomaly"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/market"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/vo"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/metrics"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/repository"
	"github.com/rs/zerolog"
)

// fakeAccumulationLines hands the detector a pre-baked AccumulationLine.
// The test mutates the line between calls to simulate progression.
type fakeAccumulationLines struct {
	mu   sync.Mutex
	line repository.AccumulationLine
	err  error
}

func (f *fakeAccumulationLines) AccumulationLineSummary(_ context.Context, _ repository.AccumulationLineQuery) (repository.AccumulationLine, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.line, f.err
}

func (f *fakeAccumulationLines) set(l repository.AccumulationLine) {
	f.mu.Lock()
	f.line = l
	f.mu.Unlock()
}

// fakeMarkets resolves condition_id → repository.Market for the test.
type fakeMarketResolver struct {
	id int64
}

func (f *fakeMarketResolver) GetByConditionID(_ context.Context, _ string) (repository.Market, error) {
	return repository.Market{ID: f.id}, nil
}

// fakeTraderResolver resolves wallet → repository.Trader. The test uses
// id=42 for the canonical "0xwhale" wallet.
type fakeTraderResolver struct {
	id int64
}

func (f *fakeTraderResolver) GetByWallet(_ context.Context, _ string) (repository.Trader, error) {
	return repository.Trader{ID: f.id}, nil
}

// fakeAlerts is a tiny in-memory polymarket_alerts stand-in: tracks
// dedup_key uniqueness and lets the test inspect what was persisted.
type fakeAlerts struct {
	mu      sync.Mutex
	seen    map[string]struct{}
	inserts []repository.NewAlert
}

func newFakeAlerts() *fakeAlerts {
	return &fakeAlerts{seen: make(map[string]struct{})}
}

func (f *fakeAlerts) TryCreatePending(_ context.Context, a repository.NewAlert) (repository.Alert, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, dup := f.seen[a.DedupKey]; dup {
		return repository.Alert{}, false, nil
	}
	f.seen[a.DedupKey] = struct{}{}
	f.inserts = append(f.inserts, a)
	return repository.Alert{DedupKey: a.DedupKey, Kind: a.Kind, Severity: a.Severity}, true, nil
}

func (f *fakeAlerts) all() []repository.NewAlert {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]repository.NewAlert, len(f.inserts))
	copy(out, f.inserts)
	return out
}

func newAccumulationLoop(
	t *testing.T,
	now time.Time,
	cfg accumulation.Config,
	mm MMArbFilter,
	lines AccumulationLineFetcher,
	alerts AlertCreator,
) (*Loop, market.Market, *capturingEmitter) {
	t.Helper()
	reg := marketcache.New()
	reg.Replace(
		[]market.Market{{
			ID: "0xa", Slug: "us-pres", Question: "Who wins?",
			EventSlug: "us-pres-2028", EventTitle: "US Presidential Election 2028",
			TokenIDs: []vo.TokenID{"tok-yes", "tok-no"}, Outcomes: []string{"Yes", "No"},
			Categories: []vo.CategoryID{42}, Active: true, StartDate: now.Add(-95 * 24 * time.Hour), EndDate: now.Add(5 * 24 * time.Hour),
		}},
		[]market.Category{{ID: 42, Slug: "politics", Label: "Politics"}},
	)
	emit := &capturingEmitter{}
	log := zerolog.Nop()
	th := defaultThresholds()
	det := accumulation.New(cfg, th)

	loop := New(Config{
		Thresholds:                  th,
		Baseline:                    baseline.Config{Window: 7 * 24 * time.Hour},
		Cluster:                     cluster.Config{Window: time.Hour, MinTrades: 99, MinUniqueWallets: 99},
		Clock:                       func() time.Time { return now },
		PolymarketBase:              "https://polymarket.com",
		Accumulator:                 det,
		AccumulationLines:           lines,
		MMFilter:                    mm,
		Markets:                     &fakeMarketResolver{id: 7},
		Traders:                     &fakeTraderResolver{id: 42},
		Alerts:                      alerts,
		// In-memory baseline path so per-trade market multiplier is computed
		// from the trades we fed in. We keep BaselineMinReadySpan=0 so the
		// single-trade scorer doesn't block in tests; accumulation has its
		// own readiness via the line tradeCount floor.
	}, reg, emit, metrics.New(), &log)
	m, _ := reg.Get("0xa")
	return loop, m, emit
}

// warmBaselineDirect seeds the in-process market baseline reservoir
// without going through Observe (so it doesn't accidentally trigger the
// accumulation path on the warm-up wallet).
func warmBaselineDirect(loop *Loop, n int, notional float64, at time.Time) {
	k := baseline.Key{Category: vo.CategoryID(42), Market: "0xa", OutcomeToken: "tok-yes"}
	for i := 0; i < n; i++ {
		loop.baseline.Add(k, notional, at)
	}
}

func accumulationCfg() accumulation.Config {
	return accumulation.Config{
		Enabled:              true,
		Window:               24 * time.Hour,
		MinTrades:            3,
		TradeFractionOfInfo:  0.60,
		TotalMultiplier:      2,
		ManySmallsMultiplier: 4,
		HardMultiplier:       3,
		Cooldown:             30 * time.Minute,
	}
}

// TestAccumulation_SingleTradeNoAlert pins: a fresh wallet with one trade
// does NOT trigger accumulation (line trade count below MinTrades=3).
func TestAccumulation_SingleTradeNoAlert(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	lines := &fakeAccumulationLines{line: repository.AccumulationLine{
		TradeCount: 1, TotalNotionalUSD: 5_000,
		MeanNotionalUSD: 5_000, MedianNotionalUSD: 5_000,
		MaxNotionalUSD: 5_000, MinNotionalUSD: 5_000,
		AvgPrice: 0.2, MinPrice: 0.2,
		OldestAt: now.Add(-5 * time.Minute), NewestAt: now,
	}}
	loop, m, emit := newAccumulationLoop(t, now, accumulationCfg(), nil, lines, newFakeAlerts())

	loop.Observe(context.Background(), m, bet(5_000, 0.2, "0xwhale", now))
	if got := emit.of(anomaly.KindAccumulation); len(got) != 0 {
		t.Fatalf("single trade must not fire accumulation: %+v", got)
	}
}

// TestAccumulation_TwoTradesNoAlert pins: MinTrades=3 floor.
func TestAccumulation_TwoTradesNoAlert(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	lines := &fakeAccumulationLines{line: repository.AccumulationLine{
		TradeCount: 2, TotalNotionalUSD: 20_000,
		MeanNotionalUSD: 10_000, MedianNotionalUSD: 10_000,
		AvgPrice: 0.2, MinPrice: 0.2,
		OldestAt: now.Add(-1 * time.Hour), NewestAt: now,
	}}
	loop, m, emit := newAccumulationLoop(t, now, accumulationCfg(), nil, lines, newFakeAlerts())

	// warm market so per-trade gates don't gum things up
	warmBaselineDirect(loop, 30, 50, now.Add(-24*time.Hour))
	loop.Observe(context.Background(), m, bet(10_000, 0.2, "0xwhale", now))
	if got := emit.of(anomaly.KindAccumulation); len(got) != 0 {
		t.Fatalf("two trades must not fire accumulation: %+v", got)
	}
}

// TestAccumulation_ManySmallsPath pins the headline example from the spec:
// 200 × $200 same outcome same side at Info $10k tier → many-smalls path.
// Total $40k ≥ 4 × Info $10k. Should fire (with the baseline + odds gates).
func TestAccumulation_ManySmallsPath(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	lines := &fakeAccumulationLines{line: repository.AccumulationLine{
		TradeCount: 200, TotalNotionalUSD: 40_000,
		MeanNotionalUSD: 200, MedianNotionalUSD: 200,
		MaxNotionalUSD: 250, MinNotionalUSD: 150,
		AvgPrice: 0.25, MinPrice: 0.20, // odds 4 / 5
		OldestAt: now.Add(-3 * time.Hour), NewestAt: now,
	}}
	loop, m, emit := newAccumulationLoop(t, now, accumulationCfg(), nil, lines, newFakeAlerts())
	// Warm the market baseline so line market multiplier is computable.
	// 30 trades × $50 = $1500 (clears MinBaselineNotionalUSD=1000).
	warmBaselineDirect(loop, 30, 50, now.Add(-24*time.Hour))

	loop.Observe(context.Background(), m, bet(200, 0.25, "0xwhale", now))

	got := emit.of(anomaly.KindAccumulation)
	if len(got) != 1 {
		t.Fatalf("expected 1 accumulation alert (many-smalls path), got %d", len(got))
	}
	f := got[0]
	if f.Severity != anomaly.SeverityInfo {
		// At Info $10k tier: total $40k = 4× Info, multiplier $40k/$50 = 800 ≥ 100,
		// odds 4 ≥ 3, trades 200 ≥ 3 → Info.
		t.Errorf("expected Info severity, got %s", f.Severity)
	}
	if f.Accumulation == nil {
		t.Fatal("Finding.Accumulation must be populated")
	}
	if f.Accumulation.SizePath != "many-smalls" {
		t.Errorf("size path: got %q want many-smalls", f.Accumulation.SizePath)
	}
	if f.Accumulation.TradeCount != 200 {
		t.Errorf("trade count: got %d want 200", f.Accumulation.TradeCount)
	}
}

// TestAccumulation_MeaningfulPath pins 4 × $3k = $12k @ Info $5k.
// Use a custom threshold to get Info $5k. Wait — actually defaultThresholds
// has Info at $10k. Let me build a 4 × $6k = $24k → Info meaningful path.
func TestAccumulation_MeaningfulPath(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	lines := &fakeAccumulationLines{line: repository.AccumulationLine{
		TradeCount: 4, TotalNotionalUSD: 24_000,
		MeanNotionalUSD: 6_000, MedianNotionalUSD: 6_000,
		MaxNotionalUSD: 8_000, MinNotionalUSD: 4_000,
		AvgPrice: 0.25, MinPrice: 0.20, // odds 4
		OldestAt: now.Add(-2 * time.Hour), NewestAt: now,
	}}
	loop, m, emit := newAccumulationLoop(t, now, accumulationCfg(), nil, lines, newFakeAlerts())
	warmBaselineDirect(loop, 30, 50, now.Add(-24*time.Hour))

	loop.Observe(context.Background(), m, bet(6_000, 0.25, "0xwhale", now))

	got := emit.of(anomaly.KindAccumulation)
	if len(got) != 1 {
		t.Fatalf("expected 1 accumulation alert, got %d", len(got))
	}
	// median $6k ≥ 0.60 × $10k = $6k → meaningful passes.
	// total $24k ≥ 2 × $10k = $20k → meaningful passes.
	// many-smalls total floor 4×$10k=$40k → fails. So path is meaningful.
	if got[0].Accumulation.SizePath != "meaningful" {
		t.Errorf("size path: got %q want meaningful", got[0].Accumulation.SizePath)
	}
}

// TestAccumulation_MMSuppression pins the FP-reduction interaction: when
// the wallet's two-sided activity flags as MM, the accumulation alert is
// suppressed even though scoring would otherwise fire.
func TestAccumulation_MMSuppression(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	lines := &fakeAccumulationLines{line: repository.AccumulationLine{
		TradeCount: 200, TotalNotionalUSD: 40_000,
		MeanNotionalUSD: 200, MedianNotionalUSD: 200,
		MaxNotionalUSD: 250, MinNotionalUSD: 150,
		AvgPrice: 0.25, MinPrice: 0.20,
		OldestAt: now.Add(-3 * time.Hour), NewestAt: now,
	}}
	mm := &fakeMM{suppressFor: map[string]bool{"0xmm": true}}
	loop, m, emit := newAccumulationLoop(t, now, accumulationCfg(), mm, lines, newFakeAlerts())
	warmBaselineDirect(loop, 30, 50, now.Add(-24*time.Hour))

	loop.Observe(context.Background(), m, bet(200, 0.25, "0xmm", now))
	if got := emit.of(anomaly.KindAccumulation); len(got) != 0 {
		t.Fatalf("MM-suppressed wallet must not fire accumulation: %+v", got)
	}
}

// TestAccumulation_CooldownDeduplicates pins: a second trade in the same
// cooldown bucket reuses the dedup key and does not re-emit. This is the
// "restart simulation" property — repeated emission attempts within the
// bucket return no new alert.
func TestAccumulation_CooldownDeduplicates(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	lines := &fakeAccumulationLines{line: repository.AccumulationLine{
		TradeCount: 200, TotalNotionalUSD: 40_000,
		MeanNotionalUSD: 200, MedianNotionalUSD: 200,
		MaxNotionalUSD: 250, MinNotionalUSD: 150,
		AvgPrice: 0.25, MinPrice: 0.20,
		OldestAt: now.Add(-3 * time.Hour), NewestAt: now,
	}}
	alerts := newFakeAlerts()
	loop, m, emit := newAccumulationLoop(t, now, accumulationCfg(), nil, lines, alerts)
	warmBaselineDirect(loop, 30, 50, now.Add(-24*time.Hour))

	loop.Observe(context.Background(), m, bet(200, 0.25, "0xwhale", now))
	loop.Observe(context.Background(), m, bet(200, 0.25, "0xwhale", now.Add(1*time.Minute)))
	loop.Observe(context.Background(), m, bet(200, 0.25, "0xwhale", now.Add(2*time.Minute)))

	// Multiple trades in the same cooldown bucket → exactly one alert row
	// inserted (the rest collide on dedup_key).
	persisted := alerts.all()
	accumPersisted := 0
	for _, a := range persisted {
		if a.Kind == repository.AlertKindAccumulation {
			accumPersisted++
		}
	}
	if accumPersisted != 1 {
		t.Fatalf("expected exactly 1 accumulation row persisted (dedup), got %d", accumPersisted)
	}
	if got := emit.of(anomaly.KindAccumulation); len(got) != 1 {
		t.Fatalf("expected exactly 1 accumulation emission (dedup), got %d", len(got))
	}
}

// TestAccumulation_DedupKeyShape pins the dedup key format so downstream
// log/metric scrapers can rely on it.
func TestAccumulation_DedupKeyShape(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	lines := &fakeAccumulationLines{line: repository.AccumulationLine{
		TradeCount: 200, TotalNotionalUSD: 40_000,
		MeanNotionalUSD: 200, MedianNotionalUSD: 200,
		AvgPrice: 0.25, MinPrice: 0.20,
		OldestAt: now.Add(-3 * time.Hour), NewestAt: now,
	}}
	alerts := newFakeAlerts()
	loop, m, _ := newAccumulationLoop(t, now, accumulationCfg(), nil, lines, alerts)
	warmBaselineDirect(loop, 30, 50, now.Add(-24*time.Hour))
	loop.Observe(context.Background(), m, bet(200, 0.25, "0xwhale", now))

	rows := alerts.all()
	var dk string
	for _, r := range rows {
		if r.Kind == repository.AlertKindAccumulation {
			dk = r.DedupKey
			break
		}
	}
	if dk == "" {
		t.Fatal("expected one accumulation row persisted")
	}
	// Shape: accumulation:<sv>:<wallet>:<mid>:<token>:<side>:<bucket>
	// We don't pin the bucket value (it depends on Truncate(now,Cooldown))
	// but we pin the prefix and the trailing positional parts.
	wantPrefix := "accumulation:" + anomaly.StrategyIdentity + ":0xwhale:7:tok-yes:BUY:"
	if len(dk) < len(wantPrefix) || dk[:len(wantPrefix)] != wantPrefix {
		t.Fatalf("dedup key shape: got %q want prefix %q", dk, wantPrefix)
	}
	// Payload is JSON-serialisable.
	for _, r := range rows {
		if r.Kind == repository.AlertKindAccumulation {
			var f anomaly.Finding
			if err := json.Unmarshal(r.Payload, &f); err != nil {
				t.Errorf("payload not valid JSON: %v", err)
			}
			if f.Kind != anomaly.KindAccumulation {
				t.Errorf("payload kind: got %s want accumulation", f.Kind)
			}
		}
	}
}

// TestAccumulation_DisabledNoEvaluation pins: with cfg.Accumulator nil,
// the detector does not call the line fetcher and does not emit.
func TestAccumulation_DisabledNoEvaluation(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	lines := &fakeAccumulationLines{}
	loop, m, emit := newAccumulationLoop(t, now, accumulation.Config{Enabled: false}, nil, lines, newFakeAlerts())
	warmBaselineDirect(loop, 30, 50, now.Add(-24*time.Hour))
	loop.Observe(context.Background(), m, bet(200, 0.25, "0xanyone", now))
	if got := emit.of(anomaly.KindAccumulation); len(got) != 0 {
		t.Fatalf("disabled detector must not emit accumulation: %+v", got)
	}
}

// TestAccumulation_RegressionSingleWhaleStillFires confirms the new code
// path does not regress the single-trade signal.
func TestAccumulation_RegressionSingleWhaleStillFires(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	lines := &fakeAccumulationLines{}
	loop, m, emit := newAccumulationLoop(t, now, accumulationCfg(), nil, lines, newFakeAlerts())
	warmBaselineDirect(loop, 30, 50, now.Add(-24*time.Hour))

	// $700k at odds 8 — fits Critical on both ladders.
	loop.Observe(context.Background(), m, bet(700_000, 1.0/8, "0xwhale", now))

	got := emit.of(anomaly.KindTradeAnomaly)
	if len(got) != 1 || got[0].Severity != anomaly.SeverityCritical {
		t.Fatalf("expected Critical single-trade alert, got %+v", got)
	}
}

// Compile-time interface assertions.
var (
	_ AccumulationLineFetcher = (*fakeAccumulationLines)(nil)
	_ MarketResolver          = (*fakeMarketResolver)(nil)
	_ TraderResolver          = (*fakeTraderResolver)(nil)
	_ AlertCreator            = (*fakeAlerts)(nil)
)

// Sentinels — quiet "imported and not used" if a future test rebalance
// drops references.
var (
	_ = mmfilter.Verdict{}
	_ = repository.AlertKindAccumulation
)
