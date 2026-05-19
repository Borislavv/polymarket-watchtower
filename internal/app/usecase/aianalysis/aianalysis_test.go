package aianalysis

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/analysis"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/anomaly"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/trade"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/metrics"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/repository"
)

// fakeAnalyzer returns a canned AlertAnalysis on every call. Tests
// assert what the Service does with it. err lets tests simulate a
// typed-provider-error path (the openai client returns Go err +
// non-OK Status on quota/rate/5xx).
type fakeAnalyzer struct {
	result analysis.AlertAnalysis
	err    error
	calls  int
}

func (f *fakeAnalyzer) AnalyzeAlert(_ context.Context, _ analysis.AlertAnalysisRequest) (analysis.AlertAnalysis, error) {
	f.calls++
	return f.result, f.err
}
func (f *fakeAnalyzer) AnalyzeMarketReport(_ context.Context, _ analysis.MarketReportRequest) (analysis.MarketReportAnalysis, error) {
	return analysis.MarketReportAnalysis{}, nil
}
func (f *fakeAnalyzer) AnalyzeOutcome(_ context.Context, _ analysis.OutcomeAnalysisRequest) (analysis.OutcomeAnalysis, error) {
	return analysis.OutcomeAnalysis{}, nil
}

// fakeStore is the in-memory AnalysisStore for tests.
type fakeStore struct {
	latest map[int64]repository.AlertAnalysis
}

func newFakeStore() *fakeStore { return &fakeStore{latest: map[int64]repository.AlertAnalysis{}} }

func (s *fakeStore) LatestVersion(_ context.Context, alertID int64) (int32, error) {
	if a, ok := s.latest[alertID]; ok {
		return a.Version, nil
	}
	return 0, nil
}
func (s *fakeStore) Latest(_ context.Context, alertID int64) (repository.AlertAnalysis, error) {
	if a, ok := s.latest[alertID]; ok {
		return a, nil
	}
	return repository.AlertAnalysis{}, repository.ErrAnalysisNotFound
}
func (s *fakeStore) Insert(_ context.Context, a repository.NewAlertAnalysis) (repository.AlertAnalysis, bool, error) {
	row := repository.AlertAnalysis{
		ID:               int64(len(s.latest) + 1),
		AlertID:          a.AlertID,
		Version:          a.Version,
		TriggerKind:      a.TriggerKind,
		AnalysisText:     a.AnalysisText,
		Status:           a.Status,
		EstimatedCostUSD: a.EstimatedCostUSD,
		CreatedAt:        time.Now(),
	}
	s.latest[a.AlertID] = row
	return row, true, nil
}

func nopLogger() *zerolog.Logger {
	l := zerolog.Nop()
	return &l
}

func newSvc(an analysis.Analyzer, st AnalysisStore) *Service {
	return New(Config{AlertsEnabled: true, LifecycleRefreshDeltaPct: 1, CLVMaterialChange: 0.02},
		an, st, nil /* request_log */, metrics.New(), nopLogger())
}

// newSvcWithRequestLog wires a fake request-log store so tests can
// assert v8 telemetry without standing up Postgres.
func newSvcWithRequestLog(an analysis.Analyzer, st AnalysisStore, rl RequestLogStore) *Service {
	return New(Config{AlertsEnabled: true, LifecycleRefreshDeltaPct: 1, CLVMaterialChange: 0.02},
		an, st, rl, metrics.New(), nopLogger())
}

// validAnalysisText returns a model-output string that passes
// validateAlertOutput. Centralised so updating the validation contract
// doesn't require touching every test literal.
func validAnalysisText(verdictWord string) string {
	return "Thesis: stable late-cycle favorite.\n" +
		"Follow?: " + verdictWord + ".\n" +
		"Why: low stddev + meaningful remaining return.\n" +
		"Risk: late news may flip the favorite.\n" +
		"Next: watch close-time + final polls.\n" +
		"Verdict: " + verdictWord + "."
}

// fakeRequestLog captures every Insert call so tests can assert the
// status/category routing without a real DB.
type fakeRequestLog struct {
	mu   sync.Mutex
	rows []repository.AIRequestLog
	err  error
}

