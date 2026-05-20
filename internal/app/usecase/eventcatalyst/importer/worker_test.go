package importer

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
	err  error
}

func (f *fakeCandidates) ListIntelligenceCandidates(_ context.Context, _ int32) ([]repository.IntelligenceCandidate, error) {
	return f.rows, f.err
}

type fakeMarkets struct {
	byID map[string]repository.Market
}

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
	calls  []string
}

func (f *fakePages) Load(_ context.Context, slug string, _ eventpagecontext.Severity) eventpagecontext.Summary {
	f.mu.Lock()
	f.calls = append(f.calls, slug)
	f.mu.Unlock()
	return f.bySlug[slug]
}

type fakeCatalystStore struct {
	mu        sync.Mutex
	rows      map[string][]repository.EventCatalyst // by event_slug
	upserts   []repository.NewEventCatalyst
	statusSet map[int64]repository.EventCatalystStatus
	upsertErr error
}

func newFakeStore() *fakeCatalystStore {
	return &fakeCatalystStore{
		rows:      map[string][]repository.EventCatalyst{},
		statusSet: map[int64]repository.EventCatalystStatus{},
	}
}

func (f *fakeCatalystStore) Upsert(_ context.Context, c repository.NewEventCatalyst) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.upsertErr != nil {
		return f.upsertErr
	}
	f.upserts = append(f.upserts, c)
	return nil
}

func (f *fakeCatalystStore) ListActive(_ context.Context, slug string) ([]repository.EventCatalyst, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []repository.EventCatalyst
	for _, r := range f.rows[slug] {
		if r.Status == repository.CatalystStatusActive || r.Status == repository.CatalystStatusExpected {
			out = append(out, r)
		}
	}
	return out, nil
}

func (f *fakeCatalystStore) ListAll(_ context.Context, slug string) ([]repository.EventCatalyst, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]repository.EventCatalyst(nil), f.rows[slug]...), nil
}

func (f *fakeCatalystStore) SetStatus(_ context.Context, id int64, status repository.EventCatalystStatus) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statusSet[id] = status
	return nil
}

type fakeExtractor struct {
	mu       sync.Mutex
	calls    int
	res      analysis.CatalystExtractionResponse
	err      error
	captured analysis.CatalystExtractionRequest
}

func (f *fakeExtractor) ExtractCatalysts(_ context.Context, req analysis.CatalystExtractionRequest) (analysis.CatalystExtractionResponse, error) {
	f.mu.Lock()
	f.calls++
	f.captured = req
	res := f.res
	err := f.err
	f.mu.Unlock()
	return res, err
}

func nopLogger() *zerolog.Logger {
	l := zerolog.Nop()
	return &l
}

// --- selection -----------------------------------------------------------

func TestImporter_Tick_SelectsCandidatesAndProcessesUniqueSlugs(t *testing.T) {
	candidates := &fakeCandidates{rows: []repository.IntelligenceCandidate{
		{ConditionID: "0xpax", Category: "Politics", Alerts24h: 5, Volume24hUSD: 100_000},
		{ConditionID: "0xpax2", Category: "Politics", Alerts24h: 3, Volume24hUSD: 50_000}, // dup event_slug
		{ConditionID: "0xsports", Category: "Sports", Alerts24h: 2},                       // filtered
		{ConditionID: "0xgeo", Category: "Geopolitics", Alerts24h: 1},
	}}
	markets := &fakeMarkets{byID: map[string]repository.Market{
		"0xpax":  {EventSlug: "texas-runoff"},
		"0xpax2": {EventSlug: "texas-runoff"}, // dup
		"0xgeo":  {EventSlug: "iran-talks"},
	}}
	pages := &fakePages{bySlug: map[string]eventpagecontext.Summary{
		"texas-runoff": {EventSlug: "texas-runoff"},
		"iran-talks":   {EventSlug: "iran-talks"},
	}}
	store := newFakeStore()
	ex := &fakeExtractor{res: analysis.CatalystExtractionResponse{Status: analysis.StatusOK}}

	w := New(Config{
		Enabled:           true,
		AIEnabled:         true,
		CategoryWhitelist: []string{"Politics", "Geopolitics"},
		BatchSize:         50,
		Concurrency:       2,
		MinConfidence:     0.55,
	}, candidates, markets, pages, store, ex, nil, nopLogger())
	w.Tick(context.Background())

	if ex.calls != 2 {
		t.Errorf("extractor must be called once per unique event slug: got %d, want 2", ex.calls)
	}
	if len(pages.calls) != 2 {
		t.Errorf("event page must be refreshed once per unique slug: got %d, want 2", len(pages.calls))
	}
	// Both slugs must be visited (order non-deterministic under
	// Concurrency > 1).
	seen := map[string]bool{}
	for _, s := range pages.calls {
		seen[s] = true
	}
	if !seen["texas-runoff"] || !seen["iran-talks"] {
		t.Errorf("missing expected slug visits: %+v", pages.calls)
	}
}

