// Package bookvacuum implements the v11.5 Liquidity Withdrawal /
// Book Vacuum BOOSTER. Detects one-side orderbook depth collapse +
// confirmation by spread widening or mid-price drift.
//
// Wave-P1/P2 status: pure-Decide() shape is final; the
// orchestration layer + book_feature_bars aggregator are PHASE B
// (require real-time perf testing on the WS fast-lane before
// promotion). Default config keeps SHADOW_ONLY=true.
//
// Spec rule: NEVER uses WS trade-side as authoritative direction.
// The detector keys off book state — bid/ask depth + spread —
// alone.
package bookvacuum

import (
	"fmt"
	"math"
)

// FeatureBar is a single 1s / 5s aggregate from
// polymarket_book_feature_bars. Orchestration loads the rolling
// baseline window for the same market.
type FeatureBar struct {
	BidDepthTopN     float64
	AskDepthTopN     float64
	MidPrice         float64
	Spread           float64
	BidDepthDeltaPct float64
	AskDepthDeltaPct float64
	MidDelta         float64
}

// Input is the pure-Decide payload.
type Input struct {
	Recent   FeatureBar // the latest bar (the candidate event)
	Baseline FeatureBar // rolling baseline over BaselineWindow
	SpreadZ  float64    // how many σ above baseline the current spread is
	MMLike   bool       // true if the market_close_review MM filter flagged this market recently
}

// VacuumVerdict is the pure-Decide output. By design no Fired
// field — booster only.
type VacuumVerdict struct {
	Detected         bool   // a vacuum event is present
	Side             string // "ask" (buy-side conviction) | "bid" (sell-side conviction) | ""
	DepthCollapsePct float64
	MidMove          float64
	Boost            float64
	Reasons          []string
	Features         map[string]any
}

// Config tunes Decide(). Defaults align with the spec floors.
type Config struct {
	MinCollapsePct float64 // default 0.5 (50%)
	MinSpreadZ     float64 // default 1.5
	MaxBoost       float64 // default 8
}

func (c *Config) applyDefaults() {
	if c.MinCollapsePct <= 0 {
		c.MinCollapsePct = 0.5
	}
	if c.MinSpreadZ <= 0 {
		c.MinSpreadZ = 1.5
	}
	if c.MaxBoost <= 0 {
		c.MaxBoost = 8
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

// Decide judges whether the current bar represents a book vacuum.
// Side="ask" means ask-side depth collapsed (buy-side pressure
// likely); Side="bid" means bid-side collapsed.
func (d *Detector) Decide(in Input) VacuumVerdict {
	v := VacuumVerdict{Features: map[string]any{}}
	if in.MMLike {
		// Spec gate: MM-like markets are dominated by MM
		// rebalancing; vacuum signal is unreliable here.
		v.Reasons = []string{"mm_like_market_skipped"}
		return v
	}
	askCollapse := -in.Recent.AskDepthDeltaPct
	bidCollapse := -in.Recent.BidDepthDeltaPct

	var side string
	var collapse float64
	if askCollapse >= d.cfg.MinCollapsePct && askCollapse > bidCollapse {
		side = "ask"
		collapse = askCollapse
	} else if bidCollapse >= d.cfg.MinCollapsePct && bidCollapse > askCollapse {
		side = "bid"
		collapse = bidCollapse
	}
	if side == "" {
		v.Reasons = []string{"no_depth_collapse"}
		return v
	}
	// Confirmation: spread widened by ≥ MinSpreadZ σ AND mid moved
	// in the direction implied by the missing side.
	spreadConfirmed := in.SpreadZ >= d.cfg.MinSpreadZ
	midConfirmed := (side == "ask" && in.Recent.MidDelta > 0) ||
		(side == "bid" && in.Recent.MidDelta < 0)
	if !spreadConfirmed && !midConfirmed {
		v.Side = side
		v.DepthCollapsePct = collapse
		v.Reasons = []string{"unconfirmed_collapse"}
		return v
	}
	v.Detected = true
	v.Side = side
	v.DepthCollapsePct = collapse
	v.MidMove = math.Abs(in.Recent.MidDelta)
	// Linear boost in collapse magnitude, capped.
	v.Boost = math.Min(d.cfg.MaxBoost, d.cfg.MaxBoost*collapse)
	v.Reasons = append(v.Reasons,
		fmt.Sprintf("side=%s collapse=%.2f spreadZ=%.2f midΔ=%+.3f",
			side, collapse, in.SpreadZ, in.Recent.MidDelta))
	v.Features["side"] = side
	v.Features["collapse_pct"] = collapse
	v.Features["spread_z"] = in.SpreadZ
	v.Features["mid_delta"] = in.Recent.MidDelta
	v.Features["boost"] = v.Boost
	return v
}
