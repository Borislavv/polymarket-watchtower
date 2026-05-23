// Package conflictresolve implements the v11.5 Quality-Weighted
// Conflict Resolution arbitration layer. NEVER standalone.
// When opposing signals reference the same market within a short
// window, the layer ranks them by per-side quality and either
// keeps/boosts the winner OR tags both as unresolved when
// dominance is too close.
package conflictresolve

import (
	"fmt"
	"math"
)

// SideSignal is one of the opposing signals (typically the buy
// and sell side of the same market) summarised by the
// orchestration layer.
type SideSignal struct {
	Side               string  // "YES" / "NO"
	WalletQualityScore float64 // 0..1 — historical wallet PnL/CLV percentile
	HolderStrength     float64 // 0..1 — holder delta concentration score
	ThesisBreadth      float64 // 0..1 — normalised breadth across event graph
	CatalystProximity  float64 // 0..1 — proximity to upcoming catalyst
	BookSupport        float64 // 0..1 — orderbook depth on this side
	MMLike             bool    // sets a penalty
}

// ConflictInput is the pure-Decide payload.
type ConflictInput struct {
	A SideSignal
	B SideSignal
}

// ConflictAction is what the orchestration layer should do to the
// loser. The winner's score is boosted by Boost; the loser's
// severity may be degraded or suppressed.
type ConflictAction string

const (
	ActionKeepBoth            ConflictAction = "keep_both"
	ActionBoostWinnerDegrade  ConflictAction = "boost_winner_degrade_loser"
	ActionBoostWinnerSuppress ConflictAction = "boost_winner_suppress_loser"
	ActionTagUnresolved       ConflictAction = "tag_unresolved"
)

// ConflictVerdict is the pure-Decide output.
type ConflictVerdict struct {
	Action      ConflictAction
	WinningSide string
	Dominance   float64
	BoostWinner float64
	Reasons     []string
	Features    map[string]any
}

// Config tunes Decide().
type Config struct {
	MMPenalty         float64 // default 0.4
	MinDominance      float64 // default 1.5
	SuppressDominance float64 // default 2.5 — above this, fully suppress loser
	MaxBoost          float64 // default 6
}

func (c *Config) applyDefaults() {
	if c.MMPenalty <= 0 {
		c.MMPenalty = 0.4
	}
	if c.MinDominance <= 0 {
		c.MinDominance = 1.5
	}
	if c.SuppressDominance <= 0 {
		c.SuppressDominance = 2.5
	}
	if c.MaxBoost <= 0 {
		c.MaxBoost = 6
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

// Decide ranks the two sides by composite quality and picks an
// action. Tie/near-tie → tag_unresolved (deterministic, never
// invents a winner).
func (d *Detector) Decide(in ConflictInput) ConflictVerdict {
	v := ConflictVerdict{Features: map[string]any{}}

	scoreA := d.score(in.A)
	scoreB := d.score(in.B)
	v.Features["score_a"] = scoreA
	v.Features["score_b"] = scoreB

	if scoreA == 0 && scoreB == 0 {
		v.Action = ActionKeepBoth
		v.Reasons = []string{"both_sides_zero_quality"}
		return v
	}
	var bigger, smaller float64
	if scoreA >= scoreB {
		bigger, smaller = scoreA, scoreB
		v.WinningSide = in.A.Side
	} else {
		bigger, smaller = scoreB, scoreA
		v.WinningSide = in.B.Side
	}
	smallerSafe := math.Max(smaller, 0.01)
	v.Dominance = bigger / smallerSafe

	switch {
	case v.Dominance >= d.cfg.SuppressDominance:
		v.Action = ActionBoostWinnerSuppress
		v.BoostWinner = d.cfg.MaxBoost
	case v.Dominance >= d.cfg.MinDominance:
		v.Action = ActionBoostWinnerDegrade
		v.BoostWinner = d.cfg.MaxBoost * (v.Dominance - 1) / (d.cfg.SuppressDominance - 1)
	default:
		v.Action = ActionTagUnresolved
		v.WinningSide = ""
		v.BoostWinner = 0
	}
	v.Reasons = append(v.Reasons,
		fmt.Sprintf("dominance=%.2f winner=%s action=%s", v.Dominance, v.WinningSide, v.Action))
	return v
}

func (d *Detector) score(s SideSignal) float64 {
	base := s.WalletQualityScore + s.HolderStrength + s.ThesisBreadth + s.CatalystProximity + s.BookSupport
	if s.MMLike {
		base *= (1.0 - d.cfg.MMPenalty)
	}
	return base
}
