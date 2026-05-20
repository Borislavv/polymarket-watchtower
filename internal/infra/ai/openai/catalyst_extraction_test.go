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

func TestParseCatalystExtractionJSON_AcceptsValidPayload(t *testing.T) {
	body := `{
        "event_slug": "texas",
        "analysis_time_utc": "2026-05-20T12:00:00Z",
        "catalysts": [
            {
                "catalyst_type": "runoff",
                "title": "Texas GOP runoff",
                "description": "Final primary resolution",
                "expected_at": "2026-06-15T12:00:00Z",
                "confidence": 0.82,
                "source": "polymarket_annotation",
                "source_url": null,
                "status": "expected",
                "blocked_reason": "market waiting for runoff",
                "bullish_scenario": "decisive Paxton win",
                "bearish_scenario": "weak result",
                "invalidation_scenario": "dispute",
                "flow_interpretation": "pre-catalyst flow is meaningful",
                "affected_outcomes": ["Ken Paxton"]
            }
        ]
    }`
	got, err := ParseCatalystExtractionJSON(body, "texas")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.EventSlug != "texas" {
		t.Errorf("event_slug: %q", got.EventSlug)
	}
	if len(got.Catalysts) != 1 {
		t.Fatalf("catalysts: got %d want 1", len(got.Catalysts))
	}
	c := got.Catalysts[0]
	if c.CatalystType != "runoff" || c.Status != "expected" {
		t.Errorf("type/status: %+v", c)
	}
	if c.ExpectedAt == nil || *c.ExpectedAt != "2026-06-15T12:00:00Z" {
		t.Errorf("expected_at: %+v", c.ExpectedAt)
	}
}

func TestParseCatalystExtractionJSON_RejectsMarkdownWrap(t *testing.T) {
	body := "```json\n{\"event_slug\":\"x\",\"catalysts\":[]}\n```"
	_, err := ParseCatalystExtractionJSON(body, "x")
	if err == nil {
		t.Fatal("expected error rejecting markdown wrap")
	}
}

func TestParseCatalystExtractionJSON_RejectsNonObject(t *testing.T) {
	body := `[1,2,3]`
	_, err := ParseCatalystExtractionJSON(body, "x")
	if err == nil {
		t.Fatal("expected error rejecting array root")
	}
}

func TestParseCatalystExtractionJSON_DropsBadEnumRows(t *testing.T) {
	body := `{
        "event_slug": "x",
        "catalysts": [
            {"catalyst_type": "wrong_type", "title": "bad", "status": "expected", "confidence": 0.9},
            {"catalyst_type": "debate", "title": "ok", "status": "wrong_status", "confidence": 0.9},
            {"catalyst_type": "debate", "title": "good", "status": "expected", "confidence": 0.9}
        ]
    }`
	got, err := ParseCatalystExtractionJSON(body, "x")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(got.Catalysts) != 1 {
		t.Fatalf("expected only 'good' row to survive, got %d", len(got.Catalysts))
	}
	if got.Catalysts[0].Title != "good" {
		t.Errorf("kept wrong row: %+v", got.Catalysts[0])
	}
}

func TestParseCatalystExtractionJSON_ClampsConfidenceAndNormalisesExpectedAt(t *testing.T) {
	body := `{
        "event_slug": "x",
        "catalysts": [
            {"catalyst_type": "debate", "title": "a", "status": "expected", "confidence": 1.5, "expected_at": "2026-05-20"},
            {"catalyst_type": "debate", "title": "b", "status": "expected", "confidence": -0.2, "expected_at": "not-a-date"},
            {"catalyst_type": "debate", "title": "c", "status": "expected", "confidence": 0.7, "expected_at": "1700-01-01T00:00:00Z"}
        ]
    }`
	got, _ := ParseCatalystExtractionJSON(body, "x")
	if len(got.Catalysts) != 3 {
		t.Fatalf("catalysts: got %d want 3", len(got.Catalysts))
	}
	for _, c := range got.Catalysts {
		if c.Confidence < 0 || c.Confidence > 1 {
			t.Errorf("confidence not clamped: %v", c.Confidence)
		}
	}
	for _, c := range got.Catalysts {
		switch c.Title {
		case "a":
			if c.ExpectedAt == nil || !strings.HasSuffix(*c.ExpectedAt, "Z") {
				t.Errorf("date-only must be normalised to RFC3339 UTC: %+v", c.ExpectedAt)
			}
		case "b", "c":
			if c.ExpectedAt != nil {
				t.Errorf("unparseable / out-of-range date must drop to nil: %+v", c.ExpectedAt)
			}
		}
	}
}

func TestParseCatalystExtractionJSON_DropsEmptyTitle(t *testing.T) {
	body := `{
        "event_slug": "x",
        "catalysts": [
            {"catalyst_type": "debate", "title": "", "status": "expected", "confidence": 0.9}
        ]
    }`
	got, _ := ParseCatalystExtractionJSON(body, "x")
	if len(got.Catalysts) != 0 {
		t.Errorf("empty title must drop: %+v", got.Catalysts)
	}
}

