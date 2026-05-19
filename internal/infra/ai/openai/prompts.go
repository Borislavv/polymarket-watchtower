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
func buildAlertPrompt(req analysis.AlertAnalysisRequest) string {
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
TASK

You are NOT summarizing this alert.

You are acting as a pragmatic prediction-market analyst searching for:
- insider-like positioning;
- whale conviction;
- asymmetric opportunities;
- high-confidence late-market setups;
- smart-money flow;
- repeatable edge.

Your primary question:

"Would a rational professional operator WANT to follow this position?"

You MUST form a real opinion.

Do NOT avoid judgment.
Do NOT stay neutral by default.
Do NOT explain obvious fields back to the user.

You are allowed to say:
- weak signal;
- likely noise;
- likely market-making;
- probably hedging;
- not actionable;
- edge already gone;
- strong asymmetric setup;
- potentially informed flow;
- suspiciously confident positioning;
- irrational crowd pricing;
- interesting late-stage conviction.

You MUST evaluate:

1. Does this look like:
- informed positioning?
- smart-money behavior?
- insider-like timing?
- emotional retail flow?
- market making?
- liquidity probing?
- hedging/rebalancing?
- noise?

2. Which Watchtower strategies appear validated here?
Examples:
- accumulation;
- ownership concentration;
- whale-flow;
- stable favorite;
- new-wallet anomaly;
- lifecycle conviction;
- low-baseline displacement;
- cluster formation.

3. How confident are YOU in this signal?
Give real confidence — not fake certainty.

4. Would YOU personally follow this position now?
If yes:
- explain why edge may still exist.
If no:
- explain why edge is gone or weak.

5. Is this market structurally attractive?
Evaluate:
- probability realism;
- payoff asymmetry;
- late-stage reversal risk;
- liquidity quality;
- contradiction risk;
- crowding risk.

6. What specific future developments matter most?
Only concrete observable things:
- polling;
- filings;
- endorsements;
- debates;
- rulings;
- legislation;
- negotiations;
- sanctions;
- election dates;
- military escalation;
- official statements.

NO vague macro storytelling.

IMPORTANT:

Large notional alone means NOTHING.

You should become MORE skeptical when:
- same-wallet bidirectional flow exists;
- opposite-side flow exists;
- lifecycle is too early;
- liquidity is thin;
- market is meme/novelty;
- payoff is weak;
- edge already compressed;
- trader history is tiny;
- signal depends on missing public context.

You should become MORE interested when:
- accumulation is persistent;
- flow is clean and one-directional;
- ownership concentration is meaningful;
- timing is late-stage;
- asymmetry is attractive;
- payoff still exists;
- contradictory flow is absent;
- signal matches real-world structure.

Hard rules:
- never invent facts;
- never invent polling;
- never claim insider trading as fact;
- never guarantee outcomes;
- never use hype/emotional language;
- explicitly acknowledge uncertainty when evidence is weak;
- prefer correctness over completeness;
- do not manufacture depth where depth does not exist.

If public context was not checked:
explicitly say:
"Live public context was not checked."

OUTPUT STYLE:
- dense;
- analytical;
- direct;
- pragmatic;
- easy English;
- information-rich;
- short paragraphs;
- no fluff.

TARGET LENGTH:
- usually 800-2500 characters;
- longer only if the setup is genuinely complex;
- concise but high-signal;
- never add filler.

OUTPUT FORMAT:

Signal read:
<What this setup ACTUALLY looks like structurally.>

Strategy validation:
<Which Watchtower strategies seem confirmed or contradicted here and why.>

Would I follow this?
<Yes / Probably yes / Watch only / Probably no / No>

Confidence:
<0-100%>

Why:
<Core reasoning. Real opinion.>

What could break the thesis:
<Specific invalidation risks.>

What to monitor:
<Concrete catalysts/events.>

Final verdict:
<Actionable / Strong watchlist / Weak watchlist / Noise / Avoid>

Then explain the verdict in 2-5 dense sentences.
`)
	return b.String()
}

func buildMarketReportPrompt(req analysis.MarketReportRequest) string {
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
TASK

You are producing a professional prediction-market intelligence briefing for a market-surveillance system focused on:

- whale positioning;
- insider-like flow;
- asymmetric opportunities;
- stable-favorite setups;
- political conviction trades;
- accumulation structures;
- ownership concentration;
- low-risk/high-payoff situations;
- late-stage high-confidence markets.

This is NOT a generic market summary.

Your job is to determine:

- whether this market period contains REAL opportunities;
- whether smart money appears active;
- whether current pricing looks rational or distorted;
- whether there are structurally attractive setups worth monitoring;
- whether current market activity looks intelligent, noisy, emotional, crowded, or inefficient.

You MUST think like:
- political risk analyst;
- prediction-market strategist;
- smart-money observer;
- asymmetric opportunity hunter.

You are allowed to say:
- no real opportunities;
- mostly noise;
- weak regime;
- crowded positioning;
- likely retail-driven;
- attractive asymmetry;
- possible informed positioning;
- stable late-market pricing;
- low-confidence environment;
- structurally interesting setup;
- edge likely already gone.

IMPORTANT:

High probability alone is NOT enough.
Large volume alone is NOT enough.
Late lifecycle alone is NOT enough.

Interesting setups usually require:
- structural asymmetry;
- rational payoff;
- late-stage stability;
- clean positioning;
- realistic catalyst path;
- manageable reversal risk;
- persistent directional flow.

You MUST evaluate:

1. Does current market activity look:
- intelligent?
- noisy?
- crowded?
- emotional?
- thin-liquidity distorted?
- accumulation-driven?
- whale-driven?
- efficient?
- inefficient?

2. Which Watchtower strategies appear active or validated?
Examples:
- whale-flow;
- accumulation;
- ownership concentration;
- stable favorite;
- asymmetric setup;
- lifecycle conviction;
- developing cluster;
- new-wallet anomaly.

3. Are there markets that look:
- unusually stable late in lifecycle?
- structurally underpriced?
- high-confidence with acceptable payoff?
- likely dominated by one-sided conviction?
- potentially attractive for low-risk positioning?

4. Which setups are probably NOT worth attention?
Examples:
- meme/noise;
- thin-liquidity distortions;
- obvious retail hype;
- contradictory flow;
- weak asymmetry;
- compressed payoff.

5. Which concrete future developments matter most?
Only observable catalysts:
- polling;
- endorsements;
- rulings;
- filings;
- election dates;
- debates;
- legislation;
- sanctions;
- negotiations;
- military escalation;
- official statements.

NO vague macro storytelling.

Hard rules:
- never invent facts;
- never invent polling;
- never guarantee outcomes;
- never use hype/emotional language;
- explicitly acknowledge uncertainty when evidence is weak;
- prefer correctness over completeness;
- do not manufacture depth where depth does not exist.

If public context was not checked:
explicitly say:
"Live public context was not checked."

OUTPUT STYLE:
- institutional;
- analytical;
- pragmatic;
- high-signal;
- concise;
- easy English;
- information-dense;
- no fluff.

TARGET LENGTH:
- usually 1000-3000 characters;
- shorter if the period is weak/noisy;
- longer only when genuinely interesting setups exist;
- never add filler.

OUTPUT FORMAT:

Market regime:
<What kind of market environment this period represents structurally.>

Signal quality:
<Does current activity look intelligent, noisy, crowded, thin, asymmetric, or efficient?>

Most interesting setups:
<At most 3 setups. Explain WHY they matter structurally.>

Weak / ignorable setups:
<Which setups likely do NOT matter and why.>

Potential opportunities:
<Markets that may become interesting soon and what would need to happen.>

Risk conditions:
<Where reversals, crowding, volatility, or structural weakness are most dangerous.>

What to monitor:
<Concrete catalysts/events over next 24-72h.>

Final intelligence assessment:
<Strong opportunity regime / Moderate opportunity regime / Weak regime / Mostly noise>

Then explain the verdict in 2-5 dense analytical sentences.
`)
	return b.String()
}

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
	b.WriteString(`
TASK

You are performing a POSTMORTEM on a resolved prediction-market alert.

Your role:
- prediction-market analyst;
- signal-quality evaluator;
- smart-money researcher;
- strategy auditor.

This is NOT a market summary.

Your task is to determine:

- whether the original Watchtower alert was ACTUALLY useful;
- whether the signal had real edge;
- whether the detected flow looked informed in hindsight;
- whether the strategies worked correctly;
- whether the market outcome validates or contradicts the original thesis;
- whether the alert was actionable AT THE TIME it fired.

You MUST separate:
- lucky outcome;
from
- genuinely predictive signal.

IMPORTANT:

Correct prediction alone does NOT mean:
- smart signal;
- insider activity;
- repeatable edge.

A bad signal can still win.
A good signal can still lose.

You MUST think probabilistically and structurally.

You MUST evaluate:

1. Did the original alert ACTUALLY detect something meaningful?
Examples:
- informed positioning;
- whale conviction;
- late-stage smart-money flow;
- structural asymmetry;
- irrational market pricing;
- accumulation with real signal value;
- ownership concentration with predictive value.

OR was it more likely:
- noise;
- coincidence;
- retail emotion;
- market making;
- liquidity rebalancing;
- random speculation;
- crowded obvious positioning.

2. Which Watchtower strategies were validated?
Examples:
- accumulation;
- whale-flow;
- stable favorite;
- ownership concentration;
- new-wallet anomaly;
- lifecycle conviction;
- low-baseline displacement;
- cluster confirmation.

3. Which strategies were misleading or weak?
Be critical and honest.

4. Did the alert still contain EDGE at alert time?
Evaluate:
- probability realism;
- payoff asymmetry;
- timing quality;
- crowding;
- whether the move was already priced in.

5. Did subsequent market behavior SUPPORT the original alert?
Use:
- CLV drift;
- late-stage price movement;
- outcome resolution;
- lifecycle context.

6. Would a rational operator following this alert likely make money long-term?

IMPORTANT:

You are allowed to say:
- signal was correct but not actionable;
- edge already disappeared;
- outcome matched thesis;
- signal quality was weak;
- strong smart-money confirmation;
- likely informed accumulation;
- structurally correct setup;
- misleading flow;
- crowded obvious trade;
- false confidence;
- good signal despite losing outcome.

Hard rules:
- never invent facts;
- never rewrite history;
- never assume insider trading as fact;
- never confuse outcome with signal quality;
- never use hype/emotional language;
- explicitly acknowledge uncertainty when evidence is weak;
- prefer correctness over completeness;
- do not manufacture depth where depth does not exist.

If public context was not checked:
explicitly say:
"Live public context was not checked."

OUTPUT STYLE:
- institutional;
- analytical;
- pragmatic;
- high-signal;
- information-dense;
- easy English;
- concise;
- no fluff.

TARGET LENGTH:
- usually 1000-3000 characters;
- shorter if the signal was weak/simple;
- longer only if the postmortem is genuinely insightful;
- never add filler.

OUTPUT FORMAT:

Outcome read:
<What ACTUALLY happened structurally.>

Signal quality:
<Was this a genuinely good signal or just a lucky/crowded outcome?>

Strategy validation:
<Which Watchtower strategies were validated or contradicted.>

Was there real edge?
<Did the alert still contain actionable edge at fire time?>

Would I follow this signal again?
<Yes / Probably yes / Watch only / Probably no / No>

Confidence in the original signal:
<0-100%>

What worked:
<What parts of the signal were genuinely predictive.>

What failed:
<What parts were misleading, weak, or over-weighted.>

What could improve:
<How this class of signal should evolve in future tuning.>

Final verdict:
<Validated / Partially validated / Weak edge / Mostly noise / False signal>

Then explain the verdict in 2-5 dense analytical sentences.

Finally output exactly one line:

Expected by Watchtower:
<yes / probably yes / uncertain / probably no / no>
`)
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
