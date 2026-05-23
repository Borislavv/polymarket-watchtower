// market_close_review.go — v11.4 OpenAI bridge for the Market
// Close Review learning loop. Strict-JSON only; budget-gated by
// the caller; admin-only output. The bridge does NOT call out to
// web search — the prompt explicitly forbids fresh-news claims.
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
	"time"
)

// MarketCloseReviewMarketSummary is the compact market header the
// AI receives. Closed/resolved timestamps + winning outcome anchor
// the verdict; the AI has no other source-of-truth for the result.
type MarketCloseReviewMarketSummary struct {
	ConditionID    string
	EventSlug      string
	Title          string
	Category       string
	OpenedAt       time.Time
	ClosedAt       time.Time
	ResolvedAt     time.Time
	WinningOutcome string
	FinalPrice     *float64
}

// MarketCloseReviewAlertEvidence is one alert row in the prompt
// data block. The IDs let the AI reference specific alerts in
// best_alert_ids / worst_alert_ids / reaction_plan.
type MarketCloseReviewAlertEvidence struct {
	ID                int64
	Kind              string
	Severity          string
	StrategyVersion   string
	Reason            string
	Timestamp         time.Time
	Side              string
	Outcome           string
	NotionalUSD       float64
	Odds              float64
	Wallet            string
	TelegramMessageID *int64
	CLV15m            *float64
	CLV1h             *float64
	CLV6h             *float64
	CLV24h            *float64
	OutcomeStatus     string
}

// MarketCloseReviewFlowEvidence summarises pre-close flow + key
// price moves. Numbers are aggregated by the worker — the AI does
// not get raw trade rows.
type MarketCloseReviewFlowEvidence struct {
	TotalNotionalUSD       float64
	LargeTradesNotionalUSD float64
	AccumulationLines      int
	ClusterEvents          int
	OwnershipConcentration float64
	PriceBefore            *float64
	PriceAfter             *float64
}

// MarketCloseReviewEventEvidence is a compact news/event row.
type MarketCloseReviewEventEvidence struct {
	Timestamp time.Time
	Source    string
	Title     string
	Summary   string
}

// MarketCloseReviewRequest is the call payload.
type MarketCloseReviewRequest struct {
	Market MarketCloseReviewMarketSummary
	Alerts []MarketCloseReviewAlertEvidence
	Flow   MarketCloseReviewFlowEvidence
	Events []MarketCloseReviewEventEvidence

	// MaxAlertsInPrompt / MaxEventsInPrompt are the bounded caps
	// the worker uses to keep the prompt size predictable. The
	// bridge re-applies them defensively before serialising.
	MaxAlertsInPrompt int
	MaxEventsInPrompt int
}

// MarketCloseReviewReactionPlan is the per-alert reaction the AI
// recommends. The worker validates that each TelegramMessageID
// matches an alert it provided in the prompt (no inventing).
type MarketCloseReviewReactionPlan struct {
	AlertID           int64  `json:"alert_id"`
	TelegramMessageID *int64 `json:"telegram_message_id,omitempty"`
	Reaction          string `json:"reaction"`
	Reason            string `json:"reason"`
}

// MarketCloseReviewStrategyAssessment is one entry in the AI's
// per-strategy quality verdict.
type MarketCloseReviewStrategyAssessment struct {
	Strategy string `json:"strategy"`
	Verdict  string `json:"verdict"`
	Reason   string `json:"reason"`
}

// MarketCloseReviewTuningRec is one tuning suggestion.
type MarketCloseReviewTuningRec struct {
	Area           string `json:"area"`
	Recommendation string `json:"recommendation"`
	Priority       string `json:"priority"`
}

// MarketCloseReviewResponse is the parsed JSON output. Mirrors
// the schema in market_close_review_prompt.go exactly.
type MarketCloseReviewResponse struct {
	Verdict               string  `json:"verdict"`
	Confidence            float64 `json:"confidence"`
	MarketOutcomeSummary  string  `json:"market_outcome_summary"`
	WatchtowerPerformance struct {
		Early                bool    `json:"early"`
		DirectionallyCorrect bool    `json:"directionally_correct"`
		AlertQuality         string  `json:"alert_quality"`
		BestAlertIDs         []int64 `json:"best_alert_ids"`
		WorstAlertIDs        []int64 `json:"worst_alert_ids"`
	} `json:"watchtower_performance"`
	FlowAssessment struct {
		InformedFlowLikely       bool   `json:"informed_flow_likely"`
		InsiderLikeRisk          string `json:"insider_like_risk"`
		SpeculationVsInformation string `json:"speculation_vs_information"`
		Rationale                string `json:"rationale"`
	} `json:"flow_assessment"`
	MarketRepricingAssessment struct {
		UnderreactionDetected bool   `json:"underreaction_detected"`
		OverreactionDetected  bool   `json:"overreaction_detected"`
		RepricingLag          string `json:"repricing_lag"`
		Rationale             string `json:"rationale"`
	} `json:"market_repricing_assessment"`
	StrategyAssessment    []MarketCloseReviewStrategyAssessment `json:"strategy_assessment"`
	TuningRecommendations []MarketCloseReviewTuningRec          `json:"tuning_recommendations"`
	AdminSummary          string                                `json:"admin_summary"`
	ReactionPlan          []MarketCloseReviewReactionPlan       `json:"reaction_plan"`

	// Token + cost telemetry filled by the bridge (not by the AI).
	PromptTokens     int
	CompletionTokens int
	EstimatedCostUSD float64
	Model            string
	RawText          string
}

