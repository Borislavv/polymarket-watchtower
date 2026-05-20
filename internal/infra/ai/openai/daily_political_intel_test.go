package openai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/analysis"
)

func TestBuildDailyPoliticalIntelUserMessage_VerbatimPromptPresent(t *testing.T) {
	msg := buildDailyPoliticalIntelUserMessage(stubDailyIntelRequest())
	for _, want := range []string{
		// Verbatim PART 5 leading phrase.
		"head of political/geopolitical prediction-market intelligence desk",
		// Output format anchor.
		"Daily political market intelligence",
		// Section anchors from PART 5.
		"1. Executive read",
		"6. What to monitor today",
		"7. Final stance",
		// Substituted placeholders.
		"Report date:\n2026-05-20",
		"event=texas",
		"category=Politics",
		// Catalyst row should pass through.
		"type=runoff",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("missing %q in user message", want)
		}
	}
	for _, ph := range []string{
		"{{REPORT_DATE}}", "{{MARKETS_WITH_ANNOTATIONS}}",
		"{{FLOW_SUMMARY}}", "{{CATALYSTS}}", "{{PREVIOUS_DAILY_REPORT}}",
	} {
		if strings.Contains(msg, ph) {
			t.Errorf("placeholder %s must be substituted", ph)
		}
	}
}

func TestGenerateDailyPoliticalIntel_HappyPath(t *testing.T) {
	var seenBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message": map[string]any{"role": "assistant", "content": "Daily political market intelligence\n\n1. Executive read\nRepricing regime."},
			}},
			"usage": map[string]any{"prompt_tokens": 800, "completion_tokens": 600},
		})
	}))
	defer srv.Close()
	c := New(Config{
		APIKey: "test-key", BaseURL: srv.URL, Model: "test-model",
		Timeout: 5 * time.Second, RatePerMin: 100, DailyBudget: 10,
		PromptCostPer1kUSD: 0.00015, CompletionCostPer1kUSD: 0.0006,
	})
	res, err := c.GenerateDailyPoliticalIntel(context.Background(), stubDailyIntelRequest())
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.Status != analysis.StatusOK {
		t.Errorf("status: %q want ok", res.Status)
	}
	if !strings.Contains(res.ReportText, "Daily political market intelligence") {
		t.Errorf("report text lost: %q", res.ReportText)
	}
	if res.PromptTokens != 800 || res.CompletionTokens != 600 {
		t.Errorf("token accounting: %+v", res)
	}
	// Wire body must NOT request JSON mode for the daily report.
	if strings.Contains(string(seenBody), "response_format") {
		t.Errorf("daily intel must NOT use JSON mode; body=%s", string(seenBody))
	}
}
