// PART 15 / PART 24: behavioural tests for the v11.0 Hourly News
// Intelligence worker. Hermetic — no network, no DB. Every external
// dependency is replaced by a fake.
package newsintel

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
)

// --- Fakes ----------------------------------------------------------------

type fakeAnnotations struct {
	rows []repository.EventAnnotation
	err  error
	mu   sync.Mutex
	hits int
}

func (f *fakeAnnotations) ListAnnotationsSince(ctx context.Context, since time.Time, limit int32) ([]repository.EventAnnotation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hits++
	if f.err != nil {
		return nil, f.err
	}
	return f.rows, nil
}

type fakeMarkets struct {
	rows map[string][]repository.EventPageMarketRow
}

func (f *fakeMarkets) ListLatestEventMarkets(ctx context.Context, eventSlug string) ([]repository.EventPageMarketRow, error) {
	return f.rows[eventSlug], nil
}

type fakeStore struct {
	mu sync.Mutex

	processed map[string]int64 // hash -> last_run_id

	runs           []repository.NewsIntelRunInsert
	finishes       []repository.NewsIntelRunFinish
	decisions      []repository.NewsIntelDecision
	nextRunID      int64
	insertRunErr   error
	filterErr      error
	markErr        error
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		processed: make(map[string]int64),
		nextRunID: 1,
	}
}

func (s *fakeStore) InsertRun(ctx context.Context, in repository.NewsIntelRunInsert) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.insertRunErr != nil {
		return 0, s.insertRunErr
	}
	s.runs = append(s.runs, in)
	id := s.nextRunID
	s.nextRunID++
	return id, nil
}

func (s *fakeStore) FinishRun(ctx context.Context, in repository.NewsIntelRunFinish) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.finishes = append(s.finishes, in)
	return nil
}

func (s *fakeStore) InsertDecision(ctx context.Context, d repository.NewsIntelDecision) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.decisions = append(s.decisions, d)
	return nil
}

func (s *fakeStore) FilterUnprocessed(ctx context.Context, hashes []string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.filterErr != nil {
		return nil, s.filterErr
	}
	out := make([]string, 0, len(hashes))
	for _, h := range hashes {
		if _, ok := s.processed[h]; !ok {
			out = append(out, h)
		}
	}
	return out, nil
}

func (s *fakeStore) MarkProcessed(ctx context.Context, hash, eventSlug, title string, runID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.markErr != nil {
		return s.markErr
	}
	s.processed[hash] = runID
	return nil
}

func (s *fakeStore) TouchProcessed(ctx context.Context, hash string) error {
	return nil
}

type fakeAnalyzer struct {
	mu      sync.Mutex
	calls   int
	result  openai.NewsIntelAIResult
	err     error
}

func (f *fakeAnalyzer) EvaluateHourlyNewsIntel(ctx context.Context, req openai.NewsIntelAIRequest) (openai.NewsIntelAIResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.result, f.err
}

type fakeTG struct {
	mu     sync.Mutex
	sent   []string
	failOn int
	hits   int
}

func (f *fakeTG) SendHTML(ctx context.Context, chatID, text string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hits++
	if f.failOn != 0 && f.hits == f.failOn {
		return 0, errors.New("telegram boom")
	}
	f.sent = append(f.sent, text)
	return int64(f.hits), nil
}

// --- Helpers --------------------------------------------------------------

func sampleAnnotation(slug, hash, title string) repository.EventAnnotation {
	return repository.EventAnnotation{
		EventSlug:   slug,
		ItemHash:    hash,
		Title:       title,
		Summary:     "summary for " + title,
		Source:      "wire",
		Timestamp:   time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC),
		FirstSeenAt: time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC),
		LastSeenAt:  time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC),
	}
}

func sampleMarket(slug, cond, title string) repository.EventPageMarketRow {
	return repository.EventPageMarketRow{
		EventSlug:      slug,
		ConditionID:    cond,
		Question:       title,
		GroupItemTitle: title,
	}
}

func buildWorker(t *testing.T, cfg Config, ann AnnotationSource, mks EventMarketSource, st IntelStore, ai Analyzer, tg TelegramSender) *Worker {
	t.Helper()
	met := metrics.New()
	w := New(cfg, ann, mks, st, ai, tg, met, nil)
	return w
}

// --- Tests ----------------------------------------------------------------

