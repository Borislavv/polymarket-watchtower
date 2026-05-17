package anomaly

import "sort"

// Thresholds defines the two independent single-trade signals:
//
//   - Multipliers: a sorted ladder applied to (trade USD notional / baseline
//     median USD notional) for the same (category, market, outcome) bucket.
//     Severity mapping: lowest→info, middle→warning, top→critical. Skipped
//     entirely when the baseline sample is below MinBaselineTrades to avoid
//     the divide-by-tiny-N false-positive class.
//
//   - AbsoluteUSDTiers: a sorted ladder applied to the trade's USD notional
//     directly, regardless of baseline. Catches "$10k bet on a fresh market"
//     where no baseline exists yet. Same info/warning/critical mapping.
//
// Both signals score every trade; the higher severity wins.
type Thresholds struct {
	Multipliers       []float64
	AbsoluteUSDTiers  []float64
	MinBaselineTrades int
}

// Normalise sorts and de-dupes both ladders in place.
func (t *Thresholds) Normalise() {
	t.Multipliers = sortedUnique(t.Multipliers)
	t.AbsoluteUSDTiers = sortedUnique(t.AbsoluteUSDTiers)
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
