package openai

import (
	"fmt"
	"strings"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/analysis"
)

// buildAlertPrompt constructs the user message for a single alert.
//
// v9 reformulation: the model's primary task is no longer to summarise
// the alert. It is to validate or invalidate the trend implied by the
// alert against fresh world/political events. When the Responses API
// path is used (Client.WebSearchEnabled), the model also has the
// web_search_preview tool and can fetch real-time news; the prompt
// asks it to do so. When the Chat Completions path is used, the
// prompt tells the model NOT to invent news and to reduce confidence.
//
// Section layout matches the request template the spec defines:
//
//	Данные алерта:        {{ALERT_DATA}}
//	Current market/price: {{MARKET_PRICE_DATA}}
//	Recent flow/anomalies:{{FLOW_DATA}}
//	Fresh news / web context: {{EVENTS_NEWS}}
//	Previous context:     {{PREVIOUS_CONTEXT}}
//
// All data fields stay structured `key: value` lines so the model can
// parse them reliably. Empty fields are elided rather than rendered
// as zeros.
func buildAlertPrompt(req analysis.AlertAnalysisRequest) string {
	var b strings.Builder
	b.WriteString("ANALYZE THIS PREDICTION-MARKET ALERT.\n\n")

	// --- Данные алерта (alert identity) ---------------------------------
	b.WriteString("Данные алерта:\n")
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

	// --- Current market/price -------------------------------------------
	b.WriteString("\nCurrent market/price:\n")
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

	// --- Recent flow / anomalies ----------------------------------------
	b.WriteString("\nRecent flow/anomalies:\n")
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

	// --- Fresh news / web context ---------------------------------------
	// The Responses API path actually populates this section via the
	// web_search_preview tool. The Chat Completions path leaves it
	// empty and the model is instructed not to invent news.
	b.WriteString("\nFresh news / web context:\n")
	if req.PublicContextEnabled {
		b.WriteString("public_context: web_search was attempted; use the web_search tool to retrieve the most recent (last 24-72h) news directly relevant to this market or event, then cite specific facts only when found by the tool.\n")
	} else {
		b.WriteString("public_context: NOT checked. Do not invent public facts; if asked, say \"Live public context was not checked.\"\n")
	}

	// --- Previous context -----------------------------------------------
	// Reserved for prior AI analysis text once we plumb it through the
	// request shape. Empty for now — the slot is still emitted so the
	// model sees a stable layout across calls.
	b.WriteString("\nPrevious context:\n")
	b.WriteString("n/a — first or refreshed analysis; prior analyst notes not threaded.\n")

	// --- TASK (v9 Russian, trend-confirmation/invalidation focus) -------
	b.WriteString(promptForAlert)
	return b.String()
}

// buildMarketReportPrompt — 2h market-news-review.
//
// v9 reformulation: same core question as the alert prompt — "do fresh
// events confirm or invalidate a tradable trend before the market
// fully reprices?" — applied at the period level over the top-N
// candidate markets. The model is asked to scan for repricing,
// underreaction, and stale flow, not to produce a generic market
// summary.
func buildMarketReportPrompt(req analysis.MarketReportRequest) string {
	var b strings.Builder
	b.WriteString("PRODUCE A 2H PREDICTION-MARKET INTELLIGENCE REVIEW.\n\n")
	fmt.Fprintf(&b, "period: %s — %s\n",
		req.PeriodStart.Format(time.RFC3339), req.PeriodEnd.Format(time.RFC3339))
	fmt.Fprintf(&b, "whale_flow_candidates: %d\n", req.WhaleFlowCandidates)
	fmt.Fprintf(&b, "stable_favorites: %d\n", req.StableFavorites)
	fmt.Fprintf(&b, "asymmetric_setups: %d\n", req.AsymmetricSetups)
	fmt.Fprintf(&b, "developing_signals: %d\n", req.DevelopingSignals)
	if req.UpcomingEventsNote != "" {
		fmt.Fprintf(&b, "upcoming_events_note: %s\n", req.UpcomingEventsNote)
	}
	b.WriteString("\nMarkets (top candidates):\n")
	for i, m := range req.Markets {
		fmt.Fprintf(&b, "%d. %s | category=%s | lifecycle=%.1f%% | prob=%.3f | rem_return_pct=%.1f | vol24h=$%.0f | trades24h=%d | alerts24h=%d | %s\n",
			i+1, oneLine(m.Title), m.Category, m.LifecyclePct, m.Probability,
			m.RemainingReturnPct, m.Volume24hUSD, m.RecentTrades24h, m.AlertsLast24h, m.Notes)
	}
	b.WriteString("\nFresh news / web context:\n")
	b.WriteString("If you have web_search available, use it to retrieve the most recent (last 24-72h) news relevant to the candidate markets. Cite specific facts only when found by the tool. If the tool is unavailable, write \"Live public context was not checked.\" and do not invent news.\n")
	b.WriteString(marketReportPrompt)
	return b.String()
}

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
