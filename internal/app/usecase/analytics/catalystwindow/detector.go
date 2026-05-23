// Package catalystwindow implements the v11.5 Scheduled Catalyst
// Window Flow BOOSTER. By spec rule "never standalone": Decide()
// returns BoostVerdict whose Boost field is folded into a parent
// detector's score by the orchestration layer. The detector NEVER
// emits its own standalone alert.
package catalystwindow

import (
	"fmt"
	"math"
	"time"
)

// Catalyst is one operator-curated or AI-extracted catalyst row
// from polymarket_event_catalysts. Decide() picks the strongest
// matching one for the (signal_time, event_slug) pair.
type Catalyst struct {
	Kind       string
	ExpectedAt time.Time
	Confidence float64
	EventSlug  string
}

// WindowSpec defines a (pre, post) window per catalyst kind. The
// orchestration layer fills this from the CATALYST_WINDOW_*_PRE /
// _POST env block.
type WindowSpec struct {
	Pre  time.Duration
	Post time.Duration
}

// Input is the pure-Decide payload.
type Input struct {
	SignalTime    time.Time
	EventSlug     string
	Catalysts     []Catalyst
	WindowsByKind map[string]WindowSpec
	MinConfidence float64 // skip catalysts whose confidence falls below this
	ParentScore   float64 // current score of the parent signal (only used for proportional boost)
}

// BoostVerdict is the pure-Decide output.
type BoostVerdict struct {
	InWindow    bool
	Catalyst    Catalyst // strongest matching one (zero-value if none)
	WindowClass string   // "pre_short" | "pre_long" | "post_short" | "post_long" | ""
	LeadTime    time.Duration
	Boost       float64 // additive boost the orchestration layer applies to ParentScore
	Reasons     []string
	Features    map[string]any
}

// Config tunes Decide(). All fields have spec-aligned defaults.
type Config struct {
	MaxBoost float64 // default 12 — max additive boost
}

func (c *Config) applyDefaults() {
	if c.MaxBoost <= 0 {
		c.MaxBoost = 12
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

// Decide picks the soonest matching catalyst whose confidence
// clears MinConfidence and whose window spec includes the signal
// time. When multiple catalysts match, the one closest to (or just
// past) the signal time wins.
//
// Never emits standalone. Returns Boost=0 + InWindow=false when no
// matching catalyst.
func (d *Detector) Decide(in Input) BoostVerdict {
	v := BoostVerdict{Features: map[string]any{}}
	var best Catalyst
	var bestLead time.Duration
	var bestClass string

	for _, c := range in.Catalysts {
		if c.Confidence < in.MinConfidence {
			continue
		}
		if c.EventSlug != "" && in.EventSlug != "" && c.EventSlug != in.EventSlug {
			continue
		}
		spec, ok := in.WindowsByKind[c.Kind]
		if !ok {
			continue
		}
		lead := c.ExpectedAt.Sub(in.SignalTime)
		// Window: signal in [expected - pre, expected + post].
		if lead < -spec.Post || lead > spec.Pre {
			continue
		}
		class := classify(lead, spec)
		// Prefer earlier signals before the catalyst (more
		// surveillance value than reaction trades).
		better := false
		switch {
		case !v.InWindow:
			better = true
		case lead > 0 && bestLead < 0:
			// New candidate is pre-catalyst; previous was post-catalyst.
			better = true
		case lead > 0 && bestLead > 0:
			// Both pre-catalyst; prefer the larger lead (more advance warning).
			better = lead > bestLead
		case lead <= 0 && bestLead <= 0:
			// Both post-catalyst; prefer closer to catalyst (smaller absolute distance).
			better = lead > bestLead
		}
		if better {
			v.InWindow = true
			best = c
			bestLead = lead
			bestClass = class
		}
	}
	if !v.InWindow {
		v.Reasons = []string{"no_matching_catalyst"}
		return v
	}
	v.Catalyst = best
	v.LeadTime = bestLead
	v.WindowClass = bestClass

	// Boost = MaxBoost * confidence * proximity. Linear in
	// proximity within the window.
	spec := in.WindowsByKind[best.Kind]
	var proximity float64
	if bestLead >= 0 {
		// Pre-catalyst: large lead → 0; signal at expected_at → 1.
		proximity = 1.0 - math.Abs(bestLead.Seconds())/math.Max(spec.Pre.Seconds(), 1)
	} else {
		// Post-catalyst: just past expected_at → 1; window edge → 0.
		proximity = 1.0 - math.Abs(bestLead.Seconds())/math.Max(spec.Post.Seconds(), 1)
	}
	if proximity < 0 {
		proximity = 0
	}
	v.Boost = d.cfg.MaxBoost * best.Confidence * proximity
	v.Reasons = append(v.Reasons,
		fmt.Sprintf("kind=%s lead=%s class=%s confidence=%.2f boost=%.2f",
			best.Kind, bestLead.Round(time.Minute), bestClass, best.Confidence, v.Boost))
	v.Features["catalyst_kind"] = best.Kind
	v.Features["lead_seconds"] = int64(bestLead.Seconds())
	v.Features["window_class"] = bestClass
	v.Features["boost"] = v.Boost
	return v
}

func classify(lead time.Duration, spec WindowSpec) string {
	if lead >= 0 {
		// Pre-catalyst.
		if lead > spec.Pre/2 {
			return "pre_long"
		}
		return "pre_short"
	}
	// Post-catalyst.
	if -lead > spec.Post/2 {
		return "post_long"
	}
	return "post_short"
}
