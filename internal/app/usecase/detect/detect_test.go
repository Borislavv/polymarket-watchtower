package detect

import (
	"context"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/analytics/baseline"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/analytics/cluster"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/category"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/marketcache"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/anomaly"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/market"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/trade"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/vo"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/metrics"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog"
)

type capturingEmitter struct {
	mu sync.Mutex
	fs []anomaly.Finding
}

func (e *capturingEmitter) Notify(_ context.Context, f anomaly.Finding) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.fs = append(e.fs, f)
	return nil
}
func (e *capturingEmitter) all() []anomaly.Finding {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]anomaly.Finding, len(e.fs))
	copy(out, e.fs)
	return out
}
func (e *capturingEmitter) of(k anomaly.Kind) []anomaly.Finding {
	var out []anomaly.Finding
	for _, f := range e.all() {
		if f.Kind == k {
			out = append(out, f)
		}
	}
	return out
}

func defaultThresholds() anomaly.Thresholds {
	return anomaly.Thresholds{
		Info:                   anomaly.Tier{MinNotionalUSD: 10_000, MinOdds: 3, MinMultiplier: 100},
		Warning:                anomaly.Tier{MinNotionalUSD: 25_000, MinOdds: 5, MinMultiplier: 1_000},
		Critical:               anomaly.Tier{MinNotionalUSD: 100_000, MinOdds: 8, MinMultiplier: 10_000},
		MinBaselineTrades:      20,
		MinBaselineNotionalUSD: 1_000,
	}
}

func newLoop(t *testing.T, now time.Time, th anomaly.Thresholds, cl cluster.Config) (*Loop, *marketcache.Cache, *capturingEmitter) {
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
	loop := New(Config{
		Thresholds:     th,
		Baseline:       baseline.Config{Window: 7 * 24 * time.Hour},
		Cluster:        cl,
		Clock:          func() time.Time { return now },
		PolymarketBase: "https://polymarket.com",
		GrafanaBase:    "http://grafana.local",
		GrafanaDashUID: "uid123",
		GrafanaContext: time.Hour,
	}, reg, emit, metrics.New(), &log)
	return loop, reg, emit
}

func bet(notional, price float64, wallet string, at time.Time) trade.Trade {
	// notional = size * price => size = notional / price
	return trade.Trade{
		ID:        "id-" + wallet,
		Market:    "0xa",
		Token:     "tok-yes",
		Side:      trade.SideBuy,
		Size:      notional / price,
		Price:     price,
		Timestamp: at,
		Taker:     wallet,
	}
}

// warm seeds the baseline with `n` trades of `notional` USD each.
func warm(loop *Loop, m market.Market, n int, notional, price float64, at time.Time) {
	for i := 0; i < n; i++ {
		loop.Observe(context.Background(), m, bet(notional, price, "wb", at))
	}
}

func TestNoAlertBelowAbsoluteFloor(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	loop, reg, emit := newLoop(t, now, defaultThresholds(), cluster.Config{Window: time.Hour, MinTrades: 99, MinUniqueWallets: 99})
	m, _ := reg.Get("0xa")
	warm(loop, m, 30, 100, 0.5, now)
	// $5k at odds 10 — strong rarity but absolute < info.
	loop.Observe(context.Background(), m, bet(5_000, 0.1, "small", now))
	if got := emit.all(); len(got) != 0 {
		t.Fatalf("expected no fire, got %d: %+v", len(got), got)
	}
}

func TestNoAlertBelowMultiplierFloor(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	loop, reg, emit := newLoop(t, now, defaultThresholds(), cluster.Config{Window: time.Hour, MinTrades: 99, MinUniqueWallets: 99})
	m, _ := reg.Get("0xa")
	warm(loop, m, 30, 500, 0.5, now)
	// $10k at odds 3 absolute=info, but multiplier = 10000/500=20 → below 100.
	loop.Observe(context.Background(), m, bet(10_000, 1.0/3, "bigfish", now))
	if got := emit.all(); len(got) != 0 {
		t.Fatalf("expected no fire, got %+v", got)
	}
}

func TestInfoAlertFires(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	loop, reg, emit := newLoop(t, now, defaultThresholds(), cluster.Config{Window: time.Hour, MinTrades: 99, MinUniqueWallets: 99})
	m, _ := reg.Get("0xa")
	warm(loop, m, 30, 60, 0.5, now)
	// $10k at odds 3, multiplier = 10000/60 ≈ 166 → info on both.
	loop.Observe(context.Background(), m, bet(10_000, 1.0/3, "wA", now))
	got := emit.of(anomaly.KindTradeAnomaly)
	if len(got) != 1 || got[0].Severity != anomaly.SeverityInfo {
		t.Fatalf("expected info, got %+v", got)
	}
	if got[0].Reason != anomaly.ReasonSingle {
		t.Fatalf("reason: %s", got[0].Reason)
	}
}

func TestWarningRequiresBoth(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	loop, reg, emit := newLoop(t, now, defaultThresholds(), cluster.Config{Window: time.Hour, MinTrades: 99, MinUniqueWallets: 99})
	m, _ := reg.Get("0xa")
	warm(loop, m, 30, 60, 0.5, now)
	// $30k at odds 6, multiplier 30000/60=500 → absolute=warning, multiplier=info → conservative info.
	loop.Observe(context.Background(), m, bet(30_000, 1.0/6, "mid", now))
	got := emit.of(anomaly.KindTradeAnomaly)
	if len(got) != 1 || got[0].Severity != anomaly.SeverityInfo {
		t.Fatalf("expected conservative info, got %+v", got)
	}
}