// ReviewMarketClose dispatches the strict-JSON Market Close Review
// call. Caller-side budget gate is the operator's responsibility —
// this bridge does not consult aibudget; it only consumes the
// ledger.
//
// Failures:
//   - rate limit / budget / no_api_key → ProviderError CategoryQuota*
//     surfaced through the standard error path; caller maps to
//     "skipped_budget_exhausted".
//   - HTTP / parse / schema validation → returned as a typed
//     ProviderError so the caller can record_log it.
func (c *Client) ReviewMarketClose(ctx context.Context, req MarketCloseReviewRequest) (MarketCloseReviewResponse, error) {
	var zero MarketCloseReviewResponse
	zero.Model = c.cfg.Model
	if c.cfg.APIKey == "" {
		return zero, &ProviderError{Category: CategoryUnknown, Message: "no_api_key"}
	}
	if !c.bucket.allow() {
		return zero, &ProviderError{Category: CategoryRateLimited, Message: "rate_limited"}
	}
	if !c.ledger.allow() {
		return zero, &ProviderError{Category: CategoryQuotaExceeded, Message: "daily_budget_exhausted"}
	}

	userMsg := BuildMarketCloseReviewUserMessage(req)

	resp, err := c.callMarketCloseReviewJSON(ctx, userMsg)
	if err != nil {
		return zero, err
	}
	cost := c.estimateCost(resp.PromptTokens, resp.CompletionTokens)
	c.ledger.consume(cost)

	parsed, parseErr := ParseMarketCloseReviewJSON(resp.Text, req)
	parsed.PromptTokens = resp.PromptTokens
	parsed.CompletionTokens = resp.CompletionTokens
	parsed.EstimatedCostUSD = cost
	parsed.Model = c.cfg.Model
	parsed.RawText = resp.Text
	if parseErr != nil {
		return parsed, &ProviderError{Category: CategoryUnknown, Message: "invalid_json: " + parseErr.Error()}
	}
	return parsed, nil
}

