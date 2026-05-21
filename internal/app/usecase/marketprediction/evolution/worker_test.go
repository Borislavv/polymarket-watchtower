package evolution

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/eventflow"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/eventpagecontext"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/marketprediction"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/repricing"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/analysis"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/repository"
)

// --- fakes ---------------------------------------------------------------

type fakePredictionStore struct {
	mu               sync.Mutex
	predictions      []repository.MarketPrediction
	listErr          error
	upserts          []repository.NewMarketPrediction
	transitions      []repository.NewMarketPredictionStateTransition
	touched          []int64
	decayed          []decayCall
	repricingUpserts []repository.NewRepricingSignal
}

type decayCall struct {
	ID     int64
	Delta  float64
	Floor  float64
	Reason string
}

func (f *fakePredictionStore) ListPredictionsForEvolution(_ context.Context, _ time.Time, _ int32) ([]repository.MarketPrediction, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return append([]repository.MarketPrediction(nil), f.predictions...), nil
}

func (f *fakePredictionStore) UpsertPrediction(_ context.Context, p repository.NewMarketPrediction) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.upserts = append(f.upserts, p)
	return int64(len(f.upserts) + 100), nil
}

func (f *fakePredictionStore) RecordStateTransition(_ context.Context, t repository.NewMarketPredictionStateTransition) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.transitions = append(f.transitions, t)
	return nil
}

func (f *fakePredictionStore) TouchPredictionEvolution(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.touched = append(f.touched, id)
	return nil
}

func (f *fakePredictionStore) ApplyPredictionDecay(_ context.Context, id int64, delta, floor float64, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.decayed = append(f.decayed, decayCall{ID: id, Delta: delta, Floor: floor, Reason: reason})
	return nil
}

func (f *fakePredictionStore) ListRepricingSignals(_ context.Context, _ string, _ int32) ([]repository.RepricingSignal, error) {
	return nil, nil
}

func (f *fakePredictionStore) UpsertRepricingSignal(_ context.Context, s repository.NewRepricingSignal) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.repricingUpserts = append(f.repricingUpserts, s)
	return nil
}

type fakePages struct {
	mu     sync.Mutex
	bySlug map[string]eventpagecontext.Summary
}

func (f *fakePages) Load(_ context.Context, slug string, _ eventpagecontext.Severity) eventpagecontext.Summary {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bySlug[slug]
}

type fakeCatalysts struct {
	bySlug map[string][]repository.EventCatalyst
}

func (f *fakeCatalysts) ListActive(_ context.Context, slug string) ([]repository.EventCatalyst, error) {
	return f.bySlug[slug], nil
}

type fakeFlow struct {
	bySlug map[string]eventflow.EventFlowSummary
}

func (f *fakeFlow) LoadEventFlowSummary(_ context.Context, slug string, _ time.Duration) (eventflow.EventFlowSummary, error) {
	return f.bySlug[slug], nil
}

type fakeRepricing struct {
	mu        sync.Mutex
	res       repricing.Signal
	err       error
	persisted int
}

func (f *fakeRepricing) Compute(_ context.Context, in repricing.AnnotationInput, persist bool) (repricing.Signal, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if persist {
		f.persisted++
	}
	res := f.res
	if res.EventSlug == "" {
		res.EventSlug = in.EventSlug
		res.ConditionID = in.ConditionID
		res.AnnotationHash = in.AnnotationHash
	}
	return res, f.err
}

type fakeAIGen struct {
	mu    sync.Mutex
	calls int
	res   analysis.PredictionEvolutionResponse
	err   error
}

func (f *fakeAIGen) RefreshPredictionThesis(_ context.Context, _ analysis.PredictionEvolutionRequest) (analysis.PredictionEvolutionResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.res, f.err
}

type fakeTelegram struct {
	mu    sync.Mutex
	sends []string
	err   error
}

