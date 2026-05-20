package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/analysis"
)

// CreatePrediction satisfies analysis.PredictionCreator. Stage 2 of
// the prediction-creation AI pipeline: full thesis generation for a
// single market the ranker selected.
//
// Output is strict JSON (response_format=json_object). The worker
// rejects markdown-wrapped output, bad enum values, and out-of-range
// confidence — the rest of the pipeline only ever sees normalised data.
func (c *Client) CreatePrediction(ctx context.Context, req analysis.PredictionCreationRequest) (analysis.PredictionCreationResponse, error) {
	zero := analysis.PredictionCreationResponse{Status: analysis.StatusError, Model: c.cfg.Model}
	if c.cfg.APIKey == "" {
		zero.Status = analysis.StatusSkipped
		zero.LastError = "no_api_key"
		return zero, nil
	}
	if !c.bucket.allow() {
		zero.Status = analysis.StatusSkipped
		zero.LastError = "rate_limited"
		return zero, nil
	}
	if !c.ledger.allow() {
		zero.Status = analysis.StatusSkipped
		zero.LastError = "daily_budget_exhausted"
		return zero, nil
	}
	if strings.TrimSpace(req.EventSlug) == "" {
		zero.Status = analysis.StatusSkipped
		zero.LastError = "no_event_slug"
		return zero, nil
	}
	userMsg := buildPredictionCreationUserMessage(req)
	httpResp, err := c.callChatJSON(ctx, userMsg)
	if err != nil {
		pe, ok := AsProviderError(err)
		if !ok {
			pe = &ProviderError{Category: CategoryUnknown, Message: err.Error()}
		}
		zero.Status = statusForCategory(pe.Category)
		zero.LastError = string(pe.Category)
		return zero, err
	}
	cost := c.estimateCost(httpResp.PromptTokens, httpResp.CompletionTokens)
	c.ledger.consume(cost)
	parsed, parseErr := ParsePredictionCreationJSON(httpResp.Text)
	parsed.Model = c.cfg.Model
	parsed.PromptTokens = httpResp.PromptTokens
	parsed.CompletionTokens = httpResp.CompletionTokens
	parsed.EstimatedCostUSD = cost
	if parseErr != nil {
		parsed.Status = analysis.StatusError
		parsed.LastError = "invalid_json"
		return parsed, parseErr
	}
	parsed.Status = analysis.StatusOK
	return parsed, nil
}

// buildPredictionCreationUserMessage substitutes the placeholders in
// predictionCreationPrompt with normalised request fields. Empty
// blocks fall back to explicit no-data sentences so the model
// doesn't infer absence as evidence.
func buildPredictionCreationUserMessage(req analysis.PredictionCreationRequest) string {
	fallback := func(s, alt string) string {
		if strings.TrimSpace(s) == "" {
			return alt
		}
		return s
	}
	repl := strings.NewReplacer(
		"{{EVENT_SLUG}}", req.EventSlug,
		"{{QUESTION}}", oneLineCompact(req.Question),
		"{{OUTCOME}}", fallback(req.Outcome, "—"),
		"{{CATEGORY}}", fallback(req.Category, "—"),
		"{{MARKET_DATA}}", fallback(req.MarketSnapshot, "(no market snapshot supplied)"),
		"{{ANNOTATIONS}}", fallback(req.AnnotationsBlock, "(no fresh annotations)"),
		"{{CATALYSTS}}", fallback(req.CatalystsBlock, "(no catalysts known)"),
		"{{REPRICING}}", fallback(req.RepricingBlock, "(no repricing signal computed)"),
		"{{FLOW_SUMMARY}}", fallback(req.FlowSummaryBlock, "(no flow signal in window)"),
		"{{MATCHED_ALERTS}}", fallback(req.MatchedAlertsBlock, "(no matched alerts)"),
	)
	return repl.Replace(predictionCreationPrompt)
}

var validCreationSideBias = map[string]struct{}{
	"bullish": {}, "bearish": {}, "neutral": {},
}

// ParsePredictionCreationJSON validates and normalises the AI's
// strict-JSON thesis response. Exported so worker + tests share the
// parser. Enum / range violations DO NOT fail the call — they are
// clamped to safe defaults so a partially-valid thesis still lands.
func ParsePredictionCreationJSON(text string) (analysis.PredictionCreationResponse, error) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return analysis.PredictionCreationResponse{}, errors.New("empty body")
	}
	if strings.HasPrefix(trimmed, "```") {
		return analysis.PredictionCreationResponse{}, errors.New("markdown-wrapped response rejected")
	}
	if trimmed[0] != '{' {
		return analysis.PredictionCreationResponse{}, errors.New("response is not a JSON object")
	}
	var raw struct {
		Summary     string  `json:"summary"`
		SideBias    string  `json:"side_bias"`
		Confidence  float64 `json:"confidence"`
		RiskFactors string  `json:"risk_factors"`
	}
	if err := json.Unmarshal([]byte(trimmed), &raw); err != nil {
		return analysis.PredictionCreationResponse{}, fmt.Errorf("json unmarshal: %w", err)
	}
	out := analysis.PredictionCreationResponse{
		Summary:     strings.TrimSpace(raw.Summary),
		SideBias:    strings.ToLower(strings.TrimSpace(raw.SideBias)),
		Confidence:  raw.Confidence,
		RiskFactors: strings.TrimSpace(raw.RiskFactors),
	}
	if _, ok := validCreationSideBias[out.SideBias]; !ok {
		out.SideBias = "neutral"
	}
	if out.Confidence < 0 {
		out.Confidence = 0
	}
	if out.Confidence > 1 {
		out.Confidence = 1
	}
	if out.Summary == "" {
		return out, errors.New("empty summary in response")
	}
	return out, nil
}
