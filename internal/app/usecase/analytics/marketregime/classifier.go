// Package marketregime classifies a Polymarket market into one of
// five deterministic regimes used by the v11.10-insider-prior
// strategy suite. Output is attached to strategy_shadow_decisions
// features_json so promotion review can stratify by regime.
//
// Classifier is PURE — no I/O. The orchestration layer passes
// event slug + category + title + description; the classifier
// returns the regime + the reasons it picked it.
//
// Regimes:
//
//   - geopolitics_military — wars, ceasefires, military operations.
//     Strongest cheaptail / walletcohort sensitivity (sudden
//     positioning by insider-prior wallets is the dominant pattern).
//
//   - politics_governance — elections, legislation, appointments,
//     court rulings. Strongest thesisaccum / repricinglag weight
//     (multi-market thesis convergence + post-news lag dominate).
//
//   - corporate_private_info — earnings, M&A, product launches,
//     CEO changes. Shadow-first; stronger holderdelta + catalyst
//     weighting (small holder pool means a single informed wallet
//     leaves a visible pctOI fingerprint).
//
//   - oracle_sensitive — resolution depends on an oracle (UMA,
//     Chainlink, off-chain attester) whose decision can be
//     disputed. Stronger rulesrisk; user flow blocked unless
//     dual confirmation from two non-rulesrisk strategies.
//
//   - other — anything not above. Default for catch-all categories.
package marketregime

import (
	"fmt"
	"strings"
)

// Regime is the classified market regime.
type Regime string

const (
	RegimeGeopoliticsMilitary  Regime = "geopolitics_military"
	RegimePoliticsGovernance   Regime = "politics_governance"
	RegimeCorporatePrivateInfo Regime = "corporate_private_info"
	RegimeOracleSensitive      Regime = "oracle_sensitive"
	RegimeOther                Regime = "other"
)

// Input is the pure-Classify payload.
type Input struct {
	CategorySlug    string // Polymarket category slug (lowercase preferred but case-insensitive)
	CategoryLabel   string
	Title           string
	Description     string
	ResolutionRules string
	EventSlug       string
}

// Verdict is the pure-Classify output.
type Verdict struct {
	Regime  Regime
	Score   float64 // 0..1 confidence the regime picked is correct
	Reasons []string
}

// Classifier is the pure verdict producer.
type Classifier struct{}

// New constructs a stateless classifier.
func New() *Classifier { return &Classifier{} }