// --- failure isolation ---------------------------------------------------

func TestImporter_Tick_FetchFailureDoesNotStopBatch(t *testing.T) {
	candidates := &fakeCandidates{rows: []repository.IntelligenceCandidate{
		{ConditionID: "0xa", Category: "Politics", Alerts24h: 2},
		{ConditionID: "0xb", Category: "Politics", Alerts24h: 1},
	}}
	markets := &fakeMarkets{byID: map[string]repository.Market{
		"0xa": {EventSlug: "ev-a"},
		"0xb": {EventSlug: "ev-b"},
	}}
	pages := &fakePages{bySlug: map[string]eventpagecontext.Summary{
		"ev-a": {}, // empty EventSlug → simulates fetch failure
		"ev-b": {EventSlug: "ev-b"},
	}}
	store := newFakeStore()
	ex := &fakeExtractor{res: analysis.CatalystExtractionResponse{Status: analysis.StatusOK}}
	w := New(Config{
		Enabled: true, AIEnabled: true, CategoryWhitelist: []string{"Politics"},
		BatchSize: 50, Concurrency: 2, MinConfidence: 0.55,
	}, candidates, markets, pages, store, ex, nil, nopLogger())
	w.Tick(context.Background())
	if ex.calls != 1 {
		t.Errorf("ev-b must still be processed despite ev-a fetch failure; calls=%d", ex.calls)
	}
}

// --- upsert + filtering --------------------------------------------------

func TestImporter_Tick_UpsertsAboveMinConfidenceOnly(t *testing.T) {
	candidates := &fakeCandidates{rows: []repository.IntelligenceCandidate{
		{ConditionID: "0xa", Category: "Politics", Alerts24h: 1},
	}}
	markets := &fakeMarkets{byID: map[string]repository.Market{"0xa": {EventSlug: "ev"}}}
	pages := &fakePages{bySlug: map[string]eventpagecontext.Summary{"ev": {EventSlug: "ev"}}}
	store := newFakeStore()

	exp := "2026-06-15T12:00:00Z"
	ex := &fakeExtractor{res: analysis.CatalystExtractionResponse{
		Status: analysis.StatusOK,
		Catalysts: []analysis.ExtractedCatalyst{
			{CatalystType: "runoff", Title: "high", Status: "expected", Confidence: 0.80, ExpectedAt: &exp},
			{CatalystType: "debate", Title: "low", Status: "expected", Confidence: 0.30, ExpectedAt: nil},
			{CatalystType: "poll", Title: "boundary", Status: "expected", Confidence: 0.55, ExpectedAt: nil},
		},
	}}
	w := New(Config{
		Enabled: true, AIEnabled: true, CategoryWhitelist: []string{"Politics"},
		BatchSize: 50, Concurrency: 1, MinConfidence: 0.55,
	}, candidates, markets, pages, store, ex, nil, nopLogger())
	w.Tick(context.Background())

	if len(store.upserts) != 2 {
		t.Fatalf("expected 2 upserts (high + boundary), got %d", len(store.upserts))
	}
	titles := map[string]bool{}
	for _, u := range store.upserts {
		titles[u.Title] = true
	}
	if !titles["high"] || !titles["boundary"] || titles["low"] {
		t.Errorf("filter wrong: %+v", titles)
	}
	// Verify expected_at parsed correctly.
	var highRow repository.NewEventCatalyst
	for _, u := range store.upserts {
		if u.Title == "high" {
			highRow = u
		}
	}
	if highRow.ExpectedAt.IsZero() || highRow.ExpectedAt.Format(time.RFC3339) != exp {
		t.Errorf("expected_at parse: %v", highRow.ExpectedAt)
	}
}

