// Package cheaptail implements the v11.5 Cheap-Tail Catalyst
// Staging detector. Identifies repeated staging of low-probability
// outcomes by a wallet/cohort before a known catalyst.
//
// Standalone-fire eligible (after promotion). Heavily-gated to
// avoid the "every low-probability trade is interesting" failure
// mode listed in the spec.
package cheaptail

import (
	"fmt"
	"time"
)

// Trade is one staging trade the orchestration layer pre-filtered
// (price in the tail band, non-dust notional).
type Trade struct {
	Price       float64
	NotionalUSD float64
	Side        string
	Timestamp   time.Time
}

// Input is the pure-Decide payload.
type Input struct {
	ConditionID           string
	Wallet                string
	CohortID              string  // empty if not cohort-aware
	Trades                []Trade // recent tail-band trades by this wallet/cohort
	HolderRankImprovement int     // current rank vs prior; positive = improved
	ThesisBreadth         int     // breadth from thesisaccum (linked-market aligned count)
	HasActiveCatalyst     bool    // catalystwindow.InWindow on the same event
	AmbiguityScore        float64 // from rulesrisk; high → block
	LifecyclePct          float64 // 0..100; later-stage markets weighted heavier
}

// Verdict is the pure-Decide output.
type Verdict struct {
	Fired         bool
	Level         string
	Score         float64
	ProbBand      string  // "deep_tail" | "near_tail" | ""
	Convexity     float64 // average 1/price across trades
	StageStrength float64 // sum of normalised notionals
	Reasons       []string
	Features      map[string]any
}

// Config tunes Decide().
type Config struct {
	MinPrice        float64 // default 0.02 (2%)
	MaxPrice        float64 // default 0.15 (15%)
	MinNotionalUSD  float64 // default 2_500 — non-dust floor
	MinTrades       int     // default 2 — staging requires repetition
	RequireCatalyst bool    // default true
	MaxAmbiguity    float64 // default 0.6 — high-ambiguity markets blocked
	WarningScore    float64 // default 30
	CriticalScore   float64 // default 60
}

func (c *Config) applyDefaults() {
	if c.MinPrice <= 0 {
		c.MinPrice = 0.02
	}
	if c.MaxPrice <= 0 {
		c.MaxPrice = 0.15
	}
	if c.MinNotionalUSD <= 0 {
		c.MinNotionalUSD = 2_500
	}
	if c.MinTrades <= 0 {
		c.MinTrades = 2
	}
	if c.MaxAmbiguity <= 0 {
		c.MaxAmbiguity = 0.6
	}
	if c.WarningScore <= 0 {
		c.WarningScore = 30
	}
	if c.CriticalScore <= 0 {
		c.CriticalScore = 60
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

// Decide judges whether the wallet/cohort is structurally staging
// a cheap-tail catalyst trade.
func (d *Detector) Decide(in Input) Verdict {
	v := Verdict{Features: map[string]any{}}
	if in.AmbiguityScore >= d.cfg.MaxAmbiguity {
		v.Reasons = []string{"blocked_by_ambiguity"}
		return v
	}
	if d.cfg.RequireCatalyst && !in.HasActiveCatalyst {
		v.Reasons = []string{"no_active_catalyst"}
		return v
	}
	// Filter trades into the tail band and accumulate.
	var convexSum, notionalSum float64
	var inBand int
	for _, t := range in.Trades {
		if t.Price < d.cfg.MinPrice || t.Price > d.cfg.MaxPrice {
			continue
		}
		if t.NotionalUSD < d.cfg.MinNotionalUSD {
			continue
		}
		inBand++
		convexSum += 1.0 / t.Price
		notionalSum += t.NotionalUSD
	}
	if inBand < d.cfg.MinTrades {
		v.Reasons = []string{"insufficient_staging"}
		return v
	}
	v.Convexity = convexSum / float64(inBand)
	v.StageStrength = notionalSum / float64(d.cfg.MinNotionalUSD)
	if in.Trades[0].Price <= 0.05 {
		v.ProbBand = "deep_tail"
	} else {
		v.ProbBand = "near_tail"
	}
	// Score components.
	v.Score = v.Convexity * v.StageStrength
	if in.HolderRankImprovement > 0 {
		v.Score *= 1.0 + float64(in.HolderRankImprovement)/10.0
	}
	if in.ThesisBreadth >= 2 {
		v.Score *= 1.0 + float64(in.ThesisBreadth)/4.0
	}
	if in.LifecyclePct > 0 {
		v.Score *= 0.5 + in.LifecyclePct/200.0 // lifecycle 100% → multiplier 1.0
	}
	v.Fired = true
	switch {
	case v.Score >= d.cfg.CriticalScore:
		v.Level = "critical"
	case v.Score >= d.cfg.WarningScore:
		v.Level = "warning"
	default:
		v.Level = "info"
	}
	v.Reasons = append(v.Reasons,
		fmt.Sprintf("band=%s convexity=%.2f stage=%.2f breadth=%d rankΔ=%d score=%.2f",
			v.ProbBand, v.Convexity, v.StageStrength, in.ThesisBreadth, in.HolderRankImprovement, v.Score))
	v.Features["prob_band"] = v.ProbBand
	v.Features["convexity"] = v.Convexity
	v.Features["stage_strength"] = v.StageStrength
	v.Features["score"] = v.Score
	return v
}
