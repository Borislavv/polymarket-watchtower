package create

import (
	"encoding/json"
	"fmt"
	"html"
	"strings"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/eventpagecontext"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/repricing"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/analysis"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/alerting"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/repository"
)

// Render helpers feed the AI prompt context blocks. They are
// duplicated rather than imported from the evolution package because
// (a) the create worker operates on PredictionCandidate, not on
// MarketPrediction, so the rendered "market snapshot" has slightly
// different shape; (b) the evolution helpers are intentionally
// package-private so neither side accidentally takes a dependency
// on the other's lifecycle. Keep them in sync if you ever change
// the structure of the prompt blocks.

func renderMarketSnapshot(c analysis.PredictionCandidate, page eventpagecontext.Summary) string {
	var b strings.Builder
	fmt.Fprintf(&b, "event_slug: %s\n", c.EventSlug)
	fmt.Fprintf(&b, "condition_id: %s\n", c.ConditionID)
	if c.Outcome != "" {
		fmt.Fprintf(&b, "outcome: %s\n", c.Outcome)
	}
	fmt.Fprintf(&b, "last_trade_price: %.3f\n", c.LastTradePrice)
	if c.OneDayPriceChange != 0 {
		fmt.Fprintf(&b, "one_day_price_change: %+.3f\n", c.OneDayPriceChange)
	}
	if c.OneWeekPriceChange != 0 {
		fmt.Fprintf(&b, "one_week_price_change: %+.3f\n", c.OneWeekPriceChange)
	}
	fmt.Fprintf(&b, "lifecycle_pct: %.0f%%\n", c.LifecyclePct)
	fmt.Fprintf(&b, "recent_alerts_24h: %d\n", c.RecentAlerts24h)
	if c.StrongestSide != "" {
		fmt.Fprintf(&b, "strongest_side: %s\n", c.StrongestSide)
	}
	if c.DirectionalSkew != 0 {
		fmt.Fprintf(&b, "directional_skew: %+.2f\n", c.DirectionalSkew)
	}
	if c.OpenCatalysts > 0 {
		fmt.Fprintf(&b, "open_catalysts: %d\n", c.OpenCatalysts)
	}
	if c.NewAnnotations24h > 0 {
		fmt.Fprintf(&b, "new_annotations_24h: %d\n", c.NewAnnotations24h)
	}
	if c.VolumeUSD24h > 0 {
		fmt.Fprintf(&b, "volume_usd_24h: %.0f\n", c.VolumeUSD24h)
	}
	if c.LiquidityUSD > 0 {
		fmt.Fprintf(&b, "liquidity_usd: %.0f\n", c.LiquidityUSD)
	}
	// Event-page-derived enrichment.
	if page.Event.Title != "" {
		fmt.Fprintf(&b, "event_title: %s\n", page.Event.Title)
	}
	if !page.Event.EndDate.IsZero() {
		fmt.Fprintf(&b, "event_end_date: %s\n", page.Event.EndDate.UTC().Format(time.RFC3339))
	}
	return strings.TrimRight(b.String(), "\n")
}

