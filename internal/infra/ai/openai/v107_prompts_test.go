package openai

import (
	"strings"
	"testing"
)

// PART 17 test 19: prompt output rules pinned.
//
// The v10.7 prompts are operator-authored verbatim. A silent edit
// risks breaking the AI sentinel contract — these tests pin the
// load-bearing phrases so a future refactor can't drift them.

func TestMarketBatchRankingPrompt_VerbatimAnchors(t *testing.T) {
	anchors := []string{
		"Ты — senior analyst prediction markets desk.",
		"AI_NO_NOTICEABLE_EDGE",
		"AI_ALREADY_PRICED",
		"AI_CONTEXT_STALE",
		"Иначе верни STRICT JSON only.",
		`"should_request_full_analysis": true`,
		`"selected": [`,
		"Select at most {{MAX_SELECTED}} markets.",
		"Do not return JSON with selected=[]; use AI_NO_NOTICEABLE_EDGE instead.",
		"Do not return avoid_noise as selected; suppress it with AI_NO_NOTICEABLE_EDGE.",
	}
	for _, a := range anchors {
		if !strings.Contains(MarketBatchRankingPromptV107, a) {
			t.Errorf("missing verbatim anchor %q in MarketBatchRankingPromptV107", a)
		}
	}
}

func TestPredictionAnalysisPrompt_VerbatimAnchors(t *testing.T) {
	anchors := []string{
		"Ты — senior political/geopolitical prediction-market analyst.",
		"AI_ONLY_RESOLUTION_BLOCKED",
		"AI_ALREADY_PRICED",
		"AI_NO_NOTICEABLE_EDGE",
		"AI_CONTEXT_STALE",
		"Prediction\n• Thesis: ...",
		"• Trade stance: consider / watch / avoid",
		"Hard max 2500 characters.",
		"If no real edge, do NOT write prose; return sentinel only.",
		`Do not say "wait for election day" as a prediction.`,
	}
	for _, a := range anchors {
		if !strings.Contains(PredictionAnalysisPromptV107, a) {
			t.Errorf("missing verbatim anchor %q in PredictionAnalysisPromptV107", a)
		}
	}
}
