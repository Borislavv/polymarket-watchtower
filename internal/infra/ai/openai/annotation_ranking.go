package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/analysis"
)

// RankAnnotations satisfies analysis.AnnotationRanker.
//
// The verbatim annotationRankingPrompt is appended after a
// structured data block built from req (period, markets,
// annotations, flow summary). Response is fetched via
// /chat/completions JSON mode and strict-parsed.
//
// Failure semantics mirror ExtractCatalysts: rate / budget /
// no-key short-circuit to StatusSkipped without an HTTP call;
// provider errors surface with Status=error + categorised LastError.
// Markdown-wrapped output is rejected; row-level enum violations
// drop the row but don't fail the call.
func (c *Client) RankAnnotations(ctx context.Context, req analysis.AnnotationRankingRequest) (analysis.AnnotationRankingResponse, error) {
	zero := analysis.AnnotationRankingResponse{
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
	userMsg := buildAnnotationRankingUserMessage(req)
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

	parsed, parseErr := ParseAnnotationRankingJSON(httpResp.Text)
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

// buildAnnotationRankingUserMessage substitutes the four placeholders
// in annotationRankingPrompt with structured data. Stable shape
// makes the model output reproducible across runs.
func buildAnnotationRankingUserMessage(req analysis.AnnotationRankingRequest) string {
	period := "—"
	if !req.PeriodStart.IsZero() && !req.PeriodEnd.IsZero() {
		period = fmt.Sprintf("%s — %s", req.PeriodStart.UTC().Format(time.RFC3339), req.PeriodEnd.UTC().Format(time.RFC3339))
	}
	limit := req.OutputLimit
	if limit <= 0 {
		limit = 10
	}

	var marketsBuf strings.Builder
	for _, m := range req.Markets {
		drift := ""
		if m.OneDayPriceChange != nil {
			drift = fmt.Sprintf(" | 1d=%+.3f", *m.OneDayPriceChange)
		}
		fmt.Fprintf(&marketsBuf, "- event=%s | market=%s | condition=%s | %s | price=%.4f%s | vol24h=%.0f\n",
			m.EventSlug, nzCompact(m.MarketSlug), m.ConditionID,
			oneLineCompact(firstNonEmptyStr(m.GroupItemTitle, m.Question)),
			m.LastPrice, drift, m.Volume24hUSD)
	}
	if marketsBuf.Len() == 0 {
		marketsBuf.WriteString("(no markets supplied)\n")
	}

	var annsBuf strings.Builder
	for _, a := range req.Annotations {
		date := "—"
		if !a.Timestamp.IsZero() {
			date = a.Timestamp.UTC().Format("2006-01-02")
		}
		fmt.Fprintf(&annsBuf, "- hash=%s | event=%s | market=%s | %s | outcome=%s | %s",
			a.AnnotationHash, a.EventSlug, nzCompact(a.MarketSlug),
			date, nzCompact(a.Outcome), oneLineCompact(a.Title))
		if a.PriceBefore != nil && a.PriceAfter != nil {
			fmt.Fprintf(&annsBuf, " | price %.3f -> %.3f", *a.PriceBefore, *a.PriceAfter)
		}
		if a.PriceChange != nil {
			fmt.Fprintf(&annsBuf, " (change %+.3f)", *a.PriceChange)
		}
		if a.Summary != "" {
			fmt.Fprintf(&annsBuf, " | %s", oneLineCompact(a.Summary))
		}
		annsBuf.WriteString("\n")
	}
	if annsBuf.Len() == 0 {
		annsBuf.WriteString("(no annotations supplied)\n")
	}

	var flowBuf strings.Builder
	fmt.Fprintf(&flowBuf, "recent_alerts_count: %d\n", req.FlowSummary.RecentAlertsCount)
	if req.FlowSummary.StrongestSide != "" {
		fmt.Fprintf(&flowBuf, "strongest_side: %s\n", req.FlowSummary.StrongestSide)
	}
	if req.FlowSummary.SameSideNotional24h > 0 {
		fmt.Fprintf(&flowBuf, "same_side_notional_24h: %.0f\n", req.FlowSummary.SameSideNotional24h)
	}
	if req.FlowSummary.OppositeSideNotional24h > 0 {
		fmt.Fprintf(&flowBuf, "opposite_side_notional_24h: %.0f\n", req.FlowSummary.OppositeSideNotional24h)
	}
	if req.FlowSummary.AccumulationNote != "" {
		fmt.Fprintf(&flowBuf, "accumulation: %s\n", oneLineCompact(req.FlowSummary.AccumulationNote))
	}

	repl := strings.NewReplacer(
		"{{OUTPUT_LIMIT}}", strconv.Itoa(limit),
		"{{PERIOD}}", period,
		"{{MARKETS}}", strings.TrimRight(marketsBuf.String(), "\n"),
		"{{ANNOTATIONS}}", strings.TrimRight(annsBuf.String(), "\n"),
		"{{FLOW_SUMMARY}}", strings.TrimRight(flowBuf.String(), "\n"),
	)
	return repl.Replace(annotationRankingPrompt)
}

// --- Strict-JSON parser -------------------------------------------------

var validProbabilityImpact = map[string]struct{}{
	"bullish": {}, "bearish": {}, "mixed": {}, "neutral": {}, "unclear": {},
}

var validMarketRead = map[string]struct{}{
	"underreacting": {}, "overreacting": {}, "already_priced": {},
	"watch": {}, "avoid": {}, "unclear": {},
}

// ParseAnnotationRankingJSON validates and normalises the strict-JSON
// ranking response. Exported so the worker + tests can hit it
// without HTTP.
func ParseAnnotationRankingJSON(text string) (analysis.AnnotationRankingResponse, error) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return analysis.AnnotationRankingResponse{}, errors.New("empty body")
	}
	if strings.HasPrefix(trimmed, "```") {
		return analysis.AnnotationRankingResponse{}, errors.New("markdown-wrapped response rejected")
	}
	if trimmed[0] != '{' {
		return analysis.AnnotationRankingResponse{}, errors.New("response is not a JSON object")
	}
	var raw struct {
		Selected []analysis.SelectedAnnotation `json:"selected"`
	}
	if err := json.Unmarshal([]byte(trimmed), &raw); err != nil {
		return analysis.AnnotationRankingResponse{}, fmt.Errorf("json unmarshal: %w", err)
	}
	out := analysis.AnnotationRankingResponse{}
	for _, s := range raw.Selected {
		clean, ok := validateSelectedAnnotation(s)
		if !ok {
			continue
		}
		out.Selected = append(out.Selected, clean)
	}
	// Stable ordering: rank asc, then importance desc.
	sort.SliceStable(out.Selected, func(i, j int) bool {
		if out.Selected[i].Rank != out.Selected[j].Rank {
			return out.Selected[i].Rank < out.Selected[j].Rank
		}
		return out.Selected[i].Importance > out.Selected[j].Importance
	})
	return out, nil
}