func TestParseCatalystExtractionJSON_NormalisesUnknownSourceToMixed(t *testing.T) {
	body := `{
        "event_slug": "x",
        "catalysts": [
            {"catalyst_type": "debate", "title": "a", "status": "expected", "confidence": 0.9, "source": "random_unknown"}
        ]
    }`
	got, _ := ParseCatalystExtractionJSON(body, "x")
	if len(got.Catalysts) != 1 || got.Catalysts[0].Source != "mixed" {
		t.Errorf("unknown source must collapse to 'mixed': %+v", got.Catalysts)
	}
}

// TestExtractCatalysts_EndToEndViaFakeOpenAI pins the production
// HTTP path: ExtractCatalysts POSTs to /chat/completions with
// response_format=json_object, parses the strict-JSON body, and
// returns a populated CatalystExtractionResponse with token
// accounting + cost stamped.
func TestExtractCatalysts_EndToEndViaFakeOpenAI(t *testing.T) {
	var seenPath string
	var seenBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		seenBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message": map[string]any{
					"role": "assistant",
					"content": `{"event_slug":"texas","analysis_time_utc":"2026-05-20T12:00:00Z","catalysts":[
                        {"catalyst_type":"runoff","title":"TX runoff","description":"final resolution","expected_at":"2026-06-15T12:00:00Z","confidence":0.78,"source":"polymarket_annotation","source_url":null,"status":"expected","blocked_reason":"awaiting runoff","bullish_scenario":"win","bearish_scenario":"loss","invalidation_scenario":"dispute","flow_interpretation":"pre-catalyst flow meaningful","affected_outcomes":["Ken Paxton"]}
                    ]}`,
				},
			}},
			"usage": map[string]any{"prompt_tokens": 420, "completion_tokens": 180},
		})
	}))
	defer srv.Close()
	c := New(Config{
		APIKey:                 "test-key",
		BaseURL:                srv.URL,
		Model:                  "test-model",
		Timeout:                3 * time.Second,
		RatePerMin:             100,
		DailyBudget:            5,
		PromptCostPer1kUSD:     0.00015,
		CompletionCostPer1kUSD: 0.0006,
	})
	res, err := c.ExtractCatalysts(context.Background(), analysis.CatalystExtractionRequest{
		EventSlug:       "texas",
		AnalysisTimeUTC: time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC),
		EventMetadata: analysis.CatalystEventMetadata{
			Title:           "Texas Senate Primary",
			Category:        "Politics",
			ResolutionRules: "First candidate called by AP wins",
		},
		Annotations: []analysis.CatalystAnnotation{
			{Timestamp: time.Date(2026, 5, 9, 0, 0, 0, 0, time.UTC), Title: "Paxton up", Outcome: "Ken Paxton"},
		},
	})
	if err != nil {
		t.Fatalf("ExtractCatalysts: %v", err)
	}
	if seenPath != "/chat/completions" {
		t.Errorf("path: %q", seenPath)
	}
	if !strings.Contains(string(seenBody), `"response_format":{"type":"json_object"}`) {
		t.Errorf("must request JSON mode; body=%s", string(seenBody))
	}
	if !strings.Contains(string(seenBody), "political/geopolitical prediction-market catalyst extractor") {
		t.Errorf("must include verbatim catalyst extraction prompt; body=%s", string(seenBody))
	}
	if !strings.Contains(string(seenBody), "event_slug: texas") {
		t.Errorf("must include event_slug in prompt data block; body=%s", string(seenBody))
	}
	if res.Status != analysis.StatusOK {
		t.Fatalf("status: %q", res.Status)
	}
	if len(res.Catalysts) != 1 {
		t.Fatalf("catalysts: got %d", len(res.Catalysts))
	}
	if res.PromptTokens != 420 || res.CompletionTokens != 180 {
		t.Errorf("token accounting wrong: %+v", res)
	}
	if res.EstimatedCostUSD <= 0 {
		t.Errorf("cost must be > 0: %v", res.EstimatedCostUSD)
	}
}

// TestExtractCatalysts_NoAPIKeySkipsCleanly pins that an unwired key
// returns StatusSkipped without any HTTP traffic.
func TestExtractCatalysts_NoAPIKeySkipsCleanly(t *testing.T) {
	c := New(Config{APIKey: "", Model: "m"})
	res, err := c.ExtractCatalysts(context.Background(), analysis.CatalystExtractionRequest{EventSlug: "x"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.Status != analysis.StatusSkipped {
		t.Errorf("status: %q want skipped", res.Status)
	}
	if res.LastError != "no_api_key" {
		t.Errorf("last_error: %q", res.LastError)
	}
}

// TestExtractCatalysts_RejectsMarkdownWrapped pins that the strict
// JSON parser refuses ```json fenced output even when 200 OK.
func TestExtractCatalysts_RejectsMarkdownWrapped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message": map[string]any{"role": "assistant", "content": "```json\n{\"event_slug\":\"x\",\"catalysts\":[]}\n```"},
			}},
			"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 5},
		})
	}))
	defer srv.Close()
	c := New(Config{APIKey: "k", BaseURL: srv.URL, Model: "m", RatePerMin: 100, DailyBudget: 5})
	res, err := c.ExtractCatalysts(context.Background(), analysis.CatalystExtractionRequest{EventSlug: "x"})
	if err == nil {
		t.Fatal("expected parse error")
	}
	if res.Status != analysis.StatusError || res.LastError != "invalid_json" {
		t.Errorf("status/last_error wrong: %+v", res)
	}
}
