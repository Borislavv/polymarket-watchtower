// PHASE 3 tests for the v11.4 Market Close Review AI parser +
// prompt builder. The strict-JSON contract is the load-bearing
// piece — any drift here breaks the worker's verdict pipeline.
package openai

import (
	"strings"
	"testing"
	"time"
)

// TestMarketCloseReviewPrompt_VerbatimAnchors pins the
// load-bearing fragments of the verbatim prompt. A future edit
// that drops or rewords any of these fails this test.
func TestMarketCloseReviewPrompt_VerbatimAnchors(t *testing.T) {
	for _, anchor := range []string{
		"You are a senior prediction-market post-resolution analyst.",
		`"verdict": "confirmed_signal|missed_signal|false_positive|inconclusive|no_edge"`,
		"Skepticism rules (LOAD-BEARING):",
		"Profitable does NOT prove insider-like trading.",
		"You do NOT have web search.",
		"alert_id MUST be one of the IDs the data block carried — do not invent.",
		`"alert_quality": "strong|mixed|weak|insufficient"`,
		`"insider_like_risk": "high|medium|low|unknown"`,
		`"reaction": "success|failure|ambiguous|none"`,
	} {
		if !strings.Contains(MarketCloseReviewPrompt, anchor) {
			t.Errorf("MarketCloseReviewPrompt missing anchor %q", anchor)
		}
	}
}

// Builder must include market header + every alert id + the
// verbatim prompt body. Truncation caps must be respected.
func TestBuildMarketCloseReviewUserMessage_CarriesIDsAndCaps(t *testing.T) {
	closedAt := time.Date(2026, 5, 20, 0, 0, 0, 0, time.UTC)
	alerts := make([]MarketCloseReviewAlertEvidence, 60)
	for i := range alerts {
		alerts[i] = MarketCloseReviewAlertEvidence{
			ID: int64(i + 1), Kind: "trade_anomaly", Severity: "info",
			Timestamp: closedAt.Add(-1 * time.Hour),
		}
	}
	msg := BuildMarketCloseReviewUserMessage(MarketCloseReviewRequest{
		Market: MarketCloseReviewMarketSummary{
			ConditionID: "0xCOND", Title: "Will it rain?",
			ClosedAt: closedAt, WinningOutcome: "YES",
		},
		Alerts:            alerts,
		MaxAlertsInPrompt: 30,
	})

	if !strings.Contains(msg, "title: Will it rain?") {
		t.Errorf("title missing from rendered prompt")
	}
	if !strings.Contains(msg, "winning_outcome: YES") {
		t.Errorf("winning_outcome missing")
	}
	if !strings.Contains(msg, "alert_id=1 ") {
		t.Errorf("first alert id missing")
	}
	if !strings.Contains(msg, "alert_id=30 ") {
		t.Errorf("30th alert id missing")
	}
	if strings.Contains(msg, "alert_id=31 ") {
		t.Errorf("MaxAlertsInPrompt=30 must cap the rendered alert list")
	}
	if !strings.Contains(msg, "Skepticism rules (LOAD-BEARING):") {
		t.Errorf("verbatim prompt body missing")
	}
}

