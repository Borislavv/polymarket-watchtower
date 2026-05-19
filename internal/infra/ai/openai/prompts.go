package openai

import (
	"fmt"
	"strings"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/analysis"
)

// buildAlertPrompt constructs the user message for a single alert.
// The output is intentionally compact (~600-1500 chars typical) so
// the model gets predictable inputs without noisy boilerplate.
// Empty fields are elided rather than rendered as zeros.
func buildAlertPrompt(req analysis.AlertAnalysisRequest, maxChars int) string {
	var b strings.Builder
	b.WriteString("ANALYZE THIS PREDICTION-MARKET ALERT.\n\n")

	// Identity block.
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

	// Trade / position numbers.
	if req.NotionalUSD > 0 || req.Price > 0 || req.ProfitIfWinUSD > 0 {
		b.WriteString("\n-- trade --\n")
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
	}
	if req.Score > 0 || req.Confidence > 0 {
		b.WriteString("\n-- score --\n")
		if req.Score > 0 {
			fmt.Fprintf(&b, "score: %.0f/100\n", req.Score)
		}
		if req.Confidence > 0 {
			fmt.Fprintf(&b, "confidence: %.2f\n", req.Confidence)
		}
	}

	// Tail / reasons.
	if req.MarketP95Ratio > 0 {
		fmt.Fprintf(&b, "market_p95_ratio: %.2f\n", req.MarketP95Ratio)
	}
	if req.TraderP95Ratio > 0 {
		fmt.Fprintf(&b, "trader_p95_ratio: %.2f\n", req.TraderP95Ratio)
	}
	if len(req.Reasons) > 0 {
		fmt.Fprintf(&b, "reasons: %s\n", strings.Join(req.Reasons, ", "))
	}

	// Context blurbs.
	for label, v := range map[string]string{
		"accumulation":   req.AccumulationNote,
		"ownership":      req.OwnershipNote,
		"quiet_market":   req.QuietMarketNote,
		"new_wallet":     req.NewWalletNote,
		"outcome_status": req.OutcomeStatus,
	} {
		if v != "" {
			fmt.Fprintf(&b, "%s: %s\n", label, v)
		}
	}

	b.WriteString(`
TASK:
- Explain why this alert may matter and how it may go wrong.
- Note what date / news / event matters next.
- End with one operator verdict word: actionable, watchlist, or avoid.
- Live external context was not checked. Say so when you cite news.
- 300-700 characters. No hype. No claim of insider trading. No guarantee.
`)
	return truncate(b.String(), maxChars)
}

func buildMarketReportPrompt(req analysis.MarketReportRequest, maxChars int) string {
	var b strings.Builder
	b.WriteString("PRODUCE A 2H PREDICTION-MARKET INTELLIGENCE SUMMARY.\n\n")
	fmt.Fprintf(&b, "period: %s — %s\n",
		req.PeriodStart.Format(time.RFC3339), req.PeriodEnd.Format(time.RFC3339))
	fmt.Fprintf(&b, "whale_flow_candidates: %d\n", req.WhaleFlowCandidates)
	fmt.Fprintf(&b, "stable_favorites: %d\n", req.StableFavorites)
	fmt.Fprintf(&b, "asymmetric_setups: %d\n", req.AsymmetricSetups)
	fmt.Fprintf(&b, "developing_signals: %d\n", req.DevelopingSignals)
	if req.UpcomingEventsNote != "" {
		fmt.Fprintf(&b, "upcoming_events_note: %s\n", req.UpcomingEventsNote)
	}
	b.WriteString("\n-- markets (top candidates) --\n")
	for i, m := range req.Markets {
		fmt.Fprintf(&b, "%d. %s | category=%s | lifecycle=%.1f%% | prob=%.3f | rem_return_pct=%.1f | vol24h=$%.0f | trades24h=%d | alerts24h=%d | %s\n",
			i+1, oneLine(m.Title), m.Category, m.LifecyclePct, m.Probability,
			m.RemainingReturnPct, m.Volume24hUSD, m.RecentTrades24h, m.AlertsLast24h, m.Notes)
	}
	b.WriteString(`
TASK:
Write an analyst summary in this exact shape:

Overview
- market activity:
- whale-flow candidates:
- stable favorites:
- asymmetric setups:
- volatility risks:

Markets to watch
1. <title> — short reason
2. ...

What matters next
- debates / polls / deadlines / volatility windows

Analyst summary
- 3-5 sentences. Concrete. Cautious. No hype. Say "external context not checked" if you cite news.
`)
	return truncate(b.String(), maxChars)
}

func buildOutcomePrompt(req analysis.OutcomeAnalysisRequest, maxChars int) string {
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
	b.WriteString(`
TASK:
Answer concisely:
1. Why did the market likely resolve this way?
2. Were Watchtower signals correct, partly correct, or wrong?
3. Was the outcome expected, given the alert?
4. What signals were misleading or absent?
5. What future lesson should be learned?

Then add a line:
LESSONS:
- <bullet 1>
- <bullet 2>

Finally a line:
Expected by Watchtower: yes | no | uncertain

Style: simple English. No hype. No claim of insider trading.
`)
	return truncate(b.String(), maxChars)
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
