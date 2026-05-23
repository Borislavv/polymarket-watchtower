package thesisaccum

import (
	"fmt"
	"math"
	"sort"
	"time"
)

// Config tunes the pure Decide(). Defaults below mirror the
// spec's recommended floors. The orchestration layer reads each
// field from the THESIS_ACCUM_* env block.
type Config struct {
	MinBreadth       int           // breadth floor; default 2
	MinConsistency   float64       // aligned / total floor; default 0.75
	MinAlignedScore  float64       // normalised aligned-exposure floor; default 1.5
	LookbackRecent   time.Duration // window for "recent" lines; default 72h
	LookbackLifetime time.Duration // window for total wallet history; default 8760h
	// Score tier thresholds — score is roughly 0..100.
	WarningScore   float64 // default 50
	CriticalScore  float64 // default 75
	CatalystBoost  float64 // max additive score boost; default 10
	LifecycleBoost float64 // max additive boost when lifecycle is late; default 5
	OpposedPenalty float64 // multiplicative penalty per opposed unit; default 0.5
}

// applyDefaults backs all zero-value fields with the spec defaults.
func (c *Config) applyDefaults() {
	if c.MinBreadth <= 0 {
		c.MinBreadth = 2
	}
	if c.MinConsistency <= 0 {
		c.MinConsistency = 0.75
	}
	if c.MinAlignedScore <= 0 {
		c.MinAlignedScore = 1.5
	}
	if c.LookbackRecent <= 0 {
		c.LookbackRecent = 72 * time.Hour
	}
	if c.LookbackLifetime <= 0 {
		c.LookbackLifetime = 8760 * time.Hour
	}
	if c.WarningScore <= 0 {
		c.WarningScore = 50
	}
	if c.CriticalScore <= 0 {
		c.CriticalScore = 75
	}
	if c.CatalystBoost <= 0 {
		c.CatalystBoost = 10
	}
	if c.LifecycleBoost <= 0 {
		c.LifecycleBoost = 5
	}
	if c.OpposedPenalty <= 0 {
		c.OpposedPenalty = 0.5
	}
}

// Detector is the pure verdict producer.
type Detector struct {
	cfg Config
}

// New constructs a detector. cfg is copied; safe for concurrent
// Decide() calls.
func New(cfg Config) *Detector {
	cfg.applyDefaults()
	return &Detector{cfg: cfg}
}