// --- stale handling ------------------------------------------------------

func TestImporter_MarkStale_AgesUnmatchedExpiredCatalysts(t *testing.T) {
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	candidates := &fakeCandidates{rows: []repository.IntelligenceCandidate{
		{ConditionID: "0xa", Category: "Politics"},
	}}
	markets := &fakeMarkets{byID: map[string]repository.Market{"0xa": {EventSlug: "ev"}}}
	pages := &fakePages{bySlug: map[string]eventpagecontext.Summary{"ev": {EventSlug: "ev"}}}

	store := newFakeStore()
	store.rows["ev"] = []repository.EventCatalyst{
		// Old (older than 7d) + expected_at past + not re-emitted → stale.
		{ID: 1, EventSlug: "ev", CatalystType: "debate", Title: "old past",
			Status:     repository.CatalystStatusExpected,
			ExpectedAt: now.Add(-30 * 24 * time.Hour), UpdatedAt: now.Add(-30 * 24 * time.Hour)},
		// Old but expected_at in the future → keep.
		{ID: 2, EventSlug: "ev", CatalystType: "runoff", Title: "future",
			Status:     repository.CatalystStatusExpected,
			ExpectedAt: now.Add(30 * 24 * time.Hour), UpdatedAt: now.Add(-30 * 24 * time.Hour)},
		// Recent (within 7d) → keep.
		{ID: 3, EventSlug: "ev", CatalystType: "poll", Title: "fresh",
			Status:     repository.CatalystStatusExpected,
			ExpectedAt: now.Add(-1 * 24 * time.Hour), UpdatedAt: now.Add(-1 * 24 * time.Hour)},
		// Already resolved → never touched.
		{ID: 4, EventSlug: "ev", CatalystType: "primary", Title: "resolved already",
			Status: repository.CatalystStatusResolved, UpdatedAt: now.Add(-30 * 24 * time.Hour)},
	}
	ex := &fakeExtractor{res: analysis.CatalystExtractionResponse{Status: analysis.StatusOK}}
	w := New(Config{
		Enabled: true, AIEnabled: true, CategoryWhitelist: []string{"Politics"},
		BatchSize: 50, Concurrency: 1, MinConfidence: 0.55,
		StaleAfter: 7 * 24 * time.Hour,
		Clock:      clock,
	}, candidates, markets, pages, store, ex, nil, nopLogger())
	w.Tick(context.Background())

	if got, want := store.statusSet[1], repository.CatalystStatusStale; got != want {
		t.Errorf("row 1 should be stale, got %q", got)
	}
	if _, set := store.statusSet[2]; set {
		t.Errorf("future-expected row should NOT be stale-marked")
	}
	if _, set := store.statusSet[3]; set {
		t.Errorf("recent row should NOT be stale-marked")
	}
	if _, set := store.statusSet[4]; set {
		t.Errorf("resolved row should NOT be touched")
	}
}

// --- AI-disabled path ----------------------------------------------------

