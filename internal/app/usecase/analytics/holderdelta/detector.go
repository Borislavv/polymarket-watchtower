// Package holderdelta implements the v11.5 True Holder Delta
// Concentration primary detector. Replaces the v6 approximate
// ownership signal (trade-flow shares) with snapshot-pair
// arithmetic: actual on-chain holder rank + share count + market
// open-interest, sampled at two adjacent timestamps.
//
// The detector is PURE. Orchestration (holdersync.Worker + the
// detect loop) is responsible for pulling holders / positions /
// OI from the Polymarket Data API and persisting snapshots.
package holderdelta

import (
	"fmt"
	"math"
	"time"
)

// Snapshot is a single point-in-time view of a (market, outcome,
// wallet) position. Orchestration loads the latest two snapshots
// per (condition_id, outcome_token, wallet) from
// polymarket_holder_snapshots and hands them to Decide.
type Snapshot struct {
	SnapshotAt  time.Time
	Wallet      string
	Rank        int // 1 = top holder
	Shares      float64
	NotionalUSD float64
	PctOI       float64 // wallet shares / market open interest
	TotalOI     float64
}

// Input is the pure-Decide payload.
type Input struct {
	ConditionID  string
	OutcomeToken string
	Wallet       string
	Now          time.Time

	Current  Snapshot
	Previous Snapshot // zero-value if no prior snapshot
}

// Verdict is the pure-Decide output.
type Verdict struct {
	Fired              bool
	Level              string // info | warning | critical | none
	Score              float64
	Confidence         float64
	PctOI              float64
	RankNow            int
	RankPrev           int
	DeltaShares        float64
	DeltaRank          int     // positive = improved (lower number)
	OIDeltaPct         float64 // signed (current - prev) / prev
	DenominatorPenalty float64
	Reasons            []string
	Features           map[string]any
}

// Config tunes Decide(). Operator knobs come from
// OWNERSHIP_V2_MIN_PCT_OI_INFO / _WARN / _CRIT + OWNERSHIP_V2_TOPK
// + OWNERSHIP_V2_SHADOW_ONLY.
type Config struct {
	MinPctOIInfo     float64 // fire at info; default 0.03 (3%)
	MinPctOIWarning  float64 // fire at warning; default 0.08
	MinPctOICritical float64 // fire at critical; default 0.15
	TopK             int     // "entered top-K" threshold; default 5
	MinSharesDelta   float64 // minimum absolute share growth; default 1.0
	OIShrinkPenalty  float64 // multiplicative penalty when OI dropped; default 0.5
}

func (c *Config) applyDefaults() {
	if c.MinPctOIInfo <= 0 {
		c.MinPctOIInfo = 0.03
	}
	if c.MinPctOIWarning <= 0 {
		c.MinPctOIWarning = 0.08
	}
	if c.MinPctOICritical <= 0 {
		c.MinPctOICritical = 0.15
	}
	if c.TopK <= 0 {
		c.TopK = 5
	}
	if c.MinSharesDelta <= 0 {
		c.MinSharesDelta = 1.0
	}
	if c.OIShrinkPenalty <= 0 {
		c.OIShrinkPenalty = 0.5
	}
}

// Detector is the pure verdict producer.
type Detector struct {
	cfg Config
}

func New(cfg Config) *Detector {
	cfg.applyDefaults()
	return &Detector{cfg: cfg}
}

// Decide judges whether the wallet's position growth represents
// true balance accumulation or a denominator artifact.
//
// Load-bearing rule (the spec's #1 concern): when OI collapses
// faster than the wallet's shares grew, the wallet's pctOI
// "improvement" is an artifact, not accumulation. The denominator
// penalty applies.
func (d *Detector) Decide(in Input) Verdict {
	v := Verdict{Features: map[string]any{}}
	cur := in.Current
	prev := in.Previous

	v.PctOI = cur.PctOI
	v.RankNow = cur.Rank
	v.RankPrev = prev.Rank
	v.DeltaShares = cur.Shares - prev.Shares
	if prev.Rank > 0 {
		v.DeltaRank = prev.Rank - cur.Rank
	}
	if prev.TotalOI > 0 {
		v.OIDeltaPct = (cur.TotalOI - prev.TotalOI) / prev.TotalOI
	}

	// Denominator penalty: when total OI shrank faster than the
	// wallet grew, the pctOI move is an artifact. Penalty grows
	// with the magnitude of the OI shrink.
	denomPenalty := 1.0
	if v.OIDeltaPct < -0.05 && v.DeltaShares < math.Abs(v.OIDeltaPct*prev.Shares)*0.5 {
		denomPenalty = d.cfg.OIShrinkPenalty
	}
	v.DenominatorPenalty = denomPenalty

	// Score combines pctOI + rank improvement + share growth.
	// score = 100 * pctOI * rankBonus * sharesBonus * denomPenalty.
	rankBonus := 1.0
	if v.DeltaRank > 0 {
		rankBonus = 1.0 + math.Min(float64(v.DeltaRank)/5.0, 1.0)
	}
	sharesBonus := 1.0
	if v.DeltaShares > 0 {
		sharesBonus = 1.0 + math.Min(v.DeltaShares/math.Max(prev.Shares, 1), 1.0)
	}
	v.Score = 100.0 * cur.PctOI * rankBonus * sharesBonus * denomPenalty
	v.Confidence = clamp01(cur.PctOI * 4) // pctOI=0.25 → confidence saturates at 1.0

	// Eligibility gates:
	//  - either pctOI clears one of the tiers, or
	//  - wallet entered the top-K AND shares grew by at least
	//    MinSharesDelta.
	enteredTopK := v.RankNow > 0 && v.RankNow <= d.cfg.TopK &&
		(v.RankPrev == 0 || v.RankPrev > d.cfg.TopK) &&
		v.DeltaShares >= d.cfg.MinSharesDelta
	switch {
	case cur.PctOI >= d.cfg.MinPctOICritical && denomPenalty == 1.0:
		v.Fired = true
		v.Level = "critical"
	case cur.PctOI >= d.cfg.MinPctOIWarning && denomPenalty == 1.0:
		v.Fired = true
		v.Level = "warning"
	case cur.PctOI >= d.cfg.MinPctOIInfo && denomPenalty == 1.0:
		v.Fired = true
		v.Level = "info"
	case enteredTopK && denomPenalty == 1.0:
		v.Fired = true
		v.Level = "info"
	default:
		v.Level = "none"
	}

	v.Reasons = append(v.Reasons,
		fmt.Sprintf("pctOI=%.3f rank=%d→%d Δshares=%.2f Δoi=%.3f penalty=%.2f",
			cur.PctOI, prev.Rank, cur.Rank, v.DeltaShares, v.OIDeltaPct, denomPenalty))
	if denomPenalty < 1.0 {
		v.Reasons = append(v.Reasons, "denominator_artifact_suspected")
	}
	if enteredTopK {
		v.Reasons = append(v.Reasons, fmt.Sprintf("entered_top_%d", d.cfg.TopK))
	}

	v.Features["pct_oi"] = cur.PctOI
	v.Features["pct_oi_prev"] = prev.PctOI
	v.Features["rank_now"] = cur.Rank
	v.Features["rank_prev"] = prev.Rank
	v.Features["delta_shares"] = v.DeltaShares
	v.Features["delta_rank"] = v.DeltaRank
	v.Features["oi_delta_pct"] = v.OIDeltaPct
	v.Features["denominator_penalty"] = denomPenalty
	v.Features["score"] = v.Score
	return v
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
