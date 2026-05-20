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

	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/analysis"
)

// GenerateDailyPoliticalIntel satisfies
// analysis.DailyPoliticalIntelGenerator. The generator dispatches via
// the existing /chat/completions transport (NOT JSON mode — the
// daily report is free-text Russian).
//
// Failure semantics mirror the other entry points: rate / budget /
// no-key short-circuit to StatusSkipped without an HTTP call;
// provider errors surface with Status=error + categorised LastError.
func (c *Client) GenerateDailyPoliticalIntel(ctx context.Context, req analysis.DailyPoliticalIntelRequest) (analysis.DailyPoliticalIntelResponse, error) {
	zero := analysis.DailyPoliticalIntelResponse{
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

	userMsg := buildDailyPoliticalIntelUserMessage(req)
	httpResp, err := c.callDailyIntel(ctx, userMsg)
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
		return zero, errors.New("empty daily intel body")
	}
	return analysis.DailyPoliticalIntelResponse{
		ReportText:       text,
		Status:           analysis.StatusOK,
		Model:            c.cfg.Model,
		PromptTokens:     httpResp.PromptTokens,
		CompletionTokens: httpResp.CompletionTokens,
		EstimatedCostUSD: cost,
	}, nil
}

// callDailyIntel is a free-text Chat Completions call (no JSON
// mode) with a higher max_completion_tokens budget to accommodate
// the 3000-7000 char report. Kept separate from callChat to avoid
// pulling the alert-prompt budget into this path.
func (c *Client) callDailyIntel(ctx context.Context, userMsg string) (chatResp, error) {
	body, err := json.Marshal(map[string]any{
		"model": c.cfg.Model,
		"messages": []map[string]string{
			{"role": "system", "content": "You are the head of a political/geopolitical prediction-market intelligence desk. Write in Russian. Dense, practical, no filler. Never invent facts."},
			{"role": "user", "content": userMsg},
		},
		"max_completion_tokens": 3500, // ~10k chars head-room
		"temperature":           0.2,
	})
	if err != nil {
		return chatResp{}, fmt.Errorf("marshal daily intel request: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return chatResp{}, fmt.Errorf("new daily intel request: %w", err)
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

// buildDailyPoliticalIntelUserMessage substitutes the five
// placeholders in dailyPoliticalIntelPrompt with structured data
// blocks. The text outside placeholders is verbatim.
func buildDailyPoliticalIntelUserMessage(req analysis.DailyPoliticalIntelRequest) string {
	reportDate := req.ReportDate.UTC().Format("2006-01-02")

	var marketsBuf strings.Builder
	for i, m := range req.Markets {
		drift := ""
		if m.OneDayPriceChange != nil {
			drift = fmt.Sprintf(" | 24h=%+.3f", *m.OneDayPriceChange)
		}
		fmt.Fprintf(&marketsBuf, "%d. %s | category=%s | event=%s | market=%s | price=%.4f%s | vol24h=%.0f | lifecycle=%.1f%% | alerts24h=%d",
			i+1, oneLineCompact(m.Question), nzCompact(m.Category), m.EventSlug,
			nzCompact(m.MarketSlug), m.LastPrice, drift, m.Volume24hUSD,
			m.LifecyclePct, m.AlertsLast24h)
		if m.StrongestSide != "" {
			fmt.Fprintf(&marketsBuf, " | strongest_side=%s", m.StrongestSide)
		}
		if m.ActiveCatalyst != "" {
			fmt.Fprintf(&marketsBuf, " | active_catalyst=%s", oneLineCompact(m.ActiveCatalyst))
		}
		marketsBuf.WriteString("\n")
		for _, a := range m.Annotations {
			date := "—"
			if !a.Timestamp.IsZero() {
				date = a.Timestamp.UTC().Format("2006-01-02")
			}
			fmt.Fprintf(&marketsBuf, "   - %s | outcome=%s | %s",
				date, nzCompact(a.Outcome), oneLineCompact(a.Title))
			if a.PriceBefore != nil && a.PriceAfter != nil {
				fmt.Fprintf(&marketsBuf, " | price %.3f -> %.3f", *a.PriceBefore, *a.PriceAfter)
			}
			if a.PriceChange != nil {
				fmt.Fprintf(&marketsBuf, " (change %+.3f)", *a.PriceChange)
			}
			if a.Summary != "" {
				fmt.Fprintf(&marketsBuf, " | %s", oneLineCompact(a.Summary))
			}
			marketsBuf.WriteString("\n")
		}
	}
	if marketsBuf.Len() == 0 {
		marketsBuf.WriteString("(no markets supplied)\n")
	}

	var flowBuf strings.Builder
	// v9.8: when every field is zero, emit the explicit "no
	// meaningful stored flow" sentence so the AI never confuses
	// silence with weak flow.
	fs := req.FlowSummary
	if fs.RecentAlertsCount == 0 && fs.SameSideNotional24h == 0 && fs.OppositeSideNotional24h == 0 &&
		fs.LargestRecentTradeUSD == 0 && strings.TrimSpace(fs.StrongestSide) == "" &&
		strings.TrimSpace(fs.AccumulationNote) == "" && strings.TrimSpace(fs.OwnershipNote) == "" &&
		strings.TrimSpace(fs.ClusterNote) == "" {
		flowBuf.WriteString("No meaningful stored flow/anomaly data found for the selected markets in the last 24h. ")
		flowBuf.WriteString("Do not infer weak flow from missing data.\n")
	} else {
		fmt.Fprintf(&flowBuf, "recent_alerts_count: %d\n", fs.RecentAlertsCount)
		if fs.StrongestSide != "" {
			fmt.Fprintf(&flowBuf, "strongest_side: %s\n", fs.StrongestSide)
		}
		if fs.SameSideNotional24h > 0 {
			fmt.Fprintf(&flowBuf, "same_side_notional_24h: %.0f\n", fs.SameSideNotional24h)
		}
		if fs.OppositeSideNotional24h > 0 {
			fmt.Fprintf(&flowBuf, "opposite_side_notional_24h: %.0f\n", fs.OppositeSideNotional24h)
		}
		if fs.LargestRecentTradeUSD > 0 {
			fmt.Fprintf(&flowBuf, "largest_recent_trade_usd: %.0f\n", fs.LargestRecentTradeUSD)
		}
		if fs.AccumulationNote != "" {
			fmt.Fprintf(&flowBuf, "accumulation: %s\n", oneLineCompact(fs.AccumulationNote))
		}
		if fs.OwnershipNote != "" {
			fmt.Fprintf(&flowBuf, "ownership: %s\n", oneLineCompact(fs.OwnershipNote))
		}
		if fs.ClusterNote != "" {
			fmt.Fprintf(&flowBuf, "cluster: %s\n", oneLineCompact(fs.ClusterNote))
		}
	}

	var catBuf strings.Builder
	for _, c := range req.KnownCatalysts {
		eta := "tbd"
		if !c.ExpectedAt.IsZero() {
			eta = c.ExpectedAt.UTC().Format(time.RFC3339)
		}
		fmt.Fprintf(&catBuf, "- event=%s | type=%s | status=%s | expected_at=%s | confidence=%.2f | title=%s\n",
			c.EventSlug, c.CatalystType, c.Status, eta, c.Confidence, oneLineCompact(c.Title))
	}
	if catBuf.Len() == 0 {
		catBuf.WriteString("(no catalysts known yet)\n")
	}

	prevText := strings.TrimSpace(req.PreviousReportText)
	if prevText == "" {
		prevText = "(no prior report on file)"
	} else if len(prevText) > 4000 {
		prevText = prevText[:3999] + "…"
	}

	repl := strings.NewReplacer(
		"{{REPORT_DATE}}", reportDate,
		"{{MARKETS_WITH_ANNOTATIONS}}", strings.TrimRight(marketsBuf.String(), "\n"),
		"{{FLOW_SUMMARY}}", strings.TrimRight(flowBuf.String(), "\n"),
		"{{CATALYSTS}}", strings.TrimRight(catBuf.String(), "\n"),
		"{{PREVIOUS_DAILY_REPORT}}", prevText,
	)
	return repl.Replace(dailyPoliticalIntelPrompt)
}
