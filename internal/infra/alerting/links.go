// links.go — v10.5 centralised link rendering.
//
// Every Telegram-bound surface uses these helpers to construct the
// "Links" block. Returning empty when a slug or base URL is missing
// AND sanitizeLinkURL pre-validating every href guarantees three
// invariants the spec asks for:
//
//  1. No broken links.
//  2. No unsafe links (loopback / localhost / non-http(s)).
//  3. No orphan "Links" header — when every href elides, the
//     block returns the empty string and the caller skips the
//     section entirely.
//
// The renderer is HTML-safe: every label + href is escaped via
// renderLink (which itself calls html.EscapeString). No surface
// passes a Polymarket-authored string through these helpers.
package alerting

import (
	"net/url"
	"strconv"
	"strings"
	"time"
)

// LinksInput is the operator-facing shape every surface fills.
// Fields are scalar — empty strings (and zero counts) cause the
// corresponding entry to elide. The Sources slice may carry up to
// MaxSourceLinks pre-validated source URLs (annotation citations).
type LinksInput struct {
	// Base URLs. Empty disables that link kind entirely.
	PolymarketBase string
	GrafanaBase    string
	GrafanaDashUID string
	GrafanaContext time.Duration

	// Per-row data.
	EventSlug     string
	MarketSlug    string
	ConditionID   string // fallback when MarketSlug is empty
	CategorySlug  string
	CategoryLabel string // optional var-category override
	Wallet        string

	// Annotation citation URLs (already validated by the caller for
	// shape; SanitizeLinkURL is re-applied here).
	Sources []SourceURL

	// Grafana extra context. Zero values are tolerated; the time
	// window centers on "now" when At is zero.
	At       time.Time
	Severity string

	// Visual + safety caps.
	MaxLinks      int // hard cap (default 5)
	MaxSourceURLs int // hard cap (default 3)
}

// SourceURL is one annotation citation. Empty Name → "Source N"
// label is rendered downstream.
type SourceURL struct {
	Name string
	URL  string
}

// BuildPolymarketEventURL constructs the canonical event page URL.
// Empty slug or base ⇒ empty string.
func BuildPolymarketEventURL(base, eventSlug string) string {
	if base == "" || eventSlug == "" {
		return ""
	}
	return SanitizeLinkURL(joinURL(base, "event", eventSlug))
}

// BuildPolymarketMarketURL prefers marketSlug; falls back to
// conditionID when slug is missing. Empty inputs ⇒ empty string.
func BuildPolymarketMarketURL(base, marketSlug, conditionID string) string {
	if base == "" {
		return ""
	}
	if marketSlug != "" {
		return SanitizeLinkURL(joinURL(base, "markets", marketSlug))
	}
	if conditionID != "" {
		return SanitizeLinkURL(joinURL(base, "markets", conditionID))
	}
	return ""
}

// BuildPolymarketCategoryURL constructs the canonical category
// /predictions/<slug> URL.
func BuildPolymarketCategoryURL(base, categorySlug string) string {
	if base == "" || categorySlug == "" {
		return ""
	}
	return SanitizeLinkURL(joinURL(base, "predictions", categorySlug))
}

// BuildTraderURL constructs the /profile/<wallet> URL.
func BuildTraderURL(base, wallet string) string {
	if base == "" || wallet == "" {
		return ""
	}
	return SanitizeLinkURL(joinURL(base, "profile", wallet))
}