func TestCriticalRequiresAllThree(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	loop, reg, emit := newLoop(t, now, defaultThresholds(), cluster.Config{Window: time.Hour, MinTrades: 99, MinUniqueWallets: 99})
	m, _ := reg.Get("0xa")
	// Warm with $60 trades (above the $50 baseline filter).
	warm(loop, m, 30, 60, 0.5, now)
	// $700k at odds 8 → multiplier 700000/60 ≈ 11666. With the new model
	// (single-trade caps at Critical), absolute=Critical (700k ≥ 100k ∧ odds
	// 8 ≥ 8) and multiplier=Critical (11666 ≥ 10000) → conservative-MIN = Critical.
	loop.Observe(context.Background(), m, bet(700_000, 1.0/8, "shark", now))
	got := emit.of(anomaly.KindTradeAnomaly)
	if len(got) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(got))
	}
	if got[0].Severity != anomaly.SeverityCritical {
		t.Fatalf("expected critical (cap for single trades), got %s", got[0].Severity)
	}
	// All payload fields populated for human review.
	tr := got[0].Trade
	if tr == nil || tr.NotionalUSD < 699_000 || tr.Odds < 7.99 {
		t.Fatalf("trade ref: %+v", tr)
	}
	if got[0].EffectiveMultiplier < 10_000 {
		t.Fatalf("multiplier: %v", got[0].EffectiveMultiplier)
	}
	if got[0].MarketURL != "https://polymarket.com/event/us-pres-2028" {
		t.Fatalf("market URL: %q", got[0].MarketURL)
	}
	if !strings.Contains(got[0].GrafanaURL, "var-severity=critical") {
		t.Fatalf("grafana URL missing severity var: %q", got[0].GrafanaURL)
	}
}

func TestInsufficientBaselineNoAlert(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	loop, reg, emit := newLoop(t, now, defaultThresholds(), cluster.Config{Window: time.Hour, MinTrades: 99, MinUniqueWallets: 99})
	m, _ := reg.Get("0xa")
	// only 5 baseline samples
	warm(loop, m, 5, 100, 0.5, now)
	loop.Observe(context.Background(), m, bet(100_000, 1.0/8, "shark", now))
	if got := emit.all(); len(got) != 0 {
		t.Fatalf("expected no fire on thin baseline, got %+v", got)
	}
}

func TestClusterHardAlert(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	loop, reg, emit := newLoop(t, now, defaultThresholds(), cluster.Config{
		Window: time.Hour, MinTrades: 3, MinUniqueWallets: 3, MinTotalUSD: 30_000,
	})
	m, _ := reg.Get("0xa")
	// Warm with $60 trades (above baseline filter).
	warm(loop, m, 30, 60, 0.5, now)
	for _, w := range []string{"shark-1", "shark-2", "shark-3"} {
		// Each $50k at odds 8 → absolute warning, multiplier ≈ 833 (info).
		// Conservative final = info. Three info fires meet the cluster floor.
		loop.Observe(context.Background(), m, bet(50_000, 1.0/8, w, now))
	}
	if n := len(emit.of(anomaly.KindTradeAnomaly)); n < 3 {
		t.Fatalf("expected ≥3 single-trade alerts, got %d", n)
	}
	hard := emit.of(anomaly.KindCategoryWatch)
	if len(hard) != 1 {
		t.Fatalf("expected 1 cluster alert, got %d", len(hard))
	}
	h := hard[0]
	if h.Severity != anomaly.SeverityHard || h.Reason != anomaly.ReasonCluster {
		t.Fatalf("cluster: severity=%s reason=%s", h.Severity, h.Reason)
	}
	if h.Cluster.UniqueWallets != 3 || h.Cluster.AnomalousTrades != 3 {
		t.Fatalf("cluster stats: %+v", h.Cluster)
	}
	if h.Cluster.TotalUSD < 149_000 || h.Cluster.TotalUSD > 151_000 {
		t.Fatalf("cluster TotalUSD: %v", h.Cluster.TotalUSD)
	}
}

// TestNonWhitelistedCategoryProducesNoAlert is the base case for the
// whitelist filter at the detect layer: a market whose category is not in
// the whitelist must produce zero findings, even on an obvious whale trade.
func TestNonWhitelistedCategoryProducesNoAlert(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	reg := marketcache.New()
	reg.Replace(
		[]market.Market{{
			ID: "0xb", Slug: "nba-finals", Question: "Who wins?",
			EventSlug: "2026-nba-finals", TokenIDs: []vo.TokenID{"tok"}, Outcomes: []string{"Yes"},
			Categories: []vo.CategoryID{77}, Active: true, StartDate: now.Add(-95 * 24 * time.Hour), EndDate: now.Add(5 * 24 * time.Hour),
		}},
		[]market.Category{{ID: 77, Slug: "nba", Label: "NBA"}},
	)
	emit := &capturingEmitter{}
	log := zerolog.Nop()
	loop := New(Config{
		Thresholds: defaultThresholds(),
		Baseline:   baseline.Config{Window: 7 * 24 * time.Hour},
		Cluster:    cluster.Config{Window: time.Hour, MinTrades: 1, MinUniqueWallets: 1},
		// Whitelist contains only Politics; the NBA market is not admitted.
		Filter: category.NewFilter([]string{"politics"}),
		Clock:  func() time.Time { return now },
	}, reg, emit, metrics.New(), &log)
	m, _ := reg.Get("0xb")
	loop.Observe(context.Background(), m, bet(100_000, 1.0/8, "shark", now))
	if got := emit.all(); len(got) != 0 {
		t.Fatalf("non-whitelisted category produced %d findings", len(got))
	}
}

func TestGrafanaURLIncludesSeverity(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	at := time.Date(2026, 5, 17, 14, 0, 0, 0, time.UTC)
	loop, _, _ := newLoop(t, now, defaultThresholds(), cluster.Config{Window: time.Hour, MinTrades: 99, MinUniqueWallets: 99})
	got := loop.grafanaURL(anomaly.CategoryRef{Label: "Politics"}, market.Market{Slug: "us-pres"}, at, anomaly.SeverityWarning)
	parsed, err := url.Parse(got)
	if err != nil {
		t.Fatalf("unparseable: %v", err)
	}
	q, _ := url.ParseQuery(parsed.RawQuery)
	if q.Get("var-severity") != "warning" {
		t.Fatalf("var-severity: %q", q.Get("var-severity"))
	}
}

