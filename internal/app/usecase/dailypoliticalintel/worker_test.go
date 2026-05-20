package dailypoliticalintel

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/eventpagecontext"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/analysis"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/repository"
)

// --- fakes ---------------------------------------------------------------

type fakeCandidates struct {
	rows []repository.IntelligenceCandidate
}

func (f *fakeCandidates) ListIntelligenceCandidates(_ context.Context, _ int32) ([]repository.IntelligenceCandidate, error) {
	return f.rows, nil
}

type fakeMarkets struct{ byID map[string]repository.Market }

func (f *fakeMarkets) GetByConditionID(_ context.Context, id string) (repository.Market, error) {
	m, ok := f.byID[id]
	if !ok {
		return repository.Market{}, errors.New("not found")
	}
	return m, nil
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

type fakeStore struct {
	mu       sync.Mutex
	upserts  []repository.NewDailyPoliticalIntelReport
	existing map[string]repository.DailyPoliticalIntelReport // by report_date key
}

func (s *fakeStore) UpsertDailyReport(_ context.Context, n repository.NewDailyPoliticalIntelReport) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.upserts = append(s.upserts, n)
	if s.existing == nil {
		s.existing = map[string]repository.DailyPoliticalIntelReport{}
	}
	s.existing[n.ReportDate.Format("2006-01-02")] = repository.DailyPoliticalIntelReport{
		ReportDate:     n.ReportDate,
		AIReportText:   n.AIReportText,
		DeliveryStatus: n.DeliveryStatus,
	}
	return int64(len(s.upserts)), nil
}

func (s *fakeStore) GetDailyReport(_ context.Context, day time.Time) (repository.DailyPoliticalIntelReport, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.existing[day.Format("2006-01-02")]
	if !ok {
		return repository.DailyPoliticalIntelReport{}, repository.ErrDailyReportNotFound
	}
	return row, nil
}

type fakeGen struct {
	res analysis.DailyPoliticalIntelResponse
	err error
}

func (g *fakeGen) GenerateDailyPoliticalIntel(_ context.Context, _ analysis.DailyPoliticalIntelRequest) (analysis.DailyPoliticalIntelResponse, error) {
	return g.res, g.err
}

type fakeTG struct {
	mu    sync.Mutex
	sends []string
	err   error
}

func (t *fakeTG) SendHTML(_ context.Context, _ string, text string) (TelegramResult, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.err != nil {
		return TelegramResult{}, t.err
	}
	t.sends = append(t.sends, text)
	return TelegramResult{MessageID: int64(len(t.sends))}, nil
}

func nopLogger() *zerolog.Logger { l := zerolog.Nop(); return &l }

// --- selection + send ---------------------------------------------------

func TestWorker_Tick_SelectsMarketsAndSends(t *testing.T) {
	candidates := &fakeCandidates{rows: []repository.IntelligenceCandidate{
		{ConditionID: "0xa", Category: "Politics", Question: "Q1", Alerts24h: 5, Volume24hUSD: 100_000, LastPrice: 0.6},
		{ConditionID: "0xb", Category: "Sports"}, // filtered
		{ConditionID: "0xc", Category: "Geopolitics", Question: "Q2", Alerts24h: 3, Volume24hUSD: 50_000, LastPrice: 0.5},
	}}
	markets := &fakeMarkets{byID: map[string]repository.Market{
		"0xa": {EventSlug: "ev-a", Slug: "ma"},
		"0xc": {EventSlug: "ev-c", Slug: "mc"},
	}}
	pages := &fakePages{bySlug: map[string]eventpagecontext.Summary{
		"ev-a": {EventSlug: "ev-a", Annotations: []repository.EventAnnotation{
			{ItemHash: "h1", Title: "Annotation A", Outcome: "Yes"}, {ItemHash: "h2", Title: "Annotation A2", Outcome: "Yes"},
		}},
		"ev-c": {EventSlug: "ev-c"},
	}}
	cats := &fakeCatalysts{bySlug: map[string][]repository.EventCatalyst{
		"ev-a": {{EventSlug: "ev-a", CatalystType: "runoff", Title: "TX runoff", Status: repository.CatalystStatusExpected, Confidence: 0.8}},
	}}
	store := &fakeStore{}
	gen := &fakeGen{res: analysis.DailyPoliticalIntelResponse{
		Status: analysis.StatusOK, ReportText: "Daily political market intelligence\n\nExecutive read: repricing.",
	}}
	tg := &fakeTG{}
	w := New(Config{
		Enabled: true, AIEnabled: true, SendTelegram: true, ChatID: "42",
		MarketLimit: 50, AnnotationsPerMarket: 2,
	}, candidates, markets, pages, cats, store, gen, tg, nil, nopLogger())

	reportDate := time.Date(2026, 5, 20, 0, 0, 0, 0, time.UTC)
	w.Tick(context.Background(), reportDate)

	if len(store.upserts) != 1 {
		t.Fatalf("expected 1 upsert, got %d", len(store.upserts))
	}
	if store.upserts[0].DeliveryStatus != "sent" {
		t.Errorf("delivery_status: got %q want sent", store.upserts[0].DeliveryStatus)
	}
	if len(tg.sends) == 0 {
		t.Fatal("expected at least one telegram send")
	}
	if !strings.Contains(tg.sends[0], "Daily political market intelligence") {
		t.Errorf("header missing in send:\n%s", tg.sends[0])
	}
	if !strings.Contains(tg.sends[0], "Top Polymarket events") {
		t.Errorf("top events block missing:\n%s", tg.sends[0])
	}
}

