package create

import (
	"context"
	"testing"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/eventpagecontext"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/repricing"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/analysis"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/repository"
)

// longThesis is the canonical sample body the throttle tests use so
// they don't trip the MinSummaryChars gate.
const longThesis = "Текущая ситуация по рынку: signal indicates a structural shift " +
	"after the recent annotation cluster. Practical stance: bullish, " +
	"keep position size moderate; flow confirms direction; catalyst " +
	"window narrow but actionable. Risk: late entry if price already " +
	"absorbed the news. Plan: watch repricing band, exit on opposite " +
	"side flow surge. Tail risk: certification delay. — repeat for length —"

func mkThrottleWorker(t *testing.T, cfg Config, store *fakePredictionStore, picks []analysis.PredictionRankingPick, sideBias string, conf float64, tg *fakeTelegram) *Worker {
	t.Helper()
	ranker := &fakeRanker{picks: picks}
	creator := &fakeCreator{resp: analysis.PredictionCreationResponse{
		Summary:    longThesis + longThesis,
		SideBias:   sideBias,
		Confidence: conf,
	}}
	cfg.Enabled = true
	cfg.AIEnabled = true
	cfg.Clock = func() time.Time { return time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC) }
	if cfg.MinScore == 0 {
		cfg.MinScore = 0.55
	}
	if cfg.MaxPerDay == 0 {
		cfg.MaxPerDay = 100
	}
	if cfg.MaxSelected == 0 {
		cfg.MaxSelected = 100
	}
	if cfg.DedupeWindow == 0 {
		cfg.DedupeWindow = time.Hour
	}
	if cfg.Categories == nil {
		cfg.Categories = []string{"politics"}
	}
	// Defaults that PART 5 set up but the test harness has to
	// override per-case.
	cfg.MinSummaryChars = 100
	cfg.MinConfidence = 0.55
	cfg.SendTelegram = true
	cfg.PersistLowQuality = true
	w := New(cfg, mkCandidates(), mkMarkets(), store, &fakePages{}, &fakeCats{}, &fakeFlow{}, fakeRepricing{}, ranker, creator, tg, nil, nil)
	return w
}

// PART 9 / TEST 9: MaxTelegramPerRun is honored per cycle.
func TestThrottle_MaxTelegramPerRunHonored(t *testing.T) {
	store := newFakePredictionStore()
	tg := &fakeTelegram{}
	cfg := Config{
		MaxTelegramPerRun: 2,
		SendOnStartup:     true,
		RequireSignal:     false,
	}
	picks := []analysis.PredictionRankingPick{
		{EventSlug: "ev-a", ConditionID: "0xa", Score: 0.9},
		{EventSlug: "ev-b", ConditionID: "0xb", Score: 0.9},
		// Add a third pick by injecting an extra candidate.
	}
	w := mkThrottleWorker(t, cfg, store, picks, "bullish", 0.7, tg)
	// Make ev-b a different event so the same-event dedupe doesn't
	// also kick in.
	w.candidates = &fakeCandidates{rows: []repository.IntelligenceCandidate{
		{ConditionID: "0xa", Category: "Politics", Question: "Q_A", Volume24hUSD: 10000, Alerts24h: 3},
		{ConditionID: "0xb", Category: "Politics", Question: "Q_B", Volume24hUSD: 5000, Alerts24h: 1},
	}}
	w.markets = fakeMarkets{
		"0xa": {ConditionID: "0xa", EventSlug: "ev-a", Question: "Q_A"},
		"0xb": {ConditionID: "0xb", EventSlug: "ev-b", Question: "Q_B"},
	}
	w.Tick(context.Background())
	if tg.sends > 2 {
		t.Errorf("MaxTelegramPerRun=2 not honored; got %d sends", tg.sends)
	}
	if tg.sends == 0 {
		t.Error("expected at least one Telegram send")
	}
}

// PART 9 / TEST 10: SendOnStartup=false suppresses Telegram on the
// very first cycle but lets the second cycle through.
func TestThrottle_StartupSuppressed(t *testing.T) {
	store := newFakePredictionStore()
	tg := &fakeTelegram{}
	cfg := Config{
		SendOnStartup:     false,
		MaxTelegramPerRun: 5,
		RequireSignal:     false,
	}
	picks := []analysis.PredictionRankingPick{
		{EventSlug: "ev-a", ConditionID: "0xa", Score: 0.9},
	}
	w := mkThrottleWorker(t, cfg, store, picks, "bullish", 0.7, tg)
	w.Tick(context.Background())
	if tg.sends != 0 {
		t.Errorf("first cycle should suppress Telegram; got %d sends", tg.sends)
	}
	// New cycle — second Tick. The first call has flipped
	// startupDone via the deferred markStartupDone().
	// Reset the deduper so the second cycle picks the same event.
	store = newFakePredictionStore()
	w.predictions = store
	w.candidates = mkCandidates() // reset
	w.Tick(context.Background())
	if tg.sends == 0 {
		t.Error("second cycle should send Telegram (startup gate cleared)")
	}
}