func TestMarketURLUsesEventSlug(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	loop, _, _ := newLoop(t, now, defaultThresholds(), cluster.Config{Window: time.Hour, MinTrades: 99, MinUniqueWallets: 99})
	loop.cfg.PolymarketBase = "https://polymarket.com/"
	want := "https://polymarket.com/event/2026-fifa-world-cup-winner-595"
	got := loop.marketURL(market.Market{Slug: "will-tunisia-win-…", EventSlug: "2026-fifa-world-cup-winner-595"})
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestLifecycleGateSkipsEarlyMarkets(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	reg := marketcache.New()
	// Market spans 100 days; trade at day 50 = 50% elapsed; below 75% floor.
	start := now.Add(-50 * 24 * time.Hour)
	end := now.Add(50 * 24 * time.Hour)
	reg.Replace(
		[]market.Market{{
			ID: "0xa", Slug: "us-pres", Question: "Who wins?",
			EventSlug:  "us-pres-2028",
			TokenIDs:   []vo.TokenID{"tok-yes"},
			Outcomes:   []string{"Yes"},
			Categories: []vo.CategoryID{42},
			Active:     true,
			StartDate:  start,
			EndDate:    end,
		}},
		[]market.Category{{ID: 42, Slug: "politics", Label: "Politics"}},
	)
	emit := &capturingEmitter{}
	log := zerolog.Nop()
	loop := New(Config{
		Thresholds:            defaultThresholds(),
		Baseline:              baseline.Config{Window: 7 * 24 * time.Hour},
		Cluster:               cluster.Config{Window: time.Hour, MinTrades: 99, MinUniqueWallets: 99},
		Clock:                 func() time.Time { return now },
		LifecycleAlertFromPct: 75,
		LifecycleHotFromPct:   90,
	}, reg, emit, metrics.New(), &log)

	m, _ := reg.Get("0xa")
	warm(loop, m, 30, 60, 0.5, now)
	loop.Observe(context.Background(), m, bet(700_000, 1.0/8, "shark", now))
	if got := emit.all(); len(got) != 0 {
		t.Fatalf("early-lifecycle market should not alert, got %d", len(got))
	}
}

func TestLifecycleMarksHotInFinalStretch(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	reg := marketcache.New()
	// 100-day span; trade at day 95 = 95% elapsed → above HOT threshold.
	start := now.Add(-95 * 24 * time.Hour)
	end := now.Add(5 * 24 * time.Hour)
	reg.Replace(
		[]market.Market{{
			ID: "0xa", Slug: "us-pres", Question: "Who wins?",
			EventSlug:  "us-pres-2028",
			TokenIDs:   []vo.TokenID{"tok-yes"},
			Outcomes:   []string{"Yes"},
			Categories: []vo.CategoryID{42},
			Active:     true,
			StartDate:  start,
			EndDate:    end,
		}},
		[]market.Category{{ID: 42, Slug: "politics", Label: "Politics"}},
	)
	emit := &capturingEmitter{}
	log := zerolog.Nop()
	loop := New(Config{
		Thresholds:            defaultThresholds(),
		Baseline:              baseline.Config{Window: 7 * 24 * time.Hour},
		Cluster:               cluster.Config{Window: time.Hour, MinTrades: 99, MinUniqueWallets: 99},
		Clock:                 func() time.Time { return now },
		LifecycleAlertFromPct: 75,
		LifecycleHotFromPct:   90,
		PolymarketBase:        "https://polymarket.com",
	}, reg, emit, metrics.New(), &log)

	m, _ := reg.Get("0xa")
	warm(loop, m, 30, 60, 0.5, now)
	loop.Observe(context.Background(), m, bet(700_000, 1.0/8, "shark", now))
	got := emit.of(anomaly.KindTradeAnomaly)
	if len(got) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(got))
	}
	if !got[0].Hot {
		t.Fatalf("expected Hot=true, got %+v", got[0])
	}
	if got[0].LifecyclePct < 90 || got[0].LifecyclePct > 100 {
		t.Fatalf("lifecycle pct: %v", got[0].LifecyclePct)
	}
}

func TestMarketWithoutLifecycleAllowsAlert(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	// newLoop seeds a market without StartDate/EndDate — the lifecycle gate
	// must not silently block it (we have nothing to gate on).
	loop, reg, emit := newLoop(t, now, defaultThresholds(), cluster.Config{Window: time.Hour, MinTrades: 99, MinUniqueWallets: 99})
	loop.cfg.LifecycleAlertFromPct = 75
	loop.cfg.LifecycleHotFromPct = 90
	m, _ := reg.Get("0xa")
	warm(loop, m, 30, 60, 0.5, now)
	loop.Observe(context.Background(), m, bet(700_000, 1.0/8, "shark", now))
	if got := emit.of(anomaly.KindTradeAnomaly); len(got) != 1 {
		t.Fatalf("expected 1 finding when lifecycle is unknown, got %d", len(got))
	}
}

func TestCategoryURLAndTraderURL(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	loop, _, _ := newLoop(t, now, defaultThresholds(), cluster.Config{Window: time.Hour, MinTrades: 99, MinUniqueWallets: 99})
	loop.cfg.PolymarketBase = "https://polymarket.com"
	if got := loop.categoryURL(anomaly.CategoryRef{Slug: "politics"}); got != "https://polymarket.com/predictions/politics" {
		t.Errorf("categoryURL: %q", got)
	}
	if got := loop.traderURL("0xabc"); got != "https://polymarket.com/profile/0xabc" {
		t.Errorf("traderURL: %q", got)
	}
	if got := loop.categoryURL(anomaly.CategoryRef{}); got != "" {
		t.Errorf("missing slug must produce empty URL, got %q", got)
	}
	if got := loop.traderURL(""); got != "" {
		t.Errorf("empty wallet must produce empty URL, got %q", got)
	}
}

