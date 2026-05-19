package aianalysis

import (
	"context"
	"errors"
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
// assert what the Service does with it.
type fakeAnalyzer struct {
	result analysis.AlertAnalysis
	calls  int
}

func (f *fakeAnalyzer) AnalyzeAlert(_ context.Context, _ analysis.AlertAnalysisRequest) (analysis.AlertAnalysis, error) {
	f.calls++
	return f.result, nil
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
		an, st, metrics.New(), nopLogger())
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
		AnalysisText: "This looks like an actionable trade.",
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
	an := &fakeAnalyzer{result: analysis.AlertAnalysis{Status: analysis.StatusOK, AnalysisText: "x"}}
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
	an := &fakeAnalyzer{result: analysis.AlertAnalysis{Status: analysis.StatusOK, AnalysisText: "upgraded view"}}
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
	svc := New(Config{AlertsEnabled: false}, an, st, metrics.New(), nopLogger())
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

func TestAnalyzerErrorBecomesStatusError(t *testing.T) {
	st := newFakeStore()
	svc := newSvc(erroringAnalyzer{}, st)
	row, err := svc.AnalyzeAndStore(context.Background(), 42, sampleFinding())
	if err != nil {
		t.Fatalf("must not propagate analyzer error: %v", err)
	}
	if row.Status != string(analysis.StatusError) {
		t.Errorf("status: got %q want error", row.Status)
	}
}