// Classify scans market metadata and returns the regime with the
// highest aggregate score. Ties broken by priority order:
// oracle_sensitive > geopolitics_military > politics_governance >
// corporate_private_info > other.
//
// Priority is deliberate: oracle_sensitive is the strictest gate
// (user flow blocked without dual confirmation) so it wins ties to
// keep the safety surface tight.
func (c *Classifier) Classify(in Input) Verdict {
	text := strings.ToLower(in.CategorySlug + " " + in.CategoryLabel + " " +
		in.Title + " " + in.Description + " " + in.ResolutionRules)

	scores := map[Regime]float64{}
	reasons := map[Regime][]string{}

	type marker struct {
		needle string
		weight float64
		regime Regime
		reason string
	}
	markers := []marker{
		// geopolitics_military
		{"war", 0.30, RegimeGeopoliticsMilitary, "war_keyword"},
		{"ceasefire", 0.40, RegimeGeopoliticsMilitary, "ceasefire_keyword"},
		{"missile", 0.35, RegimeGeopoliticsMilitary, "missile_keyword"},
		{"airstrike", 0.40, RegimeGeopoliticsMilitary, "airstrike_keyword"},
		{"invasion", 0.40, RegimeGeopoliticsMilitary, "invasion_keyword"},
		{"military", 0.20, RegimeGeopoliticsMilitary, "military_keyword"},
		{"nato", 0.25, RegimeGeopoliticsMilitary, "nato_keyword"},
		{"troops", 0.25, RegimeGeopoliticsMilitary, "troops_keyword"},
		{"hostages", 0.30, RegimeGeopoliticsMilitary, "hostages_keyword"},
		{"sanctions", 0.15, RegimeGeopoliticsMilitary, "sanctions_keyword"},
		{"ukraine", 0.20, RegimeGeopoliticsMilitary, "ukraine_keyword"},
		{"russia", 0.15, RegimeGeopoliticsMilitary, "russia_keyword"},
		{"israel", 0.15, RegimeGeopoliticsMilitary, "israel_keyword"},
		{"iran", 0.15, RegimeGeopoliticsMilitary, "iran_keyword"},
		{"gaza", 0.20, RegimeGeopoliticsMilitary, "gaza_keyword"},
		{"taiwan", 0.20, RegimeGeopoliticsMilitary, "taiwan_keyword"},
		// politics_governance
		{"election", 0.30, RegimePoliticsGovernance, "election_keyword"},
		{"president", 0.20, RegimePoliticsGovernance, "president_keyword"},
		{"senate", 0.25, RegimePoliticsGovernance, "senate_keyword"},
		{"congress", 0.20, RegimePoliticsGovernance, "congress_keyword"},
		{"primary", 0.20, RegimePoliticsGovernance, "primary_keyword"},
		{"caucus", 0.25, RegimePoliticsGovernance, "caucus_keyword"},
		{"governor", 0.20, RegimePoliticsGovernance, "governor_keyword"},
		{"mayor", 0.15, RegimePoliticsGovernance, "mayor_keyword"},
		{"vote", 0.10, RegimePoliticsGovernance, "vote_keyword"},
		{"impeach", 0.30, RegimePoliticsGovernance, "impeach_keyword"},
		{"confirmation hearing", 0.20, RegimePoliticsGovernance, "confirmation_hearing_keyword"},
		{"supreme court", 0.20, RegimePoliticsGovernance, "supreme_court_keyword"},
		{"legislation", 0.20, RegimePoliticsGovernance, "legislation_keyword"},
		{"bill", 0.10, RegimePoliticsGovernance, "bill_keyword"},
		{"politics", 0.15, RegimePoliticsGovernance, "politics_category"},
		// corporate_private_info
		{"earnings", 0.30, RegimeCorporatePrivateInfo, "earnings_keyword"},
		{"revenue", 0.20, RegimeCorporatePrivateInfo, "revenue_keyword"},
		{"acquisition", 0.30, RegimeCorporatePrivateInfo, "acquisition_keyword"},
		{"merger", 0.30, RegimeCorporatePrivateInfo, "merger_keyword"},
		{"ipo", 0.30, RegimeCorporatePrivateInfo, "ipo_keyword"},
		{"ceo", 0.20, RegimeCorporatePrivateInfo, "ceo_keyword"},
		{"resign", 0.15, RegimeCorporatePrivateInfo, "resign_keyword"},
		{"layoff", 0.20, RegimeCorporatePrivateInfo, "layoff_keyword"},
		{"product launch", 0.25, RegimeCorporatePrivateInfo, "product_launch_keyword"},
		{"company", 0.10, RegimeCorporatePrivateInfo, "company_keyword"},
		// oracle_sensitive
		{"uma", 0.40, RegimeOracleSensitive, "uma_oracle_keyword"},
		{"chainlink", 0.35, RegimeOracleSensitive, "chainlink_oracle_keyword"},
		{"oracle", 0.25, RegimeOracleSensitive, "oracle_keyword"},
		{"attester", 0.25, RegimeOracleSensitive, "attester_keyword"},
		{"dispute", 0.15, RegimeOracleSensitive, "dispute_keyword"},
		{"clarification", 0.10, RegimeOracleSensitive, "clarification_keyword"},
		{"verifiable", 0.10, RegimeOracleSensitive, "verifiable_keyword"},
		{"verified", 0.10, RegimeOracleSensitive, "verified_keyword"},
	}

	for _, m := range markers {
		if strings.Contains(text, m.needle) {
			scores[m.regime] += m.weight
			reasons[m.regime] = append(reasons[m.regime], m.reason)
		}
	}

	// Priority-aware tie-break. Walk priority order; first regime
	// whose score is >= 0.20 AND >= max(other scores) wins.
	priority := []Regime{
		RegimeOracleSensitive,
		RegimeGeopoliticsMilitary,
		RegimePoliticsGovernance,
		RegimeCorporatePrivateInfo,
	}
	var pickedRegime Regime = RegimeOther
	var pickedScore float64
	for _, r := range priority {
		s := scores[r]
		if s >= 0.20 && s >= pickedScore {
			pickedRegime = r
			pickedScore = s
		}
	}

	out := Verdict{
		Regime:  pickedRegime,
		Score:   clamp(pickedScore, 0, 1),
		Reasons: reasons[pickedRegime],
	}
	out.Reasons = append(out.Reasons,
		fmt.Sprintf("regime=%s score=%.2f", pickedRegime, out.Score))
	return out
}

// RequiresDualConfirmation reports whether user flow on this regime
// must wait for ≥2 non-rulesrisk strategy hits before promotion is
// allowed to surface to users. Currently only the oracle_sensitive
// regime triggers this constraint.
func (r Regime) RequiresDualConfirmation() bool {
	return r == RegimeOracleSensitive
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
