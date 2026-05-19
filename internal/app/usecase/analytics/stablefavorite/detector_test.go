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
	in.Window6h.Last = 0.55 // -0.20 drop, beyond v7 MaxAdverseMove6h=0.15
	v := d.Decide(in)
	if v.Fired {
		t.Fatalf("must not fire on adverse 6h drift: %+v", v)
	}
	if v.SuppressedReason != SkipAdverseDrift {
		t.Errorf("suppression: got %q want %q", v.SuppressedReason, SkipAdverseDrift)
	}
}

// TestRiskAdjustedReturnStamped pins the v7 computation: edge per
// unit of price noise. RiskAdjustedReturn = RemainingReturnPct /
// (stddev × 100). The fixture has price 0.65 → return ≈ 53.85%,
// stddev = 0.015 → 53.85 / 1.5 ≈ 35.9.
func TestRiskAdjustedReturnStamped(t *testing.T) {
	d := New(cfg())
	v := d.Decide(baseInput())
	if !v.Fired {
		t.Fatalf("expected fire: %+v", v)
	}
	want := v.RemainingReturnPct / (0.015 * 100)
	if d := v.RiskAdjustedReturn - want; d > 0.001 || d < -0.001 {
		t.Errorf("risk_adjusted_return: got %.3f want %.3f", v.RiskAdjustedReturn, want)
	}
	if !containsReason(v.Reasons, anomaly.ReasonRiskAdjustedReturn) {
		t.Errorf("expected RISK_ADJUSTED_RETURN tag in %v", v.Reasons)
	}
}

// TestRiskAdjustedReturnZeroOnZeroStddev pins the divide-by-zero
// fall-back: stddev=0 → ratio=0 (degenerate, not Inf).
func TestRiskAdjustedReturnZeroOnZeroStddev(t *testing.T) {
	d := New(cfg())
	in := baseInput()
	in.Window24h.Stddev = 0
	v := d.Decide(in)
	if v.RiskAdjustedReturn != 0 {
		t.Errorf("risk_adjusted_return on zero stddev: %v want 0", v.RiskAdjustedReturn)
	}
}

// TestHypeMarketSuppressionDowngrades pins the v7 contract: a market
// whose 24h volume is dramatically elevated AND ≥50% of it landed
// in the last 6h gets a one-tier severity downgrade plus the
// HYPE_MARKET_SUPPRESSION reason tag.
func TestHypeMarketSuppressionDowngrades(t *testing.T) {
	d := New(cfg())
	in := baseInput()
	// First, get the un-suppressed severity for comparison.
	in.LifecyclePct = 99
	in.Window24h.VolumeUSD = 250_000
	in.Window24h.Count = 200
	in.Window24h.Stddev = 0.005
	in.Window6h.VolumeUSD = 20_000 // 8% recent share — NOT hype
	v0 := d.Decide(in)
	if !v0.Fired || v0.Severity != anomaly.SeverityCritical {
		t.Fatalf("expected critical baseline, got: %+v", v0)
	}

	// Now flip Window6h to dominate the 24h volume → hype trips.
	in.Window6h.VolumeUSD = 200_000
	v := d.Decide(in)
	if !v.Fired {
		t.Fatalf("hype should NOT fully suppress firing: %+v", v)
	}
	if !v.HypeSuppressed {
		t.Error("HypeSuppressed flag must be set")
	}
	if !containsReason(v.Reasons, anomaly.ReasonHypeMarketSuppression) {
		t.Errorf("missing HYPE_MARKET_SUPPRESSION tag: %v", v.Reasons)
	}
	if v.Severity != anomaly.SeverityWarning {
		t.Errorf("hype must downgrade Critical→Warning: got %s", v.Severity)
	}
}

// TestVolatilityEventPendingTagsOnly pins that the recent-vol-vs-
// 24h-vol ratio surfaces a context tag without changing severity.
// The flag is operator-facing context; downgrading on top of the
// score-already-considers-stability blend was rejected as
// over-engineering.
func TestVolatilityEventPendingTagsOnly(t *testing.T) {
	d := New(cfg())
	in := baseInput()
	in.LifecyclePct = 99
	in.Window24h.VolumeUSD = 250_000
	in.Window24h.Count = 200
	in.Window24h.Stddev = 0.005
	// Boost recent stddev to > 1.5× the 24h stddev.
	in.Window6h.Stddev = 0.020 // 4× → clearly above the threshold
	in.Window6h.Count = 30
	v := d.Decide(in)
	if !v.Fired {
		t.Fatalf("must still fire — volatility-event-pending is tag-only: %+v", v)
	}
	if !containsReason(v.Reasons, anomaly.ReasonVolatilityEventPending) {
		t.Errorf("expected VOLATILITY_EVENT_PENDING tag in %v", v.Reasons)
	}
	// Severity must reach Critical — the tag is annotation only.
	if v.Severity != anomaly.SeverityCritical {
		t.Errorf("v7: volatility tag must NOT downgrade severity (got %s)", v.Severity)
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

// TestCriticalAllowedWithoutCrossMarket pins the v7 contract:
// cross-market is NOT a hard gate. A high-confidence, high-score,
// late-lifecycle alert can reach Critical even when the cross-market
// price is unavailable. (Cross-market still contributes to score
// and confidence — see scoreOf / confidenceOf.)
func TestCriticalAllowedWithoutCrossMarket(t *testing.T) {
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
	if v.Severity != anomaly.SeverityCritical {
		t.Errorf("v7: severity should reach Critical without cross-market (got %s)", v.Severity)
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

// TestStabilityWindowDefaultsAreApplied pins the v7 relaxed defaults.
// applyDefaults must match the env defaults in StableFavoriteConfig —
// drift between the two is a footgun (CLI subcommands instantiate
// Config{} directly and get applyDefaults, while the app reads env).
func TestStabilityWindowDefaultsAreApplied(t *testing.T) {
	c := Config{Enabled: true}.applyDefaults()
	if c.MinLifecyclePct != 92 {
		t.Errorf("MinLifecyclePct drift: %v", c.MinLifecyclePct)
	}
	if c.HotLifecyclePct != 97 {
		t.Errorf("HotLifecyclePct drift: %v", c.HotLifecyclePct)
	}
	if c.MinProbability != 0.55 {
		t.Errorf("MinProbability drift: %v", c.MinProbability)
	}
	if c.MaxProbability != 0.85 {
		t.Errorf("MaxProbability drift: %v", c.MaxProbability)
	}
	// v7 relaxation values:
	if c.StabilityWindow != 6*time.Hour {
		t.Errorf("v7 StabilityWindow drift: got %v want 6h", c.StabilityWindow)
	}
	if c.MaxPriceStddev != 0.10 {
		t.Errorf("v7 MaxPriceStddev drift: %v want 0.10", c.MaxPriceStddev)
	}
	if c.MaxDrawdown != 0.25 {
		t.Errorf("v7 MaxDrawdown drift: %v want 0.25", c.MaxDrawdown)
	}
	if c.MaxAdverseMove6h != 0.15 {
		t.Errorf("v7 MaxAdverseMove6h drift: %v want 0.15", c.MaxAdverseMove6h)
	}
	if c.MaxNegativeDrift6h != 0.10 {
		t.Errorf("v7 MaxNegativeDrift6h drift: %v want 0.10", c.MaxNegativeDrift6h)
	}
	if c.MinRecentTrades != 20 {
		t.Errorf("MinRecentTrades drift: %d", c.MinRecentTrades)
	}
}
