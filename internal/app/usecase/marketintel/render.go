// render.go — v9.7 marketintel Telegram rendering.
//
// The v9.7 pass moved the "Markets to watch" rows to carry inline
// Polymarket / Grafana links AND added a deterministic "Important
// Polymarket events" section that lists the freshest annotations for
// the top candidate events, including annotation source links.
//
// Three non-negotiable invariants:
//
//  1. Every URL passes through alerting.SanitizeLinkURL — broken /
//     localhost / loopback URLs are elided so Telegram never receives
//     a dead-link bullet.
//  2. No orphan "links:" line. When every link for a row is unsafe
//     or unconfigured, the line is dropped entirely (not rendered
//     with a label and zero hrefs).
//  3. Every operator-facing field is html.EscapeString'd. Polymarket-
//     and AI-authored strings are DATA, never instructions.
package marketintel

import (
	"encoding/json"
	"fmt"
	"html"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/analysis"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/alerting"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/repository"
)

// LinkConfig tunes the per-row link rendering. Zero values are safe
// defaults — every field passes through SanitizeLinkURL, so a misset
// base URL silently elides instead of rendering a broken link.
type LinkConfig struct {
	PolymarketBase     string
	GrafanaBase        string
	GrafanaDashUID     string
	GrafanaContext     time.Duration
	SourceLinksEnabled bool
	MaxSourceLinks     int
	MaxLinksPerRow     int
}

func (c LinkConfig) maxLinks() int {
	if c.MaxLinksPerRow <= 0 {
		return 5
	}
	return c.MaxLinksPerRow
}

func (c LinkConfig) maxSourceLinks() int {
	if c.MaxSourceLinks <= 0 {
		return 3
	}
	return c.MaxSourceLinks
}

// AnnotationItem is the rendered shape for one row in the "Important
// Polymarket events" section. The worker builds these by joining the
// candidate set (for event_slug + title) against the recent
// polymarket_event_annotations rows.
type AnnotationItem struct {
	EventSlug   string
	MarketTitle string
	Timestamp   time.Time
	Outcome     string
	PriceBefore *float64
	PriceAfter  *float64
	Title       string
	Summary     string
	SourcesJSON []byte
	SourceName  string
}

// FallbackInfo carries the operator-facing reason a deterministic
// fallback shipped instead of the AI summary. Both fields may be
// empty when the AI summary IS available.
type FallbackInfo struct {
	Reason  string // canonical: timeout | retry_exhausted | rate_limited | quota_exceeded | budget_denied | provider_error
	Message string // free-text amplification — short, HTML-escaped on render
}

// RenderInput is everything the renderer needs. Marketintel.Worker
// fills this struct from request + AI response + annotation fetch
// before calling Render.
type RenderInput struct {
	Request     analysis.MarketReportRequest
	AIResult    analysis.MarketReportAnalysis
	Candidates  []repository.IntelligenceCandidate
	Annotations []AnnotationItem
	Fallback    FallbackInfo
	Links       LinkConfig
	VisibleN    int // hard cap on rendered Markets to watch rows

	// v10.5 universal header inputs.
	Frequency    time.Duration
	LastPostedAt time.Time
	Now          time.Time
}

// LinkTallies counts how many of each kind landed in the rendered
// body. Worker uses this to push the per-kind metric so dashboards
// can verify links are actually shipping.
type LinkTallies struct {
	Event    int
	Market   int
	Category int
	Grafana  int
	Source   int
}

