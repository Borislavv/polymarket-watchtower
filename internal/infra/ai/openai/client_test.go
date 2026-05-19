package openai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/analysis"
)

// newFakeOpenAIRaw mounts an httptest server that returns the
// supplied raw body + status. Used by tests that need to exercise
// the typed-error classifier on a specific provider JSON shape
// (quota_exceeded, rate_limit_exceeded, model_not_found, etc.).
func newFakeOpenAIRaw(t *testing.T, body string, status int) (Config, func()) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	cfg := Config{
		APIKey:                 "test-key",
		BaseURL:                srv.URL,
		Model:                  "test-model",
		Timeout:                2 * time.Second,
		MaxPromptChars:         2500,
		MaxOutputChars:         700,
		RatePerMin:             100,
		DailyBudget:            5,
		PromptCostPer1kUSD:     0.00015,
		CompletionCostPer1kUSD: 0.0006,
	}
	return cfg, srv.Close
}

// newFakeOpenAI returns a Config + cleanup func wired to an
// httptest server that mimics the Chat Completions API. Tests use
// this to exercise the full client path WITHOUT hitting the real
// OpenAI service.
func newFakeOpenAI(t *testing.T, content string, status int) (Config, func()) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = body
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message": map[string]any{"role": "assistant", "content": content},
			}},
			"usage": map[string]any{"prompt_tokens": 200, "completion_tokens": 80},
		})
	}))
	cfg := Config{
		APIKey:                 "test-key",
		BaseURL:                srv.URL,
		Model:                  "test-model",
		Timeout:                2 * time.Second,
		MaxPromptChars:         2500,
		MaxOutputChars:         700,
		RatePerMin:             100,
		DailyBudget:            5,
		PromptCostPer1kUSD:     0.00015,
		CompletionCostPer1kUSD: 0.0006,
	}
	return cfg, srv.Close
}

