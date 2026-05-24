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
// v11.10-insider-prior strengthens the scorer along four orthogonal
// dimensions so reasons are richer and the score is harder to game
// by a single keyword removal:
//
//   - wording risk: vague verbs (be, become, declare, considered),
//     subjective adjectives (significant, material), hedged
//     quantifiers (some, any, more than a few);
//   - source specificity: "according to X" where X is named (AP,
//     Reuters, NYT) lowers risk; unnamed sources ("officials say",
//     "according to reports") raise it;
//   - runoff/certification/appeal patterns: more granular weighting
//     for downstream procedural dependencies that can flip resolved
//     status weeks after market close;
//   - procedural complexity: count of distinct procedural markers
//     (court + appeal + certification) — three is materially more
//     ambiguous than one.
//
// The rule set is intentionally explicit so reasons are auditable.
func (d *Detector) Decide(in Input) RiskScore {
	v := RiskScore{Features: map[string]any{}}
	text := strings.ToLower(in.Title + " " + in.Description + " " + in.ResolutionRules)

	type marker struct {
		needle   string
		weight   float64
		reason   string
		category string // "procedural" | "wording" | "source" | "deadline"
	}
	markers := []marker{
		// Procedural — runoff/certification/appeal/recount patterns.
		{"runoff", 0.20, "runoff_dependency", "procedural"},
		{"recount", 0.20, "recount_dependency", "procedural"},
		{"certification", 0.15, "certification_dependency", "procedural"},
		{"certified", 0.15, "certification_dependency", "procedural"},
		{"appeal", 0.15, "appeal_dependency", "procedural"},
		{"court", 0.10, "court_dependency", "procedural"},
		{"ruling", 0.10, "ruling_dependency", "procedural"},
		{"supreme court", 0.05, "supreme_court_dependency", "procedural"}, // additive to "court"
		{"injunction", 0.15, "injunction_dependency", "procedural"},
		{"impeachment", 0.10, "impeachment_dependency", "procedural"},
		// Wording risk — vague/subjective phrasing.
		{"definitive", 0.10, "definitive_terminology", "wording"},
		{"first", 0.05, "first_terminology", "wording"},
		{"considered", 0.10, "considered_subjective", "wording"},
		{"significant", 0.08, "significant_subjective", "wording"},
		{"material", 0.05, "material_subjective", "wording"},
		{"primarily", 0.05, "primarily_qualifier", "wording"},
		{"largely", 0.05, "largely_qualifier", "wording"},
		// Deadline ambiguity.
		{"by end of", 0.10, "vague_deadline", "deadline"},
		{"end of year", 0.05, "end_of_year_deadline", "deadline"},
		{"end of month", 0.05, "end_of_month_deadline", "deadline"},
		{"on or before", 0.05, "soft_deadline", "deadline"},
		// Source specificity — UNNAMED sources raise risk.
		{"announced", 0.10, "announcement_dependency", "source"},
		{"official", 0.10, "official_source_dependency", "source"},
		{"officially", 0.10, "officially_qualifier", "source"},
		{"reports say", 0.10, "unnamed_reports", "source"},
		{"sources say", 0.10, "unnamed_sources", "source"},
		{"according to reports", 0.10, "unnamed_reports", "source"},
	}
	proceduralHits := 0
	wordingHits := 0
	seenReasons := map[string]bool{}
	for _, m := range markers {
		if strings.Contains(text, m.needle) {
			v.AmbiguityScore += m.weight
			if !seenReasons[m.reason] {
				v.Reasons = append(v.Reasons, m.reason)
				seenReasons[m.reason] = true
			}
			switch m.category {
			case "procedural":
				proceduralHits++
			case "wording":
				wordingHits++
			}
		}
	}

	// Procedural-complexity bonus: ≥3 distinct procedural markers
	// is materially more ambiguous than one (court + appeal +
	// certification can each flip the verdict independently).
	if proceduralHits >= 3 {
		v.AmbiguityScore += 0.10
		v.Reasons = append(v.Reasons, "procedural_complexity_high")
	} else if proceduralHits >= 2 {
		v.AmbiguityScore += 0.05
		v.Reasons = append(v.Reasons, "procedural_complexity_medium")
	}

	// Wording bonus: ≥3 vague terms means subjective interpretation
	// likely.
	if wordingHits >= 3 {
		v.AmbiguityScore += 0.05
		v.Reasons = append(v.Reasons, "wording_risk_high")
	}

	// Source specificity bonus — NAMED sources LOWER risk (negative
	// weight). AP/Reuters/NYT/Bloomberg/AFP are widely trusted; their
	// presence is a partial offset against the "official"-only
	// dependency.
	namedSources := []string{
		"associated press", "ap ", "reuters", "bloomberg", "afp",
		"new york times", "nyt", "wall street journal", "wsj",
		"washington post",
	}
	for _, ns := range namedSources {
		if strings.Contains(text, ns) {
			v.AmbiguityScore -= 0.05
			v.Reasons = append(v.Reasons, "named_source_offset:"+strings.TrimSpace(ns))
			break // single offset, not stacked
		}
	}

	// Catalyst kind hint augments.
	switch in.CatalystKindHint {
	case "court_ruling", "certification":
		v.AmbiguityScore += 0.10
		v.Reasons = append(v.Reasons, "high_risk_catalyst_kind")
	}

	// Clamp.
	if v.AmbiguityScore < 0 {
		v.AmbiguityScore = 0
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
		fmt.Sprintf("ambiguity=%.2f dispute=%.2f procedural=%d wording=%d action=%s",
			v.AmbiguityScore, v.DisputeRisk, proceduralHits, wordingHits, v.Action))
	v.Features["ambiguity_score"] = v.AmbiguityScore
	v.Features["dispute_risk"] = v.DisputeRisk
	v.Features["procedural_hits"] = proceduralHits
	v.Features["wording_hits"] = wordingHits
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