func TestSingleTradeRecordsClusterPeerCount(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	loop, reg, emit := newLoop(t, now, defaultThresholds(), cluster.Config{Window: time.Hour, MinTrades: 99, MinUniqueWallets: 99})
	m, _ := reg.Get("0xa")
	warm(loop, m, 30, 60, 0.5, now)
	// First fire: alone, InCluster=false.
	loop.Observe(context.Background(), m, bet(50_000, 1.0/8, "w1", now))
	// Second fire: peer present, InCluster=true.
	loop.Observe(context.Background(), m, bet(50_000, 1.0/8, "w2", now))
	got := emit.of(anomaly.KindTradeAnomaly)
	if len(got) != 2 {
		t.Fatalf("expected 2 single-trade findings, got %d", len(got))
	}
	if got[0].InCluster {
		t.Errorf("first trade should be alone: %+v", got[0])
	}
	if !got[1].InCluster || got[1].ClusterPeerCount < 2 {
		t.Errorf("second trade should report peers: %+v", got[1])
	}
}

// TestBaselineWindowDoesNotBlockShortMarkets is the regression for the user's
// correction: a 1-month-old market with BASELINE_WINDOW=1y is analyzed using
// the 1 month of available history. The window is a cap, not a minimum.
func TestBaselineWindowDoesNotBlockShortMarkets(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	// 30-day market, currently at 80% of lifetime → past the 75% gate.
	start := now.Add(-24 * 24 * time.Hour)
	end := now.Add(6 * 24 * time.Hour)
	reg := marketcache.New()
	reg.Replace(
		[]market.Market{{
			ID: "0xa", Slug: "s", Question: "q",
			EventSlug: "e", TokenIDs: []vo.TokenID{"t"}, Outcomes: []string{"Yes"},
			Categories: []vo.CategoryID{42}, Active: true,
			StartDate: start, EndDate: end,
		}},
		[]market.Category{{ID: 42, Slug: "x", Label: "X"}},
	)
	emit := &capturingEmitter{}
	log := zerolog.Nop()
	loop := New(Config{
		Thresholds:            defaultThresholds(),
		Baseline:              baseline.Config{Window: 365 * 24 * time.Hour},
		Cluster:               cluster.Config{Window: time.Hour, MinTrades: 99, MinUniqueWallets: 99},
		Clock:                 func() time.Time { return now },
		LifecycleAlertFromPct: 75,
		LifecycleHotFromPct:   90,
		MarketMinAge:          24 * time.Hour,
		BaselineMinReadySpan:  24 * time.Hour,
		PolymarketBase:        "https://polymarket.com",
	}, reg, emit, metrics.New(), &log)
	m, _ := reg.Get("0xa")
	// Seed baseline across the market's 30-day lifetime.
	for i := 0; i < 30; i++ {
		loop.Observe(context.Background(), m, bet(60, 0.5, "wb", start.Add(time.Duration(i)*24*time.Hour)))
	}
	// Whale at now.
	loop.Observe(context.Background(), m, bet(700_000, 1.0/8, "shark", now))
	got := emit.of(anomaly.KindTradeAnomaly)
	if len(got) != 1 {
		t.Fatalf("1-month market with 1y cap should fire, got %d findings", len(got))
	}
	// The displayed baseline span must be the actual ~29 days, NOT 1y.
	if got[0].Baseline.Span < 28*24*time.Hour || got[0].Baseline.Span > 31*24*time.Hour {
		t.Fatalf("baseline span should reflect actual ~29d data, got %s", got[0].Baseline.Span)
	}
}

func TestEarlyLifecycleBlocksShortMarkets(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	// 30-day market at 50% of lifetime — below 75% gate.
	start := now.Add(-15 * 24 * time.Hour)
	end := now.Add(15 * 24 * time.Hour)
	reg := marketcache.New()
	reg.Replace(
		[]market.Market{{ID: "0xa", Slug: "s", EventSlug: "e", TokenIDs: []vo.TokenID{"t"}, Outcomes: []string{"Yes"}, Categories: []vo.CategoryID{42}, Active: true, StartDate: start, EndDate: end}},
		[]market.Category{{ID: 42, Slug: "x", Label: "X"}},
	)
	emit := &capturingEmitter{}
	log := zerolog.Nop()
	loop := New(Config{
		Thresholds: defaultThresholds(), Baseline: baseline.Config{Window: 365 * 24 * time.Hour},
		Cluster:               cluster.Config{Window: time.Hour, MinTrades: 99, MinUniqueWallets: 99},
		Clock:                 func() time.Time { return now },
		LifecycleAlertFromPct: 75, LifecycleHotFromPct: 90,
	}, reg, emit, metrics.New(), &log)
	m, _ := reg.Get("0xa")
	for i := 0; i < 30; i++ {
		loop.Observe(context.Background(), m, bet(60, 0.5, "wb", start.Add(time.Duration(i)*24*time.Hour/30*15)))
	}
	loop.Observe(context.Background(), m, bet(700_000, 1.0/8, "shark", now))
	if got := emit.all(); len(got) != 0 {
		t.Fatalf("50%%-elapsed market must not alert, got %d", len(got))
	}
}

func TestMarketMinAgeBlocksTooYoung(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	// 12h-old market, very late in lifetime but too young in absolute terms.
	start := now.Add(-12 * time.Hour)
	end := now.Add(time.Hour)
	reg := marketcache.New()
	reg.Replace(
		[]market.Market{{ID: "0xa", Slug: "s", EventSlug: "e", TokenIDs: []vo.TokenID{"t"}, Outcomes: []string{"Yes"}, Categories: []vo.CategoryID{42}, Active: true, StartDate: start, EndDate: end}},
		[]market.Category{{ID: 42, Slug: "x", Label: "X"}},
	)
	emit := &capturingEmitter{}
	log := zerolog.Nop()
	loop := New(Config{
		Thresholds: defaultThresholds(), Baseline: baseline.Config{Window: 365 * 24 * time.Hour},
		Cluster:               cluster.Config{Window: time.Hour, MinTrades: 99, MinUniqueWallets: 99},
		Clock:                 func() time.Time { return now },
		LifecycleAlertFromPct: 75, LifecycleHotFromPct: 90,
		MarketMinAge: 24 * time.Hour, // 12h < 24h → block
	}, reg, emit, metrics.New(), &log)
	m, _ := reg.Get("0xa")
	for i := 0; i < 30; i++ {
		loop.Observe(context.Background(), m, bet(60, 0.5, "wb", start))
	}
	loop.Observe(context.Background(), m, bet(700_000, 1.0/8, "shark", now))
	if got := emit.all(); len(got) != 0 {
		t.Fatalf("market younger than MarketMinAge must not alert, got %d", len(got))
	}
}

