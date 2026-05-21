// news_intel.go — v11.0 Hourly News Intelligence OpenAI bridge.
//
// Implements the single AI call the v11.0 product makes: a JSON-mode
// /chat/completions request carrying the v11.0 verbatim prompt
// (HourlyNewsIntelPromptV11) plus the compact list of NEW news items
// with linked affected markets. Output is either a v11.0 sentinel
// (AiAnswered*) or strict JSON matching the documented schema.
package openai

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/infra/aisentinel"
)

// NewsItemForAI is the compact per-item payload the worker hands the
// AI: a stable hash, the human title, optional summary + source +
// timestamp + price-move, and the linked affected markets. Cap-bound:
// the worker is responsible for trimming Title / Summary at the
// boundary; this struct does no truncation of its own.
type NewsItemForAI struct {
	Hash            string
	EventSlug       string
	Title           string
	Summary         string
	Source          string
	Timestamp       time.Time
	PriceBefore     *float64
	PriceAfter      *float64
	PriceChange     *float64
	AffectedMarkets []NewsAffectedMarketForAI
}

// NewsAffectedMarketForAI is the per-market context attached to a
// news item — condition_id, event_slug, market_title — passed
// verbatim to the AI so it can reference exact identifiers in its
// strict-JSON output.
type NewsAffectedMarketForAI struct {
	ConditionID string
	EventSlug   string
	MarketTitle string
}

// NewsIntelAIRequest is the call payload. MaxSelected substitutes the
// {{MAX_SELECTED}} placeholder in the v11.0 prompt.
type NewsIntelAIRequest struct {
	LookbackStart time.Time
	LookbackEnd   time.Time
	MaxSelected   int
	Items         []NewsItemForAI
}

// NewsIntelAIDecision is one row in the AI's `selected` array. The
// shape mirrors the v11.0 JSON schema exactly.
type NewsIntelAIDecision struct {
	NewsItemHash           string   `json:"news_item_hash"`
	EventSlug              string   `json:"event_slug"`
	ConditionID            string   `json:"condition_id"`
	MarketTitle            string   `json:"market_title"`
	Rank                   int      `json:"rank"`
	Confidence             float64  `json:"confidence"`
	ImpactDirection        string   `json:"impact_direction"`
	ExpectedPriceImpactMin *float64 `json:"expected_price_impact_min,omitempty"`
	ExpectedPriceImpactMax *float64 `json:"expected_price_impact_max,omitempty"`
	ExpectedWindow         string   `json:"expected_window"`
	WhyItMatters           string   `json:"why_it_matters"`
	WhatMarketMayMiss      string   `json:"what_market_may_miss"`
	TriggerCondition       string   `json:"trigger_condition"`
	InvalidatesIf          string   `json:"invalidates_if"`
	TradeStance            string   `json:"trade_stance"`
	TelegramWorthy         bool     `json:"telegram_worthy"`
}

// NewsIntelAIResult is the parsed call output.
//
// Sentinel != "" means the AI returned one of the v11.0 sentinels
// (AiAnsweredNotFoundNoticeable / AlreadyPriced / ContextStale /
// InsufficientData / LowConfidenceSkip). In that case Decision /
// Selected are empty and the worker MUST NOT send a Telegram.
//
// Status mirrors the rest of the openai client's status discipline:
//   - "ok"      — clean call (JSON or sentinel), Selected may be empty
//   - "skipped" — rate / budget / no_api_key guard tripped
//   - "failed"  — provider / network / parse error
type NewsIntelAIResult struct {
	Status              string
	Sentinel            string
	Decision            string
	Summary             string
	Selected            []NewsIntelAIDecision
	PromptChars         int
	OutputChars         int
	PromptTokens        int
	CompletionTokens    int
	EstimatedCostUSD    float64
	OutputFingerprintHi string // first hex chars of SHA256(rawText) for audit
	LastError           string
	RawText             string // empty on skipped/failed
}

// validImpactDirections / validExpectedWindows / validTradeStances —
// per the v11.0 schema. Out-of-range values cause the row to be
// dropped (not the whole response) so a single bad enum doesn't waste
// an entire AI call.
var validImpactDirections = map[string]struct{}{
	"YES_up":   {},
	"YES_down": {},
	"NO_up":    {},
	"NO_down":  {},
	"unclear":  {},
}