// BuildGrafanaURL constructs the operator dashboard deep link with
// the appropriate orgId / from-to / var-* fields. Returns "" when
// configuration is incomplete — the writeLinks-style caller MUST
// elide the entry on empty result.
func BuildGrafanaURL(in LinksInput) string {
	if in.GrafanaBase == "" || in.GrafanaDashUID == "" {
		return ""
	}
	u, err := url.Parse(in.GrafanaBase)
	if err != nil {
		return ""
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/d/" + in.GrafanaDashUID + "/"
	now := in.At
	if now.IsZero() {
		now = time.Now().UTC()
	}
	window := in.GrafanaContext
	if window <= 0 {
		window = time.Hour
	}
	q := url.Values{}
	q.Set("orgId", "1")
	q.Set("from", strconv.FormatInt(now.Add(-window).UnixMilli(), 10))
	q.Set("to", strconv.FormatInt(now.Add(window).UnixMilli(), 10))
	if in.CategoryLabel != "" {
		q.Set("var-category", in.CategoryLabel)
	}
	if in.MarketSlug != "" {
		q.Set("var-market", in.MarketSlug)
	}
	if in.Severity != "" {
		q.Set("var-severity", in.Severity)
	}
	u.RawQuery = q.Encode()
	return SanitizeLinkURL(u.String())
}

// RenderLinksBlock builds the "<b>Links</b>" section. Returns the
// empty string when every link elided — caller MUST NOT render a
// section header on empty output.
//
// The returned block ALREADY carries the `<b>Links</b>` header and
// per-entry bullets. Callers can append it as-is to their body.
func RenderLinksBlock(in LinksInput) string {
	if in.MaxLinks <= 0 {
		in.MaxLinks = 5
	}
	if in.MaxSourceURLs <= 0 {
		in.MaxSourceURLs = 3
	}

	type entry struct{ Label, Href string }
	primary := make([]entry, 0, 5)
	if href := BuildPolymarketEventURL(in.PolymarketBase, in.EventSlug); href != "" {
		primary = append(primary, entry{"Polymarket event", href})
	}
	if href := BuildPolymarketMarketURL(in.PolymarketBase, in.MarketSlug, in.ConditionID); href != "" {
		primary = append(primary, entry{"Market", href})
	}
	if href := BuildPolymarketCategoryURL(in.PolymarketBase, in.CategorySlug); href != "" {
		primary = append(primary, entry{"Category", href})
	}
	if href := BuildTraderURL(in.PolymarketBase, in.Wallet); href != "" {
		primary = append(primary, entry{"Trader", href})
	}
	if href := BuildGrafanaURL(in); href != "" {
		primary = append(primary, entry{"Grafana", href})
	}
	// Primary entries are capped at MaxLinks (default 5). Source
	// citations stack ABOVE that cap with their own MaxSourceURLs
	// budget — operators should not lose AP / Reuters citations
	// because the Polymarket primary entry filled the slot.
	if len(primary) > in.MaxLinks {
		primary = primary[:in.MaxLinks]
	}
	sources := make([]entry, 0, in.MaxSourceURLs)
	if in.MaxSourceURLs > 0 {
		seen := map[string]bool{}
		for _, s := range in.Sources {
			if len(sources) >= in.MaxSourceURLs {
				break
			}
			href := SanitizeLinkURL(strings.TrimSpace(s.URL))
			if href == "" {
				continue
			}
			label := strings.TrimSpace(s.Name)
			if label == "" {
				label = "Source"
			}
			key := strings.ToLower(href)
			if seen[key] {
				continue
			}
			seen[key] = true
			sources = append(sources, entry{truncateLabel(label, 28), href})
		}
	}
	candidates := append(primary, sources...)
	if len(candidates) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("<b>Links</b>\n")
	for _, c := range candidates {
		b.WriteString("• ")
		b.WriteString(RenderLink(c.Label, c.Href))
		b.WriteString("\n")
	}
	return b.String()
}

// RenderLinksInline returns the "Polymarket event · Market · …" form
// used by per-row link footers (e.g. "Markets to watch"). Returns
// empty string when no links land — caller MUST NOT render a
// "links: " label on empty output.
func RenderLinksInline(in LinksInput) string {
	if in.MaxLinks <= 0 {
		in.MaxLinks = 5
	}
	links := make([]string, 0, 5)
	if href := BuildPolymarketEventURL(in.PolymarketBase, in.EventSlug); href != "" {
		links = append(links, RenderLink("Polymarket event", href))
	}
	if href := BuildPolymarketMarketURL(in.PolymarketBase, in.MarketSlug, in.ConditionID); href != "" {
		links = append(links, RenderLink("Market", href))
	}
	if href := BuildPolymarketCategoryURL(in.PolymarketBase, in.CategorySlug); href != "" {
		links = append(links, RenderLink("Category", href))
	}
	if href := BuildTraderURL(in.PolymarketBase, in.Wallet); href != "" {
		links = append(links, RenderLink("Trader", href))
	}
	if href := BuildGrafanaURL(in); href != "" {
		links = append(links, RenderLink("Grafana", href))
	}
	if len(links) == 0 {
		return ""
	}
	if len(links) > in.MaxLinks {
		links = links[:in.MaxLinks]
	}
	return strings.Join(links, " · ")
}

func joinURL(base string, segs ...string) string {
	u, err := url.Parse(base)
	if err != nil {
		return ""
	}
	cleaned := strings.TrimRight(u.Path, "/")
	for _, s := range segs {
		if s == "" {
			return ""
		}
		cleaned += "/" + s
	}
	u.Path = cleaned
	return u.String()
}

func truncateLabel(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
