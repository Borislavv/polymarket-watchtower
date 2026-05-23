// Package repricinglag implements the v11.5 Post-news
// Underreaction / Repricing Lag primary detector.
//
// The detector is PURE. The orchestration layer
// (repricing.Worker) opens windows on annotations / catalysts and,
// at checkpoint intervals, snapshots the target market's price +
// peer markets' median price and calls Decide().
package repricinglag

import (
	"fmt"
	"sort"
)

// Input is the pure-Decide payload.
type Input struct {
	ConditionID string
	EventSlug   string
	// Observed signed move on the target market, in cents.
	// Positive = move in the expected direction.
	ObservedMoveCents float64
	// Per-peer signed move in cents. The detector uses the
	// median peer move as the "peer expected" baseline.
	PeerMovesCents []float64
	// Operator/event-rule supplied expected impact in cents.
	ExpectedImpactCents float64
	// Ambiguity / dispute risk score 0..1; high risk blocks fire.
	AmbiguityScore float64
}

// LagVerdict is the pure-Decide output. Standalone-fire eligible
// (after promotion); orchestration writes a shadow row regardless.
type LagVerdict struct {
	Fired      bool
	Level      string
	LagScore   float64
	PeerMedian float64
	SideBias   string // "underreaction" | "overreaction" | ""
	Reasons    []string
	Features   map[string]any
}

// Config tunes Decide().
type Config struct {
	MinLagCents  float64 // default 3
	MaxAmbiguity float64 // default 0.7
	PeerMinCount int     // default 2
}

func (c *Config) applyDefaults() {
	if c.MinLagCents <= 0 {
		c.MinLagCents = 3
	}
	if c.MaxAmbiguity <= 0 {
		c.MaxAmbiguity = 0.7
	}
	if c.PeerMinCount <= 0 {
		c.PeerMinCount = 2
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

// Decide compares observed move vs max(peer_median, expected).
// Underreaction = observed materially less than peer/expected.
// Overreaction = observed materially more.
func (d *Detector) Decide(in Input) LagVerdict {
	v := LagVerdict{Features: map[string]any{}}
	if in.AmbiguityScore >= d.cfg.MaxAmbiguity {
		v.Reasons = []string{"blocked_by_ambiguity"}
		return v
	}
	peerMedian := median(in.PeerMovesCents)
	v.PeerMedian = peerMedian
	if len(in.PeerMovesCents) < d.cfg.PeerMinCount && in.ExpectedImpactCents <= 0 {
		v.Reasons = []string{"insufficient_peers_and_expected"}
		return v
	}
	expected := in.ExpectedImpactCents
	if peerMedian > expected {
		expected = peerMedian
	}
	lag := expected - in.ObservedMoveCents
	v.LagScore = lag
	v.Features["expected_cents"] = expected
	v.Features["observed_cents"] = in.ObservedMoveCents
	v.Features["peer_median_cents"] = peerMedian

	if lag >= d.cfg.MinLagCents {
		v.SideBias = "underreaction"
		v.Fired = true
	} else if -lag >= d.cfg.MinLagCents {
		v.SideBias = "overreaction"
		v.Fired = true
	}
	if v.Fired {
		switch {
		case absFloat(lag) >= d.cfg.MinLagCents*3:
			v.Level = "critical"
		case absFloat(lag) >= d.cfg.MinLagCents*2:
			v.Level = "warning"
		default:
			v.Level = "info"
		}
		v.Reasons = append(v.Reasons,
			fmt.Sprintf("bias=%s lag=%.1f¢ expected=%.1f¢ observed=%.1f¢",
				v.SideBias, lag, expected, in.ObservedMoveCents))
	} else {
		v.Level = "none"
		v.Reasons = append(v.Reasons, "lag_below_threshold")
	}
	return v
}

func median(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := make([]float64, len(xs))
	copy(s, xs)
	sort.Float64s(s)
	mid := len(s) / 2
	if len(s)%2 == 0 {
		return (s[mid-1] + s[mid]) / 2
	}
	return s[mid]
}

func absFloat(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}