var validNewsExpectedWindows = map[string]struct{}{
	"2h":       {},
	"12h":      {},
	"3d":       {},
	"catalyst": {},
	"unclear":  {},
}

var validTradeStances = map[string]struct{}{
	"consider": {},
	"watch":    {},
	"avoid":    {},
}

var validNewsDecisions = map[string]struct{}{
	"actionable": {},
	"watch":      {},
	"ignore":     {},
}

// EvaluateHourlyNewsIntel runs the single AI call. Rate / budget /
// no-API-key gates short-circuit to Status="skipped" with no HTTP
// dispatched. Provider errors come back with Status="failed" and a
// short canonical LastError category. Sentinel detection happens
// before JSON parse — a sentinel line short-circuits before any JSON
// validation runs.
func (c *Client) EvaluateHourlyNewsIntel(ctx context.Context, req NewsIntelAIRequest) (NewsIntelAIResult, error) {
	out := NewsIntelAIResult{Status: "skipped"}
	if c.cfg.APIKey == "" {
		out.LastError = "no_api_key"
		return out, nil
	}
	if !c.bucket.allow() {
		out.LastError = "rate_limited"
		return out, nil
	}
	if !c.ledger.allow() {
		out.LastError = "daily_budget_exhausted"
		return out, nil
	}

	userMsg := buildHourlyNewsIntelUserMessage(req)
	out.PromptChars = len(userMsg)

	resp, err := c.callNewsIntelJSON(ctx, userMsg)
	if err != nil {
		pe, ok := AsProviderError(err)
		if !ok {
			pe = &ProviderError{Category: CategoryUnknown, Message: err.Error()}
		}
		out.Status = "failed"
		out.LastError = string(pe.Category)
		return out, err
	}
	cost := c.estimateCost(resp.PromptTokens, resp.CompletionTokens)
	c.ledger.consume(cost)
	out.PromptTokens = resp.PromptTokens
	out.CompletionTokens = resp.CompletionTokens
	out.EstimatedCostUSD = cost
	out.OutputChars = len(resp.Text)
	out.RawText = resp.Text
	out.OutputFingerprintHi = shortFingerprintHex(resp.Text)

	// Sentinel detection — runs BEFORE JSON parse so a sentinel
	// answer is never treated as malformed output.
	trimmed := strings.TrimSpace(resp.Text)
	if code, ok := detectV11Sentinel(trimmed); ok {
		out.Status = "ok"
		out.Sentinel = code
		return out, nil
	}

	parsed, perr := ParseNewsIntelJSON(trimmed)
	if perr != nil {
		out.Status = "failed"
		out.LastError = "invalid_json"
		return out, perr
	}
	out.Status = "ok"
	out.Decision = parsed.Decision
	out.Summary = parsed.Summary
	out.Selected = parsed.Selected
	return out, nil
}

// detectV11Sentinel returns (canonicalPascalCase, true) when the raw
// AI body is exactly one of the v11.0 sentinels. Accepts the
// PascalCase names from the prompt; tolerates surrounding whitespace.
// Also accepts the legacy SCREAMING_SNAKE aliases via aisentinel so
// operators that already wired alerts off the v10.x codes don't break
// on the v11.0 rollout.
func detectV11Sentinel(s string) (string, bool) {
	switch s {
	case "AiAnsweredNotFoundNoticeable",
		"AiAnsweredAlreadyPriced",
		"AiAnsweredContextStale",
		"AiAnsweredInsufficientData",
		"AiAnsweredLowConfidenceSkip":
		return s, true
	}
	if aisentinel.IsKnownSentinel(s) {
		return s, true
	}
	return "", false
}

// ParseNewsIntelJSON validates the strict-JSON body. Out-of-range
// rows are dropped silently (per v11.0 spec — bad enum should not
// nuke the whole call). Returned envelope keeps the AI's summary +
// decision intact even when every row is filtered out.
type parsedNewsIntel struct {
	Decision string
	Summary  string
	Selected []NewsIntelAIDecision
}

