package detect

import (
	"context"
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
		Multipliers: []float64{30, 100, 1000}, AbsoluteUSDTiers: []float64{3_000, 10_000, 100_000}, MinBaselineTrades: 20,
	}, cluster.Config{Window: time.Hour, MinTrades: 5, MinUniqueWallets: 3})
	m, _ := reg.Get("0xa")
	// build baseline of 30 trades at $10 (notional)
	for i := 0; i < 30; i++ {
		loop.Observe(context.Background(), m, bet(20, 0.5, "w-base", now.Add(-time.Minute)))
	}
	// next trade at $10 — same as baseline, no fire
	loop.Observe(context.Background(), m, bet(20, 0.5, "w-new", now))
	if got := emit.all(); len(got) != 0 {
		t.Fatalf("expected no findings, got %d: %+v", len(got), got)
	}
}

func TestSingleTradeFiresMultiplierTiers(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	loop, reg, emit := newLoop(t, now, anomaly.Thresholds{
		Multipliers: []float64{30, 100, 1000}, AbsoluteUSDTiers: []float64{1e9}, MinBaselineTrades: 20,
	}, cluster.Config{Window: time.Hour, MinTrades: 99, MinUniqueWallets: 99}) // cluster effectively off
	m, _ := reg.Get("0xa")
	// baseline: 30 trades at notional $10 (size=20, price=0.5)
	for i := 0; i < 30; i++ {
		loop.Observe(context.Background(), m, bet(20, 0.5, "w-base", now))
	}
	// $300 single trade -> x30 -> info
	loop.Observe(context.Background(), m, bet(600, 0.5, "wA", now))
	// $1000 single trade -> x100 -> warning
	loop.Observe(context.Background(), m, bet(2_000, 0.5, "wB", now))
	// $10_000 single trade -> x1000 -> critical
	loop.Observe(context.Background(), m, bet(20_000, 0.5, "wC", now))

	got := emit.of(anomaly.KindTradeAnomaly)
	if len(got) != 3 {
		t.Fatalf("expected 3 single-trade findings, got %d: %+v", len(got), got)
	}
	wantSev := []anomaly.Severity{anomaly.SeverityInfo, anomaly.SeverityWarning, anomaly.SeverityCritical}
	for i, f := range got {
		if f.Severity != wantSev[i] {
			t.Errorf("[%d] severity: got %s want %s", i, f.Severity, wantSev[i])
		}
		if f.Trade == nil || f.Trade.Outcome != "Yes" {
			t.Errorf("[%d] trade ref: %+v", i, f.Trade)
		}
		if f.MarketURL != "https://polymarket.com/event/us-pres" {
			t.Errorf("[%d] market URL: %q", i, f.MarketURL)
		}
		if !strings.Contains(f.GrafanaURL, "var-category=Politics") || !strings.Contains(f.GrafanaURL, "var-market=us-pres") {
			t.Errorf("[%d] grafana URL: %q", i, f.GrafanaURL)
		}
	}
}

func TestLowBaselineSkipsMultiplierButAbsoluteStillFires(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	loop, reg, emit := newLoop(t, now, anomaly.Thresholds{
		Multipliers: []float64{30}, AbsoluteUSDTiers: []float64{10_000}, MinBaselineTrades: 50,
	}, cluster.Config{Window: time.Hour, MinTrades: 99, MinUniqueWallets: 99})
	m, _ := reg.Get("0xa")
	// only 5 baseline trades — well under MinBaselineTrades=50
	for i := 0; i < 5; i++ {
		loop.Observe(context.Background(), m, bet(20, 0.5, "wb", now))
	}
	// $50k trade — absolute tier should fire (multiplier skipped due to low N)
	loop.Observe(context.Background(), m, bet(100_000, 0.5, "whale", now))

	got := emit.of(anomaly.KindTradeAnomaly)
	if len(got) != 1 {
		t.Fatalf("expected 1 finding, got %d: %+v", len(got), got)
	}
	if got[0].Reason != "absolute_tier" {
		t.Fatalf("reason: %s", got[0].Reason)
	}
}

func TestCategoryWatchHardAlertFires(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	loop, reg, emit := newLoop(t, now, anomaly.Thresholds{
		Multipliers: []float64{30}, AbsoluteUSDTiers: []float64{3_000}, MinBaselineTrades: 1_000_000, // disable multiplier
	}, cluster.Config{
		Window: time.Hour, MinTrades: 3, MinUniqueWallets: 3, MinTotalUSD: 10_000,
	})
	m, _ := reg.Get("0xa")
	wallets := []string{"shark-1", "shark-2", "shark-3"}
	for _, w := range wallets {
		loop.Observe(context.Background(), m, bet(8_000, 0.5, w, now)) // each = $4k
	}
	if len(emit.of(anomaly.KindTradeAnomaly)) != 3 {
		t.Fatalf("expected 3 single-trade findings")
	}
	hard := emit.of(anomaly.KindCategoryWatch)
	if len(hard) != 1 {
		t.Fatalf("expected exactly 1 category-watch alert, got %d", len(hard))
	}
	h := hard[0]
	if h.Severity != anomaly.SeverityHard {
		t.Fatalf("severity: %s", h.Severity)
	}
	if h.Cluster.UniqueWallets != 3 || h.Cluster.AnomalousTrades != 3 || h.Cluster.TotalUSD != 12_000 {
		t.Fatalf("cluster stats: %+v", h.Cluster)
	}
	if h.Category == nil || h.Category.Label != "Politics" {
		t.Fatalf("category: %+v", h.Category)
	}
}

func TestObserveConcurrencySafe(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	loop, reg, _ := newLoop(t, now, anomaly.Thresholds{
		Multipliers: []float64{30}, AbsoluteUSDTiers: []float64{3_000}, MinBaselineTrades: 20,
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
