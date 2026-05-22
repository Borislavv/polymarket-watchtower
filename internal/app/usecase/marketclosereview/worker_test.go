// PHASE 3d tests for the v11.4 Market Close Review worker. Hermetic;
// no DB, no network. The fakes mirror the production seams from
// worker.go.
package marketclosereview

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/infra/ai/openai"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/metrics"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/repository"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/telegram"
)

// --- Fakes ---------------------------------------------------------------

type fakeStore struct {
	mu          sync.Mutex
	candidates  []repository.MarketCloseReviewCandidate
	alerts      map[int64][]repository.Alert
	succeeded   map[string]bool
	insertedIDs []int64
	finished    []repository.MarketCloseReviewFinish
	skips       []skipRow
	failures    []failureRow
	nextID      int64
}

type skipRow struct {
	ID     int64
	Reason string
}

type failureRow struct {
	ID    int64
	Error string
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		alerts:    make(map[int64][]repository.Alert),
		succeeded: make(map[string]bool),
		nextID:    1,
	}
}

func (s *fakeStore) ListCandidates(_ context.Context, _, _ time.Time, _ int32) ([]repository.MarketCloseReviewCandidate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.candidates
	s.candidates = nil
	return out, nil
}

func (s *fakeStore) HasSucceededReview(_ context.Context, conditionID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.succeeded[conditionID], nil
}

func (s *fakeStore) InsertRunning(_ context.Context, _ int64, _, _ string, _, _ time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.nextID
	s.nextID++
	s.insertedIDs = append(s.insertedIDs, id)
	return id, nil
}

func (s *fakeStore) FinishSucceeded(_ context.Context, in repository.MarketCloseReviewFinish) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.finished = append(s.finished, in)
	return nil
}

func (s *fakeStore) FinishFailed(_ context.Context, id int64, errMsg string, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failures = append(s.failures, failureRow{ID: id, Error: errMsg})
	return nil
}

func (s *fakeStore) FinishSkipped(_ context.Context, id int64, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.skips = append(s.skips, skipRow{ID: id, Reason: reason})
	return nil
}

func (s *fakeStore) ListAlertsForReview(_ context.Context, marketID int64, _ time.Time, _ int32) ([]repository.Alert, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.alerts[marketID], nil
}

type fakeAnalyzer struct {
	mu       sync.Mutex
	calls    int
	response openai.MarketCloseReviewResponse
	err      error
}

func (f *fakeAnalyzer) ReviewMarketClose(_ context.Context, _ openai.MarketCloseReviewRequest) (openai.MarketCloseReviewResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.response, f.err
}

type fakeBudget struct {
	allowed bool
	reason  string
	charged float64
}

func (b *fakeBudget) Allow(_ string, _ float64) (bool, string) {
	if b.allowed {
		return true, ""
	}
	return false, b.reason
}

func (b *fakeBudget) Charge(_ string, cost float64) { b.charged += cost }

type fakeTG struct {
	mu       sync.Mutex
	sent     []telegram.Message
	failSend bool
}

func (f *fakeTG) Send(_ context.Context, msg telegram.Message) (telegram.SendResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failSend {
		return telegram.SendResult{}, errors.New("tg boom")
	}
	f.sent = append(f.sent, msg)
	return telegram.SendResult{MessageID: 1234}, nil
}

type fakeReactioner struct {
	mu        sync.Mutex
	reactions []reactionRecord
	err       error
}

type reactionRecord struct {
	Surface   telegram.Surface
	ChatID    string
	MessageID int64
	Emoji     string
}

func (f *fakeReactioner) SetOutcomeReaction(_ context.Context, surface telegram.Surface, chatID string, messageID int64, emoji string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.reactions = append(f.reactions, reactionRecord{Surface: surface, ChatID: chatID, MessageID: messageID, Emoji: emoji})
	return nil
}

// --- Helpers -------------------------------------------------------------

