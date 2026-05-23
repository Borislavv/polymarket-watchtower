// Package rulesrisk implements the v11.5 Resolution Ambiguity /
// Dispute-Risk safety scoring layer. NEVER an alpha detector —
// the output is used by primary detectors to cap severity or
// block specific sub-strategies (cheaptail, repricinglag) on
// markets whose resolution criteria are vulnerable to disputes.
package rulesrisk

import (
	"fmt"
	"strings"
)

// Input is the pure-Decide payload. The orchestration layer
// fetches the market title + resolution text from Gamma /
// event-page context.
type Input struct {
	ConditionID      string
	Title            string
	Description      string
	ResolutionRules  string
	CatalystKindHint string // optional; "court_ruling", "certification", etc.
}

// RiskScore is the pure-Decide output.
type RiskScore struct {
	AmbiguityScore float64 // 0..1
	DisputeRisk    float64 // 0..1
	Reasons        []string
	Action         RuleAction
	Features       map[string]any
}

// RuleAction enumerates how detectors should react.
type RuleAction string

const (
	ActionAllow             RuleAction = "allow"
	ActionCapSeverity       RuleAction = "cap_severity"
	ActionBlockRepricingLag RuleAction = "block_repricing_lag"
	ActionBlockCheapTail    RuleAction = "block_cheap_tail"
	ActionBlockBoth         RuleAction = "block_both"
)

// Config tunes Decide(). The lexical rule set is operator-curated
// in-code today; a future iteration could load it from a
// versioned config file.
type Config struct {
	HighRiskThreshold float64 // default 0.7 — above this, cap severity
	BlockRepricingAt  float64 // default 0.7
	BlockCheaptailAt  float64 // default 0.6
}

func (c *Config) applyDefaults() {
	if c.HighRiskThreshold <= 0 {
		c.HighRiskThreshold = 0.7
	}
	if c.BlockRepricingAt <= 0 {
		c.BlockRepricingAt = 0.7
	}
	if c.BlockCheaptailAt <= 0 {
		c.BlockCheaptailAt = 0.6
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

// Decide scans the title + resolution text for ambiguity markers
// and returns a 0..1 risk score plus a Rule action.
//
// The rule set is intentionally explicit so reasons are auditable.
func (d *Detector) Decide(in Input) RiskScore {
	v := RiskScore{Features: map[string]any{}}
	text := strings.ToLower(in.Title + " " + in.Description + " " + in.ResolutionRules)

	markers := []struct {
		needle string
		weight float64
		reason string
	}{
		{"runoff", 0.20, "runoff_dependency"},
		{"recount", 0.20, "recount_dependency"},
		{"certification", 0.15, "certification_dependency"},
		{"certified", 0.15, "certification_dependency"},
		{"appeal", 0.15, "appeal_dependency"},
		{"court", 0.10, "court_dependency"},
		{"definitive", 0.10, "definitive_terminology"},
		{"first", 0.05, "first_terminology"},
		{"by end of", 0.10, "vague_deadline"},
		{"announced", 0.10, "announcement_dependency"},
		{"official", 0.10, "official_source_dependency"},
	}
	for _, m := range markers {
		if strings.Contains(text, m.needle) {
			v.AmbiguityScore += m.weight
			v.Reasons = append(v.Reasons, m.reason)
		}
	}
	// Catalyst kind hint augments.
	switch in.CatalystKindHint {
	case "court_ruling", "certification":
		v.AmbiguityScore += 0.10
		v.Reasons = append(v.Reasons, "high_risk_catalyst_kind")
	}
	if v.AmbiguityScore > 1 {
		v.AmbiguityScore = 1
	}
	// Dispute risk derived from ambiguity but heavier-tailed.
	v.DisputeRisk = v.AmbiguityScore * 0.9
	if v.AmbiguityScore >= 0.6 {
		v.DisputeRisk = v.AmbiguityScore
	}
	v.Action = d.decideAction(v.AmbiguityScore)
	v.Reasons = append(v.Reasons,
		fmt.Sprintf("ambiguity=%.2f dispute=%.2f action=%s", v.AmbiguityScore, v.DisputeRisk, v.Action))
	v.Features["ambiguity_score"] = v.AmbiguityScore
	v.Features["dispute_risk"] = v.DisputeRisk
	v.Features["action"] = string(v.Action)
	return v
}

func (d *Detector) decideAction(score float64) RuleAction {
	switch {
	case score >= d.cfg.BlockRepricingAt && score >= d.cfg.BlockCheaptailAt:
		return ActionBlockBoth
	case score >= d.cfg.BlockRepricingAt:
		return ActionBlockRepricingLag
	case score >= d.cfg.BlockCheaptailAt:
		return ActionBlockCheapTail
	case score >= d.cfg.HighRiskThreshold:
		return ActionCapSeverity
	}
	return ActionAllow
}