func (f *fakeTelegram) SendHTML(_ context.Context, _ string, text string) (TelegramResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return TelegramResult{}, f.err
	}
	f.sends = append(f.sends, text)
	return TelegramResult{MessageID: int64(len(f.sends))}, nil
}

func nopLogger() *zerolog.Logger { l := zerolog.Nop(); return &l }

// --- tests ---------------------------------------------------------------

func TestTick_BatchSelectsAndProcessesEach(t *testing.T) {
	preds := []repository.MarketPrediction{
		{ID: 1, EventSlug: "tx", ConditionID: "0xa", Outcome: "Yes", CurrentState: "watching", UpdatedAt: time.Now()},
		{ID: 2, EventSlug: "ga", ConditionID: "0xb", Outcome: "Yes", CurrentState: "watching", UpdatedAt: time.Now()},
	}
	store := &fakePredictionStore{predictions: preds}
	w := New(Config{
		Enabled: true, Interval: 15 * time.Minute,
		BatchSize: 100, Concurrency: 2, Timeout: time.Second,
		AIEnabled: false, // no AI in this test
	},
		store, &fakePages{bySlug: map[string]eventpagecontext.Summary{}},
		&fakeCatalysts{}, &fakeFlow{bySlug: map[string]eventflow.EventFlowSummary{}},
		&fakeRepricing{}, analysis.NoopPredictionEvolutionGenerator{}, nil, nil, nopLogger())
	summary := w.Tick(context.Background())
	if summary.Selected != 2 || len(summary.Results) != 2 {
		t.Fatalf("expected 2 processed, got selected=%d results=%d", summary.Selected, len(summary.Results))
	}
	if len(store.touched) != 2 {
		t.Errorf("expected 2 touches, got %d", len(store.touched))
	}
}

func TestTick_OneFailureDoesNotStopBatch(t *testing.T) {
	preds := []repository.MarketPrediction{
		{ID: 1, EventSlug: "tx", ConditionID: "0xa", CurrentState: "watching", UpdatedAt: time.Now()},
		{ID: 2, EventSlug: "ga", ConditionID: "0xb", CurrentState: "watching", UpdatedAt: time.Now()},
	}
	store := &fakePredictionStore{predictions: preds}
	// fakeRepricing returns an error on every call — should NOT
	// kill the batch.
	w := New(Config{
		Enabled: true, BatchSize: 100, Concurrency: 2, Timeout: time.Second, AIEnabled: false,
	},
		store, &fakePages{bySlug: map[string]eventpagecontext.Summary{}},
		&fakeCatalysts{},
		&fakeFlow{bySlug: map[string]eventflow.EventFlowSummary{}},
		&fakeRepricing{err: errors.New("compute boom")},
		analysis.NoopPredictionEvolutionGenerator{}, nil, nil, nopLogger())
	summary := w.Tick(context.Background())
	if len(summary.Results) != 2 {
		t.Errorf("both predictions must finish despite errors: got %d", len(summary.Results))
	}
}

func TestProcessOne_BlockedWhenActiveCatalyst(t *testing.T) {
	pred := repository.MarketPrediction{
		ID: 1, EventSlug: "tx", ConditionID: "0xa", CurrentState: "watching", UpdatedAt: time.Now(),
	}
	store := &fakePredictionStore{predictions: []repository.MarketPrediction{pred}}
	cats := &fakeCatalysts{bySlug: map[string][]repository.EventCatalyst{
		"tx": {{
			Status: repository.CatalystStatusExpected, Title: "TX runoff", CatalystType: "runoff",
		}},
	}}
	w := New(Config{
		Enabled: true, BatchSize: 100, Concurrency: 1, Timeout: time.Second, AIEnabled: false,
	},
		store, &fakePages{bySlug: map[string]eventpagecontext.Summary{}},
		cats, &fakeFlow{bySlug: map[string]eventflow.EventFlowSummary{}},
		&fakeRepricing{}, analysis.NoopPredictionEvolutionGenerator{}, nil, nil, nopLogger())
	res := w.TickOne(context.Background(), pred, false)
	if res.NewState != marketprediction.StateBlocked {
		t.Errorf("expected blocked, got %q", res.NewState)
	}
	if !res.StateChanged {
		t.Errorf("changed flag must be true")
	}
}

