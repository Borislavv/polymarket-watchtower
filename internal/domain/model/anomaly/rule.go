package anomaly

// Canonical alert reasons rendered in the Telegram header and metric labels.
const (
	ReasonSingle       = "LargeRareBet"
	ReasonCluster      = "WhaleClusterDetected"
	ReasonAccumulation = "SameTraderAccumulationLine"
	ReasonOwnership    = "MarketOwnershipConcentration"
)

// Structured reason codes attached to Finding.Reasons (and to
// AccumulationRef.Reasons) so an operator can read the firing shape at a
// glance without re-deriving it from raw numbers. The detector layer
// emits these; the formatter renders them verbatim.
//
// Conventions:
//   - Strategy-A codes describe the accumulation window.
//   - Strategy-B codes describe wallet age/history (context boosters
//     attached to single-trade or accumulation Findings — never
//     standalone).
//   - Strategy-E codes describe ownership concentration (only on
//     KindOwnership Findings).
//
// Codes that originate in a sub-package (e.g. POSSIBLE_MARKET_MAKER from
// mmfilter, QUIET_MARKET_WAKEUP from quietmarket) are owned by the
// emitting package and re-exported here only when widely referenced.
const (
	// ReasonLifetimeAccumulation — accumulation Finding with
	// Window=lifetime: a slow-drip line spanning the wallet's full
	// stored history on this (market, outcome, side).
	ReasonLifetimeAccumulation = "LIFETIME_ACCUMULATION"
	// ReasonRecentAccumulation — accumulation Finding with
	// Window=recent: a burst line inside the short-window cap.
	ReasonRecentAccumulation = "RECENT_ACCUMULATION"

	// ReasonNewWalletLargeBet — single-trade Finding fired on a wallet
	// with very short stored history. Context booster — does not
	// promote severity, only annotates the alert.
	ReasonNewWalletLargeBet = "NEW_WALLET_LARGE_BET"
	// ReasonNewWalletAccumulation — accumulation Finding on a new
	// wallet. Same booster semantics as ReasonNewWalletLargeBet.
	ReasonNewWalletAccumulation = "NEW_WALLET_ACCUMULATION"
	// ReasonLowTraderHistory — wallet has fewer than the new-wallet
	// trade-count threshold. Surfaced on either Finding kind.
	ReasonLowTraderHistory = "LOW_TRADER_HISTORY"

	// ReasonMarketOwnershipConcentration — wallet has accumulated a
	// significant share of the outcome's trade-flow volume. Attached
	// only to KindOwnership Findings.
	ReasonMarketOwnershipConcentration = "MARKET_OWNERSHIP_CONCENTRATION"
	// ReasonWalletDominatesOutcome — wallet's share crossed the
	// highest (Critical) ownership tier.
	ReasonWalletDominatesOutcome = "WALLET_DOMINATES_OUTCOME"
)

// Tier is one severity rung on the single-trade ladder. A trade qualifies at
// a tier only when it clears EVERY non-zero floor declared by that tier.
// Tier semantics (v5 — tail+payoff strategy):
//
//   - MinNotionalUSD : absolute trade-size floor (spam filter).
//   - MinOdds        : implied-odds floor (= 1/price); guards against
//     near-even-money bets where there is no asymmetric-payoff angle.
//   - MinProfitUSD   : payoff floor — `notional × (odds−1)`. Filters out
//     "big bet at fair odds" shapes that scale notional without paying
//     the operator's attention.
//   - MinMarketP95Ratio / MinMarketP99Ratio : the trade must sit in the
//     tail of the per-bucket market distribution. Ratio is
//     `notional / marketP*`. Gate enforced only when the market
//     baseline is ready.
//   - MinTraderP95Ratio / MinTraderP99Ratio : same shape against the
//     wallet's own history distribution. Gate enforced only when the
//     trader baseline is ready (count ≥ Thresholds.MinBaselineTrades).
//
// A zero floor disables that specific gate. The median multiplier is no
// longer a deciding gate — see the package doc on score.Score.
type Tier struct {
	MinNotionalUSD float64

	MinOdds float64

	MinProfitUSD float64

	MinMarketP95Ratio float64
	MinMarketP99Ratio float64

	MinTraderP95Ratio float64
	MinTraderP99Ratio float64

	// MinMultiplier is the line-total / market-median floor used by the
	// accumulation detector only. Single-trade scoring ignores it — the
	// p95/p99 tail ratios above are the v5 replacement for the per-trade
	// median multiplier gate.
	MinMultiplier float64
}

// Thresholds defines the three single-trade severity rungs (Info, Warning,
// Critical) and the baseline-readiness floors required before the tail
// gates can be enforced.
//
// Final severity is the HIGHEST tier whose every configured gate clears.
// "Conservative-MIN of two ladders" is gone — there is now a single
// per-tier evaluation that combines absolute size, payoff, and tail.
//
// Single-trade severity caps at Critical. HARD is reserved for the
// cluster detector (multiple sharks converging on one category) — a
// qualitatively different signal that warrants human review.
type Thresholds struct {
	Info     Tier
	Warning  Tier
	Critical Tier

	// MinBaselineTrades is the minimum sample count required on a
	// baseline (market OR trader) before its tail percentiles can be
	// trusted. Below this, gates against that side are skipped (left
	// unenforced) rather than failed.
	MinBaselineTrades int
	// MinBaselineNotionalUSD is the minimum aggregate USD in the
	// market reservoir required before the market tail gates are
	// trusted. Does not apply to the trader axis — a small wallet's
	// p95 is meaningful at low total USD as long as the count clears.
	MinBaselineNotionalUSD float64
}

// AbsoluteTier returns the highest rung where notional AND odds both clear
// the rung's floors, or "" when none qualifies. Retained as a public helper
// for the tier-by-tier evaluator and for diagnostic introspection.
func (t Thresholds) AbsoluteTier(notionalUSD, odds float64) Severity {
	switch {
	case notionalUSD >= t.Critical.MinNotionalUSD && odds >= t.Critical.MinOdds:
		return SeverityCritical
	case notionalUSD >= t.Warning.MinNotionalUSD && odds >= t.Warning.MinOdds:
		return SeverityWarning
	case notionalUSD >= t.Info.MinNotionalUSD && odds >= t.Info.MinOdds:
		return SeverityInfo
	}
	return ""
}

// ConservativeMin returns the lower (more conservative) of two non-empty
// severities. Either side empty ⇒ "". Retained because the cluster path
// and some accumulation paths still compose severities pairwise.
func ConservativeMin(a, b Severity) Severity {
	if a == "" || b == "" {
		return ""
	}
	if rank(a) <= rank(b) {
		return a
	}
	return b
}
