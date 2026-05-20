package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/analysis"
)

// ExtractCatalysts satisfies analysis.CatalystExtractor. It builds
// the structured data block from the request, prepends it to the
// EXACT catalyst-extraction prompt (PART 4), dispatches via
// /chat/completions with response_format=json_object, and parses
// the strict-JSON response.
//
// Failure semantics mirror AnalyzeAlert: provider errors surface as
// CatalystExtractionResponse{Status: StatusError, LastError: <category>}
// + a non-nil error so the importer can request_log it. Rate/budget
// gates short-circuit to StatusSkipped without an HTTP call.
//
// The strict-JSON parser:
//   - rejects markdown-wrapped output (```json ... ```);
//   - validates enum membership for catalyst_type, status, source;
//   - clamps confidence into [0,1];
//   - drops rows below MinCatalystConfidence (caller supplies via the
//     importer config; this method enforces a 0.0 floor only —
//     downstream filtering happens after parse);
//   - normalises expected_at to UTC RFC3339;
//   - drops rows with empty titles.
func (c *Client) ExtractCatalysts(ctx context.Context, req analysis.CatalystExtractionRequest) (analysis.CatalystExtractionResponse, error) {
	zero := analysis.CatalystExtractionResponse{
		EventSlug:       req.EventSlug,
		AnalysisTimeUTC: utcNow().Format(time.RFC3339),
		Status:          analysis.StatusError,
		Model:           c.cfg.Model,
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

	userMsg := buildCatalystExtractionUserMessage(req)
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

	parsed, parseErr := ParseCatalystExtractionJSON(httpResp.Text, req.EventSlug)
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

// callChatJSON is a stripped-down variant of callChat that asks
// OpenAI for JSON-mode output. We keep it inside this file (rather
// than the main client) so the JSON contract stays close to the
// only call site that uses it.
func (c *Client) callChatJSON(ctx context.Context, userMsg string) (chatResp, error) {
	body, err := json.Marshal(map[string]any{
		"model": c.cfg.Model,
		"messages": []map[string]string{
			{"role": "system", "content": "You are a strict-JSON political-risk catalyst extractor. Reply with one JSON object that matches the requested schema. No markdown, no prose outside JSON."},
			{"role": "user", "content": userMsg},
		},
		"max_completion_tokens": 1600,
		"temperature":           0.1,
		"response_format":       map[string]string{"type": "json_object"},
	})
	if err != nil {
		return chatResp{}, fmt.Errorf("marshal catalyst request: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return chatResp{}, fmt.Errorf("new catalyst request: %w", err)
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

// utcNow is a shim so tests can stub time.Now via Client.cfg.Clock
// in the future; today the extractor just uses time.Now.
func utcNow() time.Time { return time.Now().UTC() }

// --- Prompt builder -----------------------------------------------------

// buildCatalystExtractionUserMessage prepends a structured data block
// to the verbatim PART 4 prompt. The data block uses stable
// `key: value` + bulleted formatting so the model parses reliably.
func buildCatalystExtractionUserMessage(req analysis.CatalystExtractionRequest) string {
	var b strings.Builder
	fmt.Fprintf(&b, "event_slug: %s\n", req.EventSlug)
	if !req.AnalysisTimeUTC.IsZero() {
		fmt.Fprintf(&b, "analysis_time_utc: %s\n", req.AnalysisTimeUTC.UTC().Format(time.RFC3339))
	}

	b.WriteString("\nEvent metadata:\n")
	writeKV(&b, "title", req.EventMetadata.Title)
	writeKV(&b, "category", req.EventMetadata.Category)
	if !req.EventMetadata.StartDate.IsZero() {
		writeKV(&b, "start_date", req.EventMetadata.StartDate.UTC().Format(time.RFC3339))
	}
	if !req.EventMetadata.EndDate.IsZero() {
		writeKV(&b, "end_date", req.EventMetadata.EndDate.UTC().Format(time.RFC3339))
	}
	writeKV(&b, "description", req.EventMetadata.Description)
	writeKV(&b, "resolution_rules", req.EventMetadata.ResolutionRules)
	writeKV(&b, "context_description", req.EventMetadata.ContextDescription)
	if !req.EventMetadata.ContextUpdatedAt.IsZero() {
		writeKV(&b, "context_updated_at", req.EventMetadata.ContextUpdatedAt.UTC().Format(time.RFC3339))
	}

	if len(req.Markets) > 0 {
		b.WriteString("\nMarkets:\n")
		for _, m := range req.Markets {
			price := ""
			if len(m.OutcomePrices) > 0 {
				price = m.OutcomePrices[0]
			}
			label := m.GroupItemTitle
			if label == "" {
				label = m.Question
			}
			fmt.Fprintf(&b, "- condition_id=%s | %s | price=%s | vol24h=%.0f | liquidity=%.0f | active=%t | closed=%t",
				m.ConditionID, oneLineCompact(label), price, m.Volume24hUSD, m.Liquidity, m.Active, m.Closed)
			if m.OneDayPriceChange != nil {
				fmt.Fprintf(&b, " | 1d=%+.3f", *m.OneDayPriceChange)
			}
			if m.OneWeekPriceChange != nil {
				fmt.Fprintf(&b, " | 1w=%+.3f", *m.OneWeekPriceChange)
			}
			if !m.EndDate.IsZero() {
				fmt.Fprintf(&b, " | endDate=%s", m.EndDate.UTC().Format("2006-01-02"))
			}
			b.WriteString("\n")
		}
	}

	if len(req.Annotations) > 0 {
		b.WriteString("\nPolymarket event annotations:\n")
		for _, a := range req.Annotations {
			date := "—"
			if !a.Timestamp.IsZero() {
				date = a.Timestamp.UTC().Format("2006-01-02")
			}
			fmt.Fprintf(&b, "- %s | outcome=%s | %s",
				date, nzCompact(a.Outcome), oneLineCompact(a.Title))
			if a.PriceBefore != nil && a.PriceAfter != nil {
				fmt.Fprintf(&b, " | price %.3f -> %.3f", *a.PriceBefore, *a.PriceAfter)
				if a.PriceChange != nil {
					fmt.Fprintf(&b, " (%+.3f)", *a.PriceChange)
				}
			} else if a.PriceChange != nil {
				fmt.Fprintf(&b, " | priceChange %+.3f", *a.PriceChange)
			}
			if a.Summary != "" {
				fmt.Fprintf(&b, " | %s", oneLineCompact(a.Summary))
			}
			if len(a.SourceNames) > 0 {
				fmt.Fprintf(&b, " | sources=%s", strings.Join(a.SourceNames, ", "))
			}
			b.WriteString("\n")
		}
	}

	b.WriteString("\nRecent Watchtower flow/anomaly summary:\n")
	fmt.Fprintf(&b, "recent_alerts_count: %d\n", req.FlowSummary.RecentAlertsCount)
	if req.FlowSummary.StrongestSide != "" {
		writeKV(&b, "strongest_side", req.FlowSummary.StrongestSide)
	}
	writeKV(&b, "accumulation", req.FlowSummary.AccumulationNote)
	writeKV(&b, "ownership", req.FlowSummary.OwnershipNote)
	writeKV(&b, "cluster", req.FlowSummary.ClusterNote)
	if req.FlowSummary.SameSideNotional24h > 0 {
		fmt.Fprintf(&b, "same_side_notional_24h: %.0f\n", req.FlowSummary.SameSideNotional24h)
	}
	if req.FlowSummary.OppositeSideNotional24h > 0 {
		fmt.Fprintf(&b, "opposite_side_notional_24h: %.0f\n", req.FlowSummary.OppositeSideNotional24h)
	}
	if req.FlowSummary.LargestRecentTradeUSD > 0 {
		fmt.Fprintf(&b, "largest_recent_trade_usd: %.0f\n", req.FlowSummary.LargestRecentTradeUSD)
	}

	if len(req.ExistingCatalysts) > 0 {
		b.WriteString("\nExisting catalysts already known to the system:\n")
		for _, c := range req.ExistingCatalysts {
			eta := "tbd"
			if !c.ExpectedAt.IsZero() {
				eta = c.ExpectedAt.UTC().Format(time.RFC3339)
			}
			fmt.Fprintf(&b, "- type=%s | status=%s | expected_at=%s | confidence=%.2f | title=%s\n",
				c.CatalystType, c.Status, eta, c.Confidence, oneLineCompact(c.Title))
		}
	}

	b.WriteString("\n---\n")
	b.WriteString(catalystExtractionPrompt)
	return b.String()
}

// --- Strict-JSON parser -------------------------------------------------

// validCatalystTypes mirrors the open enum from PART 4 schema. The
// importer rejects rows with values outside this set.
var validCatalystTypes = map[string]struct{}{
	"poll":               {},
	"debate":             {},
	"runoff":             {},
	"primary":            {},
	"endorsement":        {},
	"certification":      {},
	"recount":            {},
	"court_ruling":       {},
	"sanctions":          {},
	"negotiation":        {},
	"ceasefire":          {},
	"filing_deadline":    {},
	"geopolitical_event": {},
	"official_statement": {},
	"election_day":       {},
	"other":              {},
}

var validCatalystStatuses = map[string]struct{}{
	"expected":    {},
	"active":      {},
	"resolved":    {},
	"stale":       {},
	"invalidated": {},
}

var validCatalystSources = map[string]struct{}{
	"polymarket_annotation": {},
	"event_metadata":        {},
	"web_news":              {},
	"resolution_rules":      {},
	"watchtower_flow":       {},
	"existing_catalyst":     {},
	"mixed":                 {},
}

// ParseCatalystExtractionJSON validates and normalises the strict
// JSON response. Exported so the importer + tests can hit it
// without going through HTTP. Returns the (possibly partially
// filtered) response plus an error when the wire shape is invalid.
//
// Filtering vs error:
//   - wire-level invalid (markdown wrap, non-object, junk before JSON):
//     returns error; caller treats as "no catalysts this cycle".
//   - row-level invalid (bad enum, empty title, NaN confidence):
//     row is dropped from the result but parsing continues.
func ParseCatalystExtractionJSON(text, expectedSlug string) (analysis.CatalystExtractionResponse, error) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return analysis.CatalystExtractionResponse{EventSlug: expectedSlug}, errors.New("empty body")
	}
	if strings.HasPrefix(trimmed, "```") {
		return analysis.CatalystExtractionResponse{EventSlug: expectedSlug}, errors.New("markdown-wrapped response rejected")
	}
	if trimmed[0] != '{' {
		return analysis.CatalystExtractionResponse{EventSlug: expectedSlug}, errors.New("response is not a JSON object")
	}
	var raw struct {
		EventSlug       string                       `json:"event_slug"`
		AnalysisTimeUTC string                       `json:"analysis_time_utc"`
		Catalysts       []analysis.ExtractedCatalyst `json:"catalysts"`
	}
	if err := json.Unmarshal([]byte(trimmed), &raw); err != nil {
		return analysis.CatalystExtractionResponse{EventSlug: expectedSlug}, fmt.Errorf("json unmarshal: %w", err)
	}
	out := analysis.CatalystExtractionResponse{
		EventSlug:       firstNonEmptyStr(raw.EventSlug, expectedSlug),
		AnalysisTimeUTC: raw.AnalysisTimeUTC,
	}
	for _, c := range raw.Catalysts {
		clean, ok := validateExtractedCatalyst(c)
		if !ok {
			continue
		}
		out.Catalysts = append(out.Catalysts, clean)
	}
	// Stable ordering: highest confidence first, then by title.
	sort.SliceStable(out.Catalysts, func(i, j int) bool {
		if out.Catalysts[i].Confidence != out.Catalysts[j].Confidence {
			return out.Catalysts[i].Confidence > out.Catalysts[j].Confidence
		}
		return out.Catalysts[i].Title < out.Catalysts[j].Title
	})
	return out, nil
}

// validateExtractedCatalyst applies the row-level rules + light
// normalisation. Returns (normalised, true) on accept, (zero, false)
// on reject.
func validateExtractedCatalyst(c analysis.ExtractedCatalyst) (analysis.ExtractedCatalyst, bool) {
	c.Title = strings.TrimSpace(c.Title)
	if c.Title == "" {
		return analysis.ExtractedCatalyst{}, false
	}
	c.CatalystType = strings.ToLower(strings.TrimSpace(c.CatalystType))
	if _, ok := validCatalystTypes[c.CatalystType]; !ok {
		return analysis.ExtractedCatalyst{}, false
	}
	c.Status = strings.ToLower(strings.TrimSpace(c.Status))
	if c.Status == "" {
		c.Status = "expected"
	}
	if _, ok := validCatalystStatuses[c.Status]; !ok {
		return analysis.ExtractedCatalyst{}, false
	}
	c.Source = strings.ToLower(strings.TrimSpace(c.Source))
	if c.Source != "" {
		if _, ok := validCatalystSources[c.Source]; !ok {
			c.Source = "mixed"
		}
	}
	// Clamp confidence — also catches NaN via the inequality.
	if !(c.Confidence >= 0 && c.Confidence <= 1) {
		switch {
		case c.Confidence > 1:
			c.Confidence = 1
		default:
			c.Confidence = 0
		}
	}
	// Normalise expected_at to UTC RFC3339 string. Reject obviously
	// invalid values (unparseable, or pre-2000 / post-2100).
	if c.ExpectedAt != nil {
		s := strings.TrimSpace(*c.ExpectedAt)
		if s == "" || strings.EqualFold(s, "null") {
			c.ExpectedAt = nil
		} else {
			t, err := parseRFC3339Loose(s)
			if err != nil || t.Year() < 2000 || t.Year() > 2100 {
				c.ExpectedAt = nil
			} else {
				out := t.UTC().Format(time.RFC3339)
				c.ExpectedAt = &out
			}
		}
	}
	c.Description = strings.TrimSpace(c.Description)
	c.BullishScenario = strings.TrimSpace(c.BullishScenario)
	c.BearishScenario = strings.TrimSpace(c.BearishScenario)
	c.InvalidationScenario = strings.TrimSpace(c.InvalidationScenario)
	c.FlowInterpretation = strings.TrimSpace(c.FlowInterpretation)
	return c, true
}

func parseRFC3339Loose(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if t, err := time.Parse("2006-01-02T15:04:05", s); err == nil {
		return t, nil
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("parse time %q", s)
}

func writeKV(b *strings.Builder, k, v string) {
	v = strings.TrimSpace(v)
	if v == "" {
		return
	}
	fmt.Fprintf(b, "%s: %s\n", k, oneLineCompact(v))
}

func oneLineCompact(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.TrimSpace(s)
	if len(s) > 400 {
		s = s[:399] + "…"
	}
	return s
}

func nzCompact(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}

func firstNonEmptyStr(opts ...string) string {
	for _, o := range opts {
		if strings.TrimSpace(o) != "" {
			return o
		}
	}
	return ""
}