// Render builds the Telegram body + a tally of links emitted. The
// body is HTML for Telegram parse_mode=HTML and ALWAYS includes the
// Overview + Markets-to-watch sections (PART 4 — never skip on AI
// failure). Empty result indicates a fully-empty deterministic
// report; the worker is expected to skip Telegram delivery in that
// case via shouldSkipEmpty().
func Render(in RenderInput) (string, LinkTallies) {
	var (
		b     strings.Builder
		tally LinkTallies
	)
	// v10.5 universal header — Type / Trigger / Strategy / AI.
	aiStatus := alerting.AIStatusUnknown
	switch {
	case in.AIResult.Status == analysis.StatusOK && strings.TrimSpace(in.AIResult.ReportText) != "":
		aiStatus = alerting.AIStatusOK
	case in.Fallback.Reason != "":
		aiStatus = alerting.AIStatusFallback
	case in.AIResult.Status == analysis.StatusSkipped:
		aiStatus = alerting.AIStatusSkipped
	case in.AIResult.Status == analysis.StatusError:
		aiStatus = alerting.AIStatusError
	}
	freq := in.Frequency
	if freq <= 0 {
		freq = 2 * time.Hour
	}
	now := in.Now
	if now.IsZero() {
		now = in.Request.PeriodEnd
	}
	header := alerting.RenderHeader(alerting.HeaderInput{
		Type:         alerting.MessageTypeMarketIntel,
		Frequency:    freq,
		LastPostedAt: in.LastPostedAt,
		Now:          now,
		Strategies:   []string{"market_intel"},
		AI: alerting.AIInfo{
			Status:       aiStatus,
			CostUSD:      in.AIResult.EstimatedCostUSD,
			PromptTokens: in.AIResult.PromptTokens,
			OutputTokens: in.AIResult.CompletionTokens,
		},
	})
	b.WriteString(header)

	b.WriteString("<b>MARKET INTELLIGENCE</b> · 2h\n")
	fmt.Fprintf(&b, "\nperiod: %s — %s\n",
		in.Request.PeriodStart.UTC().Format(time.RFC3339),
		in.Request.PeriodEnd.UTC().Format(time.RFC3339))

	b.WriteString("\n<b>Overview</b>\n")
	fmt.Fprintf(&b, "• markets evaluated: %d\n", len(in.Request.Markets))
	fmt.Fprintf(&b, "• whale-flow candidates: %d\n", in.Request.WhaleFlowCandidates)
	fmt.Fprintf(&b, "• stable favorites: %d\n", in.Request.StableFavorites)
	fmt.Fprintf(&b, "• asymmetric setups: %d\n", in.Request.AsymmetricSetups)
	fmt.Fprintf(&b, "• developing signals: %d\n", in.Request.DevelopingSignals)

	if len(in.Request.Markets) > 0 {
		b.WriteString("\n<b>Markets to watch</b>\n")
		n := len(in.Request.Markets)
		if in.VisibleN > 0 && n > in.VisibleN {
			n = in.VisibleN
		}
		// Defensive: candidate list and request markets are 1:1 by
		// construction, but never rely on that — fall back gracefully
		// when the slice lengths drift.
		for i := 0; i < n; i++ {
			m := in.Request.Markets[i]
			var cand repository.IntelligenceCandidate
			if i < len(in.Candidates) {
				cand = in.Candidates[i]
			}
			fmt.Fprintf(&b, "%d. %s — lifecycle %.0f%%, price %.2f, vol24h $%.0f, alerts24h %d\n",
				i+1,
				html.EscapeString(truncate(m.Title, 80)),
				m.LifecyclePct, m.Probability, m.Volume24hUSD, m.AlertsLast24h,
			)
			if line, c := renderMarketLinkLine(cand, in.Links); line != "" {
				fmt.Fprintf(&b, "   %s\n", line)
				tally.Event += c.Event
				tally.Market += c.Market
				tally.Category += c.Category
				tally.Grafana += c.Grafana
			}
		}
	}

	if len(in.Annotations) > 0 {
		b.WriteString("\n<b>Important Polymarket events</b>\n")
		for i, a := range in.Annotations {
			ts := "—"
			if !a.Timestamp.IsZero() {
				ts = a.Timestamp.UTC().Format("2006-01-02 15:04 MST")
			}
			ctx := a.MarketTitle
			if ctx == "" {
				ctx = a.EventSlug
			}
			// Header: "1. <date> · <event/market> · <pB>→<pA>"
			pricePart := ""
			if a.PriceBefore != nil && a.PriceAfter != nil {
				pricePart = fmt.Sprintf(" · %.2f→%.2f", *a.PriceBefore, *a.PriceAfter)
			}
			fmt.Fprintf(&b, "%d. %s · %s%s\n",
				i+1, html.EscapeString(ts),
				html.EscapeString(truncate(ctx, 70)), pricePart,
			)
			if a.Title != "" {
				fmt.Fprintf(&b, "   %s\n", html.EscapeString(truncate(a.Title, 160)))
			}
			names, sourceLinks, count := renderAnnotationSources(a, in.Links)
			if len(names) > 0 {
				fmt.Fprintf(&b, "   sources: %s\n", strings.Join(escapeAll(names), " · "))
			}
			if count > 0 {
				// "Event · Source 1 · Source 2 …" — wrap the event
				// URL as an anchor before joining so the line is all
				// anchors, never a raw URL fragment.
				leading := []string{}
				if href := eventLink(a.EventSlug, in.Links); href != "" {
					leading = append(leading, alerting.RenderLink("Event", href))
					tally.Event++
				}
				line := assembleLinksLine(leading, sourceLinks, in.Links.maxLinks())
				if line != "" {
					fmt.Fprintf(&b, "   links: %s\n", line)
				}
				tally.Source += len(sourceLinks)
			}
		}
	}

	b.WriteString("\n<b>AI analysis</b>\n")
	switch {
	case in.AIResult.Status == analysis.StatusOK && strings.TrimSpace(in.AIResult.ReportText) != "":
		for i, line := range strings.Split(strings.TrimSpace(in.AIResult.ReportText), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			prefix := "  "
			if i == 0 {
				prefix = "• "
			}
			b.WriteString(prefix)
			b.WriteString(html.EscapeString(line))
			b.WriteString("\n")
		}
	case in.Fallback.Reason != "":
		b.WriteString("• AI summary unavailable: ")
		b.WriteString(html.EscapeString(in.Fallback.Reason))
		if in.Fallback.Message != "" {
			b.WriteString(" (")
			b.WriteString(html.EscapeString(truncate(in.Fallback.Message, 120)))
			b.WriteString(")")
		}
		b.WriteString(". Candidate list above is unranked.\n")
	default:
		b.WriteString("• AI summary unavailable. Candidate list above is unranked.\n")
	}
	return b.String(), tally
}

