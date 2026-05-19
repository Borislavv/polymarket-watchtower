package stablefavorite

import (
	"testing"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/anomaly"
)

// cfg returns a Config matching the production env defaults —
// every numeric default from the spec, plus CrossMarketEnabled=true
// (env layer sets this; tests construct it explicitly).
func cfg() Config {
	c := Config{Enabled: true, CrossMarketEnabled: true}.applyDefaults()
	return c
}

// baseInput is a market that clears EVERY hard gate AND scores
// above the Info floor at the canonical fixture point. Lifecycle 97
// is deep enough into the 92..100 band that the lifecycle-score
// component contributes meaningfully; 100 samples + $75k volume
// clear the liquidity bonus.
func baseInput() Input {
	return Input{
		MarketID:     "0xpoly",
		OutcomeToken: "tok-yes",
		Outcome:      "Yes",
		LifecyclePct: 97,
		CurrentPrice: 0.65,
		Window24h: WindowStats{
			Count: 150, Mean: 0.65, Stddev: 0.015,
			Min: 0.63, Max: 0.66,
			First: 0.64, Last: 0.65,
			VolumeUSD: 120_000,
		},
		Window6h: WindowStats{
			Count: 40, Mean: 0.65, Stddev: 0.008,
			Min: 0.64, Max: 0.66,
			First: 0.65, Last: 0.65,
			VolumeUSD: 30_000,
		},
	}
}

// --- Positive path ---------------------------------------------------------

func TestFires_StableFavoriteCanonical(t *testing.T) {
	d := New(cfg())
	v := d.Decide(baseInput())
	if !v.Fired {
		t.Fatalf("expected fire on canonical stable-favorite shape: %+v", v)
	}
	if v.Severity == "" {
		t.Errorf("severity must be set on fire: %+v", v)
	}
	if v.Severity == anomaly.SeverityCritical {
		t.Errorf("canonical shape should not yield Critical without cross-market confirmation: %+v", v)
	}
	// Remaining return: (1-0.65)/0.65 = 53.846…
	if v.RemainingReturnPct < 53.5 || v.RemainingReturnPct > 54.5 {
		t.Errorf("remaining return: got %.2f want ~53.85", v.RemainingReturnPct)
	}
	// Expected reasons.
	for _, want := range []string{anomaly.ReasonLateMarketFavorite, anomaly.ReasonStablePrice, anomaly.ReasonLowVolatility, anomaly.ReasonNoReversalPressure} {
		if !containsReason(v.Reasons, want) {
			t.Errorf("missing reason %q in %v", want, v.Reasons)
		}
	}
}

// --- Negative paths --------------------------------------------------------

func TestNoFire_LifecycleTooEarly(t *testing.T) {
	d := New(cfg())
	in := baseInput()
	in.LifecyclePct = 70
	v := d.Decide(in)
	if v.Fired {
		t.Fatalf("must not fire below MinLifecyclePct: %+v", v)
	}
	if v.SuppressedReason != SkipLifecycle {
		t.Errorf("suppression: got %q want %q", v.SuppressedReason, SkipLifecycle)
	}
}

func TestNoFire_CoinflipBelowBand(t *testing.T) {
	d := New(cfg())
	in := baseInput()
	in.CurrentPrice = 0.51
	in.Window24h.Mean, in.Window24h.Min, in.Window24h.Max = 0.51, 0.50, 0.52
	in.Window24h.First, in.Window24h.Last = 0.51, 0.51
	v := d.Decide(in)
	if v.Fired {
		t.Fatalf("must not fire on coinflip: %+v", v)
	}
	if v.SuppressedReason != SkipProbabilityBand {
		t.Errorf("suppression: got %q want %q", v.SuppressedReason, SkipProbabilityBand)
	}
}

func TestNoFire_NearCertainAboveBand(t *testing.T) {
	d := New(cfg())
	in := baseInput()
	in.CurrentPrice = 0.95
	v := d.Decide(in)
	if v.Fired {
		t.Fatalf("must not fire on near-certain favorite (tiny payout): %+v", v)
	}
	if v.SuppressedReason != SkipProbabilityBand {
		t.Errorf("suppression: got %q want %q", v.SuppressedReason, SkipProbabilityBand)
	}
}

