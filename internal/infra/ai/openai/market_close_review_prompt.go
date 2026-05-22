// market_close_review_prompt.go — verbatim v11.4 Market Close
// Review strict-JSON prompt + the structured data-block builder.
// Pinned by market_close_review_test.go.
//
// Hard rules captured in the prompt:
//   - Be skeptical. Profitable ≠ insider-like.
//   - Insider-like claim requires timing + size + direction +
//     market context + repricing + outcome consistency.
//   - If evidence is insufficient → verdict=inconclusive or
//     no_edge.
//   - Do not invent external facts not provided.
//   - If web/search is unavailable, do not claim fresh news.
//   - admin_summary capped at 900 chars (also re-clamped in
//     the parser).
//   - reaction_plan may only reference alert_ids that were
//     included in the prompt data block.
package openai

import (
	"fmt"
	"strings"
	"time"
)

// MarketCloseReviewPrompt is the strict-JSON prompt text. The
// builder appends a "---\n" delimiter + this constant to the
// structured data block so the prompt+data join is deterministic.
const MarketCloseReviewPrompt = `You are a senior prediction-market post-resolution analyst.

You are reviewing a market that has CLOSED / RESOLVED. You see structured evidence:
- market header (with closed_at, resolved_at, winning outcome);
- alerts Watchtower emitted before close (with IDs);
- pre-close flow aggregates;
- compact news/event timeline.

Your job is to judge:
- did Watchtower's alerts catch real informed flow, or were they noise;
- did the market's price move underreact / overreact / reprice in time;
- which strategies looked helpful, noisy, or insufficient;
- what to tune;
- which existing Telegram alert messages deserve a success/failure/ambiguous reaction.

Skepticism rules (LOAD-BEARING):
- Profitable does NOT prove insider-like trading.
- "Insider-like" requires timing + size + direction + market context + repricing + outcome consistency.
- If alerts disagreed, or evidence is thin, prefer "mixed" or "inconclusive".
- If you cannot tell from the evidence, return verdict="inconclusive" or verdict="no_edge".
- Do NOT invent external facts that are not in the data block.
- You do NOT have web search. If you need fresh news to judge, say so in admin_summary; do not pretend you have it.
- "missed_signal" = market clearly resolved one way + obvious flow signal was visible + Watchtower did NOT emit a useful alert.
- "false_positive" = Watchtower emitted alerts whose direction was wrong AND there was no other excuse.
- "confirmed_signal" = the alert direction matched the resolution AND the flow read holds up.

admin_summary rules:
- Operator-facing English (max 900 chars after trim).
- Concrete, not generic. No "ждать", no "monitor polls", no "already priced".
- If verdict is inconclusive / no_edge, say WHY directly.

reaction_plan rules:
- One entry per alert you have an opinion on; you can skip alerts.
- alert_id MUST be one of the IDs the data block carried — do not invent.
- reaction ∈ {success, failure, ambiguous, none}.
- "none" means leave the existing message untouched.
- Reason field is short (≤ 140 chars).

Strict JSON schema (one object, no markdown, no prose outside):

{
  "verdict": "confirmed_signal|missed_signal|false_positive|inconclusive|no_edge",
  "confidence": 0.0,
  "market_outcome_summary": "<one short sentence>",
  "watchtower_performance": {
    "early": true,
    "directionally_correct": true,
    "alert_quality": "strong|mixed|weak|insufficient",
    "best_alert_ids": [],
    "worst_alert_ids": []
  },
  "flow_assessment": {
    "informed_flow_likely": true,
    "insider_like_risk": "high|medium|low|unknown",
    "speculation_vs_information": "informed|speculative|noise|mixed|unknown",
    "rationale": "<short>"
  },
  "market_repricing_assessment": {
    "underreaction_detected": true,
    "overreaction_detected": false,
    "repricing_lag": "none|minutes|hours|days|unknown",
    "rationale": "<short>"
  },
  "strategy_assessment": [
    {
      "strategy": "single_trade_whale|accumulation|cluster_convergence|ownership_concentration|news_intel|context_booster|unknown",
      "verdict": "helpful|noisy|mixed|insufficient",
      "reason": "<short>"
    }
  ],
  "tuning_recommendations": [
    {
      "area": "thresholds|wallet|news|ownership|accumulation|mm_filter|cluster|other",
      "recommendation": "<short>",
      "priority": "low|medium|high"
    }
  ],
  "admin_summary": "<= 900 chars, operator-facing>",
  "reaction_plan": [
    {
      "alert_id": 123,
      "telegram_message_id": 456,
      "reaction": "success|failure|ambiguous|none",
      "reason": "<short>"
    }
  ]
}
`

