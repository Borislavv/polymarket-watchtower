package openai

import (
	"fmt"
	"strings"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/analysis"
)

// buildAlertPrompt constructs the user message for a single alert.
//
// PART 6 of the Political-Catalyst Intelligence spec mandates an
// EXACT prompt template (alert_prompt.go::promptForAlert). The
// template carries six placeholders:
//
//	{{ALERT_DATA}}        — alert identity + lifecycle
//	{{MARKET_STATE}}      — side, price, odds, payoff
//	{{FLOW_DATA}}         — score, anomalies, accumulation, cross-flow
//	{{EVENT_ANNOTATIONS}} — Polymarket event-page narrative
//	{{CATALYST_CONTEXT}}  — Political-Catalyst Intelligence overlay
//	{{WEB_CONTEXT}}       — web_search disclosure / fresh-news context
//
// This builder renders each section as a structured `key: value`
// block (so the model can parse reliably) and substitutes them into
// the verbatim template via strings.NewReplacer. Empty fields elide
// to "n/a" — the template stays the same shape regardless of which
// upstream contexts are wired.
func buildAlertPrompt(req analysis.AlertAnalysisRequest) string {
	alertData := buildAlertDataBlock(req)
	marketState := buildMarketStateBlock(req)
	flowData := buildFlowDataBlock(req)
	annotations := buildAnnotationsBlock(req)
	catalyst := buildCatalystBlock(req)
	web := buildWebContextBlock(req)

	repl := strings.NewReplacer(
		"{{ALERT_DATA}}", alertData,
		"{{MARKET_STATE}}", marketState,
		"{{FLOW_DATA}}", flowData,
		"{{EVENT_ANNOTATIONS}}", annotations,
		"{{CATALYST_CONTEXT}}", catalyst,
		"{{WEB_CONTEXT}}", web,
	)
	return "ANALYZE THIS PREDICTION-MARKET ALERT.\n\n" + repl.Replace(promptForAlert)
}

// buildAlertDataBlock fills {{ALERT_DATA}}. Empty fields elide.
func buildAlertDataBlock(req analysis.AlertAnalysisRequest) string {
	var b strings.Builder
	fmt.Fprintf(&b, "alert_kind: %s\n", nz(req.Kind))
	fmt.Fprintf(&b, "severity: %s\n", nz(req.Severity))
	fmt.Fprintf(&b, "reason: %s\n", nz(req.Reason))
	if req.Title != "" {
		fmt.Fprintf(&b, "market_title: %s\n", req.Title)
	}
	if req.Category != "" {
		fmt.Fprintf(&b, "category: %s\n", req.Category)
	}
	if req.OutcomeLabel != "" {
		fmt.Fprintf(&b, "outcome: %s\n", req.OutcomeLabel)
	}
	if req.LifecyclePct > 0 {
		fmt.Fprintf(&b, "lifecycle_pct: %.1f\n", req.LifecyclePct)
	}
	if !req.EndsAt.IsZero() {
		left := req.EndsAt.Sub(req.NowAt)
		fmt.Fprintf(&b, "ends_at: %s (in %s)\n", req.EndsAt.Format(time.RFC3339), humanShort(left))
	}
	return strings.TrimRight(b.String(), "\n")
}

// buildMarketStateBlock fills {{MARKET_STATE}}.
func buildMarketStateBlock(req analysis.AlertAnalysisRequest) string {
	var b strings.Builder
	if req.Side != "" {
		fmt.Fprintf(&b, "side: %s\n", req.Side)
	}
	if req.NotionalUSD > 0 {
		fmt.Fprintf(&b, "notional_usd: %.0f\n", req.NotionalUSD)
	}
	if req.Price > 0 {
		fmt.Fprintf(&b, "price: %.4f\n", req.Price)
	}
	if req.Odds > 0 {
		fmt.Fprintf(&b, "odds: %.2f\n", req.Odds)
	}
	if req.ProfitIfWinUSD > 0 {
		fmt.Fprintf(&b, "profit_if_win_usd: %.0f\n", req.ProfitIfWinUSD)
	}
	if req.RemainingReturnPct > 0 {
		fmt.Fprintf(&b, "remaining_return_pct: %.1f\n", req.RemainingReturnPct)
	}
	out := strings.TrimRight(b.String(), "\n")
	if out == "" {
		return "n/a"
	}
	return out
}