func TestAIGating_SkipsWhenNothingChanged(t *testing.T) {
	pred := repository.MarketPrediction{
		ID: 1, EventSlug: "tx", ConditionID: "0xa", CurrentState: "watching",
		Summary: "prior thesis exists", UpdatedAt: time.Now().Add(-1 * time.Hour),
	}
	store := &fakePredictionStore{predictions: []repository.MarketPrediction{pred}}
	ai := &fakeAIGen{res: analysis.PredictionEvolutionResponse{ThesisUpdate: "x", Status: analysis.StatusOK}}
	w := New(Config{
		Enabled: true, BatchSize: 100, Concurrency: 1, Timeout: time.Second,
		AIEnabled: true, AIMinInterval: 6 * time.Hour, AIMaxPerRun: 10,
	},
		store, &fakePages{bySlug: map[string]eventpagecontext.Summary{}},
		&fakeCatalysts{}, &fakeFlow{bySlug: map[string]eventflow.EventFlowSummary{}},
		&fakeRepricing{}, ai, nil, nil, nopLogger())
	res := w.TickOne(context.Background(), pred, false)
	if res.AIRefreshed {
		t.Errorf("AI must NOT run when nothing changed; ai.calls=%d skip=%q", ai.calls, res.AISkipReason)
	}
	if ai.calls != 0 {
		t.Errorf("expected 0 AI calls, got %d", ai.calls)
	}
}

func TestAIGating_RunsOnStateChange(t *testing.T) {
	pred := repository.MarketPrediction{
		ID: 1, EventSlug: "tx", ConditionID: "0xa", CurrentState: "watching",
		Summary: "prior", UpdatedAt: time.Now().Add(-1 * time.Hour),
	}
	store := &fakePredictionStore{predictions: []repository.MarketPrediction{pred}}
	cats := &fakeCatalysts{bySlug: map[string][]repository.EventCatalyst{
		"tx": {{Status: repository.CatalystStatusExpected, Title: "TX runoff", CatalystType: "runoff"}},
	}}
	ai := &fakeAIGen{res: analysis.PredictionEvolutionResponse{ThesisUpdate: "Updated thesis.", Status: analysis.StatusOK}}
	w := New(Config{
		Enabled: true, BatchSize: 100, Concurrency: 1, Timeout: time.Second,
		AIEnabled: true, AIMinInterval: 6 * time.Hour, AIMaxPerRun: 10,
	},
		store, &fakePages{bySlug: map[string]eventpagecontext.Summary{}},
		cats, &fakeFlow{bySlug: map[string]eventflow.EventFlowSummary{}},
		&fakeRepricing{}, ai, nil, nil, nopLogger())
	res := w.TickOne(context.Background(), pred, false)
	if !res.StateChanged {
		t.Fatalf("expected state change")
	}
	if !res.AIRefreshed {
		t.Errorf("AI must run on state change; skip=%q", res.AISkipReason)
	}
	if ai.calls != 1 {
		t.Errorf("expected 1 AI call, got %d", ai.calls)
	}
}

