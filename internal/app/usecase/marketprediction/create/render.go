package create

import (
	"fmt"
	"html"
	"strings"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/eventpagecontext"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/repricing"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/analysis"
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
type CreationRenderInput struct {
	EventSlug   string
	Question    string
	Outcome     string
	SideBias    string
	Confidence  float64
	Summary     string
	RiskFactors string
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
	return strings.TrimRight(b.String(), "\n")
}
