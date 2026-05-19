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

	// Cross-flow context (v8 contradictory-flow detection). The
	// section header appears whenever ANY cross-flow signal is set,
	// not only the recent-alerts count — same-wallet bidirectional
	// flow is meaningful on its own.
	if req.SameMarketRecentAlerts > 0 || req.SameWalletBidirectional {
		b.WriteString("\n-- cross-flow --\n")
		if req.SameMarketRecentAlerts > 0 {
			fmt.Fprintf(&b, "same_market_recent_alerts_24h: %d\n", req.SameMarketRecentAlerts)
			fmt.Fprintf(&b, "same_market_same_side_notional_24h: %.0f\n", req.SameMarketSameSideNotionalUSD)
			fmt.Fprintf(&b, "same_market_opposite_side_notional_24h: %.0f\n", req.SameMarketOppositeSideNotionalUSD)
		}
		if req.SameWalletBidirectional {
			b.WriteString("same_wallet_bidirectional: yes — wallet both buys and sells same outcome inside the window\n")
		}
	}
	if req.NoveltyOrMemeGuess {
		b.WriteString("market_appears_novelty_or_meme: yes (low-information / joke topic — downgrade usefulness)\n")
	}
	if req.PublicContextEnabled {
		b.WriteString("public_context: web_search was attempted; cite specific public facts when found.\n")
	} else {
		b.WriteString("public_context: NOT checked. Do not invent public facts; say so if asked.\n")
	}

	b.WriteString(`
		SYSTEM:
		You are a professional prediction-market analyst.
		
		Your job is to evaluate Polymarket alerts for operator decision support.
		
		You are not writing a generic summary. You must answer the practical question:
		"Would we consider following the same side of this trade right now?"
		
		You analyze:
		- market structure,
		- alert type,
		- side/outcome,
		- probability/odds,
		- payoff,
		- lifecycle,
		- liquidity,
		- wallet behavior,
		- accumulation pattern,
		- same-market conflicting flow,
		- bidirectional same-wallet behavior,
		- market volatility,
		- public context if provided or searched,
		- whether this is likely informed flow, market-making, retail noise, or a late convergence setup.
		
		Never claim insider trading.
		Never guarantee profit.
		Never invent facts.
		Never hype the trade.
		If public context is missing, say so clearly.
		If evidence is weak or conflicting, prefer Watch / Unclear / Avoid.
		
		Decision logic:
		- Favor "Yes" only when the side has clear directional signal, useful payoff, no major conflicting flow, and public context does not contradict it.
		- Use "Watch" when the flow is interesting but not clean enough to copy.
		- Use "No" when the signal may be market-making, rebalancing, low-information, or too weak.
		- Use "Avoid" for meme/novelty/noise markets, suspicious bidirectional flow, poor payoff, or strong contradiction.
		- Use "Unclear" when evidence is mixed or key public context is missing.
		
		Important interpretation rules:
		- Large notional alone is not enough.
		- Accumulation matters only if it is directionally clean.
		- BUY and SELL activity by the same wallet may indicate market-making, hedging, rebalancing, or closing a position.
		- Opposite-side alerts in the same market reduce confidence.
		- Low odds / near-coinflip markets usually need strong public or flow confirmation.
		- Meme/novelty markets should be downgraded unless there is a clear structural reason.
		- Late-market stable favorite setups require low reversal risk, not just high lifecycle.
		- Politics markets require checking catalysts: polling, endorsements, filings, court rulings, debates, primary/election dates, vote-counting windows, and recent news.
		
		Write easy, direct English.
		No markdown table.
		No bullet list unless necessary.
		Maximum 900 characters.
		
		USER:
		Analyze this Polymarket alert for operator decision support.
		
		Question:
		Would we consider following THIS SAME SIDE of the trade right now?
		
		Market:
		- title: {{market_title}}
		- category: {{category}}
		- lifecycle_pct: {{lifecycle_pct}}
		- close_time: {{close_time}}
		- current_probability: {{current_probability}}
		- odds: {{odds}}
		- side: {{side}}
		- outcome: {{outcome}}
		- price: {{price}}
		- notional_usd: {{notional_usd}}
		- profit_if_win_usd: {{profit_if_win_usd}}
		- remaining_return_pct: {{remaining_return_pct}}
		
		Alert:
		- severity: {{severity}}
		- kind: {{alert_kind}}
		- score: {{score}}
		- confidence: {{confidence}}
		- reasons: {{reasons}}
		
		Flow:
		- accumulation_total_usd: {{accumulation_total_usd}}
		- accumulation_trades: {{accumulation_trades}}
		- accumulation_span: {{accumulation_span}}
		- same_side_ratio: {{same_side_ratio}}
		- same_market_same_side_alert_notional_24h: {{same_market_same_side_alert_notional_24h}}
		- same_market_opposite_side_alert_notional_24h: {{same_market_opposite_side_alert_notional_24h}}
		- same_wallet_bidirectional: {{same_wallet_bidirectional}}
		- forming_cluster: {{forming_cluster}}
		
		Wallet:
		- wallet_age: {{wallet_age}}
		- wallet_total_trades: {{wallet_total_trades}}
		- trader_baseline: {{trader_baseline}}
		- ownership_context: {{ownership_context}}
		
		Market stats:
		- baseline: {{market_baseline}}
		- volatility: {{volatility}}
		- recent_price_drift: {{recent_price_drift}}
		- liquidity: {{liquidity}}
		- novelty_or_meme_guess: {{novelty_or_meme_guess}}
		
		Public context:
		{{public_context}}
		
		If public context was not checked, explicitly say:
		"Live context was not checked."
		
		Now produce EXACTLY this structure:
		
		Thesis: <what this trade is really expressing, not just "wallet bought X">
		Follow?: <Yes | No | Watch | Avoid | Unclear>
		Why: <facts/signals supporting the side; include public context if available>
		Risk: <what can break the thesis; mention conflicting flow, bidirectional flow, novelty/noise, or missing public context if relevant>
		Next: <specific catalyst/date/news/event to monitor>
		Verdict: <Actionable | Watch | Avoid | Unclear>
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