func renderAnnotationsBlock(page eventpagecontext.Summary) string {
	if len(page.Annotations) == 0 {
		return ""
	}
	var b strings.Builder
	// Newest-first, capped at 6 rows.
	rows := page.Annotations
	if len(rows) > 6 {
		rows = rows[:6]
	}
	for _, a := range rows {
		date := "—"
		if !a.Timestamp.IsZero() {
			date = a.Timestamp.UTC().Format("2006-01-02")
		}
		fmt.Fprintf(&b, "- %s | %s", date, oneLine(a.Title))
		if a.Outcome != "" {
			fmt.Fprintf(&b, " | outcome=%s", a.Outcome)
		}
		if a.PriceBefore != nil && a.PriceAfter != nil {
			fmt.Fprintf(&b, " | price %.3f -> %.3f", *a.PriceBefore, *a.PriceAfter)
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func renderCatalystsBlock(rows []repository.EventCatalyst) string {
	if len(rows) == 0 {
		return ""
	}
	var b strings.Builder
	for _, c := range rows {
		eta := "—"
		if !c.ExpectedAt.IsZero() {
			eta = c.ExpectedAt.UTC().Format(time.RFC3339)
		}
		fmt.Fprintf(&b, "- type=%s | status=%s | expected_at=%s | confidence=%.2f | title=%s\n",
			c.CatalystType, c.Status, eta, c.Confidence, oneLine(c.Title))
	}
	return strings.TrimRight(b.String(), "\n")
}

func renderRepricingBlock(sig *repricing.Signal) string {
	if sig == nil {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Repricing intelligence:\n")
	fmt.Fprintf(&b, "- repricing status: %s (confidence %.2f)\n", sig.RepricingStatus, sig.Confidence)
	if sig.FlowTiming != "" {
		fmt.Fprintf(&b, "- flow timing: %s\n", sig.FlowTiming)
	}
	if sig.PriceBefore != nil && sig.PriceAfter != nil {
		fmt.Fprintf(&b, "- annotation price: %.3f -> %.3f\n", *sig.PriceBefore, *sig.PriceAfter)
	}
	if sig.CurrentPrice != nil {
		fmt.Fprintf(&b, "- current price: %.3f\n", *sig.CurrentPrice)
	}
	if sig.SameSidePostFlowUSD != 0 || sig.OppositeSidePostFlowUSD != 0 {
		fmt.Fprintf(&b, "- post-flow USD: same=%.0f opp=%.0f\n",
			sig.SameSidePostFlowUSD, sig.OppositeSidePostFlowUSD)
	}
	if sig.Explanation != "" {
		fmt.Fprintf(&b, "- explanation: %s\n", oneLine(sig.Explanation))
	}
	return strings.TrimRight(b.String(), "\n")
}

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	return strings.TrimSpace(s)
}

// --- Telegram rendering ----------------------------------------------------

// CreationRenderInput is the structured input to the
// "PREDICTION CREATED" Telegram body. All strings are passed
// verbatim from operator-facing fields + AI output; the renderer
// is responsible for HTML escape at the boundary.
//
// The optional context blocks (Annotations, MatchedAlerts, Links)
// are appended only when populated — an empty slice renders to
// nothing, NOT to "no data". This matches the existing alert
// renderer's omit-on-empty rule so a minimal prediction (just
// summary) never ships orphan headers.
type CreationRenderInput struct {
	EventSlug   string
	Question    string
	Outcome     string
	SideBias    string
	Confidence  float64
	Summary     string
	RiskFactors string

	// Annotations is the chronological (newest-first) list of
	// Polymarket event annotations to render under
	// "Latest Polymarket events". The caller MUST pre-cap the slice
	// to the operator-configured limit (default 5).
	Annotations []repository.EventAnnotation
	// MaxAnnotationTitleChars caps the per-row title length used by
	// the renderer's compaction. 0 falls through to a sensible
	// default; callers normally pass the configured cap.
	MaxAnnotationTitleChars int
	// MaxAnnotationSourceNames caps how many citation names render
	// per annotation row. 0 falls through to a default of 3.
	MaxAnnotationSourceNames int

	// MatchedAlertCount is the count of matched Watchtower alerts
	// that pointed at the same market — populated by the worker
	// only when it has actually replayed the matched alerts. 0
	// elides the "Matched Watchtower alerts" section entirely.
	MatchedAlertCount int

	// Links — operator-actionable URLs the renderer pipes through
	// alerting.SanitizeLinkURL + alerting.RenderLink so the dead-
	// link / localhost-grafana / unsafe-scheme rules already pinned
	// by the alert tests apply here too. Empty fields elide their
	// bullet; an entirely-empty Links block elides the whole section.
	PolymarketEventURL  string
	PolymarketMarketURL string
	GrafanaURL          string
	TraderURL           string

	// --- v10.2 Prediction quality section (PART 9) -------------------
	// Optional compact block below the AI thesis. All fields elide
	// when zero/empty. UsefulnessScore is the deterministic 0..1
	// produced by the predictionusefulness package; UsefulnessReason
	// is its stable short string. Repricing / Flow / State carry the
	// current operator-facing classifier outputs.
	UsefulnessScore  float64
	UsefulnessReason string
	State            string
	RepricingStatus  string
	FlowSummary      string
}

// RenderCreationTelegram builds the HTML body used by the
// alertsender. Sections elide when their input is empty so a
// minimal prediction (just summary) doesn't ship orphan headers.
// HTML escape happens here; downstream code must not double-escape.
func RenderCreationTelegram(in CreationRenderInput) string {
	var b strings.Builder
	header := "PREDICTION CREATED"
	if in.SideBias != "" {
		header += " · " + strings.ToUpper(in.SideBias)
	}
	if in.Confidence > 0 {
		header += fmt.Sprintf(" · %.2f", in.Confidence)
	}
	if in.Question != "" {
		header += " · " + oneLine(in.Question)
	}
	fmt.Fprintf(&b, "<b>%s</b>\n", html.EscapeString(header))
	if in.EventSlug != "" {
		fmt.Fprintf(&b, "<i>event: %s</i>\n", html.EscapeString(in.EventSlug))
	}
	if in.Outcome != "" {
		fmt.Fprintf(&b, "<i>outcome: %s</i>\n", html.EscapeString(in.Outcome))
	}
	if s := strings.TrimSpace(in.Summary); s != "" {
		b.WriteString("\n")
		b.WriteString(html.EscapeString(s))
		b.WriteString("\n")
	}
	if r := strings.TrimSpace(in.RiskFactors); r != "" {
		b.WriteString("\n<b>Risk factors</b>\n")
		b.WriteString(html.EscapeString(r))
		b.WriteString("\n")
	}
	if quality := renderPredictionQualityBlock(in); quality != "" {
		b.WriteString("\n")
		b.WriteString(quality)
		b.WriteString("\n")
	}
	if in.MatchedAlertCount > 0 {
		fmt.Fprintf(&b, "\n<b>Matched Watchtower alerts</b>: %d\n", in.MatchedAlertCount)
	}
	if anns := renderAnnotationsTelegramBlock(in.Annotations, in.MaxAnnotationTitleChars, in.MaxAnnotationSourceNames); anns != "" {
		b.WriteString("\n")
		b.WriteString(anns)
		b.WriteString("\n")
	}
	if links := renderLinksTelegramBlock(in); links != "" {
		b.WriteString("\n")
		b.WriteString(links)
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderAnnotationsTelegramBlock builds the "Latest Polymarket events"
// section. Returns "" when there's nothing to render — the caller
// omits the section entirely. Per-field caps + HTML escape applied.
func renderAnnotationsTelegramBlock(rows []repository.EventAnnotation, maxTitleChars, maxSourceNames int) string {
	if len(rows) == 0 {
		return ""
	}
	if maxTitleChars <= 0 {
		maxTitleChars = 160
	}
	if maxSourceNames <= 0 {
		maxSourceNames = 3
	}
	var b strings.Builder
	b.WriteString("<b>Latest Polymarket events</b>\n")
	for i, a := range rows {
		date := "—"
		if !a.Timestamp.IsZero() {
			date = a.Timestamp.UTC().Format("2006-01-02")
		}
		// Header line: 1. <date> · <outcome> · <priceBefore>→<priceAfter> (<delta>)
		fmt.Fprintf(&b, "%d. %s", i+1, html.EscapeString(date))
		if outcome := strings.TrimSpace(a.Outcome); outcome != "" {
			fmt.Fprintf(&b, " · %s", html.EscapeString(outcome))
		}
		if a.PriceBefore != nil && a.PriceAfter != nil {
			fmt.Fprintf(&b, " · %s→%s",
				html.EscapeString(formatPrice(*a.PriceBefore)),
				html.EscapeString(formatPrice(*a.PriceAfter)))
			if a.PriceChange != nil {
				fmt.Fprintf(&b, " (%s)", html.EscapeString(formatSignedPrice(*a.PriceChange)))
			}
		}
		b.WriteString("\n")
		// Title line — truncated to MaxAnnotationTitleChars.
		if title := strings.TrimSpace(a.Title); title != "" {
			fmt.Fprintf(&b, "   %s\n", html.EscapeString(truncateChars(oneLine(title), maxTitleChars)))
		}
		// Sources line (only when we can parse them out of the
		// SourcesJSON blob). Defensive — failure here MUST NOT
		// drop the annotation row; we simply skip the sources line.
		if names := decodeSourceNames(a.SourcesJSON, maxSourceNames); len(names) > 0 {
			fmt.Fprintf(&b, "   sources: %s\n", html.EscapeString(strings.Join(names, ", ")))
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderPredictionQualityBlock builds the v10.2 compact "Prediction
// quality" section. Each row elides individually so a minimal
// prediction with no usefulness/state/etc doesn't ship orphan
// bullets. The block itself elides when every row is empty.
func renderPredictionQualityBlock(in CreationRenderInput) string {
	bullets := make([]string, 0, 5)
	if in.UsefulnessScore > 0 {
		bullets = append(bullets, fmt.Sprintf("• usefulness: %.2f", in.UsefulnessScore))
	}
	if r := strings.TrimSpace(in.UsefulnessReason); r != "" {
		bullets = append(bullets, "• reason: "+html.EscapeString(r))
	}
	if s := strings.TrimSpace(in.State); s != "" {
		bullets = append(bullets, "• state: "+html.EscapeString(s))
	}
	if s := strings.TrimSpace(in.RepricingStatus); s != "" {
		bullets = append(bullets, "• repricing: "+html.EscapeString(s))
	}
	if s := strings.TrimSpace(in.FlowSummary); s != "" {
		bullets = append(bullets, "• flow: "+html.EscapeString(s))
	}
	if len(bullets) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("<b>Prediction quality</b>\n")
	for _, line := range bullets {
		b.WriteString(line)
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderLinksTelegramBlock builds the Links section. Every URL is
// piped through alerting.SanitizeLinkURL (rejects unsafe schemes,
// loopback hosts, link-local IPs) so a localhost grafana never
// renders as a dead-text bullet. An entirely-empty section elides.
func renderLinksTelegramBlock(in CreationRenderInput) string {
	type entry struct{ label, href string }
	candidates := []entry{
		{"Polymarket event", in.PolymarketEventURL},
		{"Polymarket market", in.PolymarketMarketURL},
		{"Grafana", in.GrafanaURL},
		{"Trader", in.TraderURL},
	}
	var bullets []string
	for _, e := range candidates {
		safe := alerting.SanitizeLinkURL(e.href)
		if safe == "" {
			continue
		}
		bullets = append(bullets, "• "+alerting.RenderLink(e.label, safe))
	}
	if len(bullets) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("<b>Links</b>\n")
	for _, line := range bullets {
		b.WriteString(line)
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// formatPrice renders a 0..1 probability as "0.62" or "62%" depending
// on magnitude. Polymarket annotations historically arrive as
// 0..100 percentages; we keep the raw value and just trim the
// decimal noise so the Telegram line stays compact.
func formatPrice(v float64) string {
	if v >= 1 || v <= -1 {
		return fmt.Sprintf("%.0f", v)
	}
	return fmt.Sprintf("%.2f", v)
}

func formatSignedPrice(v float64) string {
	if v >= 1 || v <= -1 {
		return fmt.Sprintf("%+.0f", v)
	}
	return fmt.Sprintf("%+.2f", v)
}

func truncateChars(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}

// decodeSourceNames extracts source names from the annotation's
// SourcesJSON blob. Best-effort: unmarshal failure returns nil so
// the renderer simply omits the sources line. Capped at limit.
func decodeSourceNames(raw []byte, limit int) []string {
	if len(raw) == 0 {
		return nil
	}
	var rows []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil
	}
	out := make([]string, 0, limit)
	seen := map[string]bool{}
	for _, r := range rows {
		name := strings.TrimSpace(r.Name)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
		if len(out) >= limit {
			break
		}
	}
	return out
}
