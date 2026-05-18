package detect

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/analytics/accumulation"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/analytics/baseline"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/analytics/cluster"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/analytics/mmfilter"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/analytics/ownership"
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

// fakeOwnership returns a fixed Shares row regardless of the query.
type fakeOwnership struct{ shares repository.OwnershipShares }

func (f *fakeOwnership) OwnershipShares(_ context.Context, _ repository.OwnershipSharesQuery) (repository.OwnershipShares, error) {
	return f.shares, nil
}

// fakeMarkets resolves condition_id → repository.Market for the test.
type fakeMarketResolver struct {
	id int64
}

func (f *fakeMarketResolver) GetByConditionID(_ context.Context, _ string) (repository.Market, error) {
	return repository.Market{ID: f.id}, nil
}

// fakeTraderResolver resolves wallet → repository.Trader. The test uses
// id=42 for the canonical "0xwhale" wallet. FirstSeenAt defaults to the
// zero time; tests that exercise the new-wallet booster override it.
type fakeTraderResolver struct {
	id          int64
	firstSeenAt time.Time
}

func (f *fakeTraderResolver) GetByWallet(_ context.Context, _ string) (repository.Trader, error) {
	return repository.Trader{ID: f.id, FirstSeenAt: f.firstSeenAt}, nil
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
		Thresholds:        th,
		Baseline:          baseline.Config{Window: 7 * 24 * time.Hour},
		Cluster:           cluster.Config{Window: time.Hour, MinTrades: 99, MinUniqueWallets: 99},
		Clock:             func() time.Time { return now },
		PolymarketBase:    "https://polymarket.com",
		Accumulator:       det,
		AccumulationLines: lines,
		MMFilter:          mm,
		Markets:           &fakeMarketResolver{id: 7},
		Traders:           &fakeTraderResolver{id: 42},
		Alerts:            alerts,
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
	// Strategy-A: both recent and lifetime windows fire on this line
	// (identical math, identical line, two dedup namespaces).
	if len(got) != 2 {
		t.Fatalf("expected 2 accumulation alerts (recent + lifetime), got %d", len(got))
	}
	// Both Findings have the same gating outcome; assert on the first.
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
	// Strategy-A change: each Observe call evaluates BOTH the recent
	// window and the lifetime window. A line that satisfies both
	// horizons fires twice — once per window. Older expectation of
	// "exactly 1" was the pre-A single-window behaviour.
	if len(got) != 2 {
		t.Fatalf("expected 2 accumulation alerts (recent + lifetime), got %d", len(got))
	}
	// median $6k ≥ 0.60 × $10k = $6k → meaningful passes.
	// total $24k ≥ 2 × $10k = $20k → meaningful passes.
	// many-smalls total floor 4×$10k=$40k → fails. So path is meaningful.
	if got[0].Accumulation.SizePath != "meaningful" {
		t.Errorf("size path: got %q want meaningful", got[0].Accumulation.SizePath)
	}
	seenWindows := map[string]bool{}
	for _, g := range got {
		seenWindows[g.Accumulation.Window] = true
	}
	if !seenWindows["recent"] || !seenWindows["lifetime"] {
		t.Errorf("expected one recent + one lifetime alert; got windows=%v", seenWindows)
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

	// Strategy-A change: each Observe evaluates BOTH windows, so the
	// post-dedup total per line is exactly ONE recent row (cooldown
	// bucket dedup) + ONE lifetime row at the firing severity (per-tier
	// dedup keeps it from re-emitting at the same tier).
	persisted := alerts.all()
	recent, lifetime := 0, 0
	for _, a := range persisted {
		if a.Kind != repository.AlertKindAccumulation {
			continue
		}
		switch {
		case strings.Contains(a.DedupKey, ":recent:"):
			recent++
		case strings.Contains(a.DedupKey, ":lifetime:"):
			lifetime++
		}
	}
	if recent != 1 {
		t.Fatalf("expected exactly 1 recent accumulation row (cooldown dedup), got %d", recent)
	}
	if lifetime != 1 {
		t.Fatalf("expected exactly 1 lifetime accumulation row (per-tier dedup), got %d", lifetime)
	}
	if got := emit.of(anomaly.KindAccumulation); len(got) != 2 {
		t.Fatalf("expected exactly 2 accumulation emissions (1 recent + 1 lifetime), got %d", len(got))
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
	var recentKey, lifetimeKey string
	for _, r := range rows {
		if r.Kind != repository.AlertKindAccumulation {
			continue
		}
		switch {
		case strings.Contains(r.DedupKey, ":recent:"):
			recentKey = r.DedupKey
		case strings.Contains(r.DedupKey, ":lifetime:"):
			lifetimeKey = r.DedupKey
		}
	}
	if recentKey == "" || lifetimeKey == "" {
		t.Fatalf("expected one recent + one lifetime accumulation row; recent=%q lifetime=%q", recentKey, lifetimeKey)
	}
	// Strategy-A dedup-key shapes:
	//   recent   : accumulation:<sv>:recent:<wallet>:<mid>:<token>:<side>:<bucket>
	//   lifetime : accumulation:<sv>:lifetime:<wallet>:<mid>:<token>:<side>:<severity>
	// We don't pin the bucket value (depends on Truncate(now,Cooldown))
	// or the exact severity tier — just the positional shape.
	wantRecentPrefix := "accumulation:" + anomaly.StrategyIdentity + ":recent:0xwhale:7:tok-yes:BUY:"
	wantLifetimePrefix := "accumulation:" + anomaly.StrategyIdentity + ":lifetime:0xwhale:7:tok-yes:BUY:"
	if len(recentKey) < len(wantRecentPrefix) || recentKey[:len(wantRecentPrefix)] != wantRecentPrefix {
		t.Fatalf("recent dedup shape: got %q want prefix %q", recentKey, wantRecentPrefix)
	}
	if len(lifetimeKey) < len(wantLifetimePrefix) || lifetimeKey[:len(wantLifetimePrefix)] != wantLifetimePrefix {
		t.Fatalf("lifetime dedup shape: got %q want prefix %q", lifetimeKey, wantLifetimePrefix)
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

// TestAccumulation_LifetimeBothWindowsFire is the headline Strategy-A
// regression: a single Observe call evaluates BOTH windows and emits a
// recent + lifetime Finding for a line that clears both. The recent
// Finding carries Window="recent", the lifetime Finding carries
// Window="lifetime"; dedup keys reflect the namespaces.
func TestAccumulation_LifetimeBothWindowsFire(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	// Mock the SQL fake to return the SAME line shape for either Since
	// (the test isn't exercising the SQL — it's exercising the
	// detector's dual-emission contract).
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

	got := emit.of(anomaly.KindAccumulation)
	if len(got) != 2 {
		t.Fatalf("expected 2 alerts (recent+lifetime), got %d", len(got))
	}
	seen := map[string]bool{}
	for _, g := range got {
		if g.Accumulation == nil || g.Accumulation.Window == "" {
			t.Errorf("AccumulationRef.Window must be populated: %+v", g)
			continue
		}
		seen[g.Accumulation.Window] = true
	}
	if !seen["recent"] || !seen["lifetime"] {
		t.Fatalf("missing window in emissions: %v", seen)
	}
	// Reason codes: recent finding carries RECENT_ACCUMULATION,
	// lifetime finding carries LIFETIME_ACCUMULATION.
	for _, g := range got {
		want := "LIFETIME_ACCUMULATION"
		if g.Accumulation.Window == "recent" {
			want = "RECENT_ACCUMULATION"
		}
		if !contains(g.Reasons, want) {
			t.Errorf("Finding(window=%s).Reasons missing %s: %v", g.Accumulation.Window, want, g.Reasons)
		}
	}
}

// TestAccumulation_LifetimeDedupesAtSameTier confirms the lifetime
// dedup-by-severity rule: a line that fires at Info on tick 1 must NOT
// re-fire at Info on tick 2 (same severity tier dedupes), even though
// the line state has not changed. Recent has its own cooldown-bucket
// dedup so we ignore those rows here.
func TestAccumulation_LifetimeDedupesAtSameTier(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	lines := &fakeAccumulationLines{line: repository.AccumulationLine{
		TradeCount: 200, TotalNotionalUSD: 40_000,
		MeanNotionalUSD: 200, MedianNotionalUSD: 200,
		MaxNotionalUSD: 250, MinNotionalUSD: 150,
		AvgPrice: 0.25, MinPrice: 0.20,
		OldestAt: now.Add(-3 * time.Hour), NewestAt: now,
	}}
	alerts := newFakeAlerts()
	loop, m, _ := newAccumulationLoop(t, now, accumulationCfg(), nil, lines, alerts)
	warmBaselineDirect(loop, 30, 50, now.Add(-24*time.Hour))

	// Fire on three trades far apart — well beyond the recent cooldown
	// bucket so a new recent row is emitted each time, but lifetime
	// must collapse to a single Info row.
	loop.Observe(context.Background(), m, bet(200, 0.25, "0xwhale", now))
	loop.Observe(context.Background(), m, bet(200, 0.25, "0xwhale", now.Add(time.Hour)))
	loop.Observe(context.Background(), m, bet(200, 0.25, "0xwhale", now.Add(2*time.Hour)))

	lifetime := 0
	for _, a := range alerts.all() {
		if a.Kind == repository.AlertKindAccumulation && strings.Contains(a.DedupKey, ":lifetime:") {
			lifetime++
		}
	}
	if lifetime != 1 {
		t.Fatalf("lifetime per-tier dedup violated — expected exactly 1 Info-tier lifetime row, got %d", lifetime)
	}
}

// TestNewWallet_AccumulationCarriesContextReason pins Strategy-B: an
// accumulation alert fired by a wallet first seen recently (or with
// very thin history) gets NEW_WALLET_ACCUMULATION attached to its
// Reasons, and the Finding.NewWallet ref is populated.
func TestNewWallet_AccumulationCarriesContextReason(t *testing.T) {
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
	// Enable the new-wallet booster with a generous age window. The
	// fake trader resolver returns FirstSeenAt 12 hours ago — well
	// under the 168h max age.
	loop.cfg.NewWallet = NewWalletConfig{Enabled: true, MaxAge: 168 * time.Hour, MaxHistoryTrades: 10}
	loop.cfg.Traders = &fakeTraderResolver{id: 42, firstSeenAt: now.Add(-12 * time.Hour)}

	loop.Observe(context.Background(), m, bet(200, 0.25, "0xfresh", now))

	got := emit.of(anomaly.KindAccumulation)
	if len(got) == 0 {
		t.Fatal("expected at least one accumulation alert")
	}
	for _, f := range got {
		if f.NewWallet == nil {
			t.Errorf("Finding(window=%s).NewWallet must be populated", f.Accumulation.Window)
		}
		if !contains(f.Reasons, anomaly.ReasonNewWalletAccumulation) {
			t.Errorf("Finding(window=%s).Reasons missing NEW_WALLET_ACCUMULATION: %v",
				f.Accumulation.Window, f.Reasons)
		}
	}
}

// TestNewWallet_OldWalletGetsNoBoost confirms the booster does NOT
// attach to wallets older than the configured age AND with substantial
// history. The Finding fires (accumulation math is identical) but
// NewWallet stays nil and the context reason is absent.
func TestNewWallet_OldWalletGetsNoBoost(t *testing.T) {
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
	loop.cfg.NewWallet = NewWalletConfig{Enabled: true, MaxAge: 168 * time.Hour, MaxHistoryTrades: 10}
	// FirstSeenAt 2 years ago: well past 168h age cutoff. And the
	// detector reads historyTrades from traderStats; with no
	// TraderBaseliner wired it's 0 — but 0 ≤ 10 trips the history
	// gate. To exercise the *negative* path we must set
	// MaxHistoryTrades=0 so the history leg is disabled.
	loop.cfg.NewWallet.MaxHistoryTrades = 0
	loop.cfg.Traders = &fakeTraderResolver{id: 42, firstSeenAt: now.Add(-2 * 365 * 24 * time.Hour)}

	loop.Observe(context.Background(), m, bet(200, 0.25, "0xveteran", now))

	got := emit.of(anomaly.KindAccumulation)
	if len(got) == 0 {
		t.Fatal("expected accumulation alert to fire (booster is context-only, never blocks)")
	}
	for _, f := range got {
		if f.NewWallet != nil {
			t.Errorf("Finding(window=%s).NewWallet must be nil for old wallet, got %+v",
				f.Accumulation.Window, f.NewWallet)
		}
		if contains(f.Reasons, anomaly.ReasonNewWalletAccumulation) {
			t.Errorf("Finding(window=%s).Reasons must NOT include NEW_WALLET_ACCUMULATION: %v",
				f.Accumulation.Window, f.Reasons)
		}
	}
}

// TestOwnership_FiresWhenAccumulationFiresAndShareIsHigh pins the
// Strategy-E harmonization rule: an ownership_concentration Finding is
// emitted ALONGSIDE the accumulation Finding when (a) accumulation
// fired AND (b) the wallet's trade-flow share crossed an ownership
// tier AND (c) the absolute-position notional clears the floor.
func TestOwnership_FiresWhenAccumulationFiresAndShareIsHigh(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	lines := &fakeAccumulationLines{line: repository.AccumulationLine{
		TradeCount: 200, TotalNotionalUSD: 40_000,
		MeanNotionalUSD: 200, MedianNotionalUSD: 200,
		MaxNotionalUSD: 250, MinNotionalUSD: 150,
		AvgPrice: 0.25, MinPrice: 0.20,
		OldestAt: now.Add(-3 * time.Hour), NewestAt: now,
	}}
	// 30% of a 100k-share market at $0.25 → 30k shares × $0.25 = $7.5k
	// position. The default MinNotionalUSD=10k blocks fire — bump the
	// position to clear it: 60% (60k shares × $0.25 = $15k).
	own := &fakeOwnership{shares: repository.OwnershipShares{
		WalletBuyShares:  60_000,
		WalletSellShares: 0,
		MarketBuyShares:  100_000,
	}}
	alerts := newFakeAlerts()
	loop, m, emit := newAccumulationLoop(t, now, accumulationCfg(), nil, lines, alerts)
	warmBaselineDirect(loop, 30, 50, now.Add(-24*time.Hour))
	loop.cfg.Ownership = ownership.New(ownership.Config{
		Enabled: true, InfoPct: 10, WarningPct: 15, CriticalPct: 25, MinNotionalUSD: 10_000,
	})
	loop.cfg.OwnershipShares = own

	loop.Observe(context.Background(), m, bet(200, 0.25, "0xwhale", now))

	ownFindings := emit.of(anomaly.KindOwnership)
	if len(ownFindings) != 1 {
		t.Fatalf("expected exactly 1 ownership_concentration alert, got %d", len(ownFindings))
	}
	f := ownFindings[0]
	if f.Severity != anomaly.SeverityCritical {
		t.Errorf("60%% share should be Critical, got %s", f.Severity)
	}
	if f.Ownership == nil {
		t.Fatal("Finding.Ownership must be populated on ownership_concentration alert")
	}
	if !f.Ownership.Approximate {
		t.Error("Ownership.Approximate must be true (no holders endpoint wired)")
	}
	if !contains(f.Reasons, anomaly.ReasonMarketOwnershipConcentration) ||
		!contains(f.Reasons, anomaly.ReasonWalletDominatesOutcome) {
		t.Errorf("Critical ownership Finding must carry both reason codes, got %v", f.Reasons)
	}
}

// TestOwnership_NoFireWhenBelowFloor confirms the absolute-position
// floor: even when accumulation fires and the percentage is high, a
// tiny position never emits ownership_concentration.
func TestOwnership_NoFireWhenBelowFloor(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	lines := &fakeAccumulationLines{line: repository.AccumulationLine{
		TradeCount: 200, TotalNotionalUSD: 40_000,
		MeanNotionalUSD: 200, MedianNotionalUSD: 200,
		MaxNotionalUSD: 250, MinNotionalUSD: 150,
		AvgPrice: 0.25, MinPrice: 0.20,
		OldestAt: now.Add(-3 * time.Hour), NewestAt: now,
	}}
	// "Owns 50%" of a tiny outcome: 50 shares × $0.25 = $12.50 notional.
	own := &fakeOwnership{shares: repository.OwnershipShares{
		WalletBuyShares:  50,
		WalletSellShares: 0,
		MarketBuyShares:  100,
	}}
	alerts := newFakeAlerts()
	loop, m, emit := newAccumulationLoop(t, now, accumulationCfg(), nil, lines, alerts)
	warmBaselineDirect(loop, 30, 50, now.Add(-24*time.Hour))
	loop.cfg.Ownership = ownership.New(ownership.Config{
		Enabled: true, InfoPct: 10, WarningPct: 15, CriticalPct: 25, MinNotionalUSD: 10_000,
	})
	loop.cfg.OwnershipShares = own

	loop.Observe(context.Background(), m, bet(200, 0.25, "0xwhale", now))

	if got := emit.of(anomaly.KindOwnership); len(got) != 0 {
		t.Fatalf("ownership_concentration must not fire on dust positions: %+v", got)
	}
}

// TestOwnership_PerTierDedupes confirms a stable position at the same
// tier does NOT re-fire on repeated trades. Three identical Observe
// calls produce exactly one ownership row in the alerts table.
func TestOwnership_PerTierDedupes(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	lines := &fakeAccumulationLines{line: repository.AccumulationLine{
		TradeCount: 200, TotalNotionalUSD: 40_000,
		MeanNotionalUSD: 200, MedianNotionalUSD: 200,
		MaxNotionalUSD: 250, MinNotionalUSD: 150,
		AvgPrice: 0.25, MinPrice: 0.20,
		OldestAt: now.Add(-3 * time.Hour), NewestAt: now,
	}}
	own := &fakeOwnership{shares: repository.OwnershipShares{
		WalletBuyShares:  60_000,
		WalletSellShares: 0,
		MarketBuyShares:  100_000,
	}}
	alerts := newFakeAlerts()
	loop, m, _ := newAccumulationLoop(t, now, accumulationCfg(), nil, lines, alerts)
	warmBaselineDirect(loop, 30, 50, now.Add(-24*time.Hour))
	loop.cfg.Ownership = ownership.New(ownership.Config{
		Enabled: true, InfoPct: 10, WarningPct: 15, CriticalPct: 25, MinNotionalUSD: 10_000,
	})
	loop.cfg.OwnershipShares = own

	loop.Observe(context.Background(), m, bet(200, 0.25, "0xwhale", now))
	loop.Observe(context.Background(), m, bet(200, 0.25, "0xwhale", now.Add(time.Hour)))
	loop.Observe(context.Background(), m, bet(200, 0.25, "0xwhale", now.Add(2*time.Hour)))

	ownershipRows := 0
	for _, a := range alerts.all() {
		if a.Kind == repository.AlertKindOwnership {
			ownershipRows++
		}
	}
	if ownershipRows != 1 {
		t.Fatalf("expected exactly 1 ownership_concentration row (per-tier dedup), got %d", ownershipRows)
	}
}

// TestAccumulation_LifetimeUpgradeEmitsAtHigherTier confirms the
// severity-upgrade emission: a line that initially clears Info, then
// later grows to clear Warning, must emit a SECOND lifetime row at the
// higher tier (different dedup key because the key embeds severity).
func TestAccumulation_LifetimeUpgradeEmitsAtHigherTier(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	// Tick 1: Info-sized line.
	lines := &fakeAccumulationLines{line: repository.AccumulationLine{
		TradeCount: 200, TotalNotionalUSD: 40_000,
		MeanNotionalUSD: 200, MedianNotionalUSD: 200,
		MaxNotionalUSD: 250, MinNotionalUSD: 150,
		AvgPrice: 0.25, MinPrice: 0.20,
		OldestAt: now.Add(-30 * 24 * time.Hour), NewestAt: now,
	}}
	alerts := newFakeAlerts()
	loop, m, _ := newAccumulationLoop(t, now, accumulationCfg(), nil, lines, alerts)
	warmBaselineDirect(loop, 30, 50, now.Add(-24*time.Hour))
	loop.Observe(context.Background(), m, bet(200, 0.25, "0xwhale", now))

	// Tick 2: same line grown to Warning-sized via the many-smalls path
	// (Warning many-smalls floor = 4 × $25k = $100k). 600 × $200 = $120k
	// clears it; multiplier $120k / $50 baseline median = 2400× ≥ Warning
	// MinMultiplier (defaultThresholds Warning=1000×).
	lines.set(repository.AccumulationLine{
		TradeCount: 600, TotalNotionalUSD: 120_000,
		MeanNotionalUSD: 200, MedianNotionalUSD: 200,
		MaxNotionalUSD: 300, MinNotionalUSD: 150,
		AvgPrice: 0.20, MinPrice: 0.15, // avg odds 5.0 ≥ Warning MinOdds=5
		OldestAt: now.Add(-30 * 24 * time.Hour), NewestAt: now.Add(time.Hour),
	})
	loop.Observe(context.Background(), m, bet(200, 0.20, "0xwhale", now.Add(time.Hour)))

	severities := map[anomaly.Severity]int{}
	for _, a := range alerts.all() {
		if a.Kind == repository.AlertKindAccumulation && strings.Contains(a.DedupKey, ":lifetime:") {
			severities[anomaly.Severity(a.Severity)]++
		}
	}
	if severities[anomaly.SeverityInfo] != 1 {
		t.Errorf("expected exactly 1 lifetime Info-tier row, got %d (all=%v)", severities[anomaly.SeverityInfo], severities)
	}
	if severities[anomaly.SeverityWarning] != 1 {
		t.Errorf("expected exactly 1 lifetime Warning-tier row after upgrade, got %d (all=%v)", severities[anomaly.SeverityWarning], severities)
	}
}