func TestBaselineMinReadySpanBlocksThinSpan(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	start := now.Add(-30 * 24 * time.Hour)
	end := now.Add(time.Hour)
	reg := marketcache.New()
	reg.Replace(
		[]market.Market{{ID: "0xa", Slug: "s", EventSlug: "e", TokenIDs: []vo.TokenID{"t"}, Outcomes: []string{"Yes"}, Categories: []vo.CategoryID{42}, Active: true, StartDate: start, EndDate: end}},
		[]market.Category{{ID: 42, Slug: "x", Label: "X"}},
	)
	emit := &capturingEmitter{}
	log := zerolog.Nop()
	loop := New(Config{
		Thresholds: defaultThresholds(), Baseline: baseline.Config{Window: 365 * 24 * time.Hour},
		Cluster:               cluster.Config{Window: time.Hour, MinTrades: 99, MinUniqueWallets: 99},
		Clock:                 func() time.Time { return now },
		LifecycleAlertFromPct: 0, LifecycleHotFromPct: 100,
		MarketMinAge:         0,
		BaselineMinReadySpan: 24 * time.Hour,
	}, reg, emit, metrics.New(), &log)
	m, _ := reg.Get("0xa")
	// Seed 30 baseline trades all in the last 10 minutes — span < 24h.
	for i := 0; i < 30; i++ {
		loop.Observe(context.Background(), m, bet(60, 0.5, "wb", now.Add(-time.Duration(i)*20*time.Second)))
	}
	loop.Observe(context.Background(), m, bet(700_000, 1.0/8, "shark", now))
	if got := emit.all(); len(got) != 0 {
		t.Fatalf("thin-span baseline must not alert, got %d", len(got))
	}
}

// TestSeverityTableFromStrategy is the canonical table from the strategy
// document — every row asserts the chosen severity for the new defaults.
func TestSeverityTableFromStrategy(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name     string
		median   float64
		notional float64
		price    float64
		wantFire bool
		wantSev  anomaly.Severity
	}{
		// median $100
		// $100k @ odds 8, mul 1000: absTier=Critical (notional & odds clear),
		// mulTier=Warning (>=1000, <10000) → conservative-MIN = Warning.
		{"100k_odds8_mul1000_warning", 100, 100_000, 1.0 / 8, true, anomaly.SeverityWarning},
		{"100k_odds3_mul1000_info", 100, 100_000, 1.0 / 3, true, anomaly.SeverityInfo},
		{"25k_odds5_mul250_info", 100, 25_000, 1.0 / 5, true, anomaly.SeverityInfo},
		{"10k_odds3_mul100_info", 100, 10_000, 1.0 / 3, true, anomaly.SeverityInfo},
		// boundary fails
		{"9999_odds3_no_fire", 100, 9_999, 1.0 / 3, false, ""},
		{"10k_odds299_no_fire", 100, 10_000, 1.0 / 2.99, false, ""},
		// median $60 (just above the $50 baseline dust filter)
		// $100k @ odds 8 / median 60 → mul≈1666 → conservative-MIN = Warning.
		{"100k_odds8_mul1666_warning", 60, 100_000, 1.0 / 8, true, anomaly.SeverityWarning},
		// $1M @ odds 3 / median 60 → absTier=Info (odds 3 < 5 so not Warning),
		// mulTier=Critical (16666 ≥ 10000) → conservative-MIN = Info.
		// Single-trade severity caps at Critical; HARD is cluster-only.
		{"1M_odds3_low_odds_info", 60, 1_000_000, 1.0 / 3, true, anomaly.SeverityInfo},
		// $100k @ odds 8 / median 1000 → mul=100 → mulTier=Info → conservative = Info.
		{"100k_odds8_mul100_info", 1_000, 100_000, 1.0 / 8, true, anomaly.SeverityInfo},
		// Whale-grade insider trade: $300k @ odds 6 / median 60 → mul=5000.
		// absTier=Warning (odds 6 < 8), mulTier=Warning (5000 < 10000) → Warning.
		{"300k_odds6_mul5000_warning", 60, 300_000, 1.0 / 6, true, anomaly.SeverityWarning},
		// Long-shot insider: $150k @ odds 12 / median 60 → mul=2500.
		// absTier=Critical (150k ≥ 100k ∧ 12 ≥ 8), mulTier=Warning → Warning.
		{"150k_odds12_mul2500_warning", 60, 150_000, 1.0 / 12, true, anomaly.SeverityWarning},
		// Genuine top-tier shark: $700k @ odds 10 / median 60 → mul≈11666.
		// absTier=Critical, mulTier=Critical → conservative-MIN = Critical.
		{"700k_odds10_mul11666_critical", 60, 700_000, 1.0 / 10, true, anomaly.SeverityCritical},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			loop, reg, emit := newLoop(t, now, defaultThresholds(),
				cluster.Config{Window: time.Hour, MinTrades: 99, MinUniqueWallets: 99})
			m, _ := reg.Get("0xa")
			// Seed baseline at the desired median (notional = c.median).
			for i := 0; i < 30; i++ {
				loop.Observe(context.Background(), m, bet(c.median, 0.5, "wb", now))
			}
			loop.Observe(context.Background(), m, bet(c.notional, c.price, "test", now))
			got := emit.of(anomaly.KindTradeAnomaly)
			fired := len(got) > 0
			if fired != c.wantFire {
				t.Fatalf("fired=%v want=%v findings=%+v", fired, c.wantFire, got)
			}
			if fired && got[0].Severity != c.wantSev {
				t.Fatalf("severity=%s want=%s (multiplier=%v abs=%s mul=%s)",
					got[0].Severity, c.wantSev, got[0].EffectiveMultiplier, got[0].AbsoluteTier, got[0].MultiplierTier)
			}
		})
	}
}