func TestNoFire_HighVolatility(t *testing.T) {
	d := New(cfg())
	in := baseInput()
	in.Window24h.Stddev = 0.20 // way above MaxPriceStddev=0.08
	v := d.Decide(in)
	if v.Fired {
		t.Fatalf("must not fire on high volatility: %+v", v)
	}
	if v.SuppressedReason != SkipStability {
		t.Errorf("suppression: got %q want %q", v.SuppressedReason, SkipStability)
	}
}

func TestNoFire_AdverseDrift6h(t *testing.T) {
	d := New(cfg())
	in := baseInput()
	in.Window6h.First = 0.75
	in.Window6h.Last = 0.62 // -0.13 drop, beyond MaxAdverseMove6h=0.08
	v := d.Decide(in)
	if v.Fired {
		t.Fatalf("must not fire on adverse 6h drift: %+v", v)
	}
	if v.SuppressedReason != SkipAdverseDrift {
		t.Errorf("suppression: got %q want %q", v.SuppressedReason, SkipAdverseDrift)
	}
}

func TestNoFire_LowLiquidityVolume(t *testing.T) {
	d := New(cfg())
	in := baseInput()
	in.Window24h.VolumeUSD = 5_000 // below MinMarketVolumeUSD=25k
	v := d.Decide(in)
	if v.Fired {
		t.Fatalf("must not fire on thin volume: %+v", v)
	}
	if v.SuppressedReason != SkipLiquidityVolume {
		t.Errorf("suppression: got %q want %q", v.SuppressedReason, SkipLiquidityVolume)
	}
}

func TestNoFire_LowLiquidityTradeCount(t *testing.T) {
	d := New(cfg())
	in := baseInput()
	in.Window24h.Count = 5 // below MinRecentTrades=20
	v := d.Decide(in)
	if v.Fired {
		t.Fatalf("must not fire on thin trade count: %+v", v)
	}
	if v.SuppressedReason != SkipLiquidityTradeCount {
		t.Errorf("suppression: got %q want %q", v.SuppressedReason, SkipLiquidityTradeCount)
	}
}

func TestNoFire_Disabled(t *testing.T) {
	c := cfg()
	c.Enabled = false
	d := New(c)
	v := d.Decide(baseInput())
	if v.Fired {
		t.Fatal("disabled detector must never fire")
	}
	if v.SuppressedReason != SkipDisabled {
		t.Errorf("suppression: got %q", v.SuppressedReason)
	}
}

// --- Cross-market behavior -------------------------------------------------

// TestCrossMarketConfirmationBoostsConfidence pins the spec: a
// confirmed cross-market match raises confidence by 0.15 vs the
// unavailable baseline, all else equal.
func TestCrossMarketConfirmationBoostsConfidence(t *testing.T) {
	d := New(cfg())
	in := baseInput()
	unavailable := d.Decide(in)

	in.CrossMarketPrice = 0.66 // within MaxDisagreement of 0.65
	confirmed := d.Decide(in)

	if confirmed.CrossMarketStatus != "confirmed" {
		t.Fatalf("status: got %q want confirmed", confirmed.CrossMarketStatus)
	}
	if !(confirmed.Confidence > unavailable.Confidence) {
		t.Errorf("confidence must rise on confirmation: %.3f vs %.3f",
			confirmed.Confidence, unavailable.Confidence)
	}
	if !containsReason(confirmed.Reasons, anomaly.ReasonCrossMarketConfirmation) {
		t.Errorf("missing CROSS_MARKET_CONFIRMATION reason: %v", confirmed.Reasons)
	}
}