func ParseNewsIntelJSON(text string) (parsedNewsIntel, error) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return parsedNewsIntel{}, errors.New("empty body")
	}
	if strings.HasPrefix(trimmed, "```") {
		return parsedNewsIntel{}, errors.New("markdown-wrapped response rejected")
	}
	if trimmed[0] != '{' {
		return parsedNewsIntel{}, errors.New("response is not a JSON object")
	}
	var raw struct {
		Decision string                `json:"decision"`
		Summary  string                `json:"summary"`
		Selected []NewsIntelAIDecision `json:"selected"`
	}
	if err := json.Unmarshal([]byte(trimmed), &raw); err != nil {
		return parsedNewsIntel{}, fmt.Errorf("json unmarshal: %w", err)
	}
	out := parsedNewsIntel{
		Decision: strings.ToLower(strings.TrimSpace(raw.Decision)),
		Summary:  strings.TrimSpace(raw.Summary),
	}
	if _, ok := validNewsDecisions[out.Decision]; !ok && out.Decision != "" {
		out.Decision = "watch"
	}
	for _, d := range raw.Selected {
		clean, ok := validateNewsDecision(d)
		if !ok {
			continue
		}
		out.Selected = append(out.Selected, clean)
	}
	// Stable ordering: lowest rank first, then highest confidence, then
	// alphabetical condition_id so tests are deterministic.
	sort.SliceStable(out.Selected, func(i, j int) bool {
		a, b := out.Selected[i], out.Selected[j]
		if a.Rank != b.Rank {
			return a.Rank < b.Rank
		}
		if a.Confidence != b.Confidence {
			return a.Confidence > b.Confidence
		}
		return a.ConditionID < b.ConditionID
	})
	return out, nil
}

func validateNewsDecision(d NewsIntelAIDecision) (NewsIntelAIDecision, bool) {
	d.NewsItemHash = strings.TrimSpace(d.NewsItemHash)
	d.EventSlug = strings.TrimSpace(d.EventSlug)
	d.ConditionID = strings.TrimSpace(d.ConditionID)
	d.MarketTitle = strings.TrimSpace(d.MarketTitle)
	if d.NewsItemHash == "" {
		return NewsIntelAIDecision{}, false
	}
	if d.EventSlug == "" && d.ConditionID == "" {
		return NewsIntelAIDecision{}, false
	}
	if !(d.Confidence >= 0 && d.Confidence <= 1) {
		if d.Confidence > 1 {
			d.Confidence = 1
		} else {
			d.Confidence = 0
		}
	}
	d.ImpactDirection = strings.TrimSpace(d.ImpactDirection)
	if _, ok := validImpactDirections[d.ImpactDirection]; !ok {
		d.ImpactDirection = "unclear"
	}
	d.ExpectedWindow = strings.TrimSpace(d.ExpectedWindow)
	if _, ok := validNewsExpectedWindows[d.ExpectedWindow]; !ok {
		d.ExpectedWindow = "unclear"
	}
	d.TradeStance = strings.ToLower(strings.TrimSpace(d.TradeStance))
	if _, ok := validTradeStances[d.TradeStance]; !ok {
		d.TradeStance = "watch"
	}
	if d.Rank < 0 {
		d.Rank = 0
	}
	d.WhyItMatters = oneLineCompact(d.WhyItMatters)
	d.WhatMarketMayMiss = oneLineCompact(d.WhatMarketMayMiss)
	d.TriggerCondition = oneLineCompact(d.TriggerCondition)
	d.InvalidatesIf = oneLineCompact(d.InvalidatesIf)
	return d, true
}

