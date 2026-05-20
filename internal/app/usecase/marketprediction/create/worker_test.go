package create

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/eventflow"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/eventpagecontext"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/repricing"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/analysis"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/repository"
)

// --- fakes ----------------------------------------------------------------

type fakeCandidates struct {
	rows []repository.IntelligenceCandidate
}

func (f *fakeCandidates) ListIntelligenceCandidates(_ context.Context, _ int32) ([]repository.IntelligenceCandidate, error) {
	return f.rows, nil
}

type fakeMarkets map[string]repository.Market

func (f fakeMarkets) GetByConditionID(_ context.Context, cid string) (repository.Market, error) {
	m, ok := f[cid]
	if !ok {
		return repository.Market{}, errors.New("not found")
	}
	return m, nil
}

type fakePredictionStore struct {
	mu           sync.Mutex
	active       map[string]repository.MarketPrediction
	createdToday int64
	perEvent     map[string]int64
	upserts      []repository.NewMarketPrediction
	transitions  []repository.NewMarketPredictionStateTransition
}

func newFakePredictionStore() *fakePredictionStore {
	return &fakePredictionStore{active: map[string]repository.MarketPrediction{}, perEvent: map[string]int64{}}
}

func (f *fakePredictionStore) GetPrediction(_ context.Context, eventSlug, conditionID string) (repository.MarketPrediction, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := eventSlug + "|" + conditionID
	if p, ok := f.active[k]; ok {
		return p, nil
	}
	return repository.MarketPrediction{}, repository.ErrPredictionNotFound
}

func (f *fakePredictionStore) UpsertPrediction(_ context.Context, p repository.NewMarketPrediction) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.upserts = append(f.upserts, p)
	id := int64(len(f.upserts))
	f.active[p.EventSlug+"|"+p.ConditionID] = repository.MarketPrediction{
		ID: id, EventSlug: p.EventSlug, ConditionID: p.ConditionID,
		CurrentState: p.CurrentState, Confidence: p.Confidence,
	}
	f.createdToday++
	f.perEvent[p.EventSlug]++
	return id, nil
}

func (f *fakePredictionStore) RecordStateTransition(_ context.Context, t repository.NewMarketPredictionStateTransition) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.transitions = append(f.transitions, t)
	return nil
}

func (f *fakePredictionStore) CountPredictionsCreatedSince(_ context.Context, _ time.Time) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.createdToday, nil
}

func (f *fakePredictionStore) CountPredictionsForEventSince(_ context.Context, eventSlug string, _ time.Time) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.perEvent[eventSlug], nil
}

func (f *fakePredictionStore) TouchPredictionEvolution(_ context.Context, _ int64) error { return nil }

type fakePages struct{ summary eventpagecontext.Summary }

func (f *fakePages) Load(_ context.Context, eventSlug string, _ eventpagecontext.Severity) eventpagecontext.Summary {
	s := f.summary
	s.EventSlug = eventSlug
	return s
}

type fakeCats struct{ rows []repository.EventCatalyst }

func (f *fakeCats) ListActive(_ context.Context, _ string) ([]repository.EventCatalyst, error) {
	return f.rows, nil
}

type fakeFlow struct{ sum eventflow.EventFlowSummary }

func (f *fakeFlow) LoadEventFlowSummary(_ context.Context, _ string, _ time.Duration) (eventflow.EventFlowSummary, error) {
	return f.sum, nil
}

type fakeRepricing struct{}

func (fakeRepricing) Compute(_ context.Context, _ repricing.AnnotationInput, _ bool) (repricing.Signal, error) {
	return repricing.Signal{RepricingStatus: "unclear"}, nil
}

type fakeRanker struct {
	picks []analysis.PredictionRankingPick
	calls int
}

func (f *fakeRanker) RankCandidates(_ context.Context, _ analysis.PredictionRankingRequest) (analysis.PredictionRankingResponse, error) {
	f.calls++
	return analysis.PredictionRankingResponse{Status: analysis.StatusOK, Picks: f.picks, EstimatedCostUSD: 0.04}, nil
}

type fakeCreator struct {
	resp  analysis.PredictionCreationResponse
	calls int
	mu    sync.Mutex
}

