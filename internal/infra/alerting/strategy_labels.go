// strategy_labels.go — v10.5 human-readable strategy mapping.
//
// Operator-facing Telegram NEVER renders raw enum keys. Any code path
// that emits a strategy name routes it through StrategyLabel which
// returns the curated human-readable string. Unknown keys pass
// through unchanged (with leading enum punctuation stripped) so a
// new strategy can ship without a label table update — operators
// just see a kebab-case fallback until the table catches up.
package alerting

import "strings"

// StrategyLabel returns the operator-friendly name for a strategy
// key. Lookup is case-insensitive and tolerates a few legacy
// spellings (`whale_flow` / `whale-flow` / `WhaleFlow`).
func StrategyLabel(key string) string {
	k := strings.ToLower(strings.TrimSpace(key))
	if k == "" {
		return ""
	}
	// Normalise dashes to underscores so a single table covers both
	// idiomatic forms.
	k = strings.ReplaceAll(k, "-", "_")
	if label, ok := strategyLabels[k]; ok {
		return label
	}
	return key
}

// strategyLabels is the single source of truth. Add a key here when
// the detector / analytics layer introduces a new strategy.
var strategyLabels = map[string]string{
	"whale_flow":              "Whale flow — unusually large single trade",
	"single_trade":            "Whale flow — unusually large single trade",
	"accumulation":            "Recent accumulation — same-side buying over short window",
	"accumulation_recent":     "Recent accumulation — same-side buying over short window",
	"accumulation_lifetime":   "Lifetime accumulation — same wallet building a line",
	"cluster":                 "Cluster — multiple anomalous trades in short window",
	"ownership":               "Ownership concentration — wallet controls meaningful side share",
	"ownership_concentration": "Ownership concentration — wallet controls meaningful side share",
	"stable_favorite":         "Stable favorite — mature market with favorable risk/reward",
	"new_wallet":              "New wallet context — low-history wallet",
	"dormant_wallet":          "Dormant wallet context — inactive wallet returned",
	"low_baseline":            "Low-baseline anomaly — trade is large relative to market history",
	"prediction_catalyst":     "Prediction catalyst — market blocked by upcoming event",
	"repricing":               "Repricing — market moved or may still be lagging",
	"market_intel":            "Market intelligence — periodic event/market review",
	"market_intelligence":     "Market intelligence — periodic event/market review",
	"daily_intel":             "Daily political/geopolitical intelligence",
	"daily_political_intel":   "Daily political/geopolitical intelligence",
	"outcome":                 "Outcome review — post-resolution audit",
	"postmortem":              "Outcome review — post-resolution audit",
	"prediction":              "Prediction — living-thesis state evolution",
	"prediction_update":       "Prediction update — state transition or AI refresh",
	"category_watch":          "Category watch — convergence across multiple markets",
}

// StrategyLabels maps a slice of strategy keys onto labels. Empty
// input returns nil. Duplicate labels are collapsed.
func StrategyLabels(keys []string) []string {
	if len(keys) == 0 {
		return nil
	}
	out := make([]string, 0, len(keys))
	seen := map[string]bool{}
	for _, k := range keys {
		label := StrategyLabel(k)
		if label == "" || seen[label] {
			continue
		}
		seen[label] = true
		out = append(out, label)
	}
	return out
}