func TestImporter_Tick_AIDisabledStillRefreshesAnnotations(t *testing.T) {
	candidates := &fakeCandidates{rows: []repository.IntelligenceCandidate{
		{ConditionID: "0xa", Category: "Politics"},
	}}
	markets := &fakeMarkets{byID: map[string]repository.Market{"0xa": {EventSlug: "ev"}}}
	pages := &fakePages{bySlug: map[string]eventpagecontext.Summary{"ev": {EventSlug: "ev"}}}
	store := newFakeStore()
	ex := &fakeExtractor{}
	w := New(Config{
		Enabled: true, AIEnabled: false, CategoryWhitelist: []string{"Politics"},
		BatchSize: 50, Concurrency: 1, MinConfidence: 0.55,
	}, candidates, markets, pages, store, ex, nil, nopLogger())
	w.Tick(context.Background())
	if len(pages.calls) != 1 {
		t.Errorf("event page must still be refreshed: %d", len(pages.calls))
	}
	if ex.calls != 0 {
		t.Errorf("AI must NOT be called when disabled: %d", ex.calls)
	}
}

// --- request shape -------------------------------------------------------

func TestImporter_Tick_BuildsRequestWithEventDataAndExisting(t *testing.T) {
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	candidates := &fakeCandidates{rows: []repository.IntelligenceCandidate{
		{ConditionID: "0xa", Category: "Politics", Alerts24h: 4, Volume24hUSD: 50_000},
	}}
	markets := &fakeMarkets{byID: map[string]repository.Market{"0xa": {EventSlug: "ev"}}}
	priceBefore := 0.54
	priceAfter := 0.61
	priceChange := 0.07
	pages := &fakePages{bySlug: map[string]eventpagecontext.Summary{
		"ev": {
			EventSlug:     "ev",
			LastFetchedAt: now,
			Annotations: []repository.EventAnnotation{
				{Timestamp: now.Add(-2 * 24 * time.Hour), Title: "old", Outcome: "Ken Paxton"},
				{Timestamp: now.Add(-1 * time.Hour), Title: "newest", Outcome: "Ken Paxton",
					PriceBefore: &priceBefore, PriceAfter: &priceAfter, PriceChange: &priceChange},
			},
			Markets: []repository.EventPageMarketRow{
				{ConditionID: "0xa", Question: "Will Paxton win?", OutcomePrices: []string{"0.62"}, Volume24h: 95000},
			},
		},
	}}
	store := newFakeStore()
	store.rows["ev"] = []repository.EventCatalyst{
		{ID: 99, EventSlug: "ev", CatalystType: "runoff", Title: "TX runoff", Status: repository.CatalystStatusExpected,
			ExpectedAt: now.Add(30 * 24 * time.Hour), Confidence: 0.7},
	}
	ex := &fakeExtractor{res: analysis.CatalystExtractionResponse{Status: analysis.StatusOK}}
	w := New(Config{
		Enabled: true, AIEnabled: true, CategoryWhitelist: []string{"Politics"},
		BatchSize: 50, Concurrency: 1, MinConfidence: 0.55,
		MaxAnnotations: 1, Clock: func() time.Time { return now },
	}, candidates, markets, pages, store, ex, nil, nopLogger())
	w.Tick(context.Background())

	if ex.captured.EventSlug != "ev" {
		t.Errorf("event_slug: %q", ex.captured.EventSlug)
	}
	if len(ex.captured.Annotations) != 1 {
		t.Errorf("annotations cap to MaxAnnotations: got %d", len(ex.captured.Annotations))
	}
	if ex.captured.Annotations[0].Title != "newest" {
		t.Errorf("newest annotation should win: %+v", ex.captured.Annotations)
	}
	if len(ex.captured.Markets) != 1 {
		t.Errorf("markets must flow through: got %d", len(ex.captured.Markets))
	}
	if len(ex.captured.ExistingCatalysts) != 1 || ex.captured.ExistingCatalysts[0].Title != "TX runoff" {
		t.Errorf("existing catalysts must flow through: %+v", ex.captured.ExistingCatalysts)
	}
}