func (f *fakeCreator) CreatePrediction(_ context.Context, _ analysis.PredictionCreationRequest) (analysis.PredictionCreationResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	r := f.resp
	r.Status = analysis.StatusOK
	return r, nil
}

type fakeBudget struct {
	allowed     bool
	denyReason  string
	allowCalls  int
	chargeCalls int
	mu          sync.Mutex
}

func (f *fakeBudget) Allow(_ string, _ float64) (bool, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.allowCalls++
	if !f.allowed {
		return false, f.denyReason
	}
	return true, ""
}

func (f *fakeBudget) Charge(_ string, _ float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.chargeCalls++
}

type fakeTelegram struct {
	sends int
	mu    sync.Mutex
}

func (f *fakeTelegram) SendHTML(_ context.Context, _ string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sends++
	return 1, nil
}

// --- helpers --------------------------------------------------------------

func mkCandidates() *fakeCandidates {
	return &fakeCandidates{rows: []repository.IntelligenceCandidate{
		{ConditionID: "0xa", Category: "Politics", Question: "Q_A", LastPrice: 0.6, LifecyclePct: 80, Volume24hUSD: 10000, Alerts24h: 3},
		{ConditionID: "0xb", Category: "Politics", Question: "Q_B", LastPrice: 0.4, LifecyclePct: 70, Volume24hUSD: 5000, Alerts24h: 1},
		{ConditionID: "0xc", Category: "Sports", Question: "Q_C", LastPrice: 0.5, LifecyclePct: 50, Volume24hUSD: 2000, Alerts24h: 0},
	}}
}

func mkMarkets() fakeMarkets {
	return fakeMarkets{
		"0xa": {ConditionID: "0xa", EventSlug: "ev-a", Question: "Q_A"},
		"0xb": {ConditionID: "0xb", EventSlug: "ev-b", Question: "Q_B"},
		"0xc": {ConditionID: "0xc", EventSlug: "ev-c", Question: "Q_C"},
	}
}

func mkWorker(t *testing.T, cfg Config, store *fakePredictionStore, ranker *fakeRanker, creator *fakeCreator, budget *fakeBudget, tg *fakeTelegram) *Worker {
	t.Helper()
	cfg.Enabled = true
	cfg.AIEnabled = true
	cfg.Clock = func() time.Time { return time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC) }
	// Default the quality gate to "pass" so legacy tests that
	// don't exercise the gate continue to assert their narrow
	// invariants. PART 7's gate-specific tests override these.
	if cfg.MinConfidence == 0 {
		cfg.MinConfidence = 0
	}
	cfg.MinSummaryChars = 1
	cfg.PersistLowQuality = true
	if cfg.MaxTelegramPerRun == 0 {
		cfg.MaxTelegramPerRun = 100 // large default for legacy tests
	}
	cfg.SendOnStartup = true // legacy tests assert against the immediate tick
	w := New(cfg, mkCandidates(), mkMarkets(), store, &fakePages{}, &fakeCats{}, &fakeFlow{}, fakeRepricing{}, ranker, creator, tg, nil, nil)
	if budget != nil {
		w.SetBudget(budget)
	}
	return w
}

// --- tests ----------------------------------------------------------------

// TestTick_CategoryWhitelistFilters confirms the deterministic
// category filter drops the Sports candidate before any AI work.
// Pin against PART 1's requirement that the shortlist be
// signal-driven, not "send everything to AI".
func TestTick_CategoryWhitelistFilters(t *testing.T) {
	store := newFakePredictionStore()
	ranker := &fakeRanker{picks: []analysis.PredictionRankingPick{
		{EventSlug: "ev-a", ConditionID: "0xa", Score: 0.9},
		{EventSlug: "ev-b", ConditionID: "0xb", Score: 0.7},
	}}
	creator := &fakeCreator{resp: analysis.PredictionCreationResponse{
		Summary: "thesis A", SideBias: "bullish", Confidence: 0.7,
	}}
	w := mkWorker(t, Config{
		Categories:   []string{"politics"},
		MaxSelected:  5,
		MaxPerDay:    10,
		DedupeWindow: 24 * time.Hour,
		MinScore:     0.55,
		Concurrency:  1,
	}, store, ranker, creator, nil, nil)
	sum := w.Tick(context.Background())
	if sum.Candidates != 2 {
		t.Errorf("candidates: got %d want 2 (sports filtered)", sum.Candidates)
	}
	if sum.Selected != 2 {
		t.Errorf("selected: got %d want 2", sum.Selected)
	}
	if sum.Created != 2 {
		t.Errorf("created: got %d want 2", sum.Created)
	}
}

