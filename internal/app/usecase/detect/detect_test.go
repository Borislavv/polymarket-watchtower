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
func (e *capturingEmitter) of(kind anomaly.Kind) []anomaly.Finding {
	var out []anomaly.Finding
	for _, f := range e.all() {
		if f.Kind == kind {
			out = append(out, f)
		}
	}
	return out
}

func newLoop(t *testing.T, now time.Time, th anomaly.Thresholds, cl cluster.Config) (*Loop, *aggregate.MarketRegistry, *capturingEmitter) {
	t.Helper()
	reg := aggregate.NewRegistry()
	reg.Replace(
		[]market.Market{{
			ID: "0xa", Slug: "us-pres", Question: "Who wins?",
			EventSlug:  "us-pres-2028",
			EventTitle: "US Presidential Election 2028",
			TokenIDs:   []vo.TokenID{"tok-yes", "tok-no"},
			Outcomes:   []string{"Yes", "No"},
			Categories: []vo.CategoryID{42},
			Active:     true,
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
	}, aggregate.New(aggregate.Config{Bucket: time.Minute, Baseline: 7 * 24 * time.Hour}), reg, emit, metrics.New(), &log)
	return loop, reg, emit
}

func bet(size, price float64, wallet string, at time.Time) trade.Trade {
	return trade.Trade{
		ID:        "id-" + wallet,
		Market:    "0xa",
		Token:     "tok-yes",
		Side:      trade.SideBuy,
		Size:      size,
		Price:     price,
		Timestamp: at,
		Taker:     wallet,
	}
}

func TestSingleTradeIgnoredWhenSmall(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	loop, reg, emit := newLoop(t, now, anomaly.Thresholds{
		MultiplierLadder:       []float64{30, 100, 1000},
		OddsLadder:             []float64{1e9}, // odds path disabled
		MinTradeUSD:            10_000,
		MinBaselineTrades:      20,
		MinBaselineNotionalUSD: 1_000,
	}, cluster.Config{Window: time.Hour, MinTrades: 5, MinUniqueWallets: 3})
	m, _ := reg.Get("0xa")
	// build baseline of 30 trades at $10 notional — total $300 (below 1k floor on purpose)
	for i := 0; i < 30; i++ {
		loop.Observe(context.Background(), m, bet(20, 0.5, "w-base", now.Add(-time.Minute)))
	}
	// next trade — baseline guards still kick in; no fire.
	loop.Observe(context.Background(), m, bet(20, 0.5, "w-new", now))
	if got := emit.all(); len(got) != 0 {
		t.Fatalf("expected no findings, got %d: %+v", len(got), got)
	}
}

func TestWhaleFiresMultiplierTiers(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	loop, reg, emit := newLoop(t, now, anomaly.Thresholds{
		MultiplierLadder:       []float64{30, 100, 1000},
		OddsLadder:             []float64{1e9},
		MinTradeUSD:            10_000,
		MinBaselineTrades:      20,
		MinBaselineNotionalUSD: 1_000,
	}, cluster.Config{Window: time.Hour, MinTrades: 99, MinUniqueWallets: 99})
	m, _ := reg.Get("0xa")
	// Baseline: 30 trades at $100 notional = $3k total.
	for i := 0; i < 30; i++ {
		loop.Observe(context.Background(), m, bet(200, 0.5, "w-base", now))
	}
	// $10k whale @ x100 baseline => warning
	loop.Observe(context.Background(), m, bet(20_000, 0.5, "wA", now))
	// $100k whale @ x1000 baseline => critical
	loop.Observe(context.Background(), m, bet(200_000, 0.5, "wB", now))

	got := emit.of(anomaly.KindTradeAnomaly)
	if len(got) != 2 {
		t.Fatalf("expected 2 findings, got %d: %+v", len(got), got)
	}
	for _, f := range got {
		if f.Reason != anomaly.ReasonWhale {
			t.Errorf("reason: %s want %s", f.Reason, anomaly.ReasonWhale)
		}
		if f.Trade == nil || f.Trade.Outcome != "Yes" {
			t.Errorf("trade ref: %+v", f.Trade)
		}
		if f.MarketURL != "https://polymarket.com/event/us-pres-2028" {
			t.Errorf("market URL: %q (must use EventSlug)", f.MarketURL)
		}
		if !strings.Contains(f.GrafanaURL, "var-category=Politics") {
			t.Errorf("grafana URL missing category: %q", f.GrafanaURL)
		}
	}
	if got[0].Severity != anomaly.SeverityWarning {
		t.Fatalf("first severity: %s", got[0].Severity)
	}
	if got[1].Severity != anomaly.SeverityCritical {
		t.Fatalf("second severity: %s", got[1].Severity)
	}
}

func TestWhaleSkippedBelowMinTradeUSD(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	loop, reg, emit := newLoop(t, now, anomaly.Thresholds{
		MultiplierLadder:       []float64{30},
		OddsLadder:             []float64{1e9},
		MinTradeUSD:            10_000,
		MinBaselineTrades:      20,
		MinBaselineNotionalUSD: 100,
	}, cluster.Config{Window: time.Hour, MinTrades: 99, MinUniqueWallets: 99})
	m, _ := reg.Get("0xa")
	for i := 0; i < 30; i++ {
		loop.Observe(context.Background(), m, bet(20, 0.5, "wb", now))
	}
	// $5k bet at multiplier x500 but below SINGLE_MIN_TRADE_USD — must not fire.
	loop.Observe(context.Background(), m, bet(10_000, 0.5, "small-whale", now))
	if got := emit.all(); len(got) != 0 {
		t.Fatalf("expected no fire under MinTradeUSD, got %+v", got)
	}
}

func TestHighOddsAloneFires(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	loop, reg, emit := newLoop(t, now, anomaly.Thresholds{
		MultiplierLadder: []float64{1e9}, // whale path disabled
		OddsLadder:       []float64{3, 10, 25},
		MinTradeUSD:      10_000,
	}, cluster.Config{Window: time.Hour, MinTrades: 99, MinUniqueWallets: 99})
	m, _ := reg.Get("0xa")
	// $2k bet @ price 0.02 (odds 50) — top rung -> critical, even with no baseline.
	loop.Observe(context.Background(), m, bet(100_000, 0.02, "edge", now))
	got := emit.of(anomaly.KindTradeAnomaly)
	if len(got) != 1 {
		t.Fatalf("got %d findings", len(got))
	}
	if got[0].Reason != anomaly.ReasonHighOdds {
		t.Fatalf("reason: %s", got[0].Reason)
	}
	if got[0].Severity != anomaly.SeverityCritical {
		t.Fatalf("severity: %s", got[0].Severity)
	}
	if got[0].Trade.Odds < 49 || got[0].Trade.Odds > 51 {
		t.Fatalf("odds not propagated: %v", got[0].Trade.Odds)
	}
}

func TestHighOddsSilencedBelowFloor(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	loop, reg, emit := newLoop(t, now, anomaly.Thresholds{
		MultiplierLadder: []float64{1e9},
		OddsLadder:       []float64{3, 10, 25},
		MinTradeUSD:      10_000, // floor for odds path = 1000
	}, cluster.Config{Window: time.Hour, MinTrades: 99, MinUniqueWallets: 99})
	m, _ := reg.Get("0xa")
	// $50 at odds 1000 — too small to be interesting.
	loop.Observe(context.Background(), m, bet(50_000, 0.001, "noise", now))
	if got := emit.all(); len(got) != 0 {
		t.Fatalf("expected no fire below odds floor, got %+v", got)
	}
}

func TestWhaleAndHighOddsCombineToHighOddsWhale(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	loop, reg, emit := newLoop(t, now, anomaly.Thresholds{
		MultiplierLadder:       []float64{30, 100, 1000},
		OddsLadder:             []float64{3, 10, 25},
		MinTradeUSD:            10_000,
		MinBaselineTrades:      20,
		MinBaselineNotionalUSD: 1_000,
	}, cluster.Config{Window: time.Hour, MinTrades: 99, MinUniqueWallets: 99})
	m, _ := reg.Get("0xa")
	// Build baseline of $100 trades (30 * $100 = $3k).
	for i := 0; i < 30; i++ {
		loop.Observe(context.Background(), m, bet(200, 0.5, "wb", now))
	}
	// Whale + high odds: $50k at price 0.02 (odds 50). x500 multiplier, top odds rung.
	loop.Observe(context.Background(), m, bet(2_500_000, 0.02, "shark", now))
	got := emit.of(anomaly.KindTradeAnomaly)
	if len(got) != 1 {
		t.Fatalf("got %d", len(got))
	}
	if got[0].Reason != anomaly.ReasonHighOddsWhale {
		t.Fatalf("reason: %s want %s", got[0].Reason, anomaly.ReasonHighOddsWhale)
	}
	if got[0].Severity != anomaly.SeverityCritical {
		t.Fatalf("severity: %s", got[0].Severity)
	}
}

func TestCategoryWatchHardAlertFires(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	loop, reg, emit := newLoop(t, now, anomaly.Thresholds{
		MultiplierLadder: []float64{1e9}, // disable whale
		OddsLadder:       []float64{3},   // odds path always fires above floor
		MinTradeUSD:      100,
	}, cluster.Config{
		Window: time.Hour, MinTrades: 3, MinUniqueWallets: 3, MinTotalUSD: 10_000,
	})
	m, _ := reg.Get("0xa")
	wallets := []string{"shark-1", "shark-2", "shark-3"}
	for _, w := range wallets {
		// $4k at price 0.25 (odds 4) — odds rung crossed.
		loop.Observe(context.Background(), m, bet(16_000, 0.25, w, now))
	}
	if len(emit.of(anomaly.KindTradeAnomaly)) != 3 {
		t.Fatalf("expected 3 single-trade findings, got %d", len(emit.of(anomaly.KindTradeAnomaly)))
	}
	hard := emit.of(anomaly.KindCategoryWatch)
	if len(hard) != 1 {
		t.Fatalf("expected exactly 1 category-watch alert, got %d", len(hard))
	}
	h := hard[0]
	if h.Severity != anomaly.SeverityHard {
		t.Fatalf("severity: %s", h.Severity)
	}
	if h.Reason != anomaly.ReasonCluster {
		t.Fatalf("cluster reason: %s", h.Reason)
	}
	if h.Cluster.UniqueWallets != 3 || h.Cluster.AnomalousTrades != 3 || h.Cluster.TotalUSD != 12_000 {
		t.Fatalf("cluster stats: %+v", h.Cluster)
	}
	if h.Category == nil || h.Category.Label != "Politics" {
		t.Fatalf("category: %+v", h.Category)
	}
}

// TestGrafanaURLEncoding pins that link building goes through net/url.
func TestGrafanaURLEncoding(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	at := time.Date(2026, 5, 17, 14, 30, 0, 0, time.UTC)
	loop, _, _ := newLoop(t, now, anomaly.Thresholds{MultiplierLadder: []float64{30}}, cluster.Config{Window: time.Hour, MinTrades: 99, MinUniqueWallets: 99})

	cases := []struct {
		name, categoryLbl, marketSlug string
		wantSubstrs, notContains      []string
	}{
		{"ascii", "Politics", "us-pres", []string{"var-category=Politics", "var-market=us-pres", "orgId=1"}, nil},
		{"space", "US Election", "us-pres-2028", []string{"var-category=US+Election"}, nil},
		{"reserved", "A & B = C", "x", []string{"var-category=A+%26+B+%3D+C"}, []string{"var-category=A & B = C"}},
		{"slash", "AI/ML", "x", []string{"var-category=AI%2FML"}, nil},
		{"unicode", "Café — Élections", "x", []string{"var-category=Caf%C3%A9+%E2%80%94+%C3%89lections"}, nil},
		{"hash", "#Trending", "x", []string{"var-category=%23Trending"}, nil},
		{"empty_market", "Politics", "", []string{"var-category=Politics"}, []string{"var-market="}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := loop.grafanaURL(anomaly.CategoryRef{Label: c.categoryLbl}, market.Market{Slug: c.marketSlug}, at)
			parsed, err := url.Parse(got)
			if err != nil {
				t.Fatalf("unparseable URL %q: %v", got, err)
			}
			q, err := url.ParseQuery(parsed.RawQuery)
			if err != nil {
				t.Fatalf("query round-trip failed: %v", err)
			}
			if c.categoryLbl != "" && q.Get("var-category") != c.categoryLbl {
				t.Errorf("var-category: got %q want %q", q.Get("var-category"), c.categoryLbl)
			}
			for _, want := range c.wantSubstrs {
				if !strings.Contains(got, want) {
					t.Errorf("URL missing %q in %s", want, got)
				}
			}
			for _, banned := range c.notContains {
				if strings.Contains(got, banned) {
					t.Errorf("URL must not contain %q: %s", banned, got)
				}
			}
		})
	}
}