// PART 24 / PART 25: zero new news must result in NO AI call and NO
// Telegram. The cycle still persists a skipped run row so an operator
// can see the worker is alive.
func TestTick_NoNews_NoAI_NoTelegram(t *testing.T) {
	ann := &fakeAnnotations{rows: nil}
	st := newFakeStore()
	ai := &fakeAnalyzer{}
	tg := &fakeTG{}
	w := buildWorker(t, Config{
		Enabled: true, AIEnabled: true, SendTelegram: true,
		Interval: time.Hour, Lookback: time.Hour, MaxItems: 100,
		MaxMarketsPerItem: 5, MaxSelected: 8, MinConfidence: 0.6,
		DedupeEnabled: true, ChatID: "123",
	}, ann, &fakeMarkets{}, st, ai, tg)
	w.Tick(context.Background())

	if ai.calls != 0 {
		t.Fatalf("expected 0 AI calls, got %d", ai.calls)
	}
	if len(tg.sent) != 0 {
		t.Fatalf("expected 0 telegram sends, got %d", len(tg.sent))
	}
	if len(st.runs) != 1 || st.runs[0].AIStatus != "skipped" {
		t.Fatalf("expected one skipped run row, got %+v", st.runs)
	}
}

// PART 5 / PART 24: all candidate hashes already processed → no AI,
// no Telegram. Worker still records a skipped run.
func TestTick_AllItemsAlreadyProcessed_NoAI(t *testing.T) {
	ann := &fakeAnnotations{rows: []repository.EventAnnotation{
		sampleAnnotation("election-2026", "h1", "Poll moves +3"),
	}}
	mks := &fakeMarkets{rows: map[string][]repository.EventPageMarketRow{
		"election-2026": {sampleMarket("election-2026", "0xCOND", "Win 2026?")},
	}}
	st := newFakeStore()
	st.processed["h1"] = 99
	ai := &fakeAnalyzer{}
	tg := &fakeTG{}
	w := buildWorker(t, Config{
		Enabled: true, AIEnabled: true, SendTelegram: true,
		Interval: time.Hour, Lookback: time.Hour, MaxItems: 100,
		MaxMarketsPerItem: 5, MaxSelected: 8, MinConfidence: 0.6,
		DedupeEnabled: true, ChatID: "123",
	}, ann, mks, st, ai, tg)
	w.Tick(context.Background())

	if ai.calls != 0 {
		t.Fatalf("expected 0 AI calls when all items already processed, got %d", ai.calls)
	}
	if len(tg.sent) != 0 {
		t.Fatalf("expected 0 telegram sends, got %d", len(tg.sent))
	}
}

// PART 8 / PART 9 / PART 25: sentinel response → no Telegram, run row
// stamped with sentinel code, items marked processed (so the same
// item set doesn't burn another AI call next cycle).
func TestTick_SentinelResponse_NoTelegram_ItemsProcessed(t *testing.T) {
	ann := &fakeAnnotations{rows: []repository.EventAnnotation{
		sampleAnnotation("election-2026", "h1", "Poll moves +3"),
	}}
	mks := &fakeMarkets{rows: map[string][]repository.EventPageMarketRow{
		"election-2026": {sampleMarket("election-2026", "0xCOND", "Win 2026?")},
	}}
	st := newFakeStore()
	ai := &fakeAnalyzer{
		result: openai.NewsIntelAIResult{
			Status:   "ok",
			Sentinel: "AiAnsweredNotFoundNoticeable",
		},
	}
	tg := &fakeTG{}
	w := buildWorker(t, Config{
		Enabled: true, AIEnabled: true, SendTelegram: true,
		Interval: time.Hour, Lookback: time.Hour, MaxItems: 100,
		MaxMarketsPerItem: 5, MaxSelected: 8, MinConfidence: 0.6,
		DedupeEnabled: true, ChatID: "123",
	}, ann, mks, st, ai, tg)
	w.Tick(context.Background())

	if ai.calls != 1 {
		t.Fatalf("expected exactly 1 AI call, got %d", ai.calls)
	}
	if len(tg.sent) != 0 {
		t.Fatalf("sentinel response must NOT send telegram, got %d msgs", len(tg.sent))
	}
	if _, ok := st.processed["h1"]; !ok {
		t.Fatalf("sentinel response must still mark items as processed (avoid re-burning AI)")
	}
	// Run-finish should carry the sentinel code.
	found := false
	for _, f := range st.finishes {
		if f.SentinelCode == "AiAnsweredNotFoundNoticeable" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected sentinel code stamped on run finish, got: %+v", st.finishes)
	}
}

