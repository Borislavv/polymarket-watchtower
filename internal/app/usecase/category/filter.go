// Package category provides a small blacklist filter that excludes Polymarket
// categories from the watchtower pipeline.
//
// Matching is case-insensitive substring against the concatenated slug+label.
// Substring is the only predicate that actually catches Polymarket's real
// sports labels — e.g. "2026 NBA Playoffs", "UEFA Champions League Winner",
// "2026 FIFA World Cup" — with a compact token list. Exact match would force
// operators to enumerate every yearly variant.
//
// Filtering is applied twice:
//   - in discover, to strip blacklisted ids from market.Categories before they
//     reach the registry (avoids feeding the per-trade scorer for those
//     buckets at all); and
//   - in detect, as a belt-and-suspenders check so any leak still can't fire
//     an alert.
package category

import "strings"

// Filter holds the normalised blacklist tokens.
type Filter struct {
	tokens []string
}

// NewFilter constructs a Filter from a list of raw blacklist entries. Each
// entry is lowercased and trimmed; empty entries are dropped.
func NewFilter(blacklist []string) *Filter {
	f := &Filter{}
	for _, t := range blacklist {
		if n := strings.TrimSpace(strings.ToLower(t)); n != "" {
			f.tokens = append(f.tokens, n)
		}
	}
	return f
}

// Allowed reports whether the (slug, label) pair survives the filter. An empty
// token set passes everything (filter disabled). When both inputs are empty
// we err on the side of allowing — we'd rather keep an uncategorised market
// than silently drop a real signal.
func (f *Filter) Allowed(slug, label string) bool {
	if len(f.tokens) == 0 {
		return true
	}
	haystack := strings.ToLower(slug) + " " + strings.ToLower(label)
	if strings.TrimSpace(haystack) == "" {
		return true
	}
	for _, t := range f.tokens {
		if strings.Contains(haystack, t) {
			return false
		}
	}
	return true
}

// Tokens returns the normalised blacklist tokens (copy; safe to mutate).
func (f *Filter) Tokens() []string {
	out := make([]string, len(f.tokens))
	copy(out, f.tokens)
	return out
}

// Summary returns a printable representation suitable for the startup log.
func (f *Filter) Summary() string {
	if len(f.tokens) == 0 {
		return "disabled"
	}
	return strings.Join(f.tokens, ",")
}
