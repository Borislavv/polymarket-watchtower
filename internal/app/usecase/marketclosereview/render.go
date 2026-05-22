package marketclosereview

import (
	"fmt"
	"html"
	"strings"

	"github.com/Borislavv/polymarket-watchtower/internal/infra/ai/openai"
)

// RenderAdminTelegram composes the operator-facing admin body.
// One Telegram message per review; no raw JSON. Every AI string
// is HTML-escaped at the boundary.
//
// Layout:
//
//	<b>Market close review · &lt;market title&gt;</b>
//
//	Verdict: confirmed_signal
//	Confidence: 0.74
//
//	<b>Outcome</b>
//	• Winner: YES
//	• Closed: 2026-...
//
//	<b>Watchtower</b>
//	• alerts before close: 4
//	• best: critical accumulation, +8.2% 6h CLV (#42)
//	• weak/wrong: info trade_anomaly (#41)
//
//	<b>Flow</b>
//	• informed-flow likely: yes
//	• insider-like risk: medium
//	• repricing lag: hours
//
//	<b>Tuning</b>
//	• tighten ownership info threshold
//
//	<i>Admin only.</i>
func RenderAdminTelegram(market openai.MarketCloseReviewMarketSummary, alerts []openai.MarketCloseReviewAlertEvidence, resp openai.MarketCloseReviewResponse) string {
	var b strings.Builder
	title := strings.TrimSpace(market.Title)
	if title == "" {
		title = market.ConditionID
	}
	fmt.Fprintf(&b, "<b>Market close review · %s</b>\n",
		html.EscapeString(truncate(title, 120)))

	fmt.Fprintf(&b, "\nVerdict: <b>%s</b>\n", html.EscapeString(resp.Verdict))
	fmt.Fprintf(&b, "Confidence: %.2f\n", resp.Confidence)

	b.WriteString("\n<b>Outcome</b>\n")
	if market.WinningOutcome != "" {
		fmt.Fprintf(&b, "• winner: %s\n", html.EscapeString(market.WinningOutcome))
	}
	if !market.ClosedAt.IsZero() {
		fmt.Fprintf(&b, "• closed: %s\n", html.EscapeString(market.ClosedAt.UTC().Format("2006-01-02 15:04 UTC")))
	}
	if summary := strings.TrimSpace(resp.MarketOutcomeSummary); summary != "" {
		fmt.Fprintf(&b, "• summary: %s\n", html.EscapeString(truncate(summary, 240)))
	}

	b.WriteString("\n<b>Watchtower</b>\n")
	fmt.Fprintf(&b, "• alerts before close: %d\n", len(alerts))
	fmt.Fprintf(&b, "• alert quality: %s\n", html.EscapeString(resp.WatchtowerPerformance.AlertQuality))
	if len(resp.WatchtowerPerformance.BestAlertIDs) > 0 {
		fmt.Fprintf(&b, "• best: %s\n", html.EscapeString(formatAlertIDs(resp.WatchtowerPerformance.BestAlertIDs, alerts)))
	}
	if len(resp.WatchtowerPerformance.WorstAlertIDs) > 0 {
		fmt.Fprintf(&b, "• weak/wrong: %s\n", html.EscapeString(formatAlertIDs(resp.WatchtowerPerformance.WorstAlertIDs, alerts)))
	}

	b.WriteString("\n<b>Flow</b>\n")
	fmt.Fprintf(&b, "• informed-flow likely: %s\n", yesNo(resp.FlowAssessment.InformedFlowLikely))
	fmt.Fprintf(&b, "• insider-like risk: %s\n", html.EscapeString(resp.FlowAssessment.InsiderLikeRisk))
	fmt.Fprintf(&b, "• signal type: %s\n", html.EscapeString(resp.FlowAssessment.SpeculationVsInformation))
	fmt.Fprintf(&b, "• repricing lag: %s\n", html.EscapeString(resp.MarketRepricingAssessment.RepricingLag))
	if r := strings.TrimSpace(resp.FlowAssessment.Rationale); r != "" {
		fmt.Fprintf(&b, "• rationale: %s\n", html.EscapeString(truncate(r, 240)))
	}

	if len(resp.TuningRecommendations) > 0 {
		b.WriteString("\n<b>Tuning</b>\n")
		for _, t := range resp.TuningRecommendations {
			fmt.Fprintf(&b, "• [%s/%s] %s\n",
				html.EscapeString(t.Area),
				html.EscapeString(t.Priority),
				html.EscapeString(truncate(t.Recommendation, 200)),
			)
		}
	}

	if summary := strings.TrimSpace(resp.AdminSummary); summary != "" {
		b.WriteString("\n<i>")
		b.WriteString(html.EscapeString(truncate(summary, 900)))
		b.WriteString("</i>\n")
	}
	b.WriteString("\n<i>Admin only · Market Close Review</i>")
	return b.String()
}

// formatAlertIDs joins a list of alert ids into a compact label
// the operator can grep in DB. Falls back to "#id" when the
// matching alert isn't in the provided evidence slice (defensive —
// the parser should have filtered this already).
func formatAlertIDs(ids []int64, alerts []openai.MarketCloseReviewAlertEvidence) string {
	byID := make(map[int64]openai.MarketCloseReviewAlertEvidence, len(alerts))
	for _, a := range alerts {
		byID[a.ID] = a
	}
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		a, ok := byID[id]
		if !ok {
			parts = append(parts, fmt.Sprintf("#%d", id))
			continue
		}
		seg := fmt.Sprintf("%s %s #%d", a.Severity, a.Kind, id)
		if a.CLV6h != nil {
			seg += fmt.Sprintf(" (%+.1f%% 6h CLV)", *a.CLV6h*100)
		}
		parts = append(parts, seg)
	}
	return strings.Join(parts, "; ")
}

func yesNo(v bool) string {
	if v {
		return "yes"
	}
	return "no"
}

func truncate(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