func TestUnknownLifecycleFailsClosedByDefault(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	reg := marketcache.New()
	reg.Replace(
		// Market with no Start/End dates.
		[]market.Market{{ID: "0xa", Slug: "s", EventSlug: "e", TokenIDs: []vo.TokenID{"t"}, Outcomes: []string{"Yes"}, Categories: []vo.CategoryID{42}, Active: true}},
		[]market.Category{{ID: 42, Slug: "x", Label: "X"}},
	)
	emit := &capturingEmitter{}
	log := zerolog.Nop()
	loop := New(Config{
		Thresholds: defaultThresholds(),
		Baseline:   baseline.Config{Window: 7 * 24 * time.Hour},
		Cluster:    cluster.Config{Window: time.Hour, MinTrades: 99, MinUniqueWallets: 99},
		Clock:      func() time.Time { return now },
		// v4 hardening: there is no AllowUnknownMarketLifecycle config knob.
		// Unknown lifecycle is ALWAYS silenced. This test pins that contract.
		PolymarketBase: "https://polymarket.com",
	}, reg, emit, metrics.New(), &log)
	m, _ := reg.Get("0xa")
	for i := 0; i < 30; i++ {
		loop.Observe(context.Background(), m, bet(60, 0.5, "wb", now))
	}
	loop.Observe(context.Background(), m, bet(700_000, 1.0/8, "shark", now))
	if got := emit.all(); len(got) != 0 {
		t.Fatalf("unknown lifecycle must be fail-closed by default, got %d findings", len(got))
	}
}