// callMarketCloseReviewJSON is the JSON-mode HTTP path. Mirrors
// the catalyst extractor's shape; isolated here so the request
// shape stays close to the prompt.
func (c *Client) callMarketCloseReviewJSON(ctx context.Context, userMsg string) (chatResp, error) {
	body, err := json.Marshal(map[string]any{
		"model": c.cfg.Model,
		"messages": []map[string]string{
			{"role": "system", "content": "You are a strict-JSON prediction-market post-resolution analyst. Reply with exactly one JSON object matching the requested schema. No markdown. No prose outside JSON. Be skeptical."},
			{"role": "user", "content": userMsg},
		},
		"max_completion_tokens": 2200,
		"temperature":           0.1,
		"response_format":       map[string]string{"type": "json_object"},
	})
	if err != nil {
		return chatResp{}, fmt.Errorf("marshal market close review request: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return chatResp{}, fmt.Errorf("new market close review request: %w", err)
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

// --- Validation enums (pinned by tests) -----------------------------------

var validReviewVerdicts = map[string]struct{}{
	"confirmed_signal": {},
	"missed_signal":    {},
	"false_positive":   {},
	"inconclusive":     {},
	"no_edge":          {},
}

var validReviewAlertQuality = map[string]struct{}{
	"strong":       {},
	"mixed":        {},
	"weak":         {},
	"insufficient": {},
}

var validReviewInsiderRisk = map[string]struct{}{
	"high":    {},
	"medium":  {},
	"low":     {},
	"unknown": {},
}

var validReviewSpecVsInfo = map[string]struct{}{
	"informed":    {},
	"speculative": {},
	"noise":       {},
	"mixed":       {},
	"unknown":     {},
}

var validReviewRepricingLag = map[string]struct{}{
	"none":    {},
	"minutes": {},
	"hours":   {},
	"days":    {},
	"unknown": {},
}

var validReviewReactions = map[string]struct{}{
	"success":   {},
	"failure":   {},
	"ambiguous": {},
	"none":      {},
}

// ParseMarketCloseReviewJSON validates + normalises the raw JSON
// body. Out-of-range enums coerce to the inconclusive/unknown
// equivalent; rows that reference an alert_id NOT in the request
// are dropped from the reaction_plan (the AI is forbidden from
// inventing IDs).
//
// Returns (best-effort parsed, error) — when the wire shape is
// fundamentally invalid (markdown wrap, non-object) the error is
// non-nil and the caller persists the row as failed.
func ParseMarketCloseReviewJSON(text string, req MarketCloseReviewRequest) (MarketCloseReviewResponse, error) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return MarketCloseReviewResponse{}, errors.New("empty body")
	}
	if strings.HasPrefix(trimmed, "```") {
		return MarketCloseReviewResponse{}, errors.New("markdown-wrapped response rejected")
	}
	if trimmed[0] != '{' {
		return MarketCloseReviewResponse{}, errors.New("response is not a JSON object")
	}
	var out MarketCloseReviewResponse
	if err := json.Unmarshal([]byte(trimmed), &out); err != nil {
		return MarketCloseReviewResponse{}, fmt.Errorf("json unmarshal: %w", err)
	}

	out.Verdict = strings.ToLower(strings.TrimSpace(out.Verdict))
	if _, ok := validReviewVerdicts[out.Verdict]; !ok {
		out.Verdict = "inconclusive"
	}
	if !(out.Confidence >= 0 && out.Confidence <= 1) {
		if out.Confidence > 1 {
			out.Confidence = 1
		} else {
			out.Confidence = 0
		}
	}

	out.WatchtowerPerformance.AlertQuality = strings.ToLower(strings.TrimSpace(out.WatchtowerPerformance.AlertQuality))
	if _, ok := validReviewAlertQuality[out.WatchtowerPerformance.AlertQuality]; !ok {
		out.WatchtowerPerformance.AlertQuality = "insufficient"
	}
	out.FlowAssessment.InsiderLikeRisk = strings.ToLower(strings.TrimSpace(out.FlowAssessment.InsiderLikeRisk))
	if _, ok := validReviewInsiderRisk[out.FlowAssessment.InsiderLikeRisk]; !ok {
		out.FlowAssessment.InsiderLikeRisk = "unknown"
	}
	out.FlowAssessment.SpeculationVsInformation = strings.ToLower(strings.TrimSpace(out.FlowAssessment.SpeculationVsInformation))
	if _, ok := validReviewSpecVsInfo[out.FlowAssessment.SpeculationVsInformation]; !ok {
		out.FlowAssessment.SpeculationVsInformation = "unknown"
	}
	out.MarketRepricingAssessment.RepricingLag = strings.ToLower(strings.TrimSpace(out.MarketRepricingAssessment.RepricingLag))
	if _, ok := validReviewRepricingLag[out.MarketRepricingAssessment.RepricingLag]; !ok {
		out.MarketRepricingAssessment.RepricingLag = "unknown"
	}

	// admin_summary capped at 900 chars (operator-visible body).
	if len(out.AdminSummary) > 900 {
		out.AdminSummary = out.AdminSummary[:900]
	}

	// Reaction-plan validation: every alert_id must reference a
	// real alert the prompt actually contained. AI-invented IDs
	// are dropped silently.
	knownAlerts := make(map[int64]bool, len(req.Alerts))
	for _, a := range req.Alerts {
		knownAlerts[a.ID] = true
	}
	if len(out.ReactionPlan) > 0 {
		filtered := make([]MarketCloseReviewReactionPlan, 0, len(out.ReactionPlan))
		for _, p := range out.ReactionPlan {
			if !knownAlerts[p.AlertID] {
				continue
			}
			p.Reaction = strings.ToLower(strings.TrimSpace(p.Reaction))
			if _, ok := validReviewReactions[p.Reaction]; !ok {
				continue
			}
			filtered = append(filtered, p)
		}
		out.ReactionPlan = filtered
	}

	// Filter best/worst alert IDs to known alerts only.
	out.WatchtowerPerformance.BestAlertIDs = filterKnownAlertIDs(out.WatchtowerPerformance.BestAlertIDs, knownAlerts)
	out.WatchtowerPerformance.WorstAlertIDs = filterKnownAlertIDs(out.WatchtowerPerformance.WorstAlertIDs, knownAlerts)

	return out, nil
}

func filterKnownAlertIDs(in []int64, known map[int64]bool) []int64 {
	out := make([]int64, 0, len(in))
	for _, id := range in {
		if known[id] {
			out = append(out, id)
		}
	}
	return out
}