// --- category filter -----------------------------------------------------

func TestImporter_Tick_FiltersOutOfWhitelistCategories(t *testing.T) {
	candidates := &fakeCandidates{rows: []repository.IntelligenceCandidate{
		{ConditionID: "0xs", Category: "Sports"},
		{ConditionID: "0xc", Category: "Crypto"},
	}}
	markets := &fakeMarkets{byID: map[string]repository.Market{
		"0xs": {EventSlug: "sports"},
		"0xc": {EventSlug: "crypto"},
	}}
	pages := &fakePages{bySlug: map[string]eventpagecontext.Summary{}}
	store := newFakeStore()
	ex := &fakeExtractor{}
	w := New(Config{
		Enabled: true, AIEnabled: true, CategoryWhitelist: []string{"Politics", "Geopolitics"},
		BatchSize: 50, Concurrency: 1, MinConfidence: 0.55,
	}, candidates, markets, pages, store, ex, nil, nopLogger())
	w.Tick(context.Background())
	if ex.calls != 0 {
		t.Errorf("out-of-whitelist categories must be skipped: calls=%d", ex.calls)
	}
}

// --- omission preservation ----------------------------------------------

func TestImporter_MarkStale_RecentOmittedRowIsNotDeleted(t *testing.T) {
	// AI omits an existing catalyst row, but the row is recent
	// (within StaleAfter): the importer must NOT mark it stale,
	// NOT delete it. The row is simply left alone.
	now := time.Now().UTC()
	candidates := &fakeCandidates{rows: []repository.IntelligenceCandidate{
		{ConditionID: "0xa", Category: "Politics"},
	}}
	markets := &fakeMarkets{byID: map[string]repository.Market{"0xa": {EventSlug: "ev"}}}
	pages := &fakePages{bySlug: map[string]eventpagecontext.Summary{"ev": {EventSlug: "ev"}}}
	store := newFakeStore()
	store.rows["ev"] = []repository.EventCatalyst{
		{ID: 10, EventSlug: "ev", CatalystType: "debate", Title: "recent",
			Status: repository.CatalystStatusExpected, UpdatedAt: now.Add(-1 * time.Hour)},
	}
	ex := &fakeExtractor{res: analysis.CatalystExtractionResponse{Status: analysis.StatusOK}}
	w := New(Config{
		Enabled: true, AIEnabled: true, CategoryWhitelist: []string{"Politics"},
		BatchSize: 50, Concurrency: 1, MinConfidence: 0.55,
		StaleAfter: 7 * 24 * time.Hour,
	}, candidates, markets, pages, store, ex, nil, nopLogger())
	w.Tick(context.Background())
	if _, set := store.statusSet[10]; set {
		t.Errorf("recent omitted row must NOT be touched")
	}
}

// Compile-time assertions that our fakes match the seams + the
// extractor pointer interface.
var (
	_ CandidateSource            = (*fakeCandidates)(nil)
	_ MarketResolver             = (*fakeMarkets)(nil)
	_ EventPageRefresher         = (*fakePages)(nil)
	_ CatalystStore              = (*fakeCatalystStore)(nil)
	_ analysis.CatalystExtractor = (*fakeExtractor)(nil)
)

// Sanity: importer interval config parses 5m correctly.
func TestImporter_ConfigDefaultsApplyFiveMinuteInterval(t *testing.T) {
	cfg := Config{} // zero
	cfg.applyDefaults()
	if cfg.Interval != 5*time.Minute {
		t.Errorf("default interval must be 5m, got %s", cfg.Interval)
	}
	if cfg.MinConfidence != 0.55 {
		t.Errorf("default min confidence must be 0.55, got %v", cfg.MinConfidence)
	}
	if cfg.StaleAfter != 7*24*time.Hour {
		t.Errorf("default stale after must be 7d, got %s", cfg.StaleAfter)
	}
}

// Quick string suite to keep imports tidy.
var _ = strings.TrimSpace
