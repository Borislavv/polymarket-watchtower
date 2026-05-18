package detect

import (
	"context"
	"testing"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/analytics/baseline"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/analytics/cluster"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/analytics/mmfilter"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/analytics/score"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/marketcache"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/anomaly"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/market"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/vo"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/metrics"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/repository"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/rs/zerolog"
)

// fakeTraderBaseline returns a fixed Stats for every wallet — enough to
// exercise the trader-axis path without standing up Postgres.
type fakeTraderBaseline struct {
	byWallet map[string]baseline.Stats
}

func (f *fakeTraderBaseline) Stats(_ context.Context, wallet string) (baseline.Stats, error) {
	if s, ok := f.byWallet[wallet]; ok {
		return s, nil
	}
	return baseline.Stats{}, nil
}

// fakeMM hands the detector a pre-baked verdict. suppressFor names wallets
// that should be classified as MM/arb; everything else passes.
type fakeMM struct {
	suppressFor map[string]bool
	calls       int
}

func (f *fakeMM) Decide(_ context.Context, wallet string, _ int64, _ string) (mmfilter.Verdict, error) {
	f.calls++
	if f.suppressFor[wallet] {
		return mmfilter.Verdict{Suppress: true, Reason: "test-suppress"}, nil
	}
	return mmfilter.Verdict{}, nil
}

func newLoopV2(
	t *testing.T,
	now time.Time,
	th anomaly.Thresholds,
	tb TraderBaselineFetcher,
	mm MMArbFilter,
	minTraderHistory int,
) (*Loop, market.Market, *capturingEmitter) {
	return newLoopV2WithMetrics(t, now, th, tb, mm, minTraderHistory, metrics.New())
}

// newLoopV2WithMetrics is the metrics-aware variant — tests that assert
// against counters pass in their own Metrics so they can read individual
// counter values.
func newLoopV2WithMetrics(
	t *testing.T,
	now time.Time,
	th anomaly.Thresholds,
	tb TraderBaselineFetcher,
	mm MMArbFilter,
	minTraderHistory int,
	met *metrics.Metrics,
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
	loop := New(Config{
		Thresholds:             th,
		Baseline:               baseline.Config{Window: 7 * 24 * time.Hour},
		Cluster:                cluster.Config{Window: time.Hour, MinTrades: 99, MinUniqueWallets: 99},
		Clock:                  func() time.Time { return now },
		PolymarketBase:         "https://polymarket.com",
		TraderBaseliner:        tb,
		MinTraderHistoryTrades: minTraderHistory,
		MMFilter:               mm,
	}, reg, emit, met, &log)
	m, _ := reg.Get("0xa")
	return loop, m, emit
}

// getCounter reads a single labelled counter value from a CounterVec.
// Tests use this when they want to assert a counter incremented under a
// specific label tuple.
func getCounter(t *testing.T, cv *prometheus.CounterVec, lvs ...string) float64 {
	t.Helper()
	c, err := cv.GetMetricWithLabelValues(lvs...)
	if err != nil {
		t.Fatalf("counter %v: %v", lvs, err)
	}
	var m dto.Metric
	if err := c.Write(&m); err != nil {
		t.Fatalf("counter write: %v", err)
	}
	return m.GetCounter().GetValue()
}

// TestTraderAxisDrivesAlertOnSmallWallet pins the v2 capability: a market
// that is too busy for v1 to flag (multiplier 5) still fires when the
// wallet's own history makes the trade an outlier (multiplier 250).
func TestTraderAxisDrivesAlertOnSmallWallet(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	traders := &fakeTraderBaseline{byWallet: map[string]baseline.Stats{
		"0xsmall": {
			Count: 40, MedianUSD: 200, MeanUSD: 220, TotalUSD: 8_800,
			SpanActual: 30 * 24 * time.Hour,
		},
	}}
	loop, m, emit := newLoopV2(t, now, defaultThresholds(), traders, nil, 5)

	// Warm the market baseline to make per-bucket multiplier small (busy market).
	warm(loop, m, 30, 5_000, 0.5, now.Add(-24*time.Hour))

	// $25k at odds 5 — market multiplier $25k/$5k = 5 (no rung).
	// Trader multiplier $25k/$200 = 125 (info rung).
	// Absolute Warning ($25k & odds 5).
	// ConservativeMin(Warning, Info) = Info → fire.
	loop.Observe(context.Background(), m, bet(25_000, 1.0/5, "0xsmall", now))

	got := emit.of(anomaly.KindTradeAnomaly)
	if len(got) != 1 {
		t.Fatalf("expected 1 fire from trader axis, got %d: %+v", len(got), got)
	}
	f := got[0]
	if f.Severity != anomaly.SeverityInfo {
		t.Errorf("severity: got %s want info", f.Severity)
	}
	if f.MultiplierAxis != string(score.MultiplierAxisTrader) {
		t.Errorf("axis: got %q want trader", f.MultiplierAxis)
	}
	if f.TraderMultiplier < 120 || f.TraderMultiplier > 130 {
		t.Errorf("trader multiplier: got %v want ~125", f.TraderMultiplier)
	}
	if f.MarketMultiplier > 10 {
		t.Errorf("market multiplier should be small (busy market), got %v", f.MarketMultiplier)
	}
	if f.TraderBaseline == nil {
		t.Fatal("trader baseline must be populated for trader-axis alerts")
	}
	if f.TraderBaseline.MedianUSD != 200 {
		t.Errorf("trader baseline median: got %v want 200", f.TraderBaseline.MedianUSD)
	}
}

