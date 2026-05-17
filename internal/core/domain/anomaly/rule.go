package anomaly

import "sort"

// Rule defines the multiplier ladder. Multipliers must be strictly increasing
// after Normalise(). SeverityFor maps an observed ratio to a severity.
type Rule struct {
	Multipliers []float64
	MinNotional float64 // USD; recent window must clear this to fire
	MinTrades   int     // trades; recent window must clear this to fire
}

// Normalise sorts and de-dupes multipliers in place.
func (r *Rule) Normalise() {
	if len(r.Multipliers) == 0 {
		return
	}
	sort.Float64s(r.Multipliers)
	out := r.Multipliers[:0]
	var prev float64
	for i, m := range r.Multipliers {
		if i == 0 || m != prev {
			out = append(out, m)
			prev = m
		}
	}
	r.Multipliers = out
}

// SeverityFor maps a ratio to a severity. Returns ok=false when the ratio is
// below the lowest multiplier.
func (r Rule) SeverityFor(ratio float64) (Severity, bool) {
	if len(r.Multipliers) == 0 || ratio < r.Multipliers[0] {
		return "", false
	}
	switch len(r.Multipliers) {
	case 1:
		return SeverityWarn, true
	case 2:
		if ratio >= r.Multipliers[1] {
			return SeverityCritical, true
		}
		return SeverityWarn, true
	default:
		top := r.Multipliers[len(r.Multipliers)-1]
		mid := r.Multipliers[len(r.Multipliers)-2]
		switch {
		case ratio >= top:
			return SeverityFatal, true
		case ratio >= mid:
			return SeverityCritical, true
		default:
			return SeverityWarn, true
		}
	}
}
