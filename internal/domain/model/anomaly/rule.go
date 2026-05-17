package anomaly

import "sort"

// Canonical alert reasons. These appear in the Finding payload (and the
// rendered Telegram header) so reviewers can route by reason at a glance.
const (
	ReasonWhale         = "WhaleAnomaly"
	ReasonHighOdds      = "HighOddsTrade"
	ReasonHighOddsWhale = "HighOddsWhaleDetected"
	ReasonCluster       = "WhaleClusterDetected"
)

// Thresholds drives per-trade single_cluster scoring. Three independent signals
// are evaluated; the higher severity wins and the Reason is set accordingly:
//
//   - Whale (relative): trade USD >= MinTradeUSD AND baseline sample meets
//     MinBaselineTrades + MinBaselineNotionalUSD floors AND
//     (notional / baseline.median) crosses MultiplierLadder.
//   - High odds: 1/price crosses OddsLadder AND notional >= MinTradeUSD/10.
//     No baseline required — captures asymmetric-payoff insider bets on
//     cold/illiquid outcomes.
//   - Combined: both whale + odds fire => Reason = HighOddsWhaleDetected.
//
// MinBaselineNotionalUSD complements MinBaselineTrades: even 50 samples are
// useless if they total $5, so we additionally require a USD floor on the
// baseline reservoir before trusting the median.
type Thresholds struct {
	MultiplierLadder       []float64
	OddsLadder             []float64
	MinTradeUSD            float64
	MinBaselineTrades      int
	MinBaselineNotionalUSD float64
}

// Normalise sorts and de-dupes both ladders in place.
func (t *Thresholds) Normalise() {
	t.MultiplierLadder = sortedUnique(t.MultiplierLadder)
	t.OddsLadder = sortedUnique(t.OddsLadder)
}

func sortedUnique(xs []float64) []float64 {
	if len(xs) == 0 {
		return xs
	}
	sort.Float64s(xs)
	out := xs[:0]
	var prev float64
	for i, v := range xs {
		if i == 0 || v != prev {
			out = append(out, v)
			prev = v
		}
	}
	return out
}

// SeverityForLadder returns the severity for the highest ladder rung that v
// crosses, plus the rung value. ok=false when v is below every rung.
//
// Mapping is fixed at three rungs: lowest=>Info, second-highest=>Warning,
// highest=>Critical. Ladders longer than 3 collapse intermediate rungs into
// the next-lower bucket — operators who want a finer mapping should pick
// three thresholds rather than six.
func SeverityForLadder(v float64, ladder []float64) (Severity, float64, bool) {
	if len(ladder) == 0 || v < ladder[0] {
		return "", 0, false
	}
	hit := ladder[0]
	for _, rung := range ladder {
		if v >= rung {
			hit = rung
		}
	}
	return ladderSeverity(hit, ladder), hit, true
}

func ladderSeverity(hit float64, ladder []float64) Severity {
	switch len(ladder) {
	case 1:
		return SeverityInfo
	case 2:
		if hit >= ladder[1] {
			return SeverityWarning
		}
		return SeverityInfo
	default:
		top := ladder[len(ladder)-1]
		mid := ladder[len(ladder)-2]
		switch {
		case hit >= top:
			return SeverityCritical
		case hit >= mid:
			return SeverityWarning
		default:
			return SeverityInfo
		}
	}
}