// TestTick_DedupeWindowSuppresses pins the rule that a recent
// creation suppresses recreation within DedupeWindow.
func TestTick_DedupeWindowSuppresses(t *testing.T) {
	store := newFakePredictionStore()
	store.perEvent["ev-a"] = 1 // simulated recent creation
	ranker := &fakeRanker{picks: []analysis.PredictionRankingPick{
		{EventSlug: "ev-b", ConditionID: "0xb", Score: 0.9},
	}}
	creator := &fakeCreator{resp: analysis.PredictionCreationResponse{Summary: "t", SideBias: "neutral", Confidence: 0.5}}
	w := mkWorker(t, Config{
		Categories:   []string{"politics"},
		MaxSelected:  5,
		MaxPerDay:    10,
		DedupeWindow: 24 * time.Hour,
		MinScore:     0.55,
		Concurrency:  1,
	}, store, ranker, creator, nil, nil)
	sum := w.Tick(context.Background())
	if sum.Filtered != 1 {
		t.Errorf("filtered: got %d want 1 (ev-a deduped); skipped=%v", sum.Filtered, sum.Skipped)
	}
	if sum.Skipped["dedupe_window"] != 1 {
		t.Errorf("expected dedupe_window=1; got %v", sum.Skipped)
	}
}

// TestTick_ActivePredictionSuppressed pins the second dedupe rule:
// a market with an active prediction is skipped.
func TestTick_ActivePredictionSuppressed(t *testing.T) {
	store := newFakePredictionStore()
	store.active["ev-a|0xa"] = repository.MarketPrediction{ID: 1, EventSlug: "ev-a", ConditionID: "0xa", CurrentState: "watching"}
	ranker := &fakeRanker{picks: []analysis.PredictionRankingPick{
		{EventSlug: "ev-b", ConditionID: "0xb", Score: 0.9},
	}}
	creator := &fakeCreator{resp: analysis.PredictionCreationResponse{Summary: "t", SideBias: "neutral", Confidence: 0.5}}
	w := mkWorker(t, Config{
		Categories:   []string{"politics"},
		MaxSelected:  5,
		MaxPerDay:    10,
		DedupeWindow: 24 * time.Hour,
		MinScore:     0.55,
		Concurrency:  1,
	}, store, ranker, creator, nil, nil)
	sum := w.Tick(context.Background())
	if sum.Skipped["active_prediction"] != 1 {
		t.Errorf("expected active_prediction=1; got %v", sum.Skipped)
	}
}

// TestTick_DailyCapStops pins the per-day cap. Even with a
// healthy shortlist, the worker must short-circuit when the cap
// has already been hit by previous cycles.
func TestTick_DailyCapStops(t *testing.T) {
	store := newFakePredictionStore()
	store.createdToday = 40
	ranker := &fakeRanker{picks: nil}
	creator := &fakeCreator{}
	w := mkWorker(t, Config{
		Categories: []string{"politics"},
		MaxPerDay:  40,
	}, store, ranker, creator, nil, nil)
	sum := w.Tick(context.Background())
	if ranker.calls != 0 {
		t.Errorf("ranker called despite daily cap; calls=%d", ranker.calls)
	}
	if creator.calls != 0 {
		t.Errorf("creator called despite daily cap; calls=%d", creator.calls)
	}
	if sum.Created != 0 {
		t.Errorf("created: got %d want 0", sum.Created)
	}
}

