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
}

// RenderEvolutionUpdate produces the HTML body the worker posts.
// HTML-escapes every Polymarket / AI-authored field; sections elide
// when empty.
func RenderEvolutionUpdate(in EvolutionRenderInput) string {
	title := strings.TrimSpace(in.MarketTitle)
	if title == "" {
		title = "Polymarket event"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "<b>PREDICTION UPDATE: %s · %s</b>\n",
		html.EscapeString(in.NewState), html.EscapeString(title))

	b.WriteString("\n<b>Prediction state</b>\n")
	if in.OldState != "" {
		fmt.Fprintf(&b, "• previous: %s\n", html.EscapeString(in.OldState))
	}
	fmt.Fprintf(&b, "• current: %s\n", html.EscapeString(in.NewState))
	if r := strings.TrimSpace(in.Reason); r != "" {
		fmt.Fprintf(&b, "• reason: %s\n", html.EscapeString(r))
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

	if len(in.Catalysts) > 0 {
		b.WriteString("\n<b>Catalyst</b>\n")
		c := in.Catalysts[0]
		eta := "tbd"
		if !c.ExpectedAt.IsZero() {
			eta = c.ExpectedAt.UTC().Format(time.RFC3339)
		}
		fmt.Fprintf(&b, "• blocked until: %s\n", html.EscapeString(eta))
		fmt.Fprintf(&b, "• status: %s\n", html.EscapeString(string(c.Status)))
		if c.Description != "" {
			fmt.Fprintf(&b, "• reason: %s\n", html.EscapeString(oneLine(c.Description)))
		}
	}

	if in.Flow != nil && !in.Flow.Empty() {
		b.WriteString("\n<b>Flow confirmation</b>\n")
		if in.Flow.StrongestSide != "" {
			fmt.Fprintf(&b, "• strongest side: %s on %s\n",
				html.EscapeString(in.Flow.StrongestSide), html.EscapeString(in.Flow.StrongestOutcome))
		}
		fmt.Fprintf(&b, "• matched alerts: %d\n", len(in.Matched))
		fmt.Fprintf(&b, "• directional imbalance: %+.2f\n", in.Flow.DirectionalImbalance)
	}

	if ai := strings.TrimSpace(in.AIText); ai != "" {
		b.WriteString("\n<b>AI update</b>\n")
		r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
		b.WriteString(r.Replace(ai))
		b.WriteString("\n")
	}

	return b.String()
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
