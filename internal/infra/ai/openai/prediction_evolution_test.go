package openai

import (
	"strings"
	"testing"

	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/analysis"
)

func TestBuildPredictionEvolutionUserMessage_VerbatimPromptPresent(t *testing.T) {
	msg := buildPredictionEvolutionUserMessage(analysis.PredictionEvolutionRequest{
		EventSlug:          "tx",
		ConditionID:        "0xa",
		PreviousPrediction: "Watchlist. Paxton leads polls; Cornyn scandal weakens his side.",
		PredictionState:    "watching",
		StateReason:        "no decisive signal",
		MarketSnapshot:     "event_slug: tx\nlast_trade_price: 0.620\n",
		AnnotationsBlock:   "- 2026-05-09 | Final poll | outcome=Ken Paxton | price 0.54 -> 0.61\n",
		CatalystsBlock:     "- type=runoff | status=expected | expected_at=2026-06-15T12:00:00Z | confidence=0.80 | title=TX runoff\n",
		RepricingBlock:     "Repricing intelligence:\n- repricing status: underreacting (confidence 0.70)\n",
		FlowSummaryBlock:   "Recent Watchtower flow:\nwindow: last 24h\nalerts: total=7\n",
		MatchedAlertsBlock: "- critical · accumulation · score=0.82 · aligned\n",
	})
	for _, want := range []string{
		// Verbatim PART 9 phrases.
		"Ты — senior analyst на political/geopolitical prediction-market desk.",
		"обновить thesis, а не писать новый обзор",
		"Practical stance:",
		"Catalyst uncertainty likely resolved.",
		"Edge likely already priced in.",
		"Flow confirms the thesis.",
		"Flow contradicts the thesis.",
		// Substituted placeholders.
		"Paxton leads polls",
		"watching · no decisive signal",
		"last_trade_price: 0.620",
		"Final poll",
		"type=runoff",
		"underreacting",
		"window: last 24h",
		"critical · accumulation",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("missing %q in prompt body", want)
		}
	}
	for _, ph := range []string{
		"{{PREVIOUS_PREDICTION}}", "{{PREDICTION_STATE}}", "{{MARKET_DATA}}",
		"{{ANNOTATIONS}}", "{{CATALYSTS}}", "{{REPRICING}}",
		"{{FLOW_SUMMARY}}", "{{MATCHED_ALERTS}}", "{{WEB_CONTEXT}}",
	} {
		if strings.Contains(msg, ph) {
			t.Errorf("placeholder %s leaked through", ph)
		}
	}
}

func TestBuildPredictionEvolutionUserMessage_EmptyInputsRenderFallbacks(t *testing.T) {
	msg := buildPredictionEvolutionUserMessage(analysis.PredictionEvolutionRequest{
		EventSlug: "x",
	})
	for _, want := range []string{
		"(no prior thesis on file)",
		"(no market snapshot supplied)",
		"(no fresh annotations)",
		"(no catalysts known)",
		"(no repricing signal computed)",
		"(no flow signal in window)",
		"(no matched alerts)",
		"Web context: NOT checked. Do not invent public facts.",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("missing fallback %q", want)
		}
	}
}