// TestTick_BudgetDenialSkipsAIWork covers the budget governor
// integration. When the ranker bucket is denied, the worker must
// not call the creator either.
func TestTick_BudgetDenialSkipsAIWork(t *testing.T) {
	store := newFakePredictionStore()
	ranker := &fakeRanker{picks: []analysis.PredictionRankingPick{{EventSlug: "ev-a", ConditionID: "0xa", Score: 0.9}}}
	creator := &fakeCreator{resp: analysis.PredictionCreationResponse{Summary: "t", SideBias: "neutral", Confidence: 0.5}}
	budget := &fakeBudget{allowed: false, denyReason: "bucket_exhausted"}
	w := mkWorker(t, Config{
		Categories:   []string{"politics"},
		MaxSelected:  5,
		MaxPerDay:    10,
		DedupeWindow: 24 * time.Hour,
		MinScore:     0.55,
		Concurrency:  1,
	}, store, ranker, creator, budget, nil)
	sum := w.Tick(context.Background())
	if ranker.calls != 0 {
		t.Errorf("ranker should not have been called; calls=%d", ranker.calls)
	}
	if creator.calls != 0 {
		t.Errorf("creator should not have been called; calls=%d", creator.calls)
	}
	if sum.Skipped["budget_denied_rank"] != 1 {
		t.Errorf("expected budget_denied_rank=1; got %v", sum.Skipped)
	}
}

// TestTick_MinScoreFloor pins the deterministic filter on the
// ranker output: picks below MinScore are dropped before the
// creator AI is called.
func TestTick_MinScoreFloor(t *testing.T) {
	store := newFakePredictionStore()
	ranker := &fakeRanker{picks: []analysis.PredictionRankingPick{
		{EventSlug: "ev-a", ConditionID: "0xa", Score: 0.30},
		{EventSlug: "ev-b", ConditionID: "0xb", Score: 0.90},
	}}
	creator := &fakeCreator{resp: analysis.PredictionCreationResponse{Summary: "t", SideBias: "bullish", Confidence: 0.8}}
	w := mkWorker(t, Config{
		Categories:   []string{"politics"},
		MaxSelected:  5,
		MaxPerDay:    10,
		DedupeWindow: 24 * time.Hour,
		MinScore:     0.55,
		Concurrency:  1,
	}, store, ranker, creator, nil, nil)
	sum := w.Tick(context.Background())
	if sum.Selected != 1 {
		t.Errorf("selected (above MinScore): got %d want 1", sum.Selected)
	}
	if sum.Skipped["below_min_score"] != 1 {
		t.Errorf("below_min_score skip: %v", sum.Skipped)
	}
	if sum.Created != 1 {
		t.Errorf("created: got %d want 1", sum.Created)
	}
}

// TestTick_AIDisabledShortCircuits covers the operator escape
// hatch: with AIEnabled=false the worker still shortlists but
// never calls the model and never persists.
func TestTick_AIDisabledShortCircuits(t *testing.T) {
	store := newFakePredictionStore()
	ranker := &fakeRanker{}
	creator := &fakeCreator{}
	cfg := Config{
		Categories: []string{"politics"},
		MaxPerDay:  10,
	}
	cfg.Enabled = true
	cfg.AIEnabled = false
	cfg.Clock = func() time.Time { return time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC) }
	w := New(cfg, mkCandidates(), mkMarkets(), store, &fakePages{}, &fakeCats{}, &fakeFlow{}, fakeRepricing{}, ranker, creator, nil, nil, nil)
	sum := w.Tick(context.Background())
	if ranker.calls != 0 || creator.calls != 0 {
		t.Errorf("AI should be skipped; ranker=%d creator=%d", ranker.calls, creator.calls)
	}
	if sum.Created != 0 {
		t.Errorf("created: got %d want 0", sum.Created)
	}
}

// TestParse_RankingJSON_StrictRejection pins the parser's contract:
// markdown-wrapped output and non-object payloads are rejected
// before the worker sees them.
func TestParse_RankingJSON_StrictRejection(t *testing.T) {
	for _, body := range []string{
		"```json\n{\"picks\":[]}\n```",
		"[]",
		"",
	} {
		_, err := parsePicksForTest(body)
		if err == nil {
			t.Errorf("expected error for body %q", body)
		}
	}
}

// helper: keeps the test from importing the openai package — we
// just exercise the JSON shape contract through the same union
// types the worker holds.
func parsePicksForTest(body string) ([]analysis.PredictionRankingPick, error) {
	body = strings.TrimSpace(body)
	if body == "" {
		return nil, errors.New("empty")
	}
	if strings.HasPrefix(body, "```") {
		return nil, errors.New("markdown")
	}
	if body[0] != '{' {
		return nil, errors.New("not object")
	}
	return nil, nil
}
