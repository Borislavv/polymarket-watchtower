package anomaly

// Canonical alert reasons rendered in the Telegram header and metric labels.
const (
	ReasonSingle       = "LargeRareBet"
	ReasonCluster      = "WhaleClusterDetected"
	ReasonAccumulation = "SameTraderAccumulationLine"
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