func candidateMarket(id int64, cond string) repository.MarketCloseReviewCandidate {
	return repository.MarketCloseReviewCandidate{
		MarketID:    id,
		ConditionID: cond,
		EventSlug:   "test-event",
		Question:    "Will it rain?",
		EndDate:     time.Date(2026, 5, 20, 0, 0, 0, 0, time.UTC),
		Closed:      true,
	}
}

func sampleAlert(id int64, marketID int64, msgID int64) repository.Alert {
	tgID := msgID
	return repository.Alert{
		ID:                id,
		Kind:              repository.AlertKindTrade,
		Severity:          "info",
		StrategyVersion:   "informed-flow-v6",
		MarketID:          &marketID,
		TelegramMessageID: &tgID,
		Status:            repository.AlertSent,
		SentAt:            time.Date(2026, 5, 18, 0, 0, 0, 0, time.UTC),
	}
}

func buildWorker(t *testing.T, cfg Config, store CandidateSource, analyzer Analyzer, budget Budget, tg Telegram, reactioner Reactioner) *Worker {
	t.Helper()
	met := metrics.New()
	return New(cfg, store, analyzer, budget, tg, reactioner, met, nil)
}

func defaultCfg() Config {
	return Config{
		Enabled:                true,
		Interval:               30 * time.Minute,
		Lookback:               24 * time.Hour,
		MarketMaxAgeAfterClose: 72 * time.Hour,
		HistoryLookback:        8760 * time.Hour,
		MinAlerts:              1,
		RequireAlertOrNews:     true,
		MaxMarketsPerRun:       10,
		MaxAlertsPerMarket:     50,
		MaxEventsPerMarket:     30,
		AIEnabled:              true,
		AITimeout:              60 * time.Second,
		DailyBudgetUSD:         3,
		SendAdminTelegram:      true,
		SetReactions:           true,
		ReactionSuccess:        "👍",
		ReactionFailure:        "👎",
		ReactionAmbiguous:      "🤔",
		SignalChatID:           "-100signal",
		Clock:                  func() time.Time { return time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC) },
	}
}

// --- Tests ---------------------------------------------------------------

// Unresolved market (no candidate row) → no work.
func TestTick_NoCandidates_NoOp(t *testing.T) {
	store := newFakeStore()
	w := buildWorker(t, defaultCfg(), store, &fakeAnalyzer{}, nil, nil, nil)
	w.Tick(context.Background())
	if len(store.insertedIDs) != 0 || len(store.finished) != 0 {
		t.Fatalf("must be no-op when no candidates; got insertedIDs=%v finished=%v",
			store.insertedIDs, store.finished)
	}
}

// Closed market with NO evidence → skipped with reason="no_evidence".
func TestTick_ClosedMarketWithoutEvidence_Skipped(t *testing.T) {
	store := newFakeStore()
	store.candidates = []repository.MarketCloseReviewCandidate{candidateMarket(7, "0xCOND")}
	w := buildWorker(t, defaultCfg(), store, &fakeAnalyzer{}, nil, nil, nil)
	w.Tick(context.Background())
	if len(store.skips) != 1 || store.skips[0].Reason != "no_evidence" {
		t.Fatalf("want skipped=no_evidence; got %+v", store.skips)
	}
}

// Already succeeded review → silently skipped, no insertion.
func TestTick_AlreadySucceededReview_Skipped(t *testing.T) {
	store := newFakeStore()
	store.candidates = []repository.MarketCloseReviewCandidate{candidateMarket(7, "0xCOND")}
	store.succeeded["0xCOND"] = true
	w := buildWorker(t, defaultCfg(), store, &fakeAnalyzer{}, nil, nil, nil)
	w.Tick(context.Background())
	if len(store.insertedIDs) != 0 || len(store.skips) != 0 || len(store.finished) != 0 {
		t.Fatalf("already-succeeded path must short-circuit; insertedIDs=%v skips=%v finished=%v",
			store.insertedIDs, store.skips, store.finished)
	}
}