// TestAnalyzeAlert_HappyPath pins the canonical client behavior:
// receives prompt context, returns parsed analysis with verdict +
// token + cost fields populated.
func TestAnalyzeAlert_HappyPath(t *testing.T) {
	cfg, cleanup := newFakeOpenAI(t,
		"This looks like a watchlist candidate. Watch for the next debate; ends in 3 days.",
		200)
	defer cleanup()
	c := New(cfg)
	out, err := c.AnalyzeAlert(context.Background(), analysis.AlertAnalysisRequest{
		Kind:        "trade_anomaly",
		Severity:    "info",
		Reason:      "LargeRareBet",
		Title:       "Will X happen?",
		NotionalUSD: 5_000,
		Price:       0.65,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Status != analysis.StatusOK {
		t.Fatalf("status: got %q want ok", out.Status)
	}
	if out.Verdict != "watchlist" {
		t.Errorf("verdict: got %q want watchlist", out.Verdict)
	}
	if out.PromptTokens != 200 || out.CompletionTokens != 80 {
		t.Errorf("token accounting wrong: %+v", out)
	}
	if out.EstimatedCostUSD <= 0 {
		t.Errorf("estimated cost must be > 0 for a successful call: %v", out.EstimatedCostUSD)
	}
}

// TestAnalyzeAlert_LongOutputNotTruncated pins the v8.2 invariant:
// the openai client no longer post-hoc clips the model response.
// A model that produces a 3000-char structured note must reach the
// AlertAnalysis.AnalysisText field intact. Downstream is the only
// length consumer; this layer trusts the system prompt + the
// max_completion_tokens budget to shape length.
func TestAnalyzeAlert_LongOutputNotTruncated(t *testing.T) {
	long := strings.Repeat("This is a dense analytical paragraph about the alert. ", 60) // ~3300 chars
	cfg, cleanup := newFakeOpenAI(t, long, 200)
	defer cleanup()
	c := New(cfg)
	out, err := c.AnalyzeAlert(context.Background(), analysis.AlertAnalysisRequest{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Status != analysis.StatusOK {
		t.Fatalf("status: got %q want ok", out.Status)
	}
	if !strings.Contains(out.AnalysisText, "dense analytical paragraph") {
		t.Errorf("body lost: %q", out.AnalysisText)
	}
	if len(out.AnalysisText) < 3000 {
		t.Errorf("v8.2: long output must NOT be clipped; got %d chars want >= 3000", len(out.AnalysisText))
	}
}

// TestAnalyzeAlert_NoAPIKeyDegradesSilently pins the "no-key →
// service still functional" contract. AnalyzeAlert returns
// StatusSkipped without an HTTP call.
func TestAnalyzeAlert_NoAPIKeyDegradesSilently(t *testing.T) {
	c := New(Config{APIKey: ""})
	out, err := c.AnalyzeAlert(context.Background(), analysis.AlertAnalysisRequest{})
	if err != nil {
		t.Fatalf("must not error on missing key: %v", err)
	}
	if out.Status != analysis.StatusSkipped {
		t.Errorf("status: got %q want skipped", out.Status)
	}
	if !strings.Contains(out.LastError, "no_api_key") {
		t.Errorf("LastError must explain the skip: %q", out.LastError)
	}
}

// TestAnalyzeAlert_DailyBudgetStopsCalls pins the budget guard. A
// pre-spent ledger short-circuits the HTTP path.
func TestAnalyzeAlert_DailyBudgetStopsCalls(t *testing.T) {
	cfg, cleanup := newFakeOpenAI(t, "actionable", 200)
	defer cleanup()
	cfg.DailyBudget = 0.001 // basically zero
	c := New(cfg)
	// Consume the entire budget up front.
	c.ledger.consume(1.0)
	out, _ := c.AnalyzeAlert(context.Background(), analysis.AlertAnalysisRequest{Kind: "info"})
	if out.Status != analysis.StatusSkipped {
		t.Fatalf("status: got %q want skipped", out.Status)
	}
	if !strings.Contains(out.LastError, "budget") {
		t.Errorf("LastError must cite budget: %q", out.LastError)
	}
}

// TestAnalyzeAlert_RateLimited pins the per-minute token bucket.
func TestAnalyzeAlert_RateLimited(t *testing.T) {
	cfg, cleanup := newFakeOpenAI(t, "actionable", 200)
	defer cleanup()
	cfg.RatePerMin = 1
	c := New(cfg)
	// Drain the bucket with one successful call.
	if out, _ := c.AnalyzeAlert(context.Background(), analysis.AlertAnalysisRequest{}); out.Status != analysis.StatusOK {
		t.Fatalf("first call must succeed: %+v", out)
	}
	// Second immediate call must be rate-limited.
	out, _ := c.AnalyzeAlert(context.Background(), analysis.AlertAnalysisRequest{})
	if out.Status != analysis.StatusSkipped || !strings.Contains(out.LastError, "rate_limited") {
		t.Errorf("expected rate_limited skip: %+v", out)
	}
}

// TestAnalyzeAlert_Non2xxReturnsTypedProviderError pins the v8 data-
// correctness contract: a non-2xx response surfaces a typed
// *ProviderError to the caller so it can be routed into
// polymarket_ai_request_logs. LastError is the SHORT canonical
// category (e.g. "provider_5xx") — never the raw provider body.
// The analysis.AlertAnalysis result still carries StatusError so
// the alert ships without an Analyst note.
func TestAnalyzeAlert_Non2xxReturnsTypedProviderError(t *testing.T) {
	cfg, cleanup := newFakeOpenAI(t, "", 500)
	defer cleanup()
	c := New(cfg)
	out, err := c.AnalyzeAlert(context.Background(), analysis.AlertAnalysisRequest{})
	if err == nil {
		t.Fatalf("expected typed ProviderError on upstream 500")
	}
	pe, ok := AsProviderError(err)
	if !ok {
		t.Fatalf("error must be *ProviderError: %T", err)
	}
	if pe.Category != CategoryProvider5xx {
		t.Errorf("category: got %q want provider_5xx", pe.Category)
	}
	if !pe.Retryable {
		t.Error("provider_5xx must be retryable")
	}
	if out.Status != analysis.StatusError {
		t.Errorf("status: got %q want error", out.Status)
	}
	if out.LastError != string(CategoryProvider5xx) {
		t.Errorf("LastError must be the canonical category, not the raw body: %q", out.LastError)
	}
}

// TestAnalyzeAlert_429QuotaExceeded pins the load-bearing
// classification: the production incident was OpenAI 429 with
// `{"error":{"code":"insufficient_quota"}}` being stored as a fake
// analysis row. Now it MUST surface as CategoryQuotaExceeded with
// Retryable=false — quota is a billing/operator action, not a
// transient slow-down.
func TestAnalyzeAlert_429QuotaExceeded(t *testing.T) {
	body := `{"error":{"message":"You exceeded your quota","type":"insufficient_quota","code":"insufficient_quota"}}`
	cfg, cleanup := newFakeOpenAIRaw(t, body, 429)
	defer cleanup()
	c := New(cfg)
	out, err := c.AnalyzeAlert(context.Background(), analysis.AlertAnalysisRequest{})
	if err == nil {
		t.Fatalf("expected ProviderError for 429 quota")
	}
	pe, _ := AsProviderError(err)
	if pe == nil || pe.Category != CategoryQuotaExceeded {
		t.Fatalf("category: got %v want quota_exceeded", pe)
	}
	if pe.Retryable {
		t.Error("quota_exceeded must NOT be retryable")
	}
	if strings.Contains(pe.Message, "{") {
		t.Errorf("ProviderError.Message must be sanitized (no raw JSON): %q", pe.Message)
	}
	if out.LastError != string(CategoryQuotaExceeded) {
		t.Errorf("LastError must be canonical category: %q", out.LastError)
	}
	if out.AnalysisText != "" {
		t.Errorf("StatusError/Skipped row must have empty analysis text: %q", out.AnalysisText)
	}
}

// TestAnalyzeAlert_429PerMinuteRateLimit pins the OTHER 429 path:
// generic per-minute rate-limit, not quota. Must be retryable.
func TestAnalyzeAlert_429PerMinuteRateLimit(t *testing.T) {
	body := `{"error":{"message":"Rate limit reached","type":"requests","code":"rate_limit_exceeded"}}`
	cfg, cleanup := newFakeOpenAIRaw(t, body, 429)
	defer cleanup()
	c := New(cfg)
	_, err := c.AnalyzeAlert(context.Background(), analysis.AlertAnalysisRequest{})
	pe, _ := AsProviderError(err)
	if pe == nil || pe.Category != CategoryRateLimited {
		t.Fatalf("category: got %v want rate_limited", pe)
	}
	if !pe.Retryable {
		t.Error("rate_limited must be retryable")
	}
}

// TestPromptBuilder_TitleAndReasonsAppear pins that the prompt
// carries the structured signals the model needs. We don't assert
// the entire content; we assert specific tokens are present.
func TestPromptBuilder_TitleAndReasonsAppear(t *testing.T) {
	req := analysis.AlertAnalysisRequest{
		Kind: "stable_favorite", Severity: "warning",
		Title:    "Will Massie win KY-04?",
		Category: "Politics",
		Reasons:  []string{"STABLE_PRICE", "LOW_VOLATILITY"},
	}
	p := buildAlertPrompt(req)
	for _, want := range []string{
		"alert_kind: stable_favorite",
		"severity: warning",
		"market_title: Will Massie win KY-04?",
		"category: Politics",
		"reasons: STABLE_PRICE, LOW_VOLATILITY",
		// v8.2 prompt shape — structured operator-decision sections
		// in the user message; tone + anti-hallucination + density
		// live in defaultSystemPrompt (pinned by a separate test).
		"Signal read:",
		"Strategy validation:",
		"Would I follow this?",
		"Final verdict:",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q. Prompt:\n%s", want, p)
		}
	}
}

// TestPromptBuilder_CrossFlowContradictoryAlerts pins v8: when the
// detector reports same-market opposite-side notional, the prompt
// must surface the NUMBERS so the model can reason about
// contradictory flow. v8.2 moves the wording instruction
// ("conflicting flow", etc.) into the system prompt; the user-
// message test only pins the data contract.
func TestPromptBuilder_CrossFlowContradictoryAlerts(t *testing.T) {
	req := analysis.AlertAnalysisRequest{
		Kind: "accumulation", Severity: "warning",
		SameMarketRecentAlerts:            4,
		SameMarketSameSideNotionalUSD:     12_000,
		SameMarketOppositeSideNotionalUSD: 18_000,
	}
	p := buildAlertPrompt(req)
	for _, want := range []string{
		"same_market_recent_alerts_24h: 4",
		"same_market_same_side_notional_24h: 12000",
		"same_market_opposite_side_notional_24h: 18000",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q. Prompt:\n%s", want, p)
		}
	}
	// The "Why it may fail" section in the new task block tells the
	// model where to consider opposite-side flow, replacing the
	// old "conflicting flow" wording duplication.
	if !strings.Contains(p, "opposite-side flow") {
		t.Errorf("task section must point the model at opposite-side flow:\n%s", p)
	}
}

// TestPromptBuilder_BidirectionalWalletWarning pins that the
// same-wallet-bidirectional signal lands in the user-message data
// section. The "market-making" wording moved into the system
// prompt; that is asserted by TestSystemPromptCarriesAntiHallucinationRules.
func TestPromptBuilder_BidirectionalWalletWarning(t *testing.T) {
	p := buildAlertPrompt(analysis.AlertAnalysisRequest{SameWalletBidirectional: true})
	if !strings.Contains(p, "same_wallet_bidirectional: yes") {
		t.Errorf("prompt missing bidirectional flag:\n%s", p)
	}
}

// TestSystemPromptCarriesAntiHallucinationRules pins the v8.2 rule
// that style + anti-hallucination + politics grounding live in the
// system prompt (one place, not duplicated per-call). If the
// constant drifts, the model loses the load-bearing guidance.
func TestSystemPromptCarriesAntiHallucinationRules(t *testing.T) {
	for _, want := range []string{
		"prediction-market and political-risk analyst",
		"Prefer being correct over being comprehensive.",
		"Do not manufacture depth where depth does not exist.",
		"Live public context was not checked.",
		"Never claim insider trading.",
		"Target 1200-2500 characters.",
		"No filler.",
		// observable-factors list (politics grounding)
		"polling cadence",
		"endorsements",
		"election-calendar events",
	} {
		if !strings.Contains(defaultSystemPrompt, want) {
			t.Errorf("system prompt missing %q", want)
		}
	}
}

// TestPromptBuilder_NoveltyMemeDowngrade pins the novelty-bias clause.
func TestPromptBuilder_NoveltyMemeDowngrade(t *testing.T) {
	p := buildAlertPrompt(analysis.AlertAnalysisRequest{NoveltyOrMemeGuess: true})
	if !strings.Contains(p, "novelty") {
		t.Errorf("prompt missing novelty marker:\n%s", p)
	}
}

// TestPromptBuilder_PublicContextDisclosure pins both branches of
// the web_search disclosure clause — the model must always know
// whether it has live context or not.
func TestPromptBuilder_PublicContextDisclosure(t *testing.T) {
	enabled := buildAlertPrompt(analysis.AlertAnalysisRequest{PublicContextEnabled: true})
	if !strings.Contains(enabled, "web_search was attempted") {
		t.Errorf("enabled disclosure missing:\n%s", enabled)
	}
	disabled := buildAlertPrompt(analysis.AlertAnalysisRequest{PublicContextEnabled: false})
	if !strings.Contains(disabled, "NOT checked") {
		t.Errorf("disabled disclosure missing in user message:\n%s", disabled)
	}
	// The canonical disclosure sentence the model must include when
	// no public context was checked now lives in defaultSystemPrompt
	// (TestSystemPromptCarriesAntiHallucinationRules pins it). The
	// user message just instructs the model not to invent facts.
	if !strings.Contains(disabled, "Do not invent public facts") {
		t.Errorf("user-message no-invent rule missing:\n%s", disabled)
	}
}

// TestPromptBuilder_MaxCharsTrims pins the output cap. We feed an
// enormous title to force the trim path.
// TestPromptBuilder_NoLongerTruncates pins the v8.2 invariant:
// prompt-side truncation was removed because clipping a structured
// prompt mid-instruction produced worse model output than the
// unbounded cost. A huge field (5000-char title) must still land
// verbatim. Cost is bounded by the daily-budget ledger upstream.
func TestPromptBuilder_NoLongerTruncates(t *testing.T) {
	huge := strings.Repeat("x", 5000)
	p := buildAlertPrompt(analysis.AlertAnalysisRequest{Title: huge})
	if !strings.Contains(p, huge) {
		t.Errorf("huge title must survive verbatim; got len=%d", len(p))
	}
}

// TestBudgetLedger_DailyRollover pins that the ledger zeros at UTC
// midnight. We inject a controlled clock and step it across.
func TestBudgetLedger_DailyRollover(t *testing.T) {
	now := time.Date(2026, 5, 20, 23, 30, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	l := newBudgetLedger(1.0, clock)
	l.consume(0.5)
	if !l.allow() {
		t.Fatal("budget should still allow at $0.5/$1.0")
	}
	// Roll the clock past midnight UTC.
	now = time.Date(2026, 5, 21, 0, 1, 0, 0, time.UTC)
	if !l.allow() {
		t.Fatal("budget should reset after UTC rollover")
	}
	if got := l.Spent(); got != 0 {
		t.Errorf("Spent after rollover: %v want 0", got)
	}
}

// TestRateBucket_RefillsOverTime pins the token bucket's refill
// behavior. After draining and waiting we get a token back.
func TestRateBucket_RefillsOverTime(t *testing.T) {
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	b := newRateBucket(60, clock) // 60/min == 1/sec
	// Drain the bucket.
	for i := 0; i < 60; i++ {
		if !b.allow() {
			t.Fatalf("call %d should be allowed at start", i)
		}
	}
	if b.allow() {
		t.Fatal("bucket should be empty after 60 calls")
	}
	now = now.Add(2 * time.Second)
	if !b.allow() {
		t.Fatal("bucket should refill after 2 seconds at 60/min")
	}
}

// TestPickVerdict pins the canonical-verdict scraper. The scraper
// is intentionally tolerant — any of the three keywords in the body
// is enough.
func TestPickVerdict(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"This is actionable, fire away.", "actionable"},
		{"Add to the watchlist for now.", "watchlist"},
		{"Avoid — too noisy.", "avoid"},
		{"unclear, more data needed.", ""},
	}
	for _, c := range cases {
		if got := pickVerdict(c.in); got != c.want {
			t.Errorf("pickVerdict(%q): got %q want %q", c.in, got, c.want)
		}
	}
}

// TestParseOutcomeText pins the postmortem parser. Reason + lessons
// split on the "LESSONS:" marker; "Expected by Watchtower: yes/no"
// drives WonExpected.
func TestParseOutcomeText(t *testing.T) {
	text := `The favorite resolved correctly because polling held.
LESSONS:
- watch poll release dates more closely
Expected by Watchtower: yes`
	reason, lessons, won, _ := parseOutcomeText(text)
	if !strings.HasPrefix(reason, "The favorite resolved") {
		t.Errorf("reason wrong: %q", reason)
	}
	if !strings.Contains(lessons, "watch poll release") {
		t.Errorf("lessons wrong: %q", lessons)
	}
	if won == nil || !*won {
		t.Errorf("WonExpected: got %v want *true", won)
	}
}
