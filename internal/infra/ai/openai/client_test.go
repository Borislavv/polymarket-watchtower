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
	p := buildAlertPrompt(req, 5000)
	for _, want := range []string{
		"alert_kind: stable_favorite",
		"severity: warning",
		"market_title: Will Massie win KY-04?",
		"category: Politics",
		"reasons: STABLE_PRICE, LOW_VOLATILITY",
		// v8 structured-output contract: prompt mandates the
		// Thesis/Follow?/Verdict shape so output can be validated.
		"Thesis:",
		"Follow?:",
		"Verdict:",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q. Prompt:\n%s", want, p)
		}
	}
}

// TestPromptBuilder_CrossFlowContradictoryAlerts pins v8: when the
// detector reports same-market opposite-side notional, the prompt
// must surface it so the model can name conflicting flow and
// downgrade Follow? accordingly.
func TestPromptBuilder_CrossFlowContradictoryAlerts(t *testing.T) {
	req := analysis.AlertAnalysisRequest{
		Kind: "accumulation", Severity: "warning",
		SameMarketRecentAlerts:            4,
		SameMarketSameSideNotionalUSD:     12_000,
		SameMarketOppositeSideNotionalUSD: 18_000,
	}
	p := buildAlertPrompt(req, 5000)
	for _, want := range []string{
		"same_market_recent_alerts_24h: 4",
		"same_market_same_side_notional_24h: 12000",
		"same_market_opposite_side_notional_24h: 18000",
		"conflicting flow",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q. Prompt:\n%s", want, p)
		}
	}
}

// TestPromptBuilder_BidirectionalWalletWarning pins that the
// same-wallet-bidirectional signal lands in the prompt and the
// market-making warning is included.
func TestPromptBuilder_BidirectionalWalletWarning(t *testing.T) {
	p := buildAlertPrompt(analysis.AlertAnalysisRequest{SameWalletBidirectional: true}, 5000)
	if !strings.Contains(p, "same_wallet_bidirectional: yes") {
		t.Errorf("prompt missing bidirectional flag:\n%s", p)
	}
	if !strings.Contains(p, "market-making") {
		t.Errorf("prompt missing market-making warning:\n%s", p)
	}
}

// TestPromptBuilder_NoveltyMemeDowngrade pins the novelty-bias clause.
func TestPromptBuilder_NoveltyMemeDowngrade(t *testing.T) {
	p := buildAlertPrompt(analysis.AlertAnalysisRequest{NoveltyOrMemeGuess: true}, 5000)
	if !strings.Contains(p, "novelty") {
		t.Errorf("prompt missing novelty marker:\n%s", p)
	}
}

// TestPromptBuilder_PublicContextDisclosure pins both branches of
// the web_search disclosure clause — the model must always know
// whether it has live context or not.
func TestPromptBuilder_PublicContextDisclosure(t *testing.T) {
	enabled := buildAlertPrompt(analysis.AlertAnalysisRequest{PublicContextEnabled: true}, 5000)
	if !strings.Contains(enabled, "web_search was attempted") {
		t.Errorf("enabled disclosure missing:\n%s", enabled)
	}
	disabled := buildAlertPrompt(analysis.AlertAnalysisRequest{PublicContextEnabled: false}, 5000)
	if !strings.Contains(disabled, "NOT checked") {
		t.Errorf("disabled disclosure missing:\n%s", disabled)
	}
	if !strings.Contains(disabled, "Live context was not checked.") {
		t.Errorf("expected the 'Live context was not checked.' rule in:\n%s", disabled)
	}
}

// TestPromptBuilder_MaxCharsTrims pins the output cap. We feed an
// enormous title to force the trim path.
func TestPromptBuilder_MaxCharsTrims(t *testing.T) {
	huge := strings.Repeat("x", 5000)
	p := buildAlertPrompt(analysis.AlertAnalysisRequest{Title: huge}, 500)
	// truncate appends a UTF-8 ellipsis (3 bytes), so the resulting
	// length is bounded by cap + 2.
	if len(p) > 502 {
		t.Errorf("prompt not trimmed: len=%d", len(p))
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