// callNewsIntelJSON is the JSON-mode variant of callChat for the
// hourly news intel call. Mirrors the catalyst extractor's pattern;
// keeps the request-shaping local to the only call site.
func (c *Client) callNewsIntelJSON(ctx context.Context, userMsg string) (chatResp, error) {
	body, err := json.Marshal(map[string]any{
		"model": c.cfg.Model,
		"messages": []map[string]string{
			{"role": "system", "content": "You are a strict-JSON prediction-market news analyst. Reply with one JSON object matching the requested schema, OR a single sentinel line. No markdown, no prose outside JSON."},
			{"role": "user", "content": userMsg},
		},
		"max_completion_tokens": 2400,
		"temperature":           0.1,
		"response_format":       map[string]string{"type": "json_object"},
	})
	if err != nil {
		return chatResp{}, fmt.Errorf("marshal news intel request: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return chatResp{}, fmt.Errorf("new news intel request: %w", err)
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

// buildHourlyNewsIntelUserMessage composes the data block + verbatim
// v11.0 prompt. Layout:
//
//	lookback_start: <RFC3339>
//	lookback_end:   <RFC3339>
//	news_items_count: <n>
//
//	News items:
//	- [hash=<short>] <YYYY-MM-DD> | event=<slug> | <title>
//	  summary: <one line>
//	  source: <name>
//	  price: A -> B (delta)
//	  affected: (slug=X, cond=Y) "title"; ...
//
//	---
//	<HourlyNewsIntelPromptV11 with {{MAX_SELECTED}} substituted>
//
// Polymarket-authored strings are treated as DATA — no instruction
// surface. The renderer caps long titles / summaries at 400 chars per
// oneLineCompact to keep the prompt size predictable.
func buildHourlyNewsIntelUserMessage(req NewsIntelAIRequest) string {
	var b strings.Builder
	if !req.LookbackStart.IsZero() {
		fmt.Fprintf(&b, "lookback_start: %s\n", req.LookbackStart.UTC().Format(time.RFC3339))
	}
	if !req.LookbackEnd.IsZero() {
		fmt.Fprintf(&b, "lookback_end:   %s\n", req.LookbackEnd.UTC().Format(time.RFC3339))
	}
	fmt.Fprintf(&b, "news_items_count: %d\n", len(req.Items))

	if len(req.Items) > 0 {
		b.WriteString("\nNew news items:\n")
		for _, it := range req.Items {
			date := "—"
			if !it.Timestamp.IsZero() {
				date = it.Timestamp.UTC().Format("2006-01-02")
			}
			fmt.Fprintf(&b, "- [hash=%s] %s | event=%s | %s\n",
				shortHash(it.Hash), date, it.EventSlug, oneLineCompact(it.Title))
			if it.Summary != "" {
				fmt.Fprintf(&b, "  summary: %s\n", oneLineCompact(it.Summary))
			}
			if it.Source != "" {
				fmt.Fprintf(&b, "  source: %s\n", oneLineCompact(it.Source))
			}
			if it.PriceBefore != nil && it.PriceAfter != nil {
				if it.PriceChange != nil {
					fmt.Fprintf(&b, "  price: %.3f -> %.3f (%+.3f)\n", *it.PriceBefore, *it.PriceAfter, *it.PriceChange)
				} else {
					fmt.Fprintf(&b, "  price: %.3f -> %.3f\n", *it.PriceBefore, *it.PriceAfter)
				}
			} else if it.PriceChange != nil {
				fmt.Fprintf(&b, "  price_change: %+.3f\n", *it.PriceChange)
			}
			if len(it.AffectedMarkets) > 0 {
				b.WriteString("  affected:\n")
				for _, m := range it.AffectedMarkets {
					fmt.Fprintf(&b, "    - cond=%s slug=%s | %s\n",
						m.ConditionID, m.EventSlug, oneLineCompact(m.MarketTitle))
				}
			}
		}
	}

	b.WriteString("\n---\n")
	maxSel := req.MaxSelected
	if maxSel <= 0 {
		maxSel = 8
	}
	b.WriteString(strings.ReplaceAll(HourlyNewsIntelPromptV11, "{{MAX_SELECTED}}", fmt.Sprintf("%d", maxSel)))
	return b.String()
}

// shortHash trims an item_hash to 12 chars for the operator-facing
// prompt label. The full hash still flows through news_item_hash in
// the affected_markets context so the AI can echo the original.
func shortHash(h string) string {
	if len(h) <= 12 {
		return h
	}
	return h[:12]
}

// shortFingerprintHex returns the first 16 hex chars of SHA256(s).
// Used to stamp polymarket_news_intel_runs.output_fingerprint so an
// operator can correlate Telegram messages with DB rows without
// holding the raw body in memory.
func shortFingerprintHex(s string) string {
	if s == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(s))
	h := hex.EncodeToString(sum[:])
	if len(h) > 16 {
		return h[:16]
	}
	return h
}