func validateSelectedAnnotation(s analysis.SelectedAnnotation) (analysis.SelectedAnnotation, bool) {
	s.Title = strings.TrimSpace(s.Title)
	if s.Title == "" {
		return analysis.SelectedAnnotation{}, false
	}
	s.EventSlug = strings.TrimSpace(s.EventSlug)
	if s.EventSlug == "" {
		return analysis.SelectedAnnotation{}, false
	}
	s.ProbabilityImpact = strings.ToLower(strings.TrimSpace(s.ProbabilityImpact))
	if _, ok := validProbabilityImpact[s.ProbabilityImpact]; !ok {
		s.ProbabilityImpact = "unclear"
	}
	s.MarketRead = strings.ToLower(strings.TrimSpace(s.MarketRead))
	if _, ok := validMarketRead[s.MarketRead]; !ok {
		s.MarketRead = "unclear"
	}
	if !(s.Importance >= 0 && s.Importance <= 1) {
		if s.Importance > 1 {
			s.Importance = 1
		} else {
			s.Importance = 0
		}
	}
	if !(s.VolatilityPotential >= 0 && s.VolatilityPotential <= 1) {
		if s.VolatilityPotential > 1 {
			s.VolatilityPotential = 1
		} else {
			s.VolatilityPotential = 0
		}
	}
	s.Reason = strings.TrimSpace(s.Reason)
	if s.MarketSlug != nil && strings.TrimSpace(*s.MarketSlug) == "" {
		s.MarketSlug = nil
	}
	if s.AffectedOutcome != nil && strings.TrimSpace(*s.AffectedOutcome) == "" {
		s.AffectedOutcome = nil
	}
	if s.Rank <= 0 {
		s.Rank = 0
	}
	return s, true
}