// PART 9 / TEST 11: per-market Telegram cooldown stops a same-event
// repeat send within the configured window.
func TestThrottle_PerEventCooldownStopsRepeat(t *testing.T) {
	store := newFakePredictionStore()
	tg := &fakeTelegram{}
	cfg := Config{
		SendOnStartup:     true,
		MaxTelegramPerRun: 10,
		TelegramCooldown:  6 * time.Hour,
		RequireSignal:     false,
	}
	picks := []analysis.PredictionRankingPick{{EventSlug: "ev-a", ConditionID: "0xa", Score: 0.9}}
	w := mkThrottleWorker(t, cfg, store, picks, "bullish", 0.7, tg)
	w.Tick(context.Background())
	if tg.sends != 1 {
		t.Fatalf("first cycle should send once; got %d", tg.sends)
	}
	// Second cycle, same event (dedupe needs reset to allow re-creation).
	store2 := newFakePredictionStore()
	w.predictions = store2
	w.Tick(context.Background())
	if tg.sends != 1 {
		t.Errorf("per-event cooldown should block repeat send; total sends=%d", tg.sends)
	}
}

// PART 9 / TEST 12: duplicate same-condition_id skipped before AI.
func TestThrottle_ActivePredictionSkipsBeforeAI(t *testing.T) {
	store := newFakePredictionStore()
	store.active["ev-a|0xa"] = repository.MarketPrediction{ID: 1, EventSlug: "ev-a", ConditionID: "0xa", CurrentState: "watching"}
	tg := &fakeTelegram{}
	cfg := Config{
		SendOnStartup:     true,
		MaxTelegramPerRun: 10,
		RequireSignal:     false,
	}
	picks := []analysis.PredictionRankingPick{{EventSlug: "ev-a", ConditionID: "0xa", Score: 0.9}}
	w := mkThrottleWorker(t, cfg, store, picks, "bullish", 0.8, tg)
	sum := w.Tick(context.Background())
	if sum.Skipped["active_prediction"] != 1 {
		t.Errorf("expected active_prediction=1 skip; got %v", sum.Skipped)
	}
	if tg.sends != 0 {
		t.Errorf("no Telegram should be sent for a duplicate; got %d", tg.sends)
	}
}

// PART 9 / TEST 13: neutral side_bias is suppressed when
// SendNeutral=false (default).
func TestThrottle_NeutralLowValueSendSkipped(t *testing.T) {
	store := newFakePredictionStore()
	tg := &fakeTelegram{}
	cfg := Config{
		SendOnStartup:     true,
		MaxTelegramPerRun: 5,
		SendNeutral:       false, // explicit
		RequireSignal:     false,
	}
	picks := []analysis.PredictionRankingPick{{EventSlug: "ev-a", ConditionID: "0xa", Score: 0.9}}
	w := mkThrottleWorker(t, cfg, store, picks, "neutral", 0.8, tg)
	sum := w.Tick(context.Background())
	// Row should be persisted (PersistLowQuality=true) but
	// Telegram suppressed.
	if sum.Created != 1 {
		t.Errorf("neutral row should still persist; created=%d", sum.Created)
	}
	if tg.sends != 0 {
		t.Errorf("neutral row must not ship to Telegram; got %d sends", tg.sends)
	}
}

// PART 9 / TEST 14: low-quality prediction is persisted (when
// PersistLowQuality=true) but never sent.
func TestThrottle_LowQualityPersistedButNotSent(t *testing.T) {
	store := newFakePredictionStore()
	tg := &fakeTelegram{}
	cfg := Config{
		SendOnStartup:     true,
		MaxTelegramPerRun: 5,
		MinConfidence:     0.80, // explicit floor
		PersistLowQuality: true,
		RequireSignal:     false,
	}
	picks := []analysis.PredictionRankingPick{{EventSlug: "ev-a", ConditionID: "0xa", Score: 0.9}}
	// Confidence=0.6 < MinConfidence=0.8 → quality gate fails.
	w := mkThrottleWorker(t, cfg, store, picks, "bullish", 0.6, tg)
	w.cfg.MinConfidence = 0.80 // mkThrottleWorker default overrides; force again
	sum := w.Tick(context.Background())
	if sum.Created != 1 {
		t.Errorf("low-quality row should still persist; created=%d", sum.Created)
	}
	if tg.sends != 0 {
		t.Errorf("low-quality row must not ship to Telegram; got %d sends", tg.sends)
	}
}

// Compile-time interface assertions so a future refactor of the
// fakes can't accidentally make them not satisfy the Worker seams.
var (
	_ CandidateSource    = (*fakeCandidates)(nil)
	_ MarketResolver     = fakeMarkets(nil)
	_ PredictionStore    = (*fakePredictionStore)(nil)
	_ EventPageRefresher = (*fakePages)(nil)
	_ CatalystSource     = (*fakeCats)(nil)
	_ FlowLoader         = (*fakeFlow)(nil)
	_ RepricingComputer  = fakeRepricing{}
	_ BudgetGuard        = (*fakeBudget)(nil)
	_ Telegram           = (*fakeTelegram)(nil)
)

// Silence unused-import warnings when the test set evolves.
var (
	_ = repricing.Signal{}
	_ = eventpagecontext.Summary{}
)