// buildFlowDataBlock fills {{FLOW_DATA}}. Pulls every scoring +
// context-booster signal the detector stamped on the Finding.
func buildFlowDataBlock(req analysis.AlertAnalysisRequest) string {
	var b strings.Builder
	if req.Score > 0 {
		fmt.Fprintf(&b, "score: %.0f/100\n", req.Score)
	}
	if req.Confidence > 0 {
		fmt.Fprintf(&b, "confidence: %.2f\n", req.Confidence)
	}
	if req.MarketP95Ratio > 0 {
		fmt.Fprintf(&b, "market_p95_ratio: %.2f\n", req.MarketP95Ratio)
	}
	if req.TraderP95Ratio > 0 {
		fmt.Fprintf(&b, "trader_p95_ratio: %.2f\n", req.TraderP95Ratio)
	}
	if len(req.Reasons) > 0 {
		fmt.Fprintf(&b, "reasons: %s\n", strings.Join(req.Reasons, ", "))
	}
	for _, kv := range []struct{ label, value string }{
		{"accumulation", req.AccumulationNote},
		{"ownership", req.OwnershipNote},
		{"quiet_market", req.QuietMarketNote},
		{"new_wallet", req.NewWalletNote},
		{"outcome_status", req.OutcomeStatus},
	} {
		if kv.value != "" {
			fmt.Fprintf(&b, "%s: %s\n", kv.label, kv.value)
		}
	}
	if req.SameMarketRecentAlerts > 0 {
		fmt.Fprintf(&b, "same_market_recent_alerts_24h: %d\n", req.SameMarketRecentAlerts)
		fmt.Fprintf(&b, "same_market_same_side_notional_24h: %.0f\n", req.SameMarketSameSideNotionalUSD)
		fmt.Fprintf(&b, "same_market_opposite_side_notional_24h: %.0f\n", req.SameMarketOppositeSideNotionalUSD)
	}
	if req.SameWalletBidirectional {
		b.WriteString("same_wallet_bidirectional: yes — wallet both buys and sells same outcome inside the window\n")
	}
	if req.NoveltyOrMemeGuess {
		b.WriteString("market_appears_novelty_or_meme: yes (low-information / joke topic — downgrade usefulness)\n")
	}
	out := strings.TrimRight(b.String(), "\n")
	if out == "" {
		return "n/a"
	}
	return out
}

// buildAnnotationsBlock fills {{EVENT_ANNOTATIONS}} with the rendered
// Polymarket event-page narrative. The renderer prefixes its own
// "Polymarket event page context:" header which we strip here — the
// outer template already labels the slot.
func buildAnnotationsBlock(req analysis.AlertAnalysisRequest) string {
	epc := strings.TrimSpace(req.EventNarrativeContext)
	if epc == "" {
		return "unavailable. Do not invent annotations; reduce confidence."
	}
	const lead = "Polymarket event page context:"
	if strings.HasPrefix(epc, lead) {
		epc = strings.TrimSpace(epc[len(lead):])
	}
	return epc
}

// buildCatalystBlock fills {{CATALYST_CONTEXT}} with the rendered
// Political-Catalyst Intelligence overlay.
func buildCatalystBlock(req analysis.AlertAnalysisRequest) string {
	cc := strings.TrimSpace(req.CatalystContext)
	if cc == "" {
		return "no catalyst recorded for this event. Do not invent catalysts."
	}
	return cc
}

// buildWebContextBlock fills {{WEB_CONTEXT}}. The Responses API path
// instructs the model to invoke web_search; the Chat Completions
// path tells it not to invent news.
func buildWebContextBlock(req analysis.AlertAnalysisRequest) string {
	if req.PublicContextEnabled {
		return "public_context: web_search was attempted; use the web_search tool to retrieve the most recent (last 24-72h) news directly relevant to this market or event, then cite specific facts only when found by the tool."
	}
	return "public_context: NOT checked. Do not invent public facts; if asked, say \"Live public context was not checked.\""
}