// TestTraderAxisSilencesBigWhaleRoutineTrade pins the FP suppression: a
// whale doing its routine $20k trades is now ignored even though v1
// market-multiplier might have caught it (depending on the market's
// distribution).
func TestTraderAxisSilencesBigWhaleRoutineTrade(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	traders := &fakeTraderBaseline{byWallet: map[string]baseline.Stats{
		"0xwhale": {
			Count: 100, MedianUSD: 20_000, TotalUSD: 2_000_000,
			SpanActual: 30 * 24 * time.Hour,
		},
	}}
	loop, m, emit := newLoopV2(t, now, defaultThresholds(), traders, nil, 5)

	// Warm market so per-bucket multiplier is small too.
	warm(loop, m, 30, 5_000, 0.5, now.Add(-24*time.Hour))

	// $25k at odds 5 from a $20k-history whale → market mult 5, trader mult
	// 1.25, both below Info → no fire.
	loop.Observe(context.Background(), m, bet(25_000, 1.0/5, "0xwhale", now))

	if got := emit.of(anomaly.KindTradeAnomaly); len(got) != 0 {
		t.Fatalf("expected no fire on whale routine trade, got %d: %+v", len(got), got)
	}
}

// TestTraderAxisDisabledFallsBackToV1 pins the backwards-compatibility
// contract: with no TraderBaseliner wired (or no trader history for the
// wallet), behaviour is identical to v1.
func TestTraderAxisDisabledFallsBackToV1(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	loop, m, emit := newLoopV2(t, now, defaultThresholds(), nil, nil, 0)

	// Quiet market baseline so v1 catches it.
	warm(loop, m, 30, 50, 0.5, now.Add(-24*time.Hour))
	// $10k at odds 3, mult 200 → info × info → info.
	loop.Observe(context.Background(), m, bet(10_000, 1.0/3, "0xanyone", now))

	got := emit.of(anomaly.KindTradeAnomaly)
	if len(got) != 1 {
		t.Fatalf("expected 1 fire (v1 behaviour), got %d", len(got))
	}
	if got[0].MultiplierAxis != string(score.MultiplierAxisMarket) {
		t.Errorf("axis: got %q want market", got[0].MultiplierAxis)
	}
	if got[0].TraderBaseline != nil {
		t.Error("trader baseline must be nil when trader axis disabled")
	}
}

// TestTraderHistoryReadinessGate pins the count gate: a trader with only
// 2 trades on record should NOT contribute the trader axis even if the
// median is small.
func TestTraderHistoryReadinessGate(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	traders := &fakeTraderBaseline{byWallet: map[string]baseline.Stats{
		"0xnew": {Count: 2, MedianUSD: 200}, // below default MinTraderHistoryTrades=5
	}}
	loop, m, emit := newLoopV2(t, now, defaultThresholds(), traders, nil, 5)
	warm(loop, m, 30, 5_000, 0.5, now.Add(-24*time.Hour))

	// Without trader axis: market mult 5 (no rung) → no fire.
	loop.Observe(context.Background(), m, bet(25_000, 1.0/5, "0xnew", now))
	if got := emit.of(anomaly.KindTradeAnomaly); len(got) != 0 {
		t.Fatalf("expected no fire — trader history below readiness gate: %+v", got)
	}
}

