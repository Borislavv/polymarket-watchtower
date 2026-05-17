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
//
// Override promotions stack on top of the conservative-min combination:
//
//   - HardPromotionA / HardPromotionB: two independent OR branches that fire
//     Hard ("HumanReviewRequired") when ALL three of (notional, odds, mul)
//     clear their floors. Two branches let presets express e.g. "$250k AND
//     odds 5 AND 1000×" OR "$100k AND odds 10 AND 2500×".
//   - HugeWhale: forces the final severity to at least Critical when the
//     trade clears (notional, odds, mul). The conservative-min may have
//     under-classified a $250k bet at warning; this rescues it.
//   - MegaWhale: forces Hard. For absurd raw-size cases where odds/mul are
//     less relevant.
//
// Any tier with a zero field disables that override path.
type Thresholds struct {
	Info     Tier
	Warning  Tier
	Critical Tier

	HardPromotionA Tier
	HardPromotionB Tier

	HugeWhale Tier
	MegaWhale Tier

	MinBaselineTrades      int
	MinBaselineNotionalUSD float64
}

// meets reports (n, o, m) ≥ tier on every non-zero floor.
func (t Tier) meets(notional, odds, mul float64) bool {
	if t.MinNotionalUSD <= 0 || t.MinOdds <= 0 || t.MinMultiplier <= 0 {
		return false
	}
	return notional >= t.MinNotionalUSD && odds >= t.MinOdds && mul >= t.MinMultiplier
}

// MeetsHardPromotion reports whether either HardPromotion branch fires.
func (t Thresholds) MeetsHardPromotion(notional, odds, mul float64) bool {
	return t.HardPromotionA.meets(notional, odds, mul) || t.HardPromotionB.meets(notional, odds, mul)
}

// MeetsHugeWhale reports whether the HugeWhale override fires.
func (t Thresholds) MeetsHugeWhale(notional, odds, mul float64) bool {
	return t.HugeWhale.meets(notional, odds, mul)
}

// MeetsMegaWhale reports whether the MegaWhale override fires.
func (t Thresholds) MeetsMegaWhale(notional, odds, mul float64) bool {
	return t.MegaWhale.meets(notional, odds, mul)
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