func TestMarketURLUsesEventSlugNotMarketSlug(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	loop, _, _ := newLoop(t, now, anomaly.Thresholds{MultiplierLadder: []float64{30}}, cluster.Config{Window: time.Hour, MinTrades: 99, MinUniqueWallets: 99})
	loop.cfg.PolymarketBase = "https://polymarket.com/"
	tunisia := market.Market{Slug: "will-tunisia-win-the-2026-fifa-world-cup-165", EventSlug: "2026-fifa-world-cup-winner-595"}
	got := loop.marketURL(tunisia)
	want := "https://polymarket.com/event/2026-fifa-world-cup-winner-595"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	const broken = "https://polymarket.com/event/will-tunisia-win-the-2026-fifa-world-cup-165"
	if got == broken {
		t.Fatal("regression: emitted /event/<market-slug>")
	}
	if _, err := url.Parse(got); err != nil {
		t.Fatalf("unparseable URL: %v", err)
	}
}

func TestMarketURLEmptyWhenEventSlugMissing(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	loop, _, _ := newLoop(t, now, anomaly.Thresholds{MultiplierLadder: []float64{30}}, cluster.Config{Window: time.Hour, MinTrades: 99, MinUniqueWallets: 99})
	if got := loop.marketURL(market.Market{Slug: "orphan-market"}); got != "" {
		t.Fatalf("expected empty URL, got %q", got)
	}
}

func TestObserveConcurrencySafe(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	loop, reg, _ := newLoop(t, now, anomaly.Thresholds{
		MultiplierLadder:       []float64{30},
		OddsLadder:             []float64{3},
		MinTradeUSD:            100,
		MinBaselineTrades:      20,
		MinBaselineNotionalUSD: 10,
	}, cluster.Config{Window: time.Hour, MinTrades: 5, MinUniqueWallets: 3})
	m, _ := reg.Get("0xa")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				loop.Observe(context.Background(), m, bet(20, 0.5, "w", now))
			}
		}()
	}
	wg.Wait()
}