// renderMarketLinkLine returns the "links: …" line for one row plus
// per-kind counts; empty string => the line is elided (no orphan).
func renderMarketLinkLine(c repository.IntelligenceCandidate, lc LinkConfig) (string, LinkTallies) {
	var t LinkTallies
	links := make([]string, 0, 4)
	if href := eventLink(c.EventSlug, lc); href != "" {
		links = append(links, alerting.RenderLink("Polymarket event", href))
		t.Event++
	}
	if href := marketLink(c.MarketSlug, lc); href != "" {
		links = append(links, alerting.RenderLink("Market", href))
		t.Market++
	}
	if href := categoryLink(c.CategorySlug, lc); href != "" {
		links = append(links, alerting.RenderLink("Category", href))
		t.Category++
	}
	if href := grafanaLink(c, lc); href != "" {
		links = append(links, alerting.RenderLink("Grafana", href))
		t.Grafana++
	}
	if len(links) == 0 {
		return "", t
	}
	// Cap visible links per row. Excess silently dropped (the
	// canonical few are always the front of the slice).
	maxN := lc.maxLinks()
	if len(links) > maxN {
		links = links[:maxN]
	}
	return "links: " + strings.Join(links, " · "), t
}

// renderAnnotationSources extracts plaintext source names + clickable
// source-URL anchors from an annotation's sources_json. Returns
// (names, link-anchors, total). Total counts the anchors emitted; the
// names list is a separate plaintext display for sources without URLs.
func renderAnnotationSources(a AnnotationItem, lc LinkConfig) ([]string, []string, int) {
	if !lc.SourceLinksEnabled {
		return nil, nil, 0
	}
	type sourceRow struct {
		Name string `json:"name"`
		URL  string `json:"url"`
	}
	var rows []sourceRow
	_ = json.Unmarshal(a.SourcesJSON, &rows)
	// Fallback to the scalar Source field when sources_json is empty
	// or unparseable. Plaintext-only — there's no URL on the scalar.
	if len(rows) == 0 && strings.TrimSpace(a.SourceName) != "" {
		return []string{a.SourceName}, nil, 0
	}
	cap := lc.maxSourceLinks()
	names := make([]string, 0, cap)
	links := make([]string, 0, cap)
	seen := map[string]bool{}
	for _, r := range rows {
		name := strings.TrimSpace(r.Name)
		if name == "" || seen[strings.ToLower(name)] {
			continue
		}
		rawURL := strings.TrimSpace(r.URL)
		if rawURL == "" {
			// Plaintext fallback when NO URL was ever provided.
			// Does NOT consume the link cap.
			if len(name) <= 28 {
				names = append(names, name)
				seen[strings.ToLower(name)] = true
			}
			continue
		}
		safe := alerting.SanitizeLinkURL(rawURL)
		if safe == "" {
			// Source was meant to be a link but the URL is unsafe.
			// Skip the entry entirely — never render the raw URL as
			// plaintext (it would be a giant ugly string per spec).
			seen[strings.ToLower(name)] = true
			continue
		}
		links = append(links, alerting.RenderLink(truncate(name, 28), safe))
		seen[strings.ToLower(name)] = true
		if len(links) >= cap {
			break
		}
	}
	return names, links, len(links)
}

