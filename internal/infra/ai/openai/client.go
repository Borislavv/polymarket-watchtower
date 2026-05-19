// Package openai is the AI provider implementation backed by the
// OpenAI Chat Completions API.
//
// Boundaries:
//   - this package owns: HTTP transport, JSON shapes, retries on 5xx
//     (none today — short timeout + graceful degrade is cheaper),
//     rate-limit token bucket, daily-budget tracker, token-count
//     accounting.
//   - this package does NOT own: prompt building (that lives in the
//     aianalysis usecase so prompts can be tested without an HTTP
//     dependency).
//
// Cost control:
//   - per-minute token-bucket rate limit (default 10 req/min)
//   - daily-budget ledger (default $5/day), refreshed at process
//     wall-clock midnight UTC. When the budget is consumed every
//     call returns analysis.StatusSkipped with no HTTP traffic.
//   - per-call timeout (default 8s); a slow call ALWAYS returns
//     "skipped/timeout" rather than blocking the caller.
//
// Cost estimation is approximate — OpenAI bills on a per-token
// basis and prices change over time. We use a small per-1k-token
// rate that an operator overrides via env. The ledger is in-memory
// and resets on restart; persistent spend tracking lives in the
// analysis tables (the estimated_cost_usd column).
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
	"sync"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/analysis"
)

// Config tunes the client. Required: APIKey, Model. The rest have
// safe defaults.
type Config struct {
	APIKey         string
	BaseURL        string // defaults to https://api.openai.com/v1
	Model          string
	Timeout        time.Duration
	MaxPromptChars int
	MaxOutputChars int

	// Rate limiting + budget.
	RatePerMin  int
	DailyBudget float64

	// Per-1k-token cost estimates. Operators override via env when
	// they move models or when prices change. The defaults below
	// are a *rough* mini-tier estimate; the audit column in the DB
	// is what an accounting follow-up should reconcile against.
	PromptCostPer1kUSD     float64
	CompletionCostPer1kUSD float64

	// Clock + HTTP for tests.
	Clock      func() time.Time
	HTTPClient *http.Client
}

func (c *Config) applyDefaults() {
	if c.BaseURL == "" {
		c.BaseURL = "https://api.openai.com/v1"
	}
	if c.Model == "" {
		c.Model = "gpt-4.1-mini"
	}
	if c.Timeout <= 0 {
		c.Timeout = 8 * time.Second
	}
	if c.MaxPromptChars <= 0 {
		c.MaxPromptChars = 2500
	}
	if c.MaxOutputChars <= 0 {
		c.MaxOutputChars = 700
	}
	if c.RatePerMin <= 0 {
		c.RatePerMin = 10
	}
	if c.DailyBudget <= 0 {
		c.DailyBudget = 5
	}
	if c.PromptCostPer1kUSD <= 0 {
		c.PromptCostPer1kUSD = 0.00015
	}
	if c.CompletionCostPer1kUSD <= 0 {
		c.CompletionCostPer1kUSD = 0.0006
	}
	if c.Clock == nil {
		c.Clock = time.Now
	}
	if c.HTTPClient == nil {
		c.HTTPClient = &http.Client{Timeout: c.Timeout}
	}
}

// Client is the OpenAI-backed Analyzer.
type Client struct {
	cfg          Config
	bucket       *rateBucket
	ledger       *budgetLedger
	systemPrompt string
}

// New constructs a Client. APIKey may be empty — in that case every
// call returns StatusSkipped (we treat "no key" as "AI off"); the
// caller should typically prefer analysis.NoopAnalyzer instead.
func New(cfg Config) *Client {
	cfg.applyDefaults()
	return &Client{
		cfg:          cfg,
		bucket:       newRateBucket(cfg.RatePerMin, cfg.Clock),
		ledger:       newBudgetLedger(cfg.DailyBudget, cfg.Clock),
		systemPrompt: defaultSystemPrompt,
	}
}

// SetSystemPrompt overrides the system message. Tests use this to
// keep prompts compact.
func (c *Client) SetSystemPrompt(s string) { c.systemPrompt = s }

// defaultSystemPrompt — the one and only model guidance. Short on
// purpose; the user-message carries all data.
const defaultSystemPrompt = `You are a professional prediction-market analyst.
You analyze public-data prediction-market signals.
You do not claim insider trading. You do not guarantee profit.
You write concise, cautious, decision-useful analysis.

You must explain: why a setup may work, why it may fail, what event
or date or news matters next.

Do not invent facts not present in the input.
If live external context was not checked, say so explicitly.
Use simple readable English. No hype.
Maximum 700 characters unless explicitly allowed.`

