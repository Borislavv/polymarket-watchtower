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

// Tier is one rung on either the absolute (notional+odds) or multiplier ladder.
// A trade must clear ALL non-zero floors of a tier to qualify at that rung.
type Tier struct {
	// MinNotionalUSD is the trade USD notional floor.
	MinNotionalUSD float64
	// MinOdds is the implied-odds floor (= 1/price). 0 disables this gate
	// for the absolute ladder (rarely useful — operators usually keep it set).
	MinOdds float64
	// MinMultiplier is the (notional / baseline-median) floor on the
	// multiplier ladder. 0 disables this gate.
	MinMultiplier float64
}

// Thresholds defines the three single-trade severity rungs (Info, Warning,
// Critical) and the baseline-readiness floors required before any rung can
// be evaluated.
//
// A trade fires only when BOTH ladders qualify at Info or above:
//
//   - Absolute  : trade USD notional ≥ tier.MinNotionalUSD
//     AND implied odds (1/price) ≥ tier.MinOdds
//   - Multiplier: trade USD notional / baseline median ≥ tier.MinMultiplier
//
// Final severity is the *lower* of the two tier outcomes — the conservative
// minimum. This keeps precision high: a $1M bet at fair odds isn't called
// Critical just because the size is huge, and a 10,000× multiplier on a $500
// bet isn't called Critical just because the ratio is wild. Both signals
// must be present.
//
// Single-trade severity caps at Critical. HARD is reserved for the cluster
// detector (multiple sharks converging on one category) — a qualitatively
// different signal that warrants human review.
type Thresholds struct {
	Info     Tier
	Warning  Tier
	Critical Tier

	// MinBaselineTrades is the minimum sample count required on the bucket
	// reservoir before the multiplier ladder is evaluated.
	MinBaselineTrades int
	// MinBaselineNotionalUSD is the minimum aggregate USD in the reservoir
	// required before the multiplier ladder is evaluated.
	MinBaselineNotionalUSD float64
}

// AbsoluteTier returns the highest rung where notional AND odds both clear
// the rung's floors, or "" when none qualifies.
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

// MultiplierTier returns the highest rung the multiplier clears, or "" when
// it doesn't clear the Info rung.
func (t Thresholds) MultiplierTier(multiplier float64) Severity {
	switch {
	case multiplier >= t.Critical.MinMultiplier:
		return SeverityCritical
	case multiplier >= t.Warning.MinMultiplier:
		return SeverityWarning
	case multiplier >= t.Info.MinMultiplier:
		return SeverityInfo
	}
	return ""
}

// ConservativeMin returns the lower (more conservative) of two non-empty
// severities. Either side empty ⇒ "".
func ConservativeMin(a, b Severity) Severity {
	if a == "" || b == "" {
		return ""
	}
	if rank(a) <= rank(b) {
		return a
	}
	return b
}
