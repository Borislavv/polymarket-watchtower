package detect

import (
	"context"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/aggregate"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/analytics/baseline"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/analytics/cluster"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/category"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/anomaly"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/market"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/trade"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/vo"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/metrics"
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

func newLoop(t *testing.T, now time.Time, th anomaly.Thresholds, cl cluster.Config) (*Loop, *aggregate.MarketRegistry, *capturingEmitter) {
	t.Helper()
	reg := aggregate.NewRegistry()
	reg.Replace(
		[]market.Market{{
			ID: "0xa", Slug: "us-pres", Question: "Who wins?",
			EventSlug: "us-pres-2028", EventTitle: "US Presidential Election 2028",
			TokenIDs: []vo.TokenID{"tok-yes", "tok-no"}, Outcomes: []string{"Yes", "No"},
			Categories: []vo.CategoryID{42}, Active: true,
		}},
		[]market.Category{{ID: 42, Slug: "politics", Label: "Politics"}},
	)
	emit := &capturingEmitter{}
	log := zerolog.Nop()
	loop := New(Config{
		Thresholds:     th,
		Baseline:       baseline.Config{Window: 7 * 24 * time.Hour, MinTradeUSD: 50},
		Cluster:        cl,
		Clock:          func() time.Time { return now },
		PolymarketBase: "https://polymarket.com",
		GrafanaBase:    "http://grafana.local",
		GrafanaDashUID: "uid123",
		GrafanaContext: time.Hour,
	}, aggregate.New(aggregate.Config{Bucket: time.Minute, Baseline: 7 * 24 * time.Hour}), reg, emit, metrics.New(), &log)
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

// warm seeds the baseline with `n` trades of `notional` USD each. Notionals
// below BaselineMinTradeUSD are filtered out by the baseline itself.
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
	// $700k at odds 8 → multiplier 700000/60 ≈ 11666 → both critical.
	loop.Observe(context.Background(), m, bet(700_000, 1.0/8, "shark", now))
	got := emit.of(anomaly.KindTradeAnomaly)
	if len(got) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(got))
	}
	if got[0].Severity != anomaly.SeverityCritical {
		t.Fatalf("expected critical, got %s", got[0].Severity)
	}
	// All payload fields populated for human review.
	tr := got[0].Trade
	if tr == nil || tr.NotionalUSD < 699_000 || tr.Odds < 7.99 {
		t.Fatalf("trade ref: %+v", tr)
	}
	if got[0].Multiplier < 10_000 {
		t.Fatalf("multiplier: %v", got[0].Multiplier)
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

func TestBaselineFiltersMicroTrades(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	loop, reg, emit := newLoop(t, now, defaultThresholds(), cluster.Config{Window: time.Hour, MinTrades: 99, MinUniqueWallets: 99})
	m, _ := reg.Get("0xa")
	// 30 trades of $5 each — all below the $50 BaselineMinTradeUSD, baseline ends up empty.
	warm(loop, m, 30, 5, 0.5, now)
	loop.Observe(context.Background(), m, bet(100_000, 1.0/8, "shark", now))
	if got := emit.all(); len(got) != 0 {
		t.Fatalf("expected no fire when baseline only had filtered micro-trades, got %+v", got)
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

func TestBlacklistedCategoryNoAlert(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	reg := aggregate.NewRegistry()
	reg.Replace(
		[]market.Market{{
			ID: "0xb", Slug: "nba-finals", Question: "Who wins?",
			EventSlug: "2026-nba-finals", TokenIDs: []vo.TokenID{"tok"}, Outcomes: []string{"Yes"},
			Categories: []vo.CategoryID{77}, Active: true,
		}},
		[]market.Category{{ID: 77, Slug: "nba", Label: "NBA"}},
	)
	emit := &capturingEmitter{}
	log := zerolog.Nop()
	loop := New(Config{
		Thresholds: defaultThresholds(),
		Baseline:   baseline.Config{Window: 7 * 24 * time.Hour},
		Cluster:    cluster.Config{Window: time.Hour, MinTrades: 1, MinUniqueWallets: 1},
		Filter:     category.NewFilter([]string{"nba"}),
		Clock:      func() time.Time { return now },
	}, aggregate.New(aggregate.Config{Bucket: time.Minute, Baseline: 7 * 24 * time.Hour}), reg, emit, metrics.New(), &log)
	m, _ := reg.Get("0xb")
	loop.Observe(context.Background(), m, bet(100_000, 1.0/8, "shark", now))
	if got := emit.all(); len(got) != 0 {
		t.Fatalf("blacklisted category produced %d findings", len(got))
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
	reg := aggregate.NewRegistry()
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
		Baseline:              baseline.Config{Window: 7 * 24 * time.Hour, MinTradeUSD: 50},
		Cluster:               cluster.Config{Window: time.Hour, MinTrades: 99, MinUniqueWallets: 99},
		Clock:                 func() time.Time { return now },
		LifecycleAlertFromPct: 75,
		LifecycleHotFromPct:   90,
	}, aggregate.New(aggregate.Config{Bucket: time.Minute, Baseline: 7 * 24 * time.Hour}), reg, emit, metrics.New(), &log)

	m, _ := reg.Get("0xa")
	warm(loop, m, 30, 60, 0.5, now)
	loop.Observe(context.Background(), m, bet(700_000, 1.0/8, "shark", now))
	if got := emit.all(); len(got) != 0 {
		t.Fatalf("early-lifecycle market should not alert, got %d", len(got))
	}
}

func TestLifecycleMarksHotInFinalStretch(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	reg := aggregate.NewRegistry()
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
		Baseline:              baseline.Config{Window: 7 * 24 * time.Hour, MinTradeUSD: 50},
		Cluster:               cluster.Config{Window: time.Hour, MinTrades: 99, MinUniqueWallets: 99},
		Clock:                 func() time.Time { return now },
		LifecycleAlertFromPct: 75,
		LifecycleHotFromPct:   90,
		PolymarketBase:        "https://polymarket.com",
	}, aggregate.New(aggregate.Config{Bucket: time.Minute, Baseline: 7 * 24 * time.Hour}), reg, emit, metrics.New(), &log)

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