// TestFranceFifaHideFromNewStillAlerts is the regression guard for the case
// reported in the field:
//
//	Trade: $26,999 @ price 0.1768 (odds 5.66)
//	Baseline: 29 trades, median $100  → multiplier ≈ 270×
//	Category: "Hide From New" (NOT sports)
//	Market: "Will France win the 2026 FIFA World Cup?" — sports words in
//	        the question and event slug, but not in the category.
//	Lifecycle: late-stage market (well past the 75% gate).
//
// The previous (now reverted) secondary keyword scan silenced this trade by
// matching "fifa"/"world cup" against market.Question / EventSlug. The
// product decision is that filtering is category-identity-only, so this
// trade MUST emit a single-trade alert. Severity calculation under defaults:
//
//	absolute  : $26,999 ≥ $25k AND 5.66 ≥ 5 → Warning
//	multiplier: 270× ≥ 100 but < 1000        → Info
//	conservative-MIN(Warning, Info)          → Info
//
// (This matches the historical alert pasted in the bug report: "tiers:
// absolute=warning multiplier=info → final=info".)
func TestFranceFifaHideFromNewStillAlerts(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	// Market lifecycle from real Polymarket data: opened 2025-07-02, ends
	// 2026-07-20. At 2026-05-17 that's ~83% of the lifetime → past the gate.
	start := time.Date(2025, 7, 2, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	reg := marketcache.New()
	reg.Replace(
		[]market.Market{{
			ID:   "0xfifa-france",
			Slug: "will-france-win-the-2026-fifa-world-cup",
			// Sports words present in question + event slug + event title —
			// must NOT trigger any silencing under category-only filtering.
			Question:   "Will France win the 2026 FIFA World Cup?",
			EventSlug:  "2026-fifa-world-cup-winner-595",
			EventTitle: "2026 FIFA World Cup Winner",
			TokenIDs:   []vo.TokenID{"tok-yes"},
			Outcomes:   []string{"Yes"},
			// "Hide From New" — not a sports category by identity.
			Categories: []vo.CategoryID{42},
			Active:     true, Closed: false,
			StartDate: start, EndDate: end,
		}},
		[]market.Category{{ID: 42, Slug: "hide-from-new", Label: "Hide From New"}},
	)
	emit := &capturingEmitter{}
	log := zerolog.Nop()
	loop := New(Config{
		Thresholds: defaultThresholds(),
		// Baseline window long enough to cover the warming period; the test
		// seeds 29 samples of $100 over the last ~7 days so the actual span
		// clears BaselineMinReadySpan (24h).
		Baseline: baseline.Config{Window: 365 * 24 * time.Hour},
		Cluster:  cluster.Config{Window: time.Hour, MinTrades: 99, MinUniqueWallets: 99}, // never fire cluster
		// Whitelist admits this market's category (Hide From New). The point
		// of the test is that sports words in the market title/event slug
		// do NOT block the trade — filtering is category-identity only.
		Filter:                category.NewFilter([]string{"hide-from-new"}),
		Clock:                 func() time.Time { return now },
		LifecycleAlertFromPct: 75,
		LifecycleHotFromPct:   90,
		MarketMinAge:          24 * time.Hour,
		BaselineMinReadySpan:  24 * time.Hour,
		PolymarketBase:        "https://polymarket.com",
	}, reg, emit, metrics.New(), &log)
	m, _ := reg.Get("0xfifa-france")
	// Warm baseline with 29 samples of $100 across the last week so SpanActual
	// clears BaselineMinReadySpan and MedianUSD == 100.
	for i := 0; i < 29; i++ {
		at := now.Add(-time.Duration(i) * 6 * time.Hour) // ~7 days of span
		loop.Observe(context.Background(), m, bet(100, 0.5, "warmer", at))
	}
	// The exact trade from the regression report.
	loop.Observe(context.Background(), m, bet(26_999, 0.1768, "0x8ba27b7c9de2b6367f986bff5f9c8049204c1650", now))

	got := emit.of(anomaly.KindTradeAnomaly)
	if len(got) != 1 {
		t.Fatalf("expected 1 single-trade alert, got %d: %+v", len(got), got)
	}
	f := got[0]
	if f.Severity != anomaly.SeverityInfo {
		t.Errorf("expected Info (conservative-MIN of abs=Warning, mul=Info), got %s", f.Severity)
	}
	if f.AbsoluteTier != anomaly.SeverityWarning {
		t.Errorf("absolute tier: got %q want warning", f.AbsoluteTier)
	}
	if f.MultiplierTier != anomaly.SeverityInfo {
		t.Errorf("multiplier tier: got %q want info", f.MultiplierTier)
	}
	if f.Trade == nil || f.Trade.NotionalUSD < 26_998 || f.Trade.NotionalUSD > 27_000 {
		t.Errorf("trade notional: %+v", f.Trade)
	}
	if f.EffectiveMultiplier < 200 || f.EffectiveMultiplier > 350 {
		t.Errorf("multiplier must reflect 26999/100 ≈ 270, got %v", f.EffectiveMultiplier)
	}
	if !f.Hot && f.LifecyclePct < 75 {
		t.Errorf("expected lifecycle past 75%%, got %v hot=%v", f.LifecyclePct, f.Hot)
	}
}

// TestNonWhitelistedCategorySkipped confirms that markets whose category is
// not in the whitelist produce no alerts. Sports is the natural example
// because it's the category we most commonly want to exclude when the
// whitelist is "Politics".
func TestNonWhitelistedCategorySkipped(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	reg := marketcache.New()
	reg.Replace(
		[]market.Market{{
			ID: "0xa", Slug: "anything", Question: "Anything?", EventSlug: "anything",
			TokenIDs: []vo.TokenID{"t"}, Outcomes: []string{"Yes"},
			Categories: []vo.CategoryID{99}, Active: true, StartDate: now.Add(-95 * 24 * time.Hour), EndDate: now.Add(5 * 24 * time.Hour),
		}},
		[]market.Category{{ID: 99, Slug: "sports", Label: "Sports"}},
	)
	emit := &capturingEmitter{}
	log := zerolog.Nop()
	loop := New(Config{
		Thresholds: defaultThresholds(),
		Baseline:   baseline.Config{Window: 7 * 24 * time.Hour},
		Cluster:    cluster.Config{Window: time.Hour, MinTrades: 99, MinUniqueWallets: 99},
		// Whitelist contains only Politics. The market's Sports category is
		// not admitted, so even a huge late trade must produce no alert.
		Filter: category.NewFilter([]string{"politics"}),
		Clock:  func() time.Time { return now },
		// Lifecycle gate is disabled for the test; category filter must still bite.
	}, reg, emit, metrics.New(), &log)
	m, _ := reg.Get("0xa")
	for i := 0; i < 30; i++ {
		loop.Observe(context.Background(), m, bet(60, 0.5, "wb", now))
	}
	loop.Observe(context.Background(), m, bet(700_000, 1.0/8, "shark", now))
	if got := emit.all(); len(got) != 0 {
		t.Fatalf("non-whitelisted category must be silenced, got %d", len(got))
	}
}

// TestSportsLikeMarketUnderWhitelistedCategoryAllowed pins the contract:
// a market whose title / event slug contain sports words ("FIFA",
// "World Cup") but whose category is whitelisted (here `Hide From New`)
// MUST still produce an alert. Filtering is category-identity only — no
// scan of market title, event slug, or any other text.
func TestSportsLikeMarketUnderWhitelistedCategoryAllowed(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	reg := marketcache.New()
	reg.Replace(
		[]market.Market{{
			ID: "0xa", Slug: "will-france-win-fifa", Question: "Will France win the 2026 FIFA World Cup?",
			EventSlug: "2026-fifa-world-cup-winner-595", EventTitle: "FIFA World Cup 2026",
			TokenIDs: []vo.TokenID{"t"}, Outcomes: []string{"Yes"},
			Categories: []vo.CategoryID{1}, Active: true, StartDate: now.Add(-95 * 24 * time.Hour), EndDate: now.Add(5 * 24 * time.Hour),
		}},
		[]market.Category{{ID: 1, Slug: "hide-from-new", Label: "Hide From New"}},
	)
	emit := &capturingEmitter{}
	log := zerolog.Nop()
	loop := New(Config{
		Thresholds: defaultThresholds(),
		Baseline:   baseline.Config{Window: 7 * 24 * time.Hour},
		Cluster:    cluster.Config{Window: time.Hour, MinTrades: 99, MinUniqueWallets: 99},
		// Whitelist admits "hide-from-new". Sports words in market title /
		// event slug are NOT consulted, so the trade alerts.
		Filter: category.NewFilter([]string{"hide-from-new"}),
		Clock:  func() time.Time { return now },
	}, reg, emit, metrics.New(), &log)
	m, _ := reg.Get("0xa")
	for i := 0; i < 30; i++ {
		loop.Observe(context.Background(), m, bet(60, 0.5, "wb", now))
	}
	loop.Observe(context.Background(), m, bet(700_000, 1.0/8, "shark", now))
	if got := emit.of(anomaly.KindTradeAnomaly); len(got) != 1 {
		t.Fatalf("sports-themed market under whitelisted category must alert, got %d findings", len(got))
	}
}

// TestWhitelistStaysCategoryOnly is the symmetric guard: a market whose
// metadata contains sports words but whose category is whitelisted
// (Politics) must STILL alert. The whitelist looks only at the category
// slug+label, never at market wording.
func TestWhitelistStaysCategoryOnly(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	reg := marketcache.New()
	reg.Replace(
		[]market.Market{{
			ID: "0xa", Slug: "will-the-sports-bill-pass", Question: "Will the new sports betting bill pass?",
			EventSlug: "us-sports-betting-bill-2026", EventTitle: "Sports Betting Bill",
			TokenIDs: []vo.TokenID{"t"}, Outcomes: []string{"Yes"},
			Categories: []vo.CategoryID{2}, Active: true, StartDate: now.Add(-95 * 24 * time.Hour), EndDate: now.Add(5 * 24 * time.Hour),
		}},
		[]market.Category{{ID: 2, Slug: "politics", Label: "Politics"}},
	)
	emit := &capturingEmitter{}
	log := zerolog.Nop()
	loop := New(Config{
		Thresholds: defaultThresholds(),
		Baseline:   baseline.Config{Window: 7 * 24 * time.Hour},
		Cluster:    cluster.Config{Window: time.Hour, MinTrades: 99, MinUniqueWallets: 99},
		Filter:     category.NewFilter([]string{"politics"}),
		Clock:      func() time.Time { return now },
	}, reg, emit, metrics.New(), &log)
	m, _ := reg.Get("0xa")
	for i := 0; i < 30; i++ {
		loop.Observe(context.Background(), m, bet(60, 0.5, "wb", now))
	}
	loop.Observe(context.Background(), m, bet(700_000, 1.0/8, "shark", now))
	if got := emit.of(anomaly.KindTradeAnomaly); len(got) != 1 {
		t.Fatalf("whitelisted category with sports word in metadata must alert, got %d", len(got))
	}
}

func TestObserveConcurrencySafe(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	loop, reg, _ := newLoop(t, now, defaultThresholds(), cluster.Config{Window: time.Hour, MinTrades: 5, MinUniqueWallets: 3})
	m, _ := reg.Get("0xa")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				loop.Observe(context.Background(), m, bet(100, 0.5, "w", now))
			}
		}()
	}
	wg.Wait()
}

