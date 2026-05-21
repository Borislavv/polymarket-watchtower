package openai

import (
	"strings"
	"testing"
)

// PART 9 + PART 21: pin every load-bearing anchor of the v11.0 Hourly
// News Intelligence prompt. The spec says "Insert this prompt EXACTLY.
// Do not rewrite." Any rewording that drops or weakens these anchors
// breaks the AI contract — this test catches the drift before merge.
func TestHourlyNewsIntelPromptV11_VerbatimAnchors(t *testing.T) {
	anchors := []string{
		"Ты — senior prediction-market news intelligence analyst.",
		"Это НЕ обзор новостей.",
		"Это НЕ политическая сводка.",
		"Это НЕ market summary.",
		"Главный вопрос:",
		"Какая новость сейчас может быть недооценена рынком",
		// PART 21 hardening: explicit prohibitions on summary/retrospective behaviour.
		"НЕ пересказывай новости.",
		"НЕ делай обзор.",
		"НЕ делай retrospective commentary.",
		"НЕ описывай уже понятные рынку события.",
		"НЕ делай summary всех событий.",
		// All five sentinels per PART 8.
		"AiAnsweredNotFoundNoticeable",
		"AiAnsweredAlreadyPriced",
		"AiAnsweredContextStale",
		"AiAnsweredInsufficientData",
		"AiAnsweredLowConfidenceSkip",
		// Banned canned outputs (the legacy filler the v11.0 spec kills).
		"crowded market",
		"weak regime",
		"no actionable catalysts",
		"monitor polls",
		"already priced recap",
		"retrospective explanation",
		"stable favorite commentary",
		// JSON schema fields the worker depends on.
		`"decision":`,
		`"selected":`,
		`"news_item_hash":`,
		`"event_slug":`,
		`"condition_id":`,
		`"market_title":`,
		`"impact_direction"`,
		`"expected_price_impact_min"`,
		`"expected_price_impact_max"`,
		`"expected_window"`,
		`"why_it_matters"`,
		`"what_market_may_miss"`,
		`"trigger_condition"`,
		`"invalidates_if"`,
		`"trade_stance"`,
		`"telegram_worthy"`,
		// Rules.
		"Select at most {{MAX_SELECTED}} items.",
		"Do not return selected=[]; use a sentinel instead.",
		"If confidence < 0.60 for all candidates, return AiAnsweredLowConfidenceSkip.",
		"Do not summarize all news.",
	}
	for _, a := range anchors {
		if !strings.Contains(HourlyNewsIntelPromptV11, a) {
			t.Errorf("missing verbatim anchor %q in HourlyNewsIntelPromptV11", a)
		}
	}
	// Forbidden phrases — these would re-introduce the journalist /
	// newsletter / retrospective failure modes the v11.0 PART 21 spec
	// specifically calls out. (Note: "summarize all news" is allowed
	// because PART 21 uses it as a NEGATIVE rule — "Do not summarize
	// all news." — so we only ban newsletter-style framing here.)
	for _, banned := range []string{
		"newsletter", "broad macro", "journalist-style",
	} {
		if strings.Contains(strings.ToLower(HourlyNewsIntelPromptV11), banned) {
			t.Errorf("prompt must NOT contain banned phrase %q", banned)
		}
	}
}

// MAX_SELECTED placeholder must remain substitutable by the caller.
func TestHourlyNewsIntelPromptV11_HasMaxSelectedPlaceholder(t *testing.T) {
	if !strings.Contains(HourlyNewsIntelPromptV11, "{{MAX_SELECTED}}") {
		t.Fatalf("HourlyNewsIntelPromptV11 missing {{MAX_SELECTED}} placeholder")
	}
}
