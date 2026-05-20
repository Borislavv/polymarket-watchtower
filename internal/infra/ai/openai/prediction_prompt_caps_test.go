package openai

import (
	"strings"
	"testing"

	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/analysis"
)

// PART 9 / TEST 7: the prediction CREATION prompt embeds the
// v10.1 length caps (900–3000 with target 1800–2500). The test pins
// every line from the spec so a future reword can't silently relax
// the contract.
func TestPredictionCreatePrompt_HasV10_1LengthRules(t *testing.T) {
	body := buildPredictionCreationUserMessage(analysis.PredictionCreationRequest{
		EventSlug:      "tx",
		Question:       "q",
		Outcome:        "Yes",
		Category:       "Politics",
		MarketSnapshot: "k: v",
	})
	for _, want := range []string{
		"Russian language.",
		"900–3000 characters.",
		"Target length: 1800–2500 characters.",
		"Dense, practical, no filler.",
		"Do not repeat raw market fields already provided in the message.",
		"Opinionated practical stance required.",
		"If no edge, say it shortly.",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in creation prompt", want)
		}
	}
	// The old "800–1800" upper bound that lived in the prior schema
	// MUST be gone — the new wording supersedes it.
	if strings.Contains(body, "800–1800") {
		t.Error("legacy 800–1800 length rule still present in creation prompt")
	}
}

// PART 9 / TEST 8: the prediction EVOLUTION prompt embeds the
// v10.1 length caps (800–2500 with target 1200–2000).
func TestPredictionEvolutionPrompt_HasV10_1LengthRules(t *testing.T) {
	body := buildPredictionEvolutionUserMessage(analysis.PredictionEvolutionRequest{
		EventSlug:   "tx",
		ConditionID: "0xa",
	})
	for _, want := range []string{
		"Russian language.",
		"800–2500 characters.",
		"Target length: 1200–2000 characters.",
		"Dense, practical, no filler.",
		"No invented facts.",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in evolution prompt", want)
		}
	}
	if strings.Contains(body, "1000–3500 characters") {
		t.Error("legacy 1000–3500 length rule still present in evolution prompt")
	}
}