// TestObserve_TooOldTradeSkippedForLiveAlerts pins the
// LIVE_ALERT_MAX_LAG safety belt: a trade whose traded_at is older
// than now() − lag must NOT fire (Telegram-spam prevention against
// backfill / replay paths), and the skip counter must increment so
// operators can prove the gate is doing its job in production.
//
// Setup: build a loop with LiveAlertMaxLag=1h, then feed a trade
// that would otherwise fire (warm baseline + large notional +
// long-odds). Expect: zero emissions, exactly one
// trades_skipped_detection_total{reason="too_old_for_live_alert"}.
func TestObserve_TooOldTradeSkippedForLiveAlerts(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	reg := marketcache.New()
	reg.Replace(
		[]market.Market{{
			ID: "0xa", Slug: "us-pres", Question: "Who wins?",
			TokenIDs: []vo.TokenID{"tok-yes", "tok-no"}, Outcomes: []string{"Yes", "No"},
			Categories: []vo.CategoryID{42}, Active: true,
			StartDate: now.Add(-95 * 24 * time.Hour), EndDate: now.Add(5 * 24 * time.Hour),
		}},
		[]market.Category{{ID: 42, Slug: "politics", Label: "Politics"}},
	)
	emit := &capturingEmitter{}
	met := metrics.New()
	log := zerolog.Nop()
	loop := New(Config{
		Thresholds:      defaultThresholds(),
		Baseline:        baseline.Config{Window: 7 * 24 * time.Hour},
		Cluster:         cluster.Config{Window: time.Hour, MinTrades: 99, MinUniqueWallets: 99},
		Clock:           func() time.Time { return now },
		LiveAlertMaxLag: time.Hour, // anything older than 1h is "replay"
	}, reg, emit, met, &log)
	m, _ := reg.Get("0xa")

	// Warm the baseline at NOW so the gate's bookkeeping path doesn't
	// no-op on an empty bucket. (warm() bypasses LiveAlertMaxLag via
	// repeated calls at `at == now`; we measure the SKIP path only
	// for the one trade that's intentionally stale below.)
	warm(loop, m, 10, 100, 0.10, now)

	// Stale trade: traded_at 3h ago, lag = 1h → must skip.
	stale := bet(50_000, 0.10, "0xstale", now.Add(-3*time.Hour))
	loop.Observe(context.Background(), m, stale)

	if got := len(emit.all()); got != 0 {
		t.Fatalf("stale trade must not emit any Finding, got %d: %+v", got, emit.all())
	}
	skipped := getCounter(t, met.TradesSkippedDetection, "too_old_for_live_alert")
	if skipped < 1 {
		t.Fatalf("trades_skipped_detection_total{too_old_for_live_alert} should be ≥1, got %v", skipped)
	}
}

// TestObserve_RecentTradePassesLiveAlertGate is the complementary
// positive case: a trade younger than LIVE_ALERT_MAX_LAG flows
// through the gate (TradesAnalyzed increments; the skip counter
// stays where it was).
func TestObserve_RecentTradePassesLiveAlertGate(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	reg := marketcache.New()
	reg.Replace(
		[]market.Market{{
			ID: "0xa", Slug: "us-pres", Question: "Who wins?",
			TokenIDs: []vo.TokenID{"tok-yes", "tok-no"}, Outcomes: []string{"Yes", "No"},
			Categories: []vo.CategoryID{42}, Active: true,
			StartDate: now.Add(-95 * 24 * time.Hour), EndDate: now.Add(5 * 24 * time.Hour),
		}},
		[]market.Category{{ID: 42, Slug: "politics", Label: "Politics"}},
	)
	emit := &capturingEmitter{}
	met := metrics.New()
	log := zerolog.Nop()
	loop := New(Config{
		Thresholds:      defaultThresholds(),
		Baseline:        baseline.Config{Window: 7 * 24 * time.Hour},
		Cluster:         cluster.Config{Window: time.Hour, MinTrades: 99, MinUniqueWallets: 99},
		Clock:           func() time.Time { return now },
		LiveAlertMaxLag: time.Hour,
	}, reg, emit, met, &log)
	m, _ := reg.Get("0xa")

	// A fresh trade (5 minutes ago) — comfortably inside the 1h lag.
	fresh := bet(100, 0.50, "0xfresh", now.Add(-5*time.Minute))
	loop.Observe(context.Background(), m, fresh)

	skipped := getCounter(t, met.TradesSkippedDetection, "too_old_for_live_alert")
	if skipped > 0 {
		t.Errorf("fresh trade must not increment skip counter, got %v", skipped)
	}
	// metrics.New() returns counter-vecs; TradesAnalyzed is a plain
	// Counter. Use prometheus testutil to read its value without
	// importing client_model directly.
	if v := testutil.ToFloat64(met.TradesAnalyzed); v < 1 {
		t.Errorf("trades_analyzed_total should be ≥1 after a fresh Observe, got %v", v)
	}
}
