package evolution

import (
	"fmt"
	"html"
	"strings"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/eventflow"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/eventpagecontext"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/marketprediction"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/repricing"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/anomaly"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/alerting"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/repository"
)

// renderMarketSnapshot produces the {{MARKET_DATA}} block for the
// evolution AI prompt: compact "key: value" lines.
func renderMarketSnapshot(pred repository.MarketPrediction, page eventpagecontext.Summary) string {
	var b strings.Builder
	fmt.Fprintf(&b, "event_slug: %s\n", pred.EventSlug)
	fmt.Fprintf(&b, "condition_id: %s\n", pred.ConditionID)
	if pred.Outcome != "" {
		fmt.Fprintf(&b, "outcome: %s\n", pred.Outcome)
	}
	if pred.SideBias != "" {
		fmt.Fprintf(&b, "side_bias: %s\n", pred.SideBias)
	}
	for _, m := range page.Markets {
		if m.ConditionID != pred.ConditionID {
			continue
		}
		if len(m.OutcomePrices) > 0 {
			fmt.Fprintf(&b, "outcome_prices: %s\n", strings.Join(m.OutcomePrices, ", "))
		}
		if m.LastTradePrice != nil {
			fmt.Fprintf(&b, "last_trade_price: %.4f\n", *m.LastTradePrice)
		}
		if m.OneDayPriceChange != nil {
			fmt.Fprintf(&b, "one_day_price_change: %+.4f\n", *m.OneDayPriceChange)
		}
		if m.OneHourPriceChange != nil {
			fmt.Fprintf(&b, "one_hour_price_change: %+.4f\n", *m.OneHourPriceChange)
		}
		fmt.Fprintf(&b, "volume_24h_usd: %.0f\n", m.Volume24h)
		break
	}
	return b.String()
}