func TestDecay_AppliedWhenIdle(t *testing.T) {
	pred := repository.MarketPrediction{
		ID: 1, EventSlug: "tx", ConditionID: "0xa", CurrentState: "watching",
		Confidence: 0.7, UpdatedAt: time.Now(),
	}
	store := &fakePredictionStore{predictions: []repository.MarketPrediction{pred}}
	w := New(Config{
		Enabled: true, Interval: 24 * time.Hour, BatchSize: 100, Concurrency: 1,
		Timeout: time.Second, AIEnabled: false,
		DecayEnabled: true, DecayPerDay: 0.10, MinConfidence: 0.10,
	},
		store, &fakePages{bySlug: map[string]eventpagecontext.Summary{}},
		&fakeCatalysts{}, &fakeFlow{bySlug: map[string]eventflow.EventFlowSummary{}},
		&fakeRepricing{}, analysis.NoopPredictionEvolutionGenerator{}, nil, nil, nopLogger())
	res := w.TickOne(context.Background(), pred, false)
	if !res.DecayApplied {
		t.Errorf("decay must apply on idle prediction")
	}
	if len(store.decayed) != 1 {
		t.Fatalf("expected 1 decay call, got %d", len(store.decayed))
	}
	if store.decayed[0].Delta <= 0 || store.decayed[0].Delta > 0.10 {
		t.Errorf("decay delta out of bounds: %v", store.decayed[0].Delta)
	}
}

func TestDecay_NotAppliedWhenBlocked(t *testing.T) {
	pred := repository.MarketPrediction{
		ID: 1, EventSlug: "tx", ConditionID: "0xa", CurrentState: "watching",
		Confidence: 0.7, UpdatedAt: time.Now(),
	}
	store := &fakePredictionStore{predictions: []repository.MarketPrediction{pred}}
	cats := &fakeCatalysts{bySlug: map[string][]repository.EventCatalyst{
		"tx": {{Status: repository.CatalystStatusExpected, Title: "TX runoff", CatalystType: "runoff"}},
	}}
	w := New(Config{
		Enabled: true, BatchSize: 100, Concurrency: 1, Timeout: time.Second, AIEnabled: false,
		DecayEnabled: true, DecayPerDay: 0.10, MinConfidence: 0.10,
	},
		store, &fakePages{bySlug: map[string]eventpagecontext.Summary{}},
		cats, &fakeFlow{bySlug: map[string]eventflow.EventFlowSummary{}},
		&fakeRepricing{}, analysis.NoopPredictionEvolutionGenerator{}, nil, nil, nopLogger())
	res := w.TickOne(context.Background(), pred, false)
	if res.DecayApplied {
		t.Errorf("decay must NOT apply when state changes to blocked")
	}
}

func TestTelegram_OnStateChange(t *testing.T) {
	pred := repository.MarketPrediction{
		ID: 1, EventSlug: "tx", ConditionID: "0xa", CurrentState: "watching", UpdatedAt: time.Now(),
		Summary: "Will Paxton win?",
	}
	store := &fakePredictionStore{predictions: []repository.MarketPrediction{pred}}
	cats := &fakeCatalysts{bySlug: map[string][]repository.EventCatalyst{
		"tx": {{Status: repository.CatalystStatusExpected, Title: "TX runoff", CatalystType: "runoff"}},
	}}
	tg := &fakeTelegram{}
	w := New(Config{
		Enabled: true, BatchSize: 100, Concurrency: 1, Timeout: time.Second, AIEnabled: false,
		SendTelegram: true, TelegramChatID: "42", TelegramCooldown: time.Hour,
	},
		store, &fakePages{bySlug: map[string]eventpagecontext.Summary{}},
		cats, &fakeFlow{bySlug: map[string]eventflow.EventFlowSummary{}},
		&fakeRepricing{}, analysis.NoopPredictionEvolutionGenerator{}, tg, nil, nopLogger())
	res := w.TickOne(context.Background(), pred, false)
	if !res.TelegramSent {
		t.Errorf("Telegram must send on state change")
	}
	if len(tg.sends) != 1 {
		t.Fatalf("expected 1 telegram send, got %d", len(tg.sends))
	}
	// v10.5: title is "PREDICTION UPDATE · blocked" (no market title
	// in the title line — market goes in its own section below).
	if !strings.Contains(tg.sends[0], "<b>PREDICTION UPDATE</b> · blocked") {
		t.Errorf("unexpected telegram body:\n%s", tg.sends[0])
	}
	// Universal v10.5 header MUST be present.
	for _, want := range []string{"<b>Type:</b> prediction_update", "<b>Trigger:</b>", "<b>Strategy:</b>"} {
		if !strings.Contains(tg.sends[0], want) {
			t.Errorf("missing v10.5 header field %q:\n%s", want, tg.sends[0])
		}
	}
}