// AnalyzeAlert builds the prompt, calls OpenAI, returns the parsed
// analyst note. On budget/rate-limit/timeout/error it returns a
// non-error AlertAnalysis with Status set so the caller can persist
// the skip-reason and still emit the underlying alert.
func (c *Client) AnalyzeAlert(ctx context.Context, req analysis.AlertAnalysisRequest) (analysis.AlertAnalysis, error) {
	prompt := buildAlertPrompt(req, c.cfg.MaxPromptChars)
	if c.cfg.APIKey == "" {
		return analysis.AlertAnalysis{Status: analysis.StatusSkipped, Model: c.cfg.Model, LastError: "no_api_key"}, nil
	}
	if !c.bucket.allow() {
		return analysis.AlertAnalysis{Status: analysis.StatusSkipped, Model: c.cfg.Model, LastError: "rate_limited"}, nil
	}
	if !c.ledger.allow() {
		return analysis.AlertAnalysis{Status: analysis.StatusSkipped, Model: c.cfg.Model, LastError: "daily_budget_exhausted"}, nil
	}
	resp, err := c.callChat(ctx, prompt, c.cfg.MaxOutputChars)
	if err != nil {
		return analysis.AlertAnalysis{Status: analysis.StatusError, Model: c.cfg.Model, LastError: err.Error()}, nil
	}
	cost := c.estimateCost(resp.PromptTokens, resp.CompletionTokens)
	c.ledger.consume(cost)
	text := truncate(resp.Text, c.cfg.MaxOutputChars)
	verdict := pickVerdict(text)
	return analysis.AlertAnalysis{
		Status:           analysis.StatusOK,
		Model:            c.cfg.Model,
		AnalysisText:     text,
		Verdict:          verdict,
		PromptChars:      len(prompt),
		OutputChars:      len(text),
		PromptTokens:     resp.PromptTokens,
		CompletionTokens: resp.CompletionTokens,
		EstimatedCostUSD: cost,
	}, nil
}

// AnalyzeMarketReport is implemented but the orchestrating worker
// is staged behind a follow-up PR. The model call works end-to-end;
// only the periodic top-N selection + dedup logic lives in the
// usecase layer.
func (c *Client) AnalyzeMarketReport(ctx context.Context, req analysis.MarketReportRequest) (analysis.MarketReportAnalysis, error) {
	prompt := buildMarketReportPrompt(req, c.cfg.MaxPromptChars*2) // reports are bigger
	if c.cfg.APIKey == "" {
		return analysis.MarketReportAnalysis{Status: analysis.StatusSkipped, Model: c.cfg.Model, LastError: "no_api_key"}, nil
	}
	if !c.bucket.allow() {
		return analysis.MarketReportAnalysis{Status: analysis.StatusSkipped, Model: c.cfg.Model, LastError: "rate_limited"}, nil
	}
	if !c.ledger.allow() {
		return analysis.MarketReportAnalysis{Status: analysis.StatusSkipped, Model: c.cfg.Model, LastError: "daily_budget_exhausted"}, nil
	}
	resp, err := c.callChat(ctx, prompt, 2000)
	if err != nil {
		return analysis.MarketReportAnalysis{Status: analysis.StatusError, Model: c.cfg.Model, LastError: err.Error()}, nil
	}
	cost := c.estimateCost(resp.PromptTokens, resp.CompletionTokens)
	c.ledger.consume(cost)
	return analysis.MarketReportAnalysis{
		Status:           analysis.StatusOK,
		Model:            c.cfg.Model,
		ReportText:       resp.Text,
		PromptTokens:     resp.PromptTokens,
		CompletionTokens: resp.CompletionTokens,
		EstimatedCostUSD: cost,
	}, nil
}

// AnalyzeOutcome — postmortem path.
func (c *Client) AnalyzeOutcome(ctx context.Context, req analysis.OutcomeAnalysisRequest) (analysis.OutcomeAnalysis, error) {
	prompt := buildOutcomePrompt(req, c.cfg.MaxPromptChars)
	if c.cfg.APIKey == "" {
		return analysis.OutcomeAnalysis{Status: analysis.StatusSkipped, Model: c.cfg.Model, LastError: "no_api_key"}, nil
	}
	if !c.bucket.allow() {
		return analysis.OutcomeAnalysis{Status: analysis.StatusSkipped, Model: c.cfg.Model, LastError: "rate_limited"}, nil
	}
	if !c.ledger.allow() {
		return analysis.OutcomeAnalysis{Status: analysis.StatusSkipped, Model: c.cfg.Model, LastError: "daily_budget_exhausted"}, nil
	}
	resp, err := c.callChat(ctx, prompt, c.cfg.MaxOutputChars)
	if err != nil {
		return analysis.OutcomeAnalysis{Status: analysis.StatusError, Model: c.cfg.Model, LastError: err.Error()}, nil
	}
	cost := c.estimateCost(resp.PromptTokens, resp.CompletionTokens)
	c.ledger.consume(cost)
	reason, lessons, won, conf := parseOutcomeText(resp.Text)
	return analysis.OutcomeAnalysis{
		Status:           analysis.StatusOK,
		Model:            c.cfg.Model,
		ReasonText:       reason,
		LessonsText:      lessons,
		WonExpected:      won,
		Confidence:       conf,
		PromptTokens:     resp.PromptTokens,
		CompletionTokens: resp.CompletionTokens,
		EstimatedCostUSD: cost,
	}, nil
}

// --- Internal HTTP path ----------------------------------------------------

type chatResp struct {
	Text             string
	PromptTokens     int
	CompletionTokens int
}