// renderAnnotationsBlock — newest first, capped at 6.
func renderAnnotationsBlock(page eventpagecontext.Summary) string {
	if len(page.Annotations) == 0 {
		return ""
	}
	const max = 6
	rows := page.Annotations
	if len(rows) > max {
		rows = rows[:max]
	}
	var b strings.Builder
	for _, a := range rows {
		date := "—"
		if !a.Timestamp.IsZero() {
			date = a.Timestamp.UTC().Format("2006-01-02")
		}
		fmt.Fprintf(&b, "- %s | %s | outcome=%s",
			date, oneLine(a.Title), nzCompact(a.Outcome))
		if a.PriceBefore != nil && a.PriceAfter != nil {
			fmt.Fprintf(&b, " | price %.3f -> %.3f", *a.PriceBefore, *a.PriceAfter)
		}
		if a.PriceChange != nil {
			fmt.Fprintf(&b, " (change %+.3f)", *a.PriceChange)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// renderCatalystsBlock — active catalysts only.
func renderCatalystsBlock(rows []repository.EventCatalyst) string {
	if len(rows) == 0 {
		return ""
	}
	var b strings.Builder
	for _, c := range rows {
		eta := "tbd"
		if !c.ExpectedAt.IsZero() {
			eta = c.ExpectedAt.UTC().Format(time.RFC3339)
		}
		fmt.Fprintf(&b, "- type=%s | status=%s | expected_at=%s | confidence=%.2f | title=%s\n",
			c.CatalystType, c.Status, eta, c.Confidence, oneLine(c.Title))
	}
	return b.String()
}

// renderRepricingBlock — the latest signal, if any.
func renderRepricingBlock(sig *repricing.Signal) string {
	if sig == nil {
		return ""
	}
	return sig.RenderPromptBlock()
}

// renderMatchedAlertsBlock — top by score.
func renderMatchedAlertsBlock(rows []marketprediction.MatchedAlert) string {
	if len(rows) == 0 {
		return ""
	}
	const cap = 5
	if len(rows) > cap {
		rows = rows[:cap]
	}
	var b strings.Builder
	for _, m := range rows {
		fmt.Fprintf(&b, "- %s · %s · score=%.2f · %s\n",
			m.Severity, m.Kind, m.Score, m.DirectionAlignment)
	}
	return b.String()
}

// EvolutionRenderInput is the Telegram PREDICTION UPDATE payload.
// v10.5 added Event/Market/Price metadata + AI cost/status fields so
// the body can render the universal alerting header + per-row links.
type EvolutionRenderInput struct {
	OldState    string
	NewState    string
	Reason      string
	MarketTitle string
	ConditionID string
	Repricing   *repricing.Signal
	Flow        *eventflow.EventFlowSummary
	Catalysts   []repository.EventCatalyst
	Matched     []marketprediction.MatchedAlert
	AIText      string

	// v10.5: market context for the "Market" section + links.
	EventSlug     string
	MarketSlug    string
	CategorySlug  string
	CategoryLabel string
	Price         *float64
	LifecyclePct  float64
	Volume24hUSD  float64

	// v10.5: header inputs.
	LastPostedAt   time.Time
	Now            time.Time
	TriggeredBy    string
	AIStatus       alerting.AIStatus
	AICostUSD      float64
	AIPromptTokens int
	AIOutputTokens int

	// v10.5: link config.
	PolymarketBase string
	GrafanaBase    string
	GrafanaDashUID string
	GrafanaContext time.Duration
}

// RenderEvolutionUpdate produces the HTML body the worker posts.
// v10.5 redesign:
//
//  1. Universal header (Type / Trigger / Strategy / AI).
//  2. Title is the STATE — never the market title, NEVER AI text.
//  3. Market section carries market title, price, lifecycle, vol24h.
//  4. Decision / Catalyst / Repricing / Flow / AI sections.
//  5. Links section uses the centralised RenderLinksBlock.
//  6. AI text is in its own section, bounded.
//
// HTML-escapes every Polymarket / AI-authored field; sections elide
// when empty.
func RenderEvolutionUpdate(in EvolutionRenderInput) string {
	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	aiStatus := in.AIStatus
	if aiStatus == "" {
		if strings.TrimSpace(in.AIText) != "" {
			aiStatus = alerting.AIStatusOK
		} else {
			aiStatus = alerting.AIStatusSkipped
		}
	}
	var b strings.Builder

	// PART 8 universal header.
	header := alerting.RenderHeader(alerting.HeaderInput{
		Type:         alerting.MessageTypePredictionUpdate,
		Now:          now,
		LastPostedAt: in.LastPostedAt,
		TriggeredBy:  firstNonEmpty(in.TriggeredBy, "prediction_state_change"),
		Strategies:   []string{"prediction_update"},
		AI: alerting.AIInfo{
			Status:       aiStatus,
			CostUSD:      in.AICostUSD,
			PromptTokens: in.AIPromptTokens,
			OutputTokens: in.AIOutputTokens,
		},
	})
	b.WriteString(header)

	// Title — STATE ONLY. No market title, no AI text.
	fmt.Fprintf(&b, "<b>PREDICTION UPDATE</b> · %s\n", html.EscapeString(in.NewState))

	// Market section — carries title + price + lifecycle + vol24h.
	marketTitle := strings.TrimSpace(in.MarketTitle)
	if marketTitle != "" || in.Price != nil || in.LifecyclePct > 0 || in.Volume24hUSD > 0 {
		b.WriteString("\n<b>Market</b>\n")
		if marketTitle != "" {
			fmt.Fprintf(&b, "• %s\n", html.EscapeString(truncate(marketTitle, 120)))
		}
		if in.Price != nil {
			fmt.Fprintf(&b, "• price: %.2f\n", *in.Price)
		}
		if in.LifecyclePct > 0 {
			fmt.Fprintf(&b, "• lifecycle: %.0f%%\n", in.LifecyclePct)
		}
		if in.Volume24hUSD > 0 {
			fmt.Fprintf(&b, "• vol24h: $%.0f\n", in.Volume24hUSD)
		}
	}

	// Decision section.
	b.WriteString("\n<b>Decision</b>\n")
	stance := "no new trade"
	if in.NewState == "active_catalyst" || in.NewState == "blocked" {
		stance = "no new trade — wait for catalyst"
	}
	fmt.Fprintf(&b, "• stance: %s\n", html.EscapeString(stance))
	if in.OldState != "" {
		fmt.Fprintf(&b, "• state: %s → %s\n",
			html.EscapeString(in.OldState), html.EscapeString(in.NewState))
	} else {
		fmt.Fprintf(&b, "• state: %s\n", html.EscapeString(in.NewState))
	}
	if r := strings.TrimSpace(in.Reason); r != "" {
		fmt.Fprintf(&b, "• reason: %s\n", html.EscapeString(truncate(oneLine(r), 240)))
	}

	if len(in.Catalysts) > 0 {
		b.WriteString("\n<b>Catalyst</b>\n")
		c := in.Catalysts[0]
		eta := "tbd"
		if !c.ExpectedAt.IsZero() {
			eta = c.ExpectedAt.UTC().Format(time.RFC3339)
		}
		fmt.Fprintf(&b, "• blocked until: %s\n", html.EscapeString(eta))
		fmt.Fprintf(&b, "• status: %s\n", html.EscapeString(string(c.Status)))
		if c.Title != "" {
			fmt.Fprintf(&b, "• event: %s\n", html.EscapeString(truncate(oneLine(c.Title), 140)))
		}
		if c.Description != "" {
			fmt.Fprintf(&b, "• why it matters: %s\n", html.EscapeString(truncate(oneLine(c.Description), 240)))
		}
	}

	if in.Repricing != nil {
		b.WriteString("\n<b>Repricing</b>\n")
		fmt.Fprintf(&b, "• status: %s\n", html.EscapeString(in.Repricing.RepricingStatus))
		fmt.Fprintf(&b, "• flow timing: %s\n", html.EscapeString(in.Repricing.FlowTiming))
		if in.Repricing.PriceBefore != nil && in.Repricing.PriceAfter != nil {
			cur := "n/a"
			if in.Repricing.CurrentPrice != nil {
				cur = fmt.Sprintf("%.3f", *in.Repricing.CurrentPrice)
			}
			fmt.Fprintf(&b, "• before/after/current: %.3f → %.3f → %s\n",
				*in.Repricing.PriceBefore, *in.Repricing.PriceAfter, cur)
		}
	}

	if in.Flow != nil && !in.Flow.Empty() {
		b.WriteString("\n<b>Flow</b>\n")
		if in.Flow.StrongestSide != "" {
			fmt.Fprintf(&b, "• strongest side: %s on %s\n",
				html.EscapeString(in.Flow.StrongestSide), html.EscapeString(in.Flow.StrongestOutcome))
		}
		fmt.Fprintf(&b, "• matched alerts: %d\n", len(in.Matched))
		fmt.Fprintf(&b, "• directional imbalance: %+.2f\n", in.Flow.DirectionalImbalance)
	}

	if ai := strings.TrimSpace(in.AIText); ai != "" {
		b.WriteString("\n<b>AI analysis</b>\n")
		// Cap AI text at 1800 chars per spec.
		ai = truncate(ai, 1800)
		r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
		b.WriteString(r.Replace(ai))
		b.WriteString("\n")
	}

	// Links — centralised renderer. Returns "" when no link config.
	links := alerting.RenderLinksBlock(alerting.LinksInput{
		PolymarketBase: in.PolymarketBase,
		GrafanaBase:    in.GrafanaBase,
		GrafanaDashUID: in.GrafanaDashUID,
		GrafanaContext: in.GrafanaContext,
		EventSlug:      in.EventSlug,
		MarketSlug:     in.MarketSlug,
		ConditionID:    in.ConditionID,
		CategorySlug:   in.CategorySlug,
		CategoryLabel:  in.CategoryLabel,
		At:             now,
	})
	if links != "" {
		b.WriteString("\n")
		b.WriteString(links)
	}

	return b.String()
}

// marketSlugFromSummary returns the per-condition slug carried in
// the event-page snapshot. Empty when the snapshot is unavailable.
func marketSlugFromSummary(s eventpagecontext.Summary, conditionID string) string {
	for _, m := range s.Markets {
		if m.ConditionID == conditionID {
			return m.MarketSlug
		}
	}
	return ""
}

// latestPriceFor returns the per-condition price snapshot.
func latestPriceFor(s eventpagecontext.Summary, conditionID string) *float64 {
	for _, m := range s.Markets {
		if m.ConditionID == conditionID && m.LastTradePrice != nil {
			v := *m.LastTradePrice
			return &v
		}
	}
	return nil
}

// lifecyclePctFromSummary is a coarse derived value — the
// Summary doesn't carry a lifecycle percent today, so we return 0
// (the renderer elides the line). Hook left for v10.6.
func lifecyclePctFromSummary(_ eventpagecontext.Summary, _ string) float64 { return 0 }

// volume24hFromSummary returns the per-condition 24h USD volume.
func volume24hFromSummary(s eventpagecontext.Summary, conditionID string) float64 {
	for _, m := range s.Markets {
		if m.ConditionID == conditionID {
			return m.Volume24h
		}
	}
	return 0
}

func firstNonEmpty(xs ...string) string {
	for _, s := range xs {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

func truncate(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func oneLine(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\n", " "), "\r", " ")
}

func nzCompact(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}

// anomalyKind + anomalySeverity coerce stringly-typed enum values
// from the flow summary into the typed enums marketprediction.Score
// expects.
func anomalyKind(s string) anomaly.Kind {
	return anomaly.Kind(s)
}

func anomalySeverity(s string) anomaly.Severity {
	return anomaly.Severity(s)
}