// Decide is the load-bearing pure function. It maps the (link
// graph, wallet lines, catalyst windows) triple onto a tiered
// verdict. No I/O.
//
// Algorithm:
//
//  1. Build link map (dst -> direction|confidence). Source market
//     is treated as DirAligned by definition (this is the wallet's
//     current alert anchor).
//  2. For each wallet line: normalise the USD exposure against the
//     market's liquidity floor + baseline median (caps single-line
//     dominance). Map the line's side onto a thesis direction via
//     the link's direction (DirAligned keeps the sign, DirOpposed
//     flips it, DirUnknown contributes only to breadth, not
//     exposure).
//  3. Aggregate breadth (count distinct aligned markets),
//     aligned exposure, opposed exposure, consistency.
//  4. Compute base score from breadth * consistency * aligned.
//  5. Apply catalyst + lifecycle additive boosts; apply opposed
//     multiplicative penalty.
//  6. Compare to tier thresholds.
func (d *Detector) Decide(in Input) Verdict {
	v := Verdict{
		Features: map[string]any{},
	}

	// Source market is always aligned with itself.
	linkByDst := make(map[string]Link, len(in.Links)+1)
	linkByDst[in.SourceConditionID] = Link{
		DstConditionID: in.SourceConditionID,
		LinkType:       "self",
		Direction:      DirAligned,
		Confidence:     1.0,
	}
	for _, l := range in.Links {
		// Skip self-edges defensively.
		if l.DstConditionID == in.SourceConditionID {
			continue
		}
		linkByDst[l.DstConditionID] = l
	}

	cutoff := in.Now.Add(-d.cfg.LookbackLifetime)

	aligned := make(map[string]float64) // conditionID -> normalised aligned exposure
	opposed := make(map[string]float64)
	unknown := make(map[string]float64)

	for _, line := range in.WalletLines {
		link, ok := linkByDst[line.ConditionID]
		if !ok {
			// Wallet has activity on a market not in the link
			// graph; ignore for thesis aggregation.
			continue
		}
		if !line.WindowStart.IsZero() && line.WindowStart.Before(cutoff) {
			continue
		}
		norm := normaliseExposure(line)
		if norm <= 0 {
			continue
		}
		// Direction folding: aligned keeps norm, opposed flips it,
		// unknown contributes only to breadth.
		switch link.Direction {
		case DirAligned:
			aligned[line.ConditionID] += norm * link.Confidence
		case DirOpposed:
			// A wallet that holds the OPPOSED side of a mirror
			// market in the same volume contributes positively to
			// the thesis (they're long both legs of the same bet).
			// If the line is on the SAME side as the alert's side
			// on the source market and the link is "opposed",
			// that's actually contradictory exposure — penalise.
			if line.Side == in.Side {
				opposed[line.ConditionID] += norm * link.Confidence
			} else {
				aligned[line.ConditionID] += norm * link.Confidence
			}
		case DirUnknown:
			unknown[line.ConditionID] += norm * link.Confidence
		}
	}

	// Source market — at minimum the alert line itself counts as
	// aligned exposure even if WalletLines is empty (defensive).
	if _, ok := aligned[in.SourceConditionID]; !ok {
		aligned[in.SourceConditionID] = 0
	}

	v.AlignedMarkets = sortedKeys(aligned)
	v.OpposedMarkets = sortedKeys(opposed)
	v.AlignedExposure = sumValues(aligned)
	v.OpposedExposure = sumValues(opposed)
	v.Breadth = countNonZero(aligned)
	if v.AlignedExposure+v.OpposedExposure == 0 {
		v.Consistency = 0
	} else {
		v.Consistency = v.AlignedExposure / (v.AlignedExposure + v.OpposedExposure)
	}

	// Base score: breadth * consistency * sqrt(aligned).
	base := float64(v.Breadth) * v.Consistency * math.Sqrt(math.Max(v.AlignedExposure, 0))
	// Catalyst boost: closest high-confidence catalyst inside next
	// 30 days adds up to CatalystBoost points.
	catBoost := catalystBoost(d.cfg.CatalystBoost, in.Now, in.Catalysts)
	// Lifecycle boost: late-stage linked markets get a small bump.
	lifecycleBoost := d.cfg.LifecycleBoost * math.Max(0, math.Min(1, in.MaxLifecyclePctOnGraph/100.0))
	// Opposed penalty: multiplicative.
	penalty := 1.0
	if v.OpposedExposure > 0 {
		penalty = 1.0 / (1.0 + d.cfg.OpposedPenalty*v.OpposedExposure)
	}
	v.Score = (base + catBoost + lifecycleBoost) * penalty
	v.Confidence = clamp01(v.Consistency)

	v.Reasons = make([]string, 0, 4)
	v.Reasons = append(v.Reasons, fmt.Sprintf("breadth=%d consistency=%.2f aligned=%.2f", v.Breadth, v.Consistency, v.AlignedExposure))
	if catBoost > 0 {
		v.Reasons = append(v.Reasons, fmt.Sprintf("catalyst_boost=%.2f", catBoost))
	}
	if v.OpposedExposure > 0 {
		v.Reasons = append(v.Reasons, fmt.Sprintf("opposed=%.2f penalty=%.2f", v.OpposedExposure, penalty))
	}

	// Tier decision.
	if v.Breadth >= d.cfg.MinBreadth &&
		v.Consistency >= d.cfg.MinConsistency &&
		v.AlignedExposure >= d.cfg.MinAlignedScore {
		v.Fired = true
		switch {
		case v.Score >= d.cfg.CriticalScore:
			v.Level = "critical"
		case v.Score >= d.cfg.WarningScore:
			v.Level = "warning"
		default:
			v.Level = "info"
		}
	} else {
		v.Level = "none"
	}

	v.Features["breadth"] = v.Breadth
	v.Features["consistency"] = roundTo(v.Consistency, 4)
	v.Features["aligned_exposure"] = roundTo(v.AlignedExposure, 4)
	v.Features["opposed_exposure"] = roundTo(v.OpposedExposure, 4)
	v.Features["score"] = roundTo(v.Score, 4)
	v.Features["catalyst_boost"] = roundTo(catBoost, 4)
	v.Features["lifecycle_boost"] = roundTo(lifecycleBoost, 4)
	return v
}

// normaliseExposure caps a single line's USD against the market's
// liquidity floor and baseline median so one massive position on a
// thin market can't dominate the thesis aggregate.
func normaliseExposure(line WalletLine) float64 {
	if line.NetSharesUSD <= 0 {
		return 0
	}
	floor := math.Max(line.LiquidityFloor, line.BaselineMedianUSD)
	if floor <= 0 {
		floor = 1000 // safety: prevent divide-by-zero on uninitialised lines
	}
	// log-scale normalisation keeps the metric bounded for large
	// notionals while still rewarding bigger lines.
	return math.Log1p(line.NetSharesUSD / floor)
}

// catalystBoost returns up to maxBoost points based on the soonest
// high-confidence catalyst within the next 30 days.
func catalystBoost(maxBoost float64, now time.Time, catalysts []Catalyst) float64 {
	if len(catalysts) == 0 {
		return 0
	}
	best := 0.0
	for _, c := range catalysts {
		if c.Confidence < 0.5 {
			continue
		}
		dt := c.ExpectedAt.Sub(now)
		if dt < 0 || dt > 30*24*time.Hour {
			continue
		}
		// Closer catalyst, larger boost. Linear ramp over 30 days.
		factor := 1.0 - dt.Hours()/(30*24)
		score := maxBoost * factor * c.Confidence
		if score > best {
			best = score
		}
	}
	return best
}

func sortedKeys(m map[string]float64) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		if v <= 0 {
			continue
		}
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sumValues(m map[string]float64) float64 {
	s := 0.0
	for _, v := range m {
		if v > 0 {
			s += v
		}
	}
	return s
}

func countNonZero(m map[string]float64) int {
	n := 0
	for _, v := range m {
		if v > 0 {
			n++
		}
	}
	return n
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

func roundTo(v float64, digits int) float64 {
	p := math.Pow10(digits)
	return math.Round(v*p) / p
}