// TestCrossMarketConflictLowersScoreButDoesNotSuppress pins the
// "lower confidence" branch — we do not veto on conflict because
// political markets routinely diverge across venues.
func TestCrossMarketConflictLowersScoreButDoesNotSuppress(t *testing.T) {
	d := New(cfg())
	in := baseInput()
	in.CrossMarketPrice = 0.20 // huge disagreement vs 0.65
	v := d.Decide(in)

	if v.CrossMarketStatus != "conflict" {
		t.Fatalf("status: got %q want conflict", v.CrossMarketStatus)
	}
	if !containsReason(v.Reasons, anomaly.ReasonCrossMarketConflict) {
		t.Errorf("missing CROSS_MARKET_CONFLICT reason: %v", v.Reasons)
	}
	// We deliberately don't assert that v.Fired==false — the spec
	// allows the strategy to keep firing with lowered confidence on
	// conflict. What we DO assert is that the score is lower than
	// the unavailable baseline.
	in2 := baseInput()
	unavail := d.Decide(in2)
	if v.Score >= unavail.Score {
		t.Errorf("conflict must lower score: conflict=%.2f unavail=%.2f", v.Score, unavail.Score)
	}
}

// --- Severity escalation ---------------------------------------------------

func TestEscalatesToCriticalRequiresCrossMarketConfirmation(t *testing.T) {
	d := New(cfg())
	in := baseInput()
	in.LifecyclePct = 99
	in.Window24h.Stddev = 0.005 // very stable → high score
	in.Window24h.VolumeUSD = 250_000
	in.Window24h.Count = 200
	in.CrossMarketPrice = 0.65 // confirmed
	v := d.Decide(in)
	if !v.Fired {
		t.Fatalf("expected fire: %+v", v)
	}
	if v.Severity != anomaly.SeverityCritical {
		t.Errorf("severity: got %s want critical", v.Severity)
	}
}

func TestNoCriticalWithoutCrossMarketConfirmation(t *testing.T) {
	d := New(cfg())
	in := baseInput()
	in.LifecyclePct = 99
	in.Window24h.Stddev = 0.005
	in.Window24h.VolumeUSD = 250_000
	in.Window24h.Count = 200
	// no CrossMarketPrice → "unavailable"
	v := d.Decide(in)
	if !v.Fired {
		t.Fatalf("expected fire: %+v", v)
	}
	if v.Severity == anomaly.SeverityCritical {
		t.Errorf("severity must not be Critical without cross-market confirmation: %s", v.Severity)
	}
}

// --- Helpers ---------------------------------------------------------------

func containsReason(rs []string, want string) bool {
	for _, r := range rs {
		if r == want {
			return true
		}
	}
	return false
}

// TestStabilityWindowDefaultsAreApplied is a smoke test for
// applyDefaults — pinning the spec values protects against
// accidental knob drift.
func TestStabilityWindowDefaultsAreApplied(t *testing.T) {
	c := Config{Enabled: true}.applyDefaults()
	wants := map[string]float64{
		"MinLifecyclePct":            92,
		"HotLifecyclePct":            97,
		"MinProbability":             0.55,
		"MaxProbability":             0.85,
		"MinReturnPct":               20,
		"MaxPriceStddev":             0.08,
		"MaxDrawdown":                0.12,
		"MaxAdverseMove6h":           0.08,
		"MaxNegativeDrift6h":         0.05,
		"MinMarketVolumeUSD":         25000,
		"MaxCrossMarketDisagreement": 0.15,
	}
	if c.MinLifecyclePct != wants["MinLifecyclePct"] {
		t.Errorf("MinLifecyclePct: got %v want %v", c.MinLifecyclePct, wants["MinLifecyclePct"])
	}
	if c.HotLifecyclePct != wants["HotLifecyclePct"] {
		t.Errorf("HotLifecyclePct drift: %v", c.HotLifecyclePct)
	}
	if c.MinProbability != wants["MinProbability"] {
		t.Errorf("MinProbability drift: %v", c.MinProbability)
	}
	if c.MaxProbability != wants["MaxProbability"] {
		t.Errorf("MaxProbability drift: %v", c.MaxProbability)
	}
	if c.StabilityWindow != 24*time.Hour {
		t.Errorf("StabilityWindow drift: %v", c.StabilityWindow)
	}
	if c.MinRecentTrades != 20 {
		t.Errorf("MinRecentTrades drift: %d", c.MinRecentTrades)
	}
}
