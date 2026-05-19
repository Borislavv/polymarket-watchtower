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
		// 4000 chars ≈ 1333 tokens at the standard 4-chars/token
		// rule of thumb. The model is asked to target 1200-2500
		// chars and stay under 4000; this is the BUDGET for the
		// max_completion_tokens parameter, not a post-hoc clip.
		c.MaxOutputChars = 4000
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

// defaultSystemPrompt — the analyst persona + load-bearing rules.
// Single source for tone, anti-hallucination, density bias, and
// output discipline. The per-call user message carries DATA + TASK
// only; everything stylistic lives here so we don't pay tokens for
// duplicated instructions.
const defaultSystemPrompt = `You are a senior prediction-market and political-risk analyst.
Style: institutional, dense, analytical, skeptical, uncertainty-aware.
Audience: a surveillance operator hunting informed flow on Polymarket.

Form a real opinion. Express judgment. Call weak signals weak. Say "likely market-making", "edge already gone", "contradictory flow", "probably noise" when that is the honest read. Do not summarize the alert back at the operator.

Anti-hallucination (load-bearing):
- Prefer being correct over being comprehensive.
- If evidence is weak, say so directly.
- Do not manufacture depth where depth does not exist.
- If public/live context was not checked, write "Live public context was not checked." rather than implying confirmation.
- Never invent polls, endorsements, rulings, or news. Reason about observable factors only: polling cadence, endorsements on record, filings, legal rulings, debates, coalition shifts, official statements, election-calendar events, sanctions, negotiations, legislation, military escalations, treaty developments.
- Never claim insider trading. Never use "guaranteed", "risk-free", "sure thing", "easy money".

Density discipline:
- Target 1200-2500 characters. Use up to 4000 only when the setup is genuinely complex.
- No filler. Never pad to satisfy a length target.
- No markdown tables. Minimal bullets. Short paragraphs allowed.
- Plain readable English. No hype.`

// AnalyzeAlert builds the prompt, calls OpenAI, returns the parsed
// analyst note. On budget/rate-limit/timeout/error it returns a
// non-error AlertAnalysis with Status set so the caller can persist
// the skip-reason and still emit the underlying alert.
func (c *Client) AnalyzeAlert(ctx context.Context, req analysis.AlertAnalysisRequest) (analysis.AlertAnalysis, error) {
	prompt := buildAlertPrompt(req)
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
		// Surface a short canonical category in LastError (never the
		// raw provider body). The aianalysis layer routes the full
		// typed error into polymarket_ai_request_logs.
		pe, ok := AsProviderError(err)
		if !ok {
			pe = &ProviderError{Category: CategoryUnknown, Message: err.Error()}
		}
		return analysis.AlertAnalysis{
			Status:    statusForCategory(pe.Category),
			Model:     c.cfg.Model,
			LastError: string(pe.Category),
		}, err
	}
	cost := c.estimateCost(resp.PromptTokens, resp.CompletionTokens)
	c.ledger.consume(cost)
	// v8.2: post-hoc output truncation removed. The model is asked to
	// self-regulate via the system prompt and `max_completion_tokens`
	// budget; arbitrary mid-sentence clipping was throwing away
	// useful analysis. The Telegram formatter is the only remaining
	// length consumer downstream — if a single alert body grows past
	// Telegram's 4096-char limit, the sender marks the row as a
	// permanent failure with a clear "message is too long" error,
	// which is the right place to fail loud rather than silent.
	text := strings.TrimSpace(resp.Text)
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

// statusForCategory maps the typed provider category back to the
// legacy analysis.Status enum the wrapper still returns for
// backward-compat. quota/budget/disabled → skipped (no retry);
// everything else → error.
func statusForCategory(cat ErrorCategory) analysis.Status {
	switch cat {
	case CategoryQuotaExceeded:
		return analysis.StatusSkipped
	default:
		return analysis.StatusError
	}
}

// AnalyzeMarketReport is implemented but the orchestrating worker
// is staged behind a follow-up PR. The model call works end-to-end;
// only the periodic top-N selection + dedup logic lives in the
// usecase layer.
func (c *Client) AnalyzeMarketReport(ctx context.Context, req analysis.MarketReportRequest) (analysis.MarketReportAnalysis, error) {
	prompt := buildMarketReportPrompt(req) // reports are bigger
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
		pe, ok := AsProviderError(err)
		if !ok {
			pe = &ProviderError{Category: CategoryUnknown, Message: err.Error()}
		}
		return analysis.MarketReportAnalysis{
			Status:    statusForCategory(pe.Category),
			Model:     c.cfg.Model,
			LastError: string(pe.Category),
		}, err
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
	prompt := buildOutcomePrompt(req)
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
		pe, ok := AsProviderError(err)
		if !ok {
			pe = &ProviderError{Category: CategoryUnknown, Message: err.Error()}
		}
		return analysis.OutcomeAnalysis{
			Status:    statusForCategory(pe.Category),
			Model:     c.cfg.Model,
			LastError: string(pe.Category),
		}, err
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
		// Network-class failure. Context-deadline maps to timeout
		// so the caller can route it through the retry policy.
		cat := CategoryNetwork
		if errors.Is(err, context.DeadlineExceeded) {
			cat = CategoryTimeout
		}
		return chatResp{}, &ProviderError{
			Category:  cat,
			Message:   sanitizeAndCap(err.Error(), 200),
			Retryable: true,
		}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return chatResp{}, &ProviderError{
			Category:  CategoryNetwork,
			Message:   sanitizeAndCap(err.Error(), 200),
			Retryable: true,
		}
	}
	if resp.StatusCode/100 != 2 {
		// Typed, structured failure — the upstream service routes
		// this into polymarket_ai_request_logs and metrics, NEVER
		// into polymarket_alert_analyses.
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
		return chatResp{}, &ProviderError{
			Category:  CategoryUnknown,
			Message:   sanitizeAndCap("unmarshal: "+err.Error(), 200),
			Retryable: false,
		}
	}
	if len(parsed.Choices) == 0 {
		return chatResp{}, &ProviderError{
			Category:  CategoryEmptyResponse,
			Message:   "no choices in response",
			Retryable: true,
		}
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