// PART 7 / PART 10: real selected[] → Telegram sent, decision rows
// persisted, items marked processed, run row stamped ok.
func TestTick_ActionableResult_PersistsAndSends(t *testing.T) {
	ann := &fakeAnnotations{rows: []repository.EventAnnotation{
		sampleAnnotation("election-2026", "h1", "Major endorsement breaks"),
	}}
	mks := &fakeMarkets{rows: map[string][]repository.EventPageMarketRow{
		"election-2026": {sampleMarket("election-2026", "0xCOND", "Will candidate win?")},
	}}
	st := newFakeStore()
	ai := &fakeAnalyzer{
		result: openai.NewsIntelAIResult{
			Status:   "ok",
			Decision: "actionable",
			Summary:  "endorsement creates fresh repricing window",
			Selected: []openai.NewsIntelAIDecision{
				{
					NewsItemHash:    "h1",
					EventSlug:       "election-2026",
					ConditionID:     "0xCOND",
					MarketTitle:     "Will candidate win?",
					Rank:            1,
					Confidence:      0.82,
					ImpactDirection: "YES_up",
					ExpectedWindow:  "12h",
					WhyItMatters:    "endorser controls demographic that swings the runoff",
					TriggerCondition: "next poll shows +3 movement",
					TradeStance:     "consider",
					TelegramWorthy:  true,
				},
			},
		},
	}
	tg := &fakeTG{}
	w := buildWorker(t, Config{
		Enabled: true, AIEnabled: true, SendTelegram: true,
		Interval: time.Hour, Lookback: time.Hour, MaxItems: 100,
		MaxMarketsPerItem: 5, MaxSelected: 8, MinConfidence: 0.6,
		DedupeEnabled: true, ChatID: "123",
	}, ann, mks, st, ai, tg)
	w.Tick(context.Background())

	if ai.calls != 1 {
		t.Fatalf("expected 1 AI call, got %d", ai.calls)
	}
	if len(st.decisions) != 1 {
		t.Fatalf("expected 1 decision row, got %d", len(st.decisions))
	}
	if len(tg.sent) == 0 {
		t.Fatalf("expected telegram send, got 0")
	}
	if !strings.Contains(tg.sent[0], "News intel") {
		t.Fatalf("telegram body missing expected header: %q", tg.sent[0])
	}
	if !strings.Contains(tg.sent[0], "ACTIONABLE") {
		t.Fatalf("decision header should be ACTIONABLE: %q", tg.sent[0])
	}
}

// PART 10: HTML-escaping the operator-visible boundary. AI-authored
// fields (which are DATA, not instructions) must NOT be able to
// inject markup into the message.
func TestTick_TelegramHTMLEscapesEvilFields(t *testing.T) {
	ann := &fakeAnnotations{rows: []repository.EventAnnotation{
		sampleAnnotation("election-2026", "h1", "<script>alert(1)</script>"),
	}}
	mks := &fakeMarkets{rows: map[string][]repository.EventPageMarketRow{
		"election-2026": {sampleMarket("election-2026", "0xCOND", "<b>untrusted</b>")},
	}}
	st := newFakeStore()
	ai := &fakeAnalyzer{
		result: openai.NewsIntelAIResult{
			Status:   "ok",
			Decision: "watch",
			Summary:  "<i>summary</i>",
			Selected: []openai.NewsIntelAIDecision{
				{
					NewsItemHash:   "h1",
					EventSlug:      "election-2026",
					ConditionID:    "0xCOND",
					MarketTitle:    "<script>X</script>",
					Confidence:     0.7,
					ImpactDirection: "YES_up",
					ExpectedWindow: "12h",
					WhyItMatters:   "<b>injection</b>",
					TradeStance:    "watch",
				},
			},
		},
	}
	tg := &fakeTG{}
	w := buildWorker(t, Config{
		Enabled: true, AIEnabled: true, SendTelegram: true,
		Interval: time.Hour, Lookback: time.Hour, MaxItems: 100,
		MaxMarketsPerItem: 5, MaxSelected: 8, MinConfidence: 0.6,
		DedupeEnabled: true, ChatID: "123",
	}, ann, mks, st, ai, tg)
	w.Tick(context.Background())

	if len(tg.sent) == 0 {
		t.Fatalf("expected a telegram send")
	}
	body := tg.sent[0]
	// Unescaped angle brackets from AI fields must NOT appear in the
	// rendered body — they should have been replaced by &lt;/&gt;.
	if strings.Contains(body, "<script>") {
		t.Fatalf("unescaped <script> tag leaked into Telegram body: %s", body)
	}
	if strings.Contains(body, "<b>injection</b>") {
		t.Fatalf("unescaped <b> from AI text leaked into Telegram body: %s", body)
	}
}

