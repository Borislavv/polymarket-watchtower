package openai

import (
	"strings"
	"testing"
)

// PART 11 / PART 19: pin every load-bearing anchor of the v10.9
// evaluator prompt. A future edit that drops or rewords any of these
// breaks the AI contract — the test catches the drift before merge.
func TestUnifiedEvaluatorPromptV109_VerbatimAnchors(t *testing.T) {
	anchors := []string{
		"Ты — senior prediction-market desk analyst и political/geopolitical risk analyst.",
		"Это НЕ новостная сводка.",
		"Это НЕ политический комментарий.",
		"Это НЕ market summary.",
		"Главный вопрос:",
		"Что рынок сейчас НЕ понимает",
		"AiAnsweredNotFoundNoticeable",
		"AiAnsweredAlreadyPriced",
		"AiAnsweredContextStale",
		"AiAnsweredOnlyResolutionBlocked",
		"AiAnsweredLowConfidenceSkip",
		`"decision":`,
		`"regime":`,
		`"selected":`,
		`"telegram_worthy"`,
		`"expected_direction"`,
		`"expected_price_min"`,
		`"expected_price_max"`,
		`"expected_window"`,
		`"why_market_misprices"`,
		`"what_market_will_understand"`,
		`"trigger_condition"`,
		`"invalidates_if"`,
		`"trade_stance"`,
		"Select at most {{MAX_SELECTED}} markets.",
		"Do not return selected=[]; use a sentinel instead.",
		"If confidence < 0.60 for all candidates, return AiAnsweredLowConfidenceSkip.",
	}
	for _, a := range anchors {
		if !strings.Contains(UnifiedEvaluatorPromptV109, a) {
			t.Errorf("missing verbatim anchor %q in UnifiedEvaluatorPromptV109", a)
		}
	}
	// Forbidden phrases — these would re-introduce the journalist
	// failure mode the v10.9 spec specifically calls out.
	for _, banned := range []string{
		"summarize", "newsletter", "broad macro",
	} {
		if strings.Contains(strings.ToLower(UnifiedEvaluatorPromptV109), banned) {
			t.Errorf("prompt must NOT contain banned phrase %q", banned)
		}
	}
}