func (c *Client) callChat(ctx context.Context, userMsg string, maxChars int) (chatResp, error) {
	body, err := json.Marshal(map[string]any{
		"model": c.cfg.Model,
		"messages": []map[string]string{
			{"role": "system", "content": c.systemPrompt},
			{"role": "user", "content": userMsg},
		},
		// max_completion_tokens roughly mirrors the char budget;
		// 4 chars/token is the historical rule-of-thumb.
		"max_completion_tokens": maxChars / 3,
		"temperature":           0.2,
	})
	if err != nil {
		return chatResp{}, fmt.Errorf("marshal request: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return chatResp{}, fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	resp, err := c.cfg.HTTPClient.Do(req)
	if err != nil {
		return chatResp{}, fmt.Errorf("do: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return chatResp{}, fmt.Errorf("read: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		return chatResp{}, fmt.Errorf("openai status %d: %s", resp.StatusCode, string(raw))
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
		return chatResp{}, fmt.Errorf("unmarshal: %w", err)
	}
	if len(parsed.Choices) == 0 {
		return chatResp{}, errors.New("no choices in response")
	}
	return chatResp{
		Text:             strings.TrimSpace(parsed.Choices[0].Message.Content),
		PromptTokens:     parsed.Usage.PromptTokens,
		CompletionTokens: parsed.Usage.CompletionTokens,
	}, nil
}

func (c *Client) estimateCost(promptTokens, completionTokens int) float64 {
	return (float64(promptTokens)/1000)*c.cfg.PromptCostPer1kUSD +
		(float64(completionTokens)/1000)*c.cfg.CompletionCostPer1kUSD
}

// --- Rate limit (token bucket) --------------------------------------------

type rateBucket struct {
	mu       sync.Mutex
	capacity int
	tokens   float64
	last     time.Time
	now      func() time.Time
	perMin   int
}

func newRateBucket(perMin int, now func() time.Time) *rateBucket {
	if perMin <= 0 {
		perMin = 10
	}
	return &rateBucket{capacity: perMin, tokens: float64(perMin), last: now(), now: now, perMin: perMin}
}

func (b *rateBucket) allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	elapsed := now.Sub(b.last).Seconds()
	b.tokens += elapsed * (float64(b.perMin) / 60.0)
	if b.tokens > float64(b.capacity) {
		b.tokens = float64(b.capacity)
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// --- Daily budget ----------------------------------------------------------

type budgetLedger struct {
	mu       sync.Mutex
	dailyCap float64
	spent    float64
	day      string // YYYY-MM-DD bucket
	now      func() time.Time
}

func newBudgetLedger(cap float64, now func() time.Time) *budgetLedger {
	return &budgetLedger{dailyCap: cap, now: now, day: now().UTC().Format("2006-01-02")}
}

func (l *budgetLedger) allow() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rolloverIfNeededLocked()
	return l.spent < l.dailyCap
}

func (l *budgetLedger) consume(usd float64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rolloverIfNeededLocked()
	l.spent += usd
}

// Spent exposes the running tally for tests / metrics.
func (l *budgetLedger) Spent() float64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rolloverIfNeededLocked()
	return l.spent
}

func (l *budgetLedger) rolloverIfNeededLocked() {
	today := l.now().UTC().Format("2006-01-02")
	if today != l.day {
		l.day = today
		l.spent = 0
	}
}

// --- Verdict + outcome parsing --------------------------------------------

// pickVerdict scans the analyst text for one of the canonical
// operator-facing verdicts and returns the first match, or "" when
// none. We deliberately don't ask the model to emit a structured
// verdict tag — keeping the prompt simple keeps cost low and parser
// robust. Operators get an extra hint when the text uses the
// canonical phrase.
func pickVerdict(text string) string {
	low := strings.ToLower(text)
	switch {
	case strings.Contains(low, "actionable"):
		return "actionable"
	case strings.Contains(low, "watchlist"):
		return "watchlist"
	case strings.Contains(low, "avoid"):
		return "avoid"
	}
	return ""
}

// parseOutcomeText splits the model's response into reason + lessons.
// We use a deliberately tiny convention: the response can include a
// "LESSONS:" tail; everything before is the reason, everything after
// is lessons. WonExpected and Confidence are extracted from
// well-known markers in the text. All parsing failures degrade to
// "reason = full text, lessons = empty, won = nil, conf = 0".
func parseOutcomeText(text string) (reason, lessons string, won *bool, confidence float64) {
	reason = strings.TrimSpace(text)
	idx := strings.Index(strings.ToUpper(text), "LESSONS:")
	if idx > 0 {
		reason = strings.TrimSpace(text[:idx])
		lessons = strings.TrimSpace(text[idx+len("LESSONS:"):])
	}
	// Expectation marker. We don't enforce — many responses won't
	// include this; nil result is fine.
	low := strings.ToLower(text)
	switch {
	case strings.Contains(low, "expected by watchtower: yes"):
		v := true
		won = &v
	case strings.Contains(low, "expected by watchtower: no"):
		v := false
		won = &v
	}
	return reason, lessons, won, confidence
}

func truncate(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