// PART 25: confidence below threshold filters the row out. Empty
// `selected` after filtering must NOT send a Telegram (mirrors the
// sentinel path).
func TestTick_LowConfidence_Filtered_NoTelegram(t *testing.T) {
	ann := &fakeAnnotations{rows: []repository.EventAnnotation{
		sampleAnnotation("election-2026", "h1", "Weak signal"),
	}}
	mks := &fakeMarkets{rows: map[string][]repository.EventPageMarketRow{
		"election-2026": {sampleMarket("election-2026", "0xCOND", "Win 2026?")},
	}}
	st := newFakeStore()
	ai := &fakeAnalyzer{
		result: openai.NewsIntelAIResult{
			Status:   "ok",
			Decision: "watch",
			Summary:  "weak",
			Selected: []openai.NewsIntelAIDecision{
				{
					NewsItemHash:    "h1",
					EventSlug:       "election-2026",
					ConditionID:     "0xCOND",
					Confidence:      0.35, // below threshold
					ImpactDirection: "YES_up",
					ExpectedWindow:  "12h",
				},
			},
		},
	}
	tg := &fakeTG{}
	w := buildWorker(t, Config{
		Enabled: true, AIEnabled: true, SendTelegram: true,
		Interval: time.Hour, Lookback: time.Hour, MaxItems: 100,
		MaxMarketsPerItem: 5, MaxSelected: 8, MinConfidence: 0.6,
		DedupeEnabled: true, ChatID: "123",
	}, ann, mks, st, ai, tg)
	w.Tick(context.Background())

	if len(tg.sent) != 0 {
		t.Fatalf("low-confidence result must not Telegram, got %d msgs", len(tg.sent))
	}
}

// "ignore" decision + SuppressNoEdge=true must NOT Telegram.
func TestTick_IgnoreDecision_Suppressed(t *testing.T) {
	ann := &fakeAnnotations{rows: []repository.EventAnnotation{
		sampleAnnotation("election-2026", "h1", "Already priced"),
	}}
	mks := &fakeMarkets{rows: map[string][]repository.EventPageMarketRow{
		"election-2026": {sampleMarket("election-2026", "0xCOND", "Win 2026?")},
	}}
	st := newFakeStore()
	ai := &fakeAnalyzer{
		result: openai.NewsIntelAIResult{
			Status:   "ok",
			Decision: "ignore",
			Summary:  "already in price",
			Selected: []openai.NewsIntelAIDecision{{
				NewsItemHash: "h1", EventSlug: "election-2026", ConditionID: "0xCOND",
				Confidence: 0.7, ImpactDirection: "unclear", ExpectedWindow: "unclear",
			}},
		},
	}
	tg := &fakeTG{}
	w := buildWorker(t, Config{
		Enabled: true, AIEnabled: true, SendTelegram: true,
		Interval: time.Hour, Lookback: time.Hour, MaxItems: 100,
		MaxMarketsPerItem: 5, MaxSelected: 8, MinConfidence: 0.5,
		DedupeEnabled: true, SuppressNoEdge: true, ChatID: "123",
	}, ann, mks, st, ai, tg)
	w.Tick(context.Background())

	if len(tg.sent) != 0 {
		t.Fatalf("ignore decision with SuppressNoEdge=true must not Telegram")
	}
}

// PART 7 + PART 22: items whose event_slug yields zero affected
// markets are skipped before reaching the AI — the AI never sees
// orphan annotations.
func TestTick_AnnotationsWithoutMarkets_SkippedBeforeAI(t *testing.T) {
	ann := &fakeAnnotations{rows: []repository.EventAnnotation{
		sampleAnnotation("orphan-slug", "h-orphan", "no linked markets"),
	}}
	mks := &fakeMarkets{rows: map[string][]repository.EventPageMarketRow{}}
	st := newFakeStore()
	ai := &fakeAnalyzer{}
	tg := &fakeTG{}
	w := buildWorker(t, Config{
		Enabled: true, AIEnabled: true, SendTelegram: true,
		Interval: time.Hour, Lookback: time.Hour, MaxItems: 100,
		MaxMarketsPerItem: 5, MaxSelected: 8, MinConfidence: 0.6,
		DedupeEnabled: true, ChatID: "123",
	}, ann, mks, st, ai, tg)
	w.Tick(context.Background())

	if ai.calls != 0 {
		t.Fatalf("orphan annotations should not reach the AI")
	}
}