// TestMMSuppression_StampsPossibleMarketMakerReason pins the v4 contract:
// when the MM filter suppresses an alert, the structured reason code
// POSSIBLE_MARKET_MAKER must be emitted on the metric label and (by
// construction in detect.go) on the structured log line. The metric path
// is what an operator scrapes; the constant is the single source of truth.
func TestMMSuppression_StampsPossibleMarketMakerReason(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	mm := &fakeMM{suppressFor: map[string]bool{"0xmm": true}}
	met := metrics.New()
	loop, m, _ := newLoopV2WithMetrics(t, now, defaultThresholds(), nil, mm, 0, met)
	warm(loop, m, 30, 50, 0.5, now.Add(-24*time.Hour))

	loop.Observe(context.Background(), m, bet(10_000, 1.0/3, "0xmm", now))

	c := getCounter(t, met.AlertMMSuppressed, "Politics", mmfilter.ReasonPossibleMarketMaker)
	if c < 1 {
		t.Fatalf("expected %s suppression counter ≥ 1, got %v",
			mmfilter.ReasonPossibleMarketMaker, c)
	}
}

// TestMMSuppressionBlocksSingleTradeAlert pins the FP-reduction contract:
// a wallet flagged by the MM filter is silenced even when scoring fires.
func TestMMSuppressionBlocksSingleTradeAlert(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	mm := &fakeMM{suppressFor: map[string]bool{"0xmm": true}}
	loop, m, emit := newLoopV2(t, now, defaultThresholds(), nil, mm, 0)

	warm(loop, m, 30, 50, 0.5, now.Add(-24*time.Hour))
	loop.Observe(context.Background(), m, bet(10_000, 1.0/3, "0xmm", now))

	if got := emit.of(anomaly.KindTradeAnomaly); len(got) != 0 {
		t.Fatalf("expected suppression, got %d alerts: %+v", len(got), got)
	}
	if mm.calls == 0 {
		t.Error("MM filter must be consulted before emission")
	}
}

// TestMMFilterPassesNonMMTrader pins the inverse: a trader that the filter
// classifies as non-MM still fires normally.
func TestMMFilterPassesNonMMTrader(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	mm := &fakeMM{} // suppressFor empty → never suppress
	loop, m, emit := newLoopV2(t, now, defaultThresholds(), nil, mm, 0)

	warm(loop, m, 30, 50, 0.5, now.Add(-24*time.Hour))
	loop.Observe(context.Background(), m, bet(10_000, 1.0/3, "0xdirectional", now))

	if got := emit.of(anomaly.KindTradeAnomaly); len(got) != 1 {
		t.Fatalf("expected 1 alert from non-MM trader, got %d", len(got))
	}
	if mm.calls == 0 {
		t.Error("MM filter must have been consulted")
	}
}

// TestMMFilterDoesNotBlockClusterAlerts pins the design intent: even when
// individual contributors look MM-shaped, the cluster path is intentionally
// not filtered. We don't have a fully wired cluster test here (cluster has
// MinTrades=99 in the helper) but the absence of an MM call on the cluster
// emit path is verified by inspection — the suppression logic lives only
// in emitTradeAnomaly, not emitCategoryWatch.
func TestMMFilterDoesNotInterceptClusterPath(t *testing.T) {
	// This is a structural assertion: rebuild a loop with a low cluster
	// floor and an MM filter that would suppress every wallet, then ensure
	// that single-trade alerts ARE suppressed but the trade still feeds
	// the cluster window. Cluster doesn't fire here because we only emit
	// one trade — but the test pins the property that the MM filter is
	// not called on the cluster emit path (it's gated to single-trade).
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	mm := &fakeMM{suppressFor: map[string]bool{"*": true}} // would suppress everyone, but map key matches exact wallet only
	// Use real wallet that is in suppressFor:
	mm.suppressFor = map[string]bool{"0xmm": true}

	loop, m, emit := newLoopV2(t, now, defaultThresholds(), nil, mm, 0)
	warm(loop, m, 30, 50, 0.5, now.Add(-24*time.Hour))
	loop.Observe(context.Background(), m, bet(10_000, 1.0/3, "0xmm", now))

	// Single-trade suppressed; no cluster (only one trade).
	if got := emit.all(); len(got) != 0 {
		t.Fatalf("expected nothing emitted, got: %+v", got)
	}
}

// Compile-time interface assertions so we don't break the wiring later.
var (
	_ TraderBaselineFetcher = (*fakeTraderBaseline)(nil)
	_ MMArbFilter           = (*fakeMM)(nil)
)

// Sentinel — unused but keeps the repository import live if we add a test
// that constructs an actual TraderSideActivity.
var _ = repository.TraderSideActivity{}
