// Package category provides a whitelist filter that selects which Polymarket
// categories the watchtower will monitor.
//
// Whitelist semantics: a category passes the filter when ANY whitelist
// entry is a case-insensitive substring of `slug + " " + label`. Match is
// against the category identity ONLY — market titles, event slugs, market
// slugs, and tags are deliberately NOT consulted. A sports-themed market
// filed under a whitelisted non-sports category (e.g. a FIFA question
// inside Politics) is still real prediction-market activity and is
// analysed normally.
//
// Why whitelist:
//   - Operators monitor a small set of categories at a time (Politics,
//     Macro, …). A whitelist is the natural shape: list what you want.
//   - It bounds API + DB load: only whitelisted categories drive backfill
//     and trade collection.
//   - A blacklist would force operators to enumerate every yearly variant
//     of every excluded league ("2026 NBA Playoffs", "2027 NBA Playoffs",
//     …). The whitelist sidesteps this entirely.
//
// Filtering is applied twice for defence in depth:
//   - in discover, to strip non-whitelisted category ids from
//     market.Categories before they reach the registry; and
//   - in detect, so any leak still can't fire an alert.
package category

import "strings"

// Filter holds the normalised whitelist tokens.
type Filter struct {
	tokens []string
}

// NewFilter constructs a Filter from a list of raw whitelist entries.
// Each entry is lowercased and trimmed; empty entries are dropped.
//
// An empty whitelist disables the filter (every category passes). This is
// deliberately permissive so a misconfiguration during local development
// does not silently silence everything; operators must opt out explicitly
// by leaving the list empty.
func NewFilter(whitelist []string) *Filter {
	f := &Filter{}
	for _, t := range whitelist {
		if n := strings.TrimSpace(strings.ToLower(t)); n != "" {
			f.tokens = append(f.tokens, n)
		}
	}
	return f
}

// Allowed reports whether the (slug, label) pair survives the filter.
//
//   - Empty token set → everything passes (filter disabled).
//   - Empty (slug, label) when the filter is active → blocked. Uncategorised
//     markets cannot match a whitelist, so they are not eligible.
//   - Otherwise → the lowercased `slug + " " + label` must contain at least
//     one whitelist token.
func (f *Filter) Allowed(slug, label string) bool {
	if len(f.tokens) == 0 {
		return true
	}
	haystack := strings.ToLower(slug) + " " + strings.ToLower(label)
	if strings.TrimSpace(haystack) == "" {
		return false
	}
	for _, t := range f.tokens {
		if strings.Contains(haystack, t) {
			return true
		}
	}
	return false
}

// Tokens returns the normalised whitelist tokens (copy; safe to mutate).
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