// BuildMarketCloseReviewUserMessage composes the structured data
// block + the verbatim prompt. The block is small, deterministic,
// and ID-bearing so the AI can echo alert_ids back. Truncation
// caps mirror the worker's MaxAlertsInPrompt / MaxEventsInPrompt
// so a single huge market can't blow the context window.
func BuildMarketCloseReviewUserMessage(req MarketCloseReviewRequest) string {
	var b strings.Builder

	// --- Market header ---
	b.WriteString("Market:\n")
	writeKV(&b, "condition_id", req.Market.ConditionID)
	writeKV(&b, "event_slug", req.Market.EventSlug)
	writeKV(&b, "title", req.Market.Title)
	writeKV(&b, "category", req.Market.Category)
	if !req.Market.OpenedAt.IsZero() {
		writeKV(&b, "opened_at", req.Market.OpenedAt.UTC().Format(time.RFC3339))
	}
	if !req.Market.ClosedAt.IsZero() {
		writeKV(&b, "closed_at", req.Market.ClosedAt.UTC().Format(time.RFC3339))
	}
	if !req.Market.ResolvedAt.IsZero() {
		writeKV(&b, "resolved_at", req.Market.ResolvedAt.UTC().Format(time.RFC3339))
	}
	writeKV(&b, "winning_outcome", req.Market.WinningOutcome)
	if req.Market.FinalPrice != nil {
		fmt.Fprintf(&b, "final_price: %.3f\n", *req.Market.FinalPrice)
	}

	// --- Alerts ---
	maxAlerts := req.MaxAlertsInPrompt
	if maxAlerts <= 0 {
		maxAlerts = 50
	}
	alerts := req.Alerts
	if len(alerts) > maxAlerts {
		alerts = alerts[:maxAlerts]
	}
	if len(alerts) > 0 {
		b.WriteString("\nAlerts (before close):\n")
		for _, a := range alerts {
			ts := "-"
			if !a.Timestamp.IsZero() {
				ts = a.Timestamp.UTC().Format(time.RFC3339)
			}
			fmt.Fprintf(&b, "- alert_id=%d kind=%s severity=%s sv=%s ts=%s",
				a.ID, a.Kind, a.Severity, a.StrategyVersion, ts)
			if a.Side != "" {
				fmt.Fprintf(&b, " side=%s", a.Side)
			}
			if a.Outcome != "" {
				fmt.Fprintf(&b, " outcome=%s", a.Outcome)
			}
			if a.NotionalUSD > 0 {
				fmt.Fprintf(&b, " notional=$%.0f", a.NotionalUSD)
			}
			if a.Odds > 0 {
				fmt.Fprintf(&b, " odds=%.2f", a.Odds)
			}
			if a.Wallet != "" {
				fmt.Fprintf(&b, " wallet=%s", oneLineCompact(a.Wallet))
			}
			if a.TelegramMessageID != nil {
				fmt.Fprintf(&b, " tg_message_id=%d", *a.TelegramMessageID)
			}
			if a.Reason != "" {
				fmt.Fprintf(&b, " reason=%s", oneLineCompact(a.Reason))
			}
			if a.CLV1h != nil {
				fmt.Fprintf(&b, " clv1h=%+.3f", *a.CLV1h)
			}
			if a.CLV6h != nil {
				fmt.Fprintf(&b, " clv6h=%+.3f", *a.CLV6h)
			}
			if a.CLV24h != nil {
				fmt.Fprintf(&b, " clv24h=%+.3f", *a.CLV24h)
			}
			if a.OutcomeStatus != "" {
				fmt.Fprintf(&b, " outcome_status=%s", a.OutcomeStatus)
			}
			b.WriteString("\n")
		}
	}

	// --- Flow ---
	b.WriteString("\nFlow:\n")
	fmt.Fprintf(&b, "total_notional_usd: %.0f\n", req.Flow.TotalNotionalUSD)
	fmt.Fprintf(&b, "large_trades_notional_usd: %.0f\n", req.Flow.LargeTradesNotionalUSD)
	fmt.Fprintf(&b, "accumulation_lines: %d\n", req.Flow.AccumulationLines)
	fmt.Fprintf(&b, "cluster_events: %d\n", req.Flow.ClusterEvents)
	fmt.Fprintf(&b, "ownership_concentration: %.3f\n", req.Flow.OwnershipConcentration)
	if req.Flow.PriceBefore != nil {
		fmt.Fprintf(&b, "price_before: %.3f\n", *req.Flow.PriceBefore)
	}
	if req.Flow.PriceAfter != nil {
		fmt.Fprintf(&b, "price_after: %.3f\n", *req.Flow.PriceAfter)
	}

	// --- Events ---
	maxEvents := req.MaxEventsInPrompt
	if maxEvents <= 0 {
		maxEvents = 30
	}
	events := req.Events
	if len(events) > maxEvents {
		events = events[:maxEvents]
	}
	if len(events) > 0 {
		b.WriteString("\nEvents:\n")
		for _, ev := range events {
			ts := "-"
			if !ev.Timestamp.IsZero() {
				ts = ev.Timestamp.UTC().Format(time.RFC3339)
			}
			fmt.Fprintf(&b, "- %s | source=%s | %s",
				ts, oneLineCompact(ev.Source), oneLineCompact(ev.Title))
			if ev.Summary != "" {
				fmt.Fprintf(&b, " | %s", oneLineCompact(ev.Summary))
			}
			b.WriteString("\n")
		}
	}

	b.WriteString("\n---\n")
	b.WriteString(MarketCloseReviewPrompt)
	return b.String()
}