// Budget exhausted → recorded as skipped, AI not called.
func TestTick_BudgetExhausted_SkippedNoAI(t *testing.T) {
	store := newFakeStore()
	store.candidates = []repository.MarketCloseReviewCandidate{candidateMarket(7, "0xCOND")}
	store.alerts[7] = []repository.Alert{sampleAlert(1, 7, 100)}
	analyzer := &fakeAnalyzer{}
	budget := &fakeBudget{allowed: false, reason: "global_exhausted"}
	w := buildWorker(t, defaultCfg(), store, analyzer, budget, nil, nil)
	w.Tick(context.Background())
	if analyzer.calls != 0 {
		t.Fatalf("AI must NOT be called when budget denied; calls=%d", analyzer.calls)
	}
	if len(store.skips) != 1 || !strings.HasPrefix(store.skips[0].Reason, "budget_") {
		t.Fatalf("want budget-skipped; got %+v", store.skips)
	}
}

// AI disabled → skipped="ai_disabled", no analyzer call.
func TestTick_AIDisabled_Skipped(t *testing.T) {
	store := newFakeStore()
	store.candidates = []repository.MarketCloseReviewCandidate{candidateMarket(7, "0xCOND")}
	store.alerts[7] = []repository.Alert{sampleAlert(1, 7, 100)}
	cfg := defaultCfg()
	cfg.AIEnabled = false
	w := buildWorker(t, cfg, store, nil, &fakeBudget{allowed: true}, nil, nil)
	w.Tick(context.Background())
	if len(store.skips) != 1 || store.skips[0].Reason != "ai_disabled" {
		t.Fatalf("want skipped=ai_disabled; got %+v", store.skips)
	}
}

// Happy path: evidence + budget + AI → succeeded persisted, admin
// Telegram dispatched, reactions applied to known alert message ids.
func TestTick_HappyPath_PersistsAdminAndReacts(t *testing.T) {
	store := newFakeStore()
	store.candidates = []repository.MarketCloseReviewCandidate{candidateMarket(7, "0xCOND")}
	store.alerts[7] = []repository.Alert{sampleAlert(1, 7, 100), sampleAlert(2, 7, 200)}
	analyzer := &fakeAnalyzer{
		response: openai.MarketCloseReviewResponse{
			Verdict:    "confirmed_signal",
			Confidence: 0.78,
			AdminSummary: "Alerts caught the move; YES resolved as expected.",
			ReactionPlan: []openai.MarketCloseReviewReactionPlan{
				{AlertID: 1, Reaction: "success", Reason: "won"},
				{AlertID: 2, Reaction: "failure", Reason: "lost"},
				{AlertID: 999, Reaction: "success", Reason: "INVENTED — must be dropped by parser"},
			},
			EstimatedCostUSD: 0.04,
			Model:            "test-model",
		},
	}
	budget := &fakeBudget{allowed: true}
	tg := &fakeTG{}
	reactioner := &fakeReactioner{}

	w := buildWorker(t, defaultCfg(), store, analyzer, budget, tg, reactioner)
	w.Tick(context.Background())

	if analyzer.calls != 1 {
		t.Fatalf("AI must be called exactly once; calls=%d", analyzer.calls)
	}
	if len(store.finished) != 1 || store.finished[0].Verdict != "confirmed_signal" {
		t.Fatalf("must persist succeeded verdict; got %+v", store.finished)
	}
	if len(tg.sent) != 1 || tg.sent[0].Surface != telegram.SurfaceMarketCloseReview {
		t.Fatalf("must dispatch admin Telegram with SurfaceMarketCloseReview; got %+v", tg.sent)
	}
	if budget.charged != 0.04 {
		t.Errorf("budget.Charge expected 0.04; got %v", budget.charged)
	}
	// Reactions: alert IDs 1 and 2 — alert 999 was AI-invented and
	// must NOT result in a reaction call. The fakeStore mock does
	// NOT carry the parser pipeline; the worker passes the raw
	// response to applyReactions, which filters on TelegramMessageID
	// presence + byID map — alert 999 isn't in the alerts slice so
	// it's silently dropped by the worker too.
	if len(reactioner.reactions) != 2 {
		t.Fatalf("expected exactly 2 reactions; got %d (%+v)", len(reactioner.reactions), reactioner.reactions)
	}
	if reactioner.reactions[0].ChatID != "-100signal" {
		t.Errorf("reactions must target signal chat; got %s", reactioner.reactions[0].ChatID)
	}
}

