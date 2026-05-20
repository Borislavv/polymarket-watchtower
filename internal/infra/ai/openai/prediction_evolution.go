package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/analysis"
)

// RefreshPredictionThesis satisfies
// analysis.PredictionEvolutionGenerator. Free-text Russian, no JSON
// mode — the evolution worker treats the model output as a
// prose-style thesis update rendered HTML-escaped into Telegram.
//
// Failure semantics mirror GenerateDailyPoliticalIntel: rate /
// budget / no-key short-circuit to StatusSkipped without an HTTP
// call. Provider errors return Status=error + categorised LastError
// so the worker request-logs and never persists provider bodies as
// thesis text.
func (c *Client) RefreshPredictionThesis(ctx context.Context, req analysis.PredictionEvolutionRequest) (analysis.PredictionEvolutionResponse, error) {
	zero := analysis.PredictionEvolutionResponse{
		Status: analysis.StatusError,
		Model:  c.cfg.Model,
	}
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

	userMsg := buildPredictionEvolutionUserMessage(req)
	httpResp, err := c.callPredictionEvolution(ctx, userMsg)
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
	text := strings.TrimSpace(httpResp.Text)
	if text == "" {
		zero.Status = analysis.StatusError
		zero.LastError = "empty_text"
		return zero, errors.New("empty prediction evolution body")
	}
	return analysis.PredictionEvolutionResponse{
		ThesisUpdate:     text,
		Status:           analysis.StatusOK,
		Model:            c.cfg.Model,
		PromptTokens:     httpResp.PromptTokens,
		CompletionTokens: httpResp.CompletionTokens,
		EstimatedCostUSD: cost,
	}, nil
}

// callPredictionEvolution is the per-request Chat Completions call.
// Larger max_completion_tokens than the per-alert path (the
// 1000-3500 char target fits comfortably in ~1200 tokens).
func (c *Client) callPredictionEvolution(ctx context.Context, userMsg string) (chatResp, error) {
	body, err := json.Marshal(map[string]any{
		"model": c.cfg.Model,
		"messages": []map[string]string{
			{"role": "system", "content": "You are a senior political/geopolitical prediction-market desk analyst. Update an existing thesis, do not write a new report. Russian. Dense, practical, no filler. Never invent facts."},
			{"role": "user", "content": userMsg},
		},
		"max_completion_tokens": 2000,
		"temperature":           0.2,
	})
	if err != nil {
		return chatResp{}, fmt.Errorf("marshal prediction evolution request: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return chatResp{}, fmt.Errorf("new prediction evolution request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	resp, err := c.cfg.HTTPClient.Do(httpReq)
	if err != nil {
		cat := CategoryNetwork
		if errors.Is(err, context.DeadlineExceeded) {
			cat = CategoryTimeout
		}
		return chatResp{}, &ProviderError{Category: cat, Message: sanitizeAndCap(err.Error(), 200), Retryable: true}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return chatResp{}, &ProviderError{Category: CategoryNetwork, Message: sanitizeAndCap(err.Error(), 200), Retryable: true}
	}
	if resp.StatusCode/100 != 2 {
		return chatResp{}, classifyHTTPError(resp.StatusCode, raw)
	}
	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return chatResp{}, &ProviderError{Category: CategoryUnknown, Message: sanitizeAndCap("unmarshal: "+err.Error(), 200), Retryable: false}
	}
	if len(parsed.Choices) == 0 {
		return chatResp{}, &ProviderError{Category: CategoryEmptyResponse, Message: "no choices", Retryable: true}
	}
	return chatResp{
		Text:             strings.TrimSpace(parsed.Choices[0].Message.Content),
		PromptTokens:     parsed.Usage.PromptTokens,
		CompletionTokens: parsed.Usage.CompletionTokens,
	}, nil
}

// buildPredictionEvolutionUserMessage substitutes the nine
// placeholders. Caller passes already-rendered structured blocks;
// this function just stitches them into the verbatim template.
func buildPredictionEvolutionUserMessage(req analysis.PredictionEvolutionRequest) string {
	prev := strings.TrimSpace(req.PreviousPrediction)
	if prev == "" {
		prev = "(no prior thesis on file)"
	}
	state := strings.TrimSpace(req.PredictionState)
	if reason := strings.TrimSpace(req.StateReason); reason != "" {
		state = state + " · " + reason
	}
	market := strings.TrimSpace(req.MarketSnapshot)
	if market == "" {
		market = "(no market snapshot supplied)"
	}
	annotations := strings.TrimSpace(req.AnnotationsBlock)
	if annotations == "" {
		annotations = "(no fresh annotations)"
	}
	catalysts := strings.TrimSpace(req.CatalystsBlock)
	if catalysts == "" {
		catalysts = "(no catalysts known)"
	}
	repricing := strings.TrimSpace(req.RepricingBlock)
	if repricing == "" {
		repricing = "(no repricing signal computed)"
	}
	flow := strings.TrimSpace(req.FlowSummaryBlock)
	if flow == "" {
		flow = "(no flow signal in window)"
	}
	matched := strings.TrimSpace(req.MatchedAlertsBlock)
	if matched == "" {
		matched = "(no matched alerts)"
	}
	web := strings.TrimSpace(req.WebContextBlock)
	if web == "" {
		if req.PublicContextOn {
			web = "Web context: web_search was attempted; use the tool for the latest news affecting this event."
		} else {
			web = "Web context: NOT checked. Do not invent public facts."
		}
	}
	repl := strings.NewReplacer(
		"{{PREVIOUS_PREDICTION}}", prev,
		"{{PREDICTION_STATE}}", state,
		"{{MARKET_DATA}}", market,
		"{{ANNOTATIONS}}", annotations,
		"{{CATALYSTS}}", catalysts,
		"{{REPRICING}}", repricing,
		"{{FLOW_SUMMARY}}", flow,
		"{{MATCHED_ALERTS}}", matched,
		"{{WEB_CONTEXT}}", web,
	)
	return repl.Replace(predictionEvolutionPrompt)
}