func (f *fakeRequestLog) Insert(_ context.Context, l repository.AIRequestLog) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.rows = append(f.rows, l)
	return nil
}

func sampleFinding() anomaly.Finding {
	return anomaly.Finding{
		Kind:     anomaly.KindTradeAnomaly,
		Severity: anomaly.SeverityInfo,
		Reason:   anomaly.ReasonSingle,
		At:       time.Now(),
		Trade: &anomaly.TradeRef{
			Question:    "Will X win?",
			Outcome:     "Yes",
			Side:        trade.SideBuy,
			NotionalUSD: 5_000,
			Price:       0.65,
			Odds:        1.0 / 0.65,
		},
		ProfitIfWinUSD: 2_692,
		LifecyclePct:   85,
	}
}

// TestFirstTimeAnalysisInsertsVersion1 pins the canonical first-run
// flow: no prior row → analyzer called → version 1 persisted.
func TestFirstTimeAnalysisInsertsVersion1(t *testing.T) {
	an := &fakeAnalyzer{result: analysis.AlertAnalysis{
		Status: analysis.StatusOK, Model: "test-model",
		AnalysisText: validAnalysisText("watchlist"),
		Verdict:      "actionable",
		PromptTokens: 150, CompletionTokens: 60, EstimatedCostUSD: 0.0001,
	}}
	st := newFakeStore()
	svc := newSvc(an, st)

	row, err := svc.AnalyzeAndStore(context.Background(), 42, sampleFinding())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if row.Version != 1 {
		t.Errorf("version: got %d want 1", row.Version)
	}
	if row.TriggerKind != "initial" {
		t.Errorf("trigger_kind: got %q want initial", row.TriggerKind)
	}
	if an.calls != 1 {
		t.Errorf("analyzer call count: got %d want 1", an.calls)
	}
}