// Admin Telegram surface routing: the worker hands the router a
// telegram.Message with SurfaceMarketCloseReview. Combined with
// the Router test for admin destination, this proves the body
// can never reach the signal chat.
func TestTick_AdminTelegramSurfaceLabel(t *testing.T) {
	store := newFakeStore()
	store.candidates = []repository.MarketCloseReviewCandidate{candidateMarket(7, "0xCOND")}
	store.alerts[7] = []repository.Alert{sampleAlert(1, 7, 100)}
	analyzer := &fakeAnalyzer{
		response: openai.MarketCloseReviewResponse{
			Verdict: "inconclusive", Confidence: 0.3, AdminSummary: "thin",
		},
	}
	tg := &fakeTG{}
	w := buildWorker(t, defaultCfg(), store, analyzer, &fakeBudget{allowed: true}, tg, nil)
	w.Tick(context.Background())
	if len(tg.sent) != 1 {
		t.Fatalf("admin send count: %d", len(tg.sent))
	}
	if tg.sent[0].Surface != telegram.SurfaceMarketCloseReview {
		t.Errorf("must label admin body with SurfaceMarketCloseReview; got %s", tg.sent[0].Surface)
	}
}

// AI failure → finished as failed, no Telegram dispatch.
func TestTick_AIFailure_RecordedAsFailedNoTelegram(t *testing.T) {
	store := newFakeStore()
	store.candidates = []repository.MarketCloseReviewCandidate{candidateMarket(7, "0xCOND")}
	store.alerts[7] = []repository.Alert{sampleAlert(1, 7, 100)}
	analyzer := &fakeAnalyzer{err: errors.New("provider 500")}
	tg := &fakeTG{}
	w := buildWorker(t, defaultCfg(), store, analyzer, &fakeBudget{allowed: true}, tg, nil)
	w.Tick(context.Background())
	if len(store.failures) != 1 {
		t.Fatalf("AI failure must record failed row; got %+v", store.failures)
	}
	if len(tg.sent) != 0 {
		t.Fatalf("admin Telegram must NOT fire on AI failure; got %d", len(tg.sent))
	}
}

// Reaction failure does NOT fail the review (admin Telegram still
// fires, persistence still succeeded).
func TestTick_ReactionFailureDoesNotFailReview(t *testing.T) {
	store := newFakeStore()
	store.candidates = []repository.MarketCloseReviewCandidate{candidateMarket(7, "0xCOND")}
	store.alerts[7] = []repository.Alert{sampleAlert(1, 7, 100)}
	analyzer := &fakeAnalyzer{
		response: openai.MarketCloseReviewResponse{
			Verdict: "confirmed_signal", Confidence: 0.6,
			ReactionPlan: []openai.MarketCloseReviewReactionPlan{
				{AlertID: 1, Reaction: "success", Reason: "won"},
			},
		},
	}
	reactioner := &fakeReactioner{err: errors.New("reaction boom")}
	tg := &fakeTG{}
	w := buildWorker(t, defaultCfg(), store, analyzer, &fakeBudget{allowed: true}, tg, reactioner)
	w.Tick(context.Background())
	if len(store.finished) != 1 {
		t.Fatalf("succeeded review must persist even when reaction fails; got %+v", store.finished)
	}
	if len(tg.sent) != 1 {
		t.Fatalf("admin Telegram must still fire; got %d", len(tg.sent))
	}
}

// Disabled worker → Run() returns immediately, no DB calls.
func TestRun_DisabledWorkerNoOp(t *testing.T) {
	store := newFakeStore()
	cfg := defaultCfg()
	cfg.Enabled = false
	w := buildWorker(t, cfg, store, &fakeAnalyzer{}, nil, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w.Run(ctx)
	if len(store.insertedIDs) != 0 {
		t.Fatalf("disabled worker must not act; insertedIDs=%v", store.insertedIDs)
	}
}
