package openai

import (
	"strings"
	"testing"
)

// PART 14 / PART 17: the v10.8 evaluator prompt is operator-authored
// verbatim. Pinning load-bearing anchors prevents silent drift that
// would re-introduce journalist-style outputs.
func TestUnifiedEvaluatorPromptV108_VerbatimAnchors(t *testing.T) {
	anchors := []string{
		"Ты — senior analyst event-driven prediction-market desk.",
		"AiAnsweredNotFoundNoticeable",
		"AiAnsweredAlreadyPriced",
		"AiAnsweredContextStale",
		"AiAnsweredOnlyResolutionBlocked",
		"AiAnsweredInsufficientData",
		`"regime":`,
		`"selected":`,
		"Empty selected[] is NEVER acceptable",
		"Select at most {{MAX_SELECTED}} markets.",
		`No "monitor polls"`,
		`"wait for catalyst" filler`,
		"Do not invent news/polls/endorsements.",
		// Tone gate: the prompt MUST tell the model what to ignore.
		"already priced",
		"weak regime",
		"no fresh news",
		// Tone gate: the prompt MUST NOT use the journalist verbs.
	}
	for _, a := range anchors {
		if !strings.Contains(UnifiedEvaluatorPromptV108, a) {
			t.Errorf("missing verbatim anchor %q in UnifiedEvaluatorPromptV108", a)
		}
	}
	// Forbidden words that would re-introduce v10.7 noise. The word
	// "watchlist" is allowed because the prompt explicitly tells the
	// model NOT to produce one ("НЕ пиши watchlist").
	for _, banned := range []string{
		"summarize", "newsletter", "broad macro",
	} {
		if strings.Contains(strings.ToLower(UnifiedEvaluatorPromptV108), banned) {
			t.Errorf("prompt must NOT contain banned phrase %q", banned)
		}
	}
}