// TestRefreshSkippedWhenNothingMoved pins the cost-control rule:
// re-running the service for the same alert with the same finding
// must NOT call the analyzer.
func TestRefreshSkippedWhenNothingMoved(t *testing.T) {
	an := &fakeAnalyzer{result: analysis.AlertAnalysis{Status: analysis.StatusOK, AnalysisText: validAnalysisText("watch")}}
	st := newFakeStore()
	st.latest[42] = repository.AlertAnalysis{
		AlertID: 42, Version: 1, Status: string(analysis.StatusOK),
		AnalysisText: "old text",
		// triggerDetailFromContext format — durable record of prior severity.
		TriggerDetail: "severity=info lifecycle=85.0",
		CreatedAt:     time.Now(),
	}
	svc := newSvc(an, st)
	if _, err := svc.AnalyzeAndStore(context.Background(), 42, sampleFinding()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if an.calls != 0 {
		t.Errorf("analyzer should NOT be called on a same-state refresh; got %d", an.calls)
	}
}

// TestRefreshTriggersOnSeverityUpgrade pins that a severity bump
// forces a re-analysis even within the refresh window.
func TestRefreshTriggersOnSeverityUpgrade(t *testing.T) {
	an := &fakeAnalyzer{result: analysis.AlertAnalysis{Status: analysis.StatusOK, AnalysisText: validAnalysisText("watchlist")}}
	st := newFakeStore()
	st.latest[42] = repository.AlertAnalysis{
		AlertID: 42, Version: 1, Status: string(analysis.StatusOK),
		CreatedAt: time.Now(),
		// extractPriorSeverity returns "" (stub), severityRank("")=0 →
		// any non-empty severity counts as an upgrade.
	}
	svc := newSvc(an, st)
	f := sampleFinding()
	f.Severity = anomaly.SeverityWarning
	if _, err := svc.AnalyzeAndStore(context.Background(), 42, f); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if an.calls != 1 {
		t.Errorf("severity upgrade must trigger refresh; got %d", an.calls)
	}
	if st.latest[42].Version != 2 {
		t.Errorf("version: got %d want 2", st.latest[42].Version)
	}
}

// TestServiceDisabledShortCircuits pins that AlertsEnabled=false
// means the analyzer is NEVER called.
func TestServiceDisabledShortCircuits(t *testing.T) {
	an := &fakeAnalyzer{result: analysis.AlertAnalysis{Status: analysis.StatusOK}}
	st := newFakeStore()
	svc := New(Config{AlertsEnabled: false}, an, st, nil, metrics.New(), nopLogger())
	if _, err := svc.AnalyzeAndStore(context.Background(), 42, sampleFinding()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if an.calls != 0 {
		t.Errorf("disabled service must not call analyzer; got %d", an.calls)
	}
}

// TestLatestTextReturnsEmptyOnSkipped pins that a skipped analysis
// doesn't render: Telegram path must elide the Analyst-note block
// entirely.
func TestLatestTextReturnsEmptyOnSkipped(t *testing.T) {
	an := &fakeAnalyzer{result: analysis.AlertAnalysis{Status: analysis.StatusSkipped, LastError: "rate_limited"}}
	st := newFakeStore()
	svc := newSvc(an, st)
	if _, err := svc.AnalyzeAndStore(context.Background(), 42, sampleFinding()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got, reason := svc.LatestText(context.Background(), 42)
	if got != "" {
		t.Errorf("LatestText should be empty for non-OK status; got %q", got)
	}
	if reason == "" {
		t.Errorf("LatestText must surface a non-empty reason for empty result")
	}
}

// TestBuildAlertRequestPopulatesEveryField pins the prompt-input
// builder. We don't assert on every field; just the carries.
func TestBuildAlertRequestPopulatesEveryField(t *testing.T) {
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	req := BuildAlertRequest(sampleFinding(), now)
	if req.Kind != "trade_anomaly" || req.Severity != "info" {
		t.Errorf("kind/severity not carried: %+v", req)
	}
	if req.Title != "Will X win?" || req.OutcomeLabel != "Yes" {
		t.Errorf("title/outcome not carried: %+v", req)
	}
	if req.NotionalUSD != 5000 {
		t.Errorf("notional not carried: %+v", req)
	}
	if req.NowAt != now {
		t.Errorf("NowAt not stamped: %+v", req)
	}
}

// TestAnalyzerErrorBecomesStatusError pins that an analyzer that
// returns a Go error is logged and recorded as StatusError, NEVER
// propagated as a Go error (the caller must still emit the alert).
type erroringAnalyzer struct{}

func (erroringAnalyzer) AnalyzeAlert(_ context.Context, _ analysis.AlertAnalysisRequest) (analysis.AlertAnalysis, error) {
	return analysis.AlertAnalysis{}, errors.New("upstream blew up")
}
func (erroringAnalyzer) AnalyzeMarketReport(_ context.Context, _ analysis.MarketReportRequest) (analysis.MarketReportAnalysis, error) {
	return analysis.MarketReportAnalysis{}, nil
}
func (erroringAnalyzer) AnalyzeOutcome(_ context.Context, _ analysis.OutcomeAnalysisRequest) (analysis.OutcomeAnalysis, error) {
	return analysis.OutcomeAnalysis{}, nil
}

// TestAnalyzerErrorWritesRequestLogNotAnalysis pins v8 semantics:
// an analyzer Go error must NOT land in polymarket_alert_analyses.
// It is recorded in polymarket_ai_request_logs only. The returned
// AlertAnalysis carries Status=error so the caller skips the
// Analyst-note block, but no row is persisted in the analytical table.
func TestAnalyzerErrorWritesRequestLogNotAnalysis(t *testing.T) {
	st := newFakeStore()
	rl := &fakeRequestLog{}
	svc := newSvcWithRequestLog(erroringAnalyzer{}, st, rl)

	row, err := svc.AnalyzeAndStore(context.Background(), 42, sampleFinding())
	if err != nil {
		t.Fatalf("must not propagate analyzer error: %v", err)
	}
	if row.Status != string(analysis.StatusError) {
		t.Errorf("status: got %q want error", row.Status)
	}
	if len(st.latest) != 0 {
		t.Errorf("v8: failed analyses must NOT touch polymarket_alert_analyses; got %d rows", len(st.latest))
	}
	if len(rl.rows) != 1 {
		t.Fatalf("expected 1 ai_request_logs row, got %d", len(rl.rows))
	}
	if rl.rows[0].TargetKind != "alert" {
		t.Errorf("target_kind: %q", rl.rows[0].TargetKind)
	}
	if !strings.HasPrefix(rl.rows[0].Status, "failed") {
		t.Errorf("status: %q (must start with failed_)", rl.rows[0].Status)
	}
}

// TestProviderQuotaExceededIsNotStoredAsAnalysis is the canonical
// regression for the production incident: a 429 quota error must
// land in ai_request_logs with category=quota_exceeded, NEVER in
// polymarket_alert_analyses with raw JSON in last_error.
func TestProviderQuotaExceededIsNotStoredAsAnalysis(t *testing.T) {
	// fakeAnalyzer returns Status=skipped + LastError=quota_exceeded
	// — exactly what the openai client surfaces after the typed
	// classifier categorises a 429 quota response.
	an := &fakeAnalyzer{result: analysis.AlertAnalysis{
		Status:    analysis.StatusSkipped,
		Model:     "gpt-4o-mini",
		LastError: "quota_exceeded",
	}}
	st := newFakeStore()
	rl := &fakeRequestLog{}
	svc := newSvcWithRequestLog(an, st, rl)

	_, err := svc.AnalyzeAndStore(context.Background(), 42, sampleFinding())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(st.latest) != 0 {
		t.Errorf("v8: quota_exceeded must NOT write to alert_analyses; got %d rows", len(st.latest))
	}
	if len(rl.rows) != 1 {
		t.Fatalf("expected 1 ai_request_logs row, got %d", len(rl.rows))
	}
	if got := rl.rows[0].ErrorCategory; got != "quota_exceeded" {
		t.Errorf("error_category: %q want quota_exceeded", got)
	}
	if !strings.HasPrefix(rl.rows[0].Status, "skipped_") {
		t.Errorf("status: %q (must start with skipped_)", rl.rows[0].Status)
	}
}

// TestFreeFormAnswerIsAccepted pins the v8.1 contract: non-empty
// model output WITHOUT the Thesis/Follow?/Verdict markers MUST be
// persisted as analysis and rendered. Prompt engineering shapes the
// format; we never throw away paid model tokens because a label is
// missing.
func TestFreeFormAnswerIsAccepted(t *testing.T) {
	freeForm := "This wallet's accumulation is large but the opposite-side flow last 24h is comparable, so I would watch rather than copy."
	an := &fakeAnalyzer{result: analysis.AlertAnalysis{
		Status: analysis.StatusOK, Model: "gpt-4o-mini",
		AnalysisText: freeForm,
	}}
	st := newFakeStore()
	rl := &fakeRequestLog{}
	svc := newSvcWithRequestLog(an, st, rl)

	row, err := svc.AnalyzeAndStore(context.Background(), 42, sampleFinding())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if row.Status != string(analysis.StatusOK) {
		t.Errorf("status: got %q want ok", row.Status)
	}
	if len(st.latest) != 1 {
		t.Fatalf("free-form analysis must be persisted; got %d rows", len(st.latest))
	}
	if got := st.latest[42].AnalysisText; got != freeForm {
		t.Errorf("analysis_text not preserved verbatim:\n got: %q\nwant: %q", got, freeForm)
	}
	// The request_log row must be the success path, not validation_failed.
	if len(rl.rows) != 1 || rl.rows[0].Status != "success" {
		t.Errorf("request_log must record success; got %+v", rl.rows)
	}
}

// TestEmptyAnswerIsRejected pins the one validation that survives:
// a 200 with empty / whitespace-only text is rejected so we don't
// render a blank Analyst-note block.
func TestEmptyAnswerIsRejected(t *testing.T) {
	an := &fakeAnalyzer{result: analysis.AlertAnalysis{
		Status: analysis.StatusOK, Model: "gpt-4o-mini",
		AnalysisText: "   \n\t  ",
	}}
	st := newFakeStore()
	rl := &fakeRequestLog{}
	svc := newSvcWithRequestLog(an, st, rl)

	_, err := svc.AnalyzeAndStore(context.Background(), 42, sampleFinding())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(st.latest) != 0 {
		t.Errorf("empty output must NOT be stored as analysis; got %d rows", len(st.latest))
	}
	if len(rl.rows) != 1 || rl.rows[0].ErrorCategory != "validation_failed:empty_text" {
		t.Errorf("expected validation_failed:empty_text; got %+v", rl.rows)
	}
}

// TestProviderErrorJSONIsRejected pins the defence-in-depth path:
// even if the openai client's typed-error machinery misses (e.g. a
// 200-with-error body, a parser bug), aianalysis MUST NOT persist a
// JSON payload that looks like a provider error as if it were an
// AI answer.
func TestProviderErrorJSONIsRejected(t *testing.T) {
	an := &fakeAnalyzer{result: analysis.AlertAnalysis{
		Status: analysis.StatusOK, Model: "gpt-4o-mini",
		AnalysisText: `{"error":{"code":"insufficient_quota","message":"You exceeded your quota"}}`,
	}}
	st := newFakeStore()
	rl := &fakeRequestLog{}
	svc := newSvcWithRequestLog(an, st, rl)

	_, err := svc.AnalyzeAndStore(context.Background(), 42, sampleFinding())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(st.latest) != 0 {
		t.Errorf("provider-error text must NOT be stored as analysis; got %d rows", len(st.latest))
	}
	if len(rl.rows) != 1 || !strings.Contains(rl.rows[0].ErrorCategory, "provider_error_text") {
		t.Errorf("expected provider_error_text category; got %+v", rl.rows)
	}
}

// TestLongAnswerIsAccepted pins that long but non-empty model
// output is NOT rejected by aianalysis. Length capping is the
// openai client's job (MaxOutputChars + truncate); the analysis
// layer must not double-gate.
func TestLongAnswerIsAccepted(t *testing.T) {
	long := strings.Repeat("This wallet accumulated steadily over the last 24h. ", 30) // ~1560 chars
	an := &fakeAnalyzer{result: analysis.AlertAnalysis{
		Status: analysis.StatusOK, Model: "gpt-4o-mini",
		AnalysisText: long,
	}}
	st := newFakeStore()
	rl := &fakeRequestLog{}
	svc := newSvcWithRequestLog(an, st, rl)

	if _, err := svc.AnalyzeAndStore(context.Background(), 42, sampleFinding()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(st.latest) != 1 {
		t.Errorf("long answer must be persisted; got %d rows", len(st.latest))
	}
}

// TestErrorMessageIsCappedAt500 pins that a provider error message
// — say the raw 429 JSON body — never lands at full length in the
// DB. The repo wrapper caps at 500 chars before write.
func TestErrorMessageIsCappedAt500(t *testing.T) {
	huge := strings.Repeat("x", 1500)
	rl := &fakeRequestLog{}
	// Insert through the cap path by hand (the repo wrapper's logic).
	_ = rl // smoke seam: the real repo wrapper handles capping; the
	// fake records what's passed in. Exercise the cap via the
	// service's classifyAnalyzerError path:
	an := &fakeAnalyzer{result: analysis.AlertAnalysis{Status: analysis.StatusError, Model: "x", LastError: "rate_limited"}, err: errors.New(huge)}
	st := newFakeStore()
	svc := newSvcWithRequestLog(an, st, rl)

	if _, err := svc.AnalyzeAndStore(context.Background(), 1, sampleFinding()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rl.rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rl.rows))
	}
	// 500 char soft cap + the multi-byte ellipsis rune appended on
	// truncation = up to ~503 bytes. The load-bearing contract is
	// "no 1500-byte error JSON in the DB", and 503 satisfies it.
	if got := len(rl.rows[0].ErrorMessage); got > 510 {
		t.Errorf("error_message must be capped near 500 chars; got %d", got)
	}
	if got := len(rl.rows[0].ErrorMessage); got < 100 {
		t.Errorf("error_message must retain useful prefix; got %d chars", got)
	}
}