func TestTelegram_NotSentWithoutChange(t *testing.T) {
	pred := repository.MarketPrediction{
		ID: 1, EventSlug: "tx", ConditionID: "0xa", CurrentState: "watching", UpdatedAt: time.Now(),
	}
	store := &fakePredictionStore{predictions: []repository.MarketPrediction{pred}}
	tg := &fakeTelegram{}
	w := New(Config{
		Enabled: true, BatchSize: 100, Concurrency: 1, Timeout: time.Second, AIEnabled: false,
		SendTelegram: true, TelegramChatID: "42",
	},
		store, &fakePages{bySlug: map[string]eventpagecontext.Summary{}},
		&fakeCatalysts{}, &fakeFlow{bySlug: map[string]eventflow.EventFlowSummary{}},
		&fakeRepricing{}, analysis.NoopPredictionEvolutionGenerator{}, tg, nil, nopLogger())
	_ = w.TickOne(context.Background(), pred, false)
	if len(tg.sends) != 0 {
		t.Errorf("no-change cycle must not send telegram: %d", len(tg.sends))
	}
}

func TestDryRun_NoWrites(t *testing.T) {
	pred := repository.MarketPrediction{
		ID: 1, EventSlug: "tx", ConditionID: "0xa", CurrentState: "watching", UpdatedAt: time.Now(),
	}
	store := &fakePredictionStore{predictions: []repository.MarketPrediction{pred}}
	cats := &fakeCatalysts{bySlug: map[string][]repository.EventCatalyst{
		"tx": {{Status: repository.CatalystStatusExpected, Title: "TX runoff"}},
	}}
	tg := &fakeTelegram{}
	w := New(Config{
		Enabled: true, BatchSize: 100, Concurrency: 1, Timeout: time.Second, AIEnabled: false,
		SendTelegram: true, TelegramChatID: "42", DecayEnabled: true, DecayPerDay: 0.10,
	},
		store, &fakePages{bySlug: map[string]eventpagecontext.Summary{}},
		cats, &fakeFlow{bySlug: map[string]eventflow.EventFlowSummary{}},
		&fakeRepricing{}, analysis.NoopPredictionEvolutionGenerator{}, tg, nil, nopLogger())
	res := w.TickOne(context.Background(), pred, true) // dryRun=true
	if len(store.upserts) != 0 || len(store.transitions) != 0 ||
		len(store.touched) != 0 || len(store.decayed) != 0 {
		t.Errorf("dry-run must produce zero DB writes; got upserts=%d trans=%d touch=%d decay=%d",
			len(store.upserts), len(store.transitions), len(store.touched), len(store.decayed))
	}
	if len(tg.sends) != 0 {
		t.Errorf("dry-run must not send telegram: %d", len(tg.sends))
	}
	// State decision still computed.
	if res.NewState == "" {
		t.Errorf("dry-run still computes state: %+v", res)
	}
}

// Compile-time assertions.
var (
	_ PredictionStore                       = (*fakePredictionStore)(nil)
	_ EventPageRefresher                    = (*fakePages)(nil)
	_ CatalystSource                        = (*fakeCatalysts)(nil)
	_ FlowLoader                            = (*fakeFlow)(nil)
	_ RepricingComputer                     = (*fakeRepricing)(nil)
	_ Telegram                              = (*fakeTelegram)(nil)
	_ analysis.PredictionEvolutionGenerator = (*fakeAIGen)(nil)
)