func TestWorker_Tick_AIFailureMarksRowAndDoesNotSend(t *testing.T) {
	candidates := &fakeCandidates{rows: []repository.IntelligenceCandidate{
		{ConditionID: "0xa", Category: "Politics", Question: "Q1"},
	}}
	markets := &fakeMarkets{byID: map[string]repository.Market{"0xa": {EventSlug: "ev-a"}}}
	pages := &fakePages{bySlug: map[string]eventpagecontext.Summary{"ev-a": {EventSlug: "ev-a"}}}
	store := &fakeStore{}
	gen := &fakeGen{err: errors.New("provider exploded")}
	tg := &fakeTG{}
	w := New(Config{
		Enabled: true, AIEnabled: true, SendTelegram: true, ChatID: "42",
	}, candidates, markets, pages, &fakeCatalysts{}, store, gen, tg, nil, nopLogger())

	w.Tick(context.Background(), time.Date(2026, 5, 20, 0, 0, 0, 0, time.UTC))

	if len(store.upserts) != 1 || store.upserts[0].DeliveryStatus != "ai_failed" {
		t.Errorf("expected ai_failed row, got %+v", store.upserts)
	}
	if len(tg.sends) != 0 {
		t.Errorf("must NOT send on AI failure: %d", len(tg.sends))
	}
}

func TestWorker_Tick_DedupSentSameDay(t *testing.T) {
	candidates := &fakeCandidates{rows: []repository.IntelligenceCandidate{
		{ConditionID: "0xa", Category: "Politics", Question: "Q1"},
	}}
	markets := &fakeMarkets{byID: map[string]repository.Market{"0xa": {EventSlug: "ev-a"}}}
	pages := &fakePages{bySlug: map[string]eventpagecontext.Summary{"ev-a": {EventSlug: "ev-a"}}}
	store := &fakeStore{existing: map[string]repository.DailyPoliticalIntelReport{}}
	// Pre-populate the store with a sent row for today.
	day := time.Date(2026, 5, 20, 0, 0, 0, 0, time.UTC)
	store.existing[day.Format("2006-01-02")] = repository.DailyPoliticalIntelReport{
		ReportDate: day, DeliveryStatus: "sent",
	}
	gen := &fakeGen{res: analysis.DailyPoliticalIntelResponse{Status: analysis.StatusOK, ReportText: "x"}}
	tg := &fakeTG{}
	w := New(Config{
		Enabled: true, AIEnabled: true, SendTelegram: true, ChatID: "42",
	}, candidates, markets, pages, &fakeCatalysts{}, store, gen, tg, nil, nopLogger())
	w.Tick(context.Background(), day)
	if len(store.upserts) != 0 {
		t.Errorf("dedup must skip: %d upserts", len(store.upserts))
	}
	if len(tg.sends) != 0 {
		t.Errorf("dedup must skip telegram: %d sends", len(tg.sends))
	}
}

// --- splitting + rendering ----------------------------------------------

func TestSplitTelegramBody_SplitsOnBlankLineBoundaries(t *testing.T) {
	body := "Section one line.\n\nSection two line.\n\nSection three line."
	chunks := SplitTelegramBody(body, 25)
	if len(chunks) < 2 {
		t.Fatalf("expected splits at section boundaries, got %d: %+v", len(chunks), chunks)
	}
	for _, c := range chunks {
		if len(c) > 25 {
			t.Errorf("chunk exceeds cap: len=%d %q", len(c), c)
		}
	}
}

func TestSplitTelegramBody_NoSplitWhenWithinCap(t *testing.T) {
	chunks := SplitTelegramBody("short body", 4096)
	if len(chunks) != 1 || chunks[0] != "short body" {
		t.Errorf("no-split case wrong: %+v", chunks)
	}
}

func TestRenderTopMarketsBlock_FormatAndOrder(t *testing.T) {
	drift := 0.05
	markets := []analysis.DailyIntelMarket{
		{Question: "Will Paxton win?", LastPrice: 0.62, OneDayPriceChange: &drift, Volume24hUSD: 95000},
		{Question: "Will Cornyn win?", LastPrice: 0.28, Volume24hUSD: 50000},
	}
	out := RenderTopMarketsBlock(markets, 10)
	if !strings.Contains(out, "1. Will Paxton win?") {
		t.Errorf("top events first item wrong:\n%s", out)
	}
	if !strings.Contains(out, "2. Will Cornyn win?") {
		t.Errorf("top events second item wrong:\n%s", out)
	}
	if !strings.Contains(out, "24h=+0.05") {
		t.Errorf("drift rendering wrong:\n%s", out)
	}
}

func TestParseHHMM_ConfigDefaults(t *testing.T) {
	cfg := Config{}
	cfg.applyDefaults()
	if cfg.TimeOfDay != "08:00" {
		t.Errorf("default time_of_day must be 08:00, got %q", cfg.TimeOfDay)
	}
	if cfg.Timezone != "Europe/Tallinn" {
		t.Errorf("default tz must be Europe/Tallinn, got %q", cfg.Timezone)
	}
}

// Compile-time assertions.
var (
	_ CandidateSource    = (*fakeCandidates)(nil)
	_ MarketResolver     = (*fakeMarkets)(nil)
	_ EventPageRefresher = (*fakePages)(nil)
	_ CatalystSource     = (*fakeCatalysts)(nil)
	_ ReportStore        = (*fakeStore)(nil)
	_ Telegram           = (*fakeTG)(nil)
)
