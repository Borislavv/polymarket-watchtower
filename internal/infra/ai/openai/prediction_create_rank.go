package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/analysis"
)

// RankCandidates satisfies analysis.PredictionRanker. Stage 1 of the
// prediction-creation AI pipeline.
//
// Failure semantics mirror the other v9.x AI methods on this client:
// no key / rate / daily budget short-circuit to StatusSkipped without
// an HTTP call; provider errors surface with Status=error +
// categorised LastError; markdown-wrapped output rejected.
func (c *Client) RankCandidates(ctx context.Context, req analysis.PredictionRankingRequest) (analysis.PredictionRankingResponse, error) {
	zero := analysis.PredictionRankingResponse{Status: analysis.StatusError, Model: c.cfg.Model}
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
	if len(req.Candidates) == 0 {
		zero.Status = analysis.StatusSkipped
		zero.LastError = "no_candidates"
		return zero, nil
	}
	maxSel := req.MaxSelected
	if maxSel <= 0 {
		maxSel = 10
	}
	userMsg := buildPredictionRankingUserMessage(req, maxSel)
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
	parsed, parseErr := ParsePredictionRankingJSON(httpResp.Text)
	parsed.Model = c.cfg.Model
	parsed.PromptTokens = httpResp.PromptTokens
	parsed.CompletionTokens = httpResp.CompletionTokens
	parsed.EstimatedCostUSD = cost
	if parseErr != nil {
		parsed.Status = analysis.StatusError
		parsed.LastError = "invalid_json"
		return parsed, parseErr
	}
	// Cross-check picks against input slugs — the model is instructed
	// not to invent, but the worker is the source of truth.
	allowed := make(map[string]string, len(req.Candidates))
	for _, c := range req.Candidates {
		allowed[c.EventSlug] = c.ConditionID
	}
	filtered := parsed.Picks[:0]
	for _, p := range parsed.Picks {
		cid, ok := allowed[p.EventSlug]
		if !ok {
			continue
		}
		if p.ConditionID == "" {
			p.ConditionID = cid
		}
		filtered = append(filtered, p)
	}
	parsed.Picks = filtered
	parsed.Status = analysis.StatusOK
	return parsed, nil
}

// buildPredictionRankingUserMessage renders the candidates block +
// substitutes placeholders in predictionRankingPrompt.
func buildPredictionRankingUserMessage(req analysis.PredictionRankingRequest, maxSel int) string {
	now := req.AnalysisTimeUTC
	if now.IsZero() {
		now = time.Now().UTC()
	}
	var b strings.Builder
	for i, c := range req.Candidates {
		fmt.Fprintf(&b, "%d. event_slug=%s condition_id=%s outcome=%q price=%.3f 1d=%+.3f 1w=%+.3f lifecycle=%.0f%% alerts24h=%d catalysts=%d annotations24h=%d strongest_side=%s skew=%+.2f vol24h=%.0f liquidity=%.0f baseline_median_usd=%.0f category=%s\n   q=%s\n",
			i+1, c.EventSlug, c.ConditionID, c.Outcome,
			c.LastTradePrice, c.OneDayPriceChange, c.OneWeekPriceChange,
			c.LifecyclePct, c.RecentAlerts24h, c.OpenCatalysts, c.NewAnnotations24h,
			nzStr(c.StrongestSide), c.DirectionalSkew,
			c.VolumeUSD24h, c.LiquidityUSD, c.BaselineMedianUSD,
			nzStr(c.Category), oneLineCompact(c.Question))
	}
	if b.Len() == 0 {
		b.WriteString("(no candidates supplied)\n")
	}
	repl := strings.NewReplacer(
		"{{MAX_SELECTED}}", fmt.Sprintf("%d", maxSel),
		"{{ANALYSIS_TIME}}", now.Format(time.RFC3339),
		"{{CANDIDATES}}", strings.TrimRight(b.String(), "\n"),
	)
	return repl.Replace(predictionRankingPrompt)
}

// ParsePredictionRankingJSON validates and normalises the strict-JSON
// ranking response. Exported so the worker and tests exercise the
// same path without making an HTTP call.
func ParsePredictionRankingJSON(text string) (analysis.PredictionRankingResponse, error) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return analysis.PredictionRankingResponse{}, errors.New("empty body")
	}
	if strings.HasPrefix(trimmed, "```") {
		return analysis.PredictionRankingResponse{}, errors.New("markdown-wrapped response rejected")
	}
	if trimmed[0] != '{' {
		return analysis.PredictionRankingResponse{}, errors.New("response is not a JSON object")
	}
	var raw struct {
		Picks []analysis.PredictionRankingPick `json:"picks"`
	}
	if err := json.Unmarshal([]byte(trimmed), &raw); err != nil {
		return analysis.PredictionRankingResponse{}, fmt.Errorf("json unmarshal: %w", err)
	}
	out := analysis.PredictionRankingResponse{}
	for _, p := range raw.Picks {
		p.EventSlug = strings.TrimSpace(p.EventSlug)
		p.Reason = strings.TrimSpace(p.Reason)
		if p.EventSlug == "" {
			continue
		}
		if p.Score < 0 {
			p.Score = 0
		}
		if p.Score > 1 {
			p.Score = 1
		}
		out.Picks = append(out.Picks, p)
	}
	sort.SliceStable(out.Picks, func(i, j int) bool { return out.Picks[i].Score > out.Picks[j].Score })
	return out, nil
}

func nzStr(s string) string {
	if s == "" {
		return "—"
	}
	return s
}
