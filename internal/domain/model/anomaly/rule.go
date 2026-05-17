package anomaly

// Canonical alert reasons rendered in the Telegram header and metric labels.
const (
	ReasonSingle  = "LargeRareBet"
	ReasonCluster = "WhaleClusterDetected"
)

// Tier holds the three thresholds a trade must clear simultaneously to qualify
// at the given severity level on the *absolute* ladder.
type Tier struct {
	// MinNotionalUSD is the trade USD notional floor.
	MinNotionalUSD float64
	// MinOdds is the implied-odds floor (= 1/price). 0 disables the gate.
	MinOdds float64
	// MinMultiplier is the (notional / baseline median) floor used by the
	// rarity ladder. 0 disables the gate.
	MinMultiplier float64
}

// Thresholds drives the combined per-trade single_cluster detector. A trade
// fires only when BOTH ladders qualify at info or above:
//
//   - Absolute ladder: notional AND odds must clear the tier's floors.
//   - Multiplier ladder: notional / baseline-median must clear the tier's
//     floor; only evaluated when the baseline meets MinBaselineTrades and
//     MinBaselineNotionalUSD.
//
// Final severity is the *lower* of the two tier outcomes (conservative AND).
// Below-info on either side ⇒ no alert.
type Thresholds struct {
	Info     Tier
	Warning  Tier
	Critical Tier

	MinBaselineTrades      int
	MinBaselineNotionalUSD float64
}

// AbsoluteTier returns the highest tier where notional AND odds both meet the
// tier's floors, or "" when none qualifies.
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

// MultiplierTier returns the highest tier the multiplier clears, or "" when
// it doesn't clear the info rung.
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
// severities. If either is empty, returns "".
func ConservativeMin(a, b Severity) Severity {
	if a == "" || b == "" {
		return ""
	}
	if rank(a) <= rank(b) {
		return a
	}
	return b
}