// Parser: happy-path JSON → all fields land on the response.
// Reaction plan with alert_id=999 (not in request) is dropped.
func TestParseMarketCloseReviewJSON_HappyPathAndInvented(t *testing.T) {
	body := `{
		"verdict": "confirmed_signal",
		"confidence": 0.78,
		"market_outcome_summary": "YES won",
		"watchtower_performance": {
			"early": true,
			"directionally_correct": true,
			"alert_quality": "strong",
			"best_alert_ids": [1, 999],
			"worst_alert_ids": []
		},
		"flow_assessment": {
			"informed_flow_likely": true,
			"insider_like_risk": "medium",
			"speculation_vs_information": "informed",
			"rationale": "ok"
		},
		"market_repricing_assessment": {
			"underreaction_detected": true,
			"overreaction_detected": false,
			"repricing_lag": "hours",
			"rationale": "ok"
		},
		"strategy_assessment": [
			{"strategy": "accumulation", "verdict": "helpful", "reason": "early"}
		],
		"tuning_recommendations": [
			{"area": "thresholds", "recommendation": "tighten", "priority": "medium"}
		],
		"admin_summary": "ok",
		"reaction_plan": [
			{"alert_id": 1, "telegram_message_id": 100, "reaction": "success", "reason": "won"},
			{"alert_id": 999, "telegram_message_id": 999, "reaction": "success", "reason": "invented"}
		]
	}`
	req := MarketCloseReviewRequest{Alerts: []MarketCloseReviewAlertEvidence{{ID: 1}}}
	out, err := ParseMarketCloseReviewJSON(body, req)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if out.Verdict != "confirmed_signal" {
		t.Errorf("verdict: %s", out.Verdict)
	}
	if len(out.WatchtowerPerformance.BestAlertIDs) != 1 || out.WatchtowerPerformance.BestAlertIDs[0] != 1 {
		t.Errorf("best_alert_ids must filter invented IDs; got %v", out.WatchtowerPerformance.BestAlertIDs)
	}
	if len(out.ReactionPlan) != 1 || out.ReactionPlan[0].AlertID != 1 {
		t.Errorf("reaction_plan must filter invented alert_id=999; got %+v", out.ReactionPlan)
	}
}

// Bad enums coerce to safe defaults.
func TestParseMarketCloseReviewJSON_CoercesBadEnums(t *testing.T) {
	body := `{
		"verdict": "TOTALLY_UNKNOWN",
		"confidence": 1.5,
		"watchtower_performance": {"alert_quality": "GARBAGE"},
		"flow_assessment": {"insider_like_risk": "NOPE", "speculation_vs_information": "WUT"},
		"market_repricing_assessment": {"repricing_lag": "FAST"},
		"admin_summary": "x",
		"reaction_plan": []
	}`
	out, err := ParseMarketCloseReviewJSON(body, MarketCloseReviewRequest{})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if out.Verdict != "inconclusive" {
		t.Errorf("bad verdict must coerce to inconclusive; got %s", out.Verdict)
	}
	if out.Confidence != 1.0 {
		t.Errorf("confidence must clamp to [0,1]; got %v", out.Confidence)
	}
	if out.WatchtowerPerformance.AlertQuality != "insufficient" {
		t.Errorf("alert_quality coerce: %s", out.WatchtowerPerformance.AlertQuality)
	}
	if out.FlowAssessment.InsiderLikeRisk != "unknown" {
		t.Errorf("insider_like_risk coerce: %s", out.FlowAssessment.InsiderLikeRisk)
	}
	if out.MarketRepricingAssessment.RepricingLag != "unknown" {
		t.Errorf("repricing_lag coerce: %s", out.MarketRepricingAssessment.RepricingLag)
	}
}

// Markdown-wrapped output rejected.
func TestParseMarketCloseReviewJSON_RejectsMarkdown(t *testing.T) {
	body := "```json\n{\"verdict\":\"inconclusive\"}\n```"
	if _, err := ParseMarketCloseReviewJSON(body, MarketCloseReviewRequest{}); err == nil {
		t.Fatalf("markdown-wrapped output must be rejected")
	}
}

// admin_summary capped at 900 chars.
func TestParseMarketCloseReviewJSON_CapsAdminSummary(t *testing.T) {
	long := strings.Repeat("a", 1500)
	body := `{"verdict":"inconclusive","admin_summary":"` + long + `"}`
	out, err := ParseMarketCloseReviewJSON(body, MarketCloseReviewRequest{})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(out.AdminSummary) > 900 {
		t.Fatalf("admin_summary must be capped at 900 chars; got %d", len(out.AdminSummary))
	}
}