// assembleLinksLine merges leading anchors (e.g. the event link)
// with the source-URL anchors, capping at maxN, and returns the
// joined string. Empty leading or trailing slices are tolerated.
func assembleLinksLine(leading []string, sources []string, maxN int) string {
	out := make([]string, 0, len(leading)+len(sources))
	for _, s := range leading {
		if strings.TrimSpace(s) == "" {
			continue
		}
		out = append(out, s)
	}
	out = append(out, sources...)
	if len(out) == 0 {
		return ""
	}
	if maxN > 0 && len(out) > maxN {
		out = out[:maxN]
	}
	return strings.Join(out, " · ")
}

func eventLink(eventSlug string, lc LinkConfig) string {
	if lc.PolymarketBase == "" || eventSlug == "" {
		return ""
	}
	return alerting.SanitizeLinkURL(joinURL(lc.PolymarketBase, "event", eventSlug))
}

func marketLink(marketSlug string, lc LinkConfig) string {
	if lc.PolymarketBase == "" || marketSlug == "" {
		return ""
	}
	// Polymarket 308-redirects /markets/<slug> → /markets/<slug>;
	// market_slug-level pages are valid for sub-card markets within
	// the same event.
	return alerting.SanitizeLinkURL(joinURL(lc.PolymarketBase, "markets", marketSlug))
}

func categoryLink(categorySlug string, lc LinkConfig) string {
	if lc.PolymarketBase == "" || categorySlug == "" {
		return ""
	}
	return alerting.SanitizeLinkURL(joinURL(lc.PolymarketBase, "predictions", categorySlug))
}

func grafanaLink(c repository.IntelligenceCandidate, lc LinkConfig) string {
	if lc.GrafanaBase == "" || lc.GrafanaDashUID == "" {
		return ""
	}
	u, err := url.Parse(lc.GrafanaBase)
	if err != nil {
		return ""
	}
	base := strings.TrimRight(u.Path, "/")
	u.Path = base + "/d/" + lc.GrafanaDashUID + "/"

	q := url.Values{}
	q.Set("orgId", "1")
	now := time.Now().UTC()
	window := lc.GrafanaContext
	if window <= 0 {
		window = time.Hour
	}
	q.Set("from", strconv.FormatInt(now.Add(-window).UnixMilli(), 10))
	q.Set("to", strconv.FormatInt(now.Add(window).UnixMilli(), 10))
	if c.Category != "" {
		q.Set("var-category", c.Category)
	}
	if c.MarketSlug != "" {
		q.Set("var-market", c.MarketSlug)
	}
	u.RawQuery = q.Encode()
	return alerting.SanitizeLinkURL(u.String())
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

func escapeAll(xs []string) []string {
	out := make([]string, len(xs))
	for i, s := range xs {
		out[i] = html.EscapeString(s)
	}
	return out
}

func truncate(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