// v11.2: buildMarketReportPrompt removed. The 2h market intelligence
// report (and its renderer / worker / prompt body) was retired as part
// of the v11 simplification. The Analyzer.AnalyzeMarketReport method
// is also gone; AlertAnalysis is the only kept Analyzer surface.

// buildOutcomePrompt — postmortem on a resolved alert. Unchanged in
// v9; the goal here is signal-quality auditing, not trend validation.
func buildOutcomePrompt(req analysis.OutcomeAnalysisRequest) string {
	var b strings.Builder
	b.WriteString("POSTMORTEM ON A RESOLVED PREDICTION-MARKET ALERT.\n\n")
	fmt.Fprintf(&b, "alert_kind: %s\n", nz(req.Kind))
	fmt.Fprintf(&b, "severity: %s\n", nz(req.Severity))
	fmt.Fprintf(&b, "title: %s\n", oneLine(req.Title))
	if req.Category != "" {
		fmt.Fprintf(&b, "category: %s\n", req.Category)
	}
	if req.OutcomeLabel != "" {
		fmt.Fprintf(&b, "alert_outcome: %s\n", req.OutcomeLabel)
	}
	if req.WinningOutcome != "" {
		fmt.Fprintf(&b, "winning_outcome: %s\n", req.WinningOutcome)
	}
	fmt.Fprintf(&b, "outcome_status: %s\n", nz(req.OutcomeStatus))
	if req.NotionalUSD > 0 {
		fmt.Fprintf(&b, "notional_usd: %.0f\n", req.NotionalUSD)
	}
	if req.Probability > 0 {
		fmt.Fprintf(&b, "probability_at_alert: %.3f\n", req.Probability)
	}
	if req.RemainingReturnPct > 0 {
		fmt.Fprintf(&b, "remaining_return_pct_at_alert: %.1f\n", req.RemainingReturnPct)
	}
	if req.Score > 0 {
		fmt.Fprintf(&b, "score_at_alert: %.0f/100\n", req.Score)
	}
	if req.Confidence > 0 {
		fmt.Fprintf(&b, "confidence_at_alert: %.2f\n", req.Confidence)
	}
	if len(req.Reasons) > 0 {
		fmt.Fprintf(&b, "reasons: %s\n", strings.Join(req.Reasons, ", "))
	}
	if req.CLV15m != 0 || req.CLV1h != 0 || req.CLV6h != 0 || req.CLV24h != 0 {
		fmt.Fprintf(&b, "clv: 15m=%.3f, 1h=%.3f, 6h=%.3f, 24h=%.3f\n",
			req.CLV15m, req.CLV1h, req.CLV6h, req.CLV24h)
	}
	b.WriteString("\nPolymarket event page context:\n")
	if epc := strings.TrimSpace(req.EventNarrativeContext); epc != "" {
		const lead = "Polymarket event page context:"
		if strings.HasPrefix(epc, lead) {
			epc = strings.TrimSpace(epc[len(lead):])
		}
		b.WriteString(epc)
		b.WriteString("\n")
	} else {
		b.WriteString("unavailable. Do not invent market news; reduce confidence.\n")
	}
	b.WriteString(outcomePrompt)
	return b.String()
}

func nz(s string) string {
	if s == "" {
		return "n/a"
	}
	return s
}

// oneLine strips embedded newlines so titles don't break the
// prompt's "one line per field" layout.
func oneLine(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\n", " "), "\r", " ")
}

// humanShort renders a duration as "Xd", "Xh", or "Xm" — enough
// resolution for a prompt header. Negative durations render as
// "elapsed".
func humanShort(d time.Duration) string {
	if d <= 0 {
		return "elapsed"
	}
	if d >= 24*time.Hour {
		return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
	}
	if d >= time.Hour {
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}
	return fmt.Sprintf("%dm", int(d/time.Minute))
}
