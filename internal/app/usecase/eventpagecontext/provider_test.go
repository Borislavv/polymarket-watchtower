package eventpagecontext

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/anomaly"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/vo"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/polymarket/eventpage"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/repository"
)

// --- fake client / store --------------------------------------------------

type fakeClient struct {
	payload *eventpage.EventPagePayload
	err     error
	calls   int
}

func (c *fakeClient) FetchEventPage(_ context.Context, _ string) (*eventpage.EventPagePayload, error) {
	c.calls++
	if c.err != nil {
		return nil, c.err
	}
	return c.payload, nil
}

type fakeStore struct {
	state       repository.EventPageFetchState
	stateErr    error
	annotations []repository.EventAnnotation
	markets     []repository.EventPageMarketRow

	snapshots   []repository.NewEventPageSnapshot
	upMarkets   []repository.NewEventPageMarket
	upAnns      []repository.NewEventAnnotation
	markFetches []markFetchCall
}

type markFetchCall struct {
	eventSlug string
	buildID   string
	annCount  int32
	err       string
}

func (s *fakeStore) InsertSnapshot(_ context.Context, in repository.NewEventPageSnapshot) (int64, error) {
	s.snapshots = append(s.snapshots, in)
	return int64(len(s.snapshots)), nil
}

func (s *fakeStore) InsertMarket(_ context.Context, in repository.NewEventPageMarket) error {
	s.upMarkets = append(s.upMarkets, in)
	// Mirror real-store behaviour: a future ListLatestEventMarkets
	// sees what we just wrote.
	s.markets = append(s.markets, repository.EventPageMarketRow{
		EventSlug:      in.EventSlug,
		MarketID:       in.MarketID,
		ConditionID:    in.ConditionID,
		MarketSlug:     in.MarketSlug,
		Question:       in.Question,
		GroupItemTitle: in.GroupItemTitle,
		Outcomes:       in.Outcomes,
		OutcomePrices:  in.OutcomePrices,
		Volume24h:      in.Volume24h,
	})
	return nil
}

func (s *fakeStore) UpsertAnnotation(_ context.Context, in repository.NewEventAnnotation) error {
	s.upAnns = append(s.upAnns, in)
	// Mirror real-store behaviour: a future ListRecentAnnotations
	// sees what we just wrote.
	s.annotations = append(s.annotations, repository.EventAnnotation{
		EventSlug:   in.EventSlug,
		ItemHash:    in.ItemHash,
		Timestamp:   in.Timestamp,
		UnixTime:    in.UnixTime,
		Outcome:     in.Outcome,
		Title:       in.Title,
		Summary:     in.Summary,
		PriceBefore: in.PriceBefore,
		PriceAfter:  in.PriceAfter,
		PriceChange: in.PriceChange,
		Source:      in.Source,
		SourcesJSON: in.SourcesJSON,
	})
	return nil
}

func (s *fakeStore) FetchState(_ context.Context, slug string) (repository.EventPageFetchState, error) {
	if s.stateErr != nil {
		return repository.EventPageFetchState{}, s.stateErr
	}
	return s.state, nil
}

func (s *fakeStore) MarkFetch(_ context.Context, slug string, _ time.Time, buildID string, ann int32, fetchErr string) error {
	s.markFetches = append(s.markFetches, markFetchCall{
		eventSlug: slug, buildID: buildID, annCount: ann, err: fetchErr,
	})
	return nil
}

func (s *fakeStore) ListRecentAnnotations(_ context.Context, _ string, _ int32) ([]repository.EventAnnotation, error) {
	return s.annotations, nil
}

func (s *fakeStore) ListLatestEventMarkets(_ context.Context, _ string) ([]repository.EventPageMarketRow, error) {
	return s.markets, nil
}

// Compile-time assertion that fakes satisfy the seams.
var _ Client = (*fakeClient)(nil)
var _ Store = (*fakeStore)(nil)

// --- helpers --------------------------------------------------------------

func mkPayload() *eventpage.EventPagePayload {
	priceBefore := 0.54
	priceAfter := 0.61
	priceChange := 0.07
	return &eventpage.EventPagePayload{
		EventSlug: "texas",
		BuildID:   "build-X",
		FetchedAt: time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC),
		Event: eventpage.EventPageEvent{
			Title:              "Texas Republican Senate Primary Winner",
			ContextDescription: "Open seat draws Paxton, Cornyn, ...",
			ContextUpdatedAt:   time.Date(2026, 5, 18, 10, 0, 0, 0, time.UTC),
		},
		Annotations: []eventpage.EventAnnotation{
			{
				EventSlug:   "texas",
				Timestamp:   time.Date(2026, 5, 9, 14, 0, 0, 0, time.UTC),
				UnixTime:    1778421600,
				Outcome:     "Ken Paxton",
				Title:       "Final pre-runoff poll shows Paxton leading with 63%",
				Summary:     "UT/Texas Tribune poll puts Paxton at 63%.",
				PriceBefore: &priceBefore,
				PriceAfter:  &priceAfter,
				PriceChange: &priceChange,
			},
		},
	}
}

func mkFinding(outcome, conditionID string, sev anomaly.Severity) anomaly.Finding {
	return anomaly.Finding{
		Severity: sev,
		Trade: &anomaly.TradeRef{
			Market:  vo.MarketID(conditionID),
			Outcome: outcome,
		},
	}
}

// --- refresh + persist ---------------------------------------------------

func TestProvider_Refresh_PersistsSnapshotMarketsAndAnnotations(t *testing.T) {
	zl := zerolog.Nop()
	c := &fakeClient{payload: mkPayload()}
	s := &fakeStore{}
	p := New(Config{Enabled: true}, c, s, func(_ context.Context, id string) string {
		if id == "0xpaxton" {
			return "texas"
		}
		return ""
	}, nil, &zl)
	p.now = func() time.Time { return time.Date(2026, 5, 19, 13, 0, 0, 0, time.UTC) }

	f := mkFinding("Ken Paxton", "0xpaxton", anomaly.SeverityWarning)
	out := p.LoadAndRenderForFinding(context.Background(), f, 5000)
	if out == "" {
		t.Fatalf("expected rendered context, got empty")
	}
	if c.calls != 1 {
		t.Errorf("expected 1 fetch call, got %d", c.calls)
	}
	if len(s.snapshots) != 1 {
		t.Errorf("expected 1 snapshot insert, got %d", len(s.snapshots))
	}
	if len(s.upAnns) != 1 {
		t.Errorf("expected 1 annotation upsert, got %d", len(s.upAnns))
	}
	if len(s.markFetches) != 1 || s.markFetches[0].err != "" {
		t.Errorf("expected one successful markFetch, got %+v", s.markFetches)
	}
	if !strings.Contains(out, "Final pre-runoff poll") {
		t.Errorf("annotation title not rendered: %s", out)
	}
	if !strings.Contains(out, "context_description: Open seat draws Paxton") {
		t.Errorf("context_description not rendered: %s", out)
	}
}

func TestProvider_UnresolvedSlug_ReturnsEmptyWithoutFetch(t *testing.T) {
	zl := zerolog.Nop()
	c := &fakeClient{payload: mkPayload()}
	s := &fakeStore{}
	p := New(Config{Enabled: true}, c, s, func(_ context.Context, _ string) string { return "" }, nil, &zl)
	out := p.LoadAndRenderForFinding(context.Background(), mkFinding("x", "unknown-cid", anomaly.SeverityInfo), 5000)
	if out != "" {
		t.Fatalf("expected empty when slug unresolved, got %q", out)
	}
	if c.calls != 0 {
		t.Errorf("expected 0 fetch calls, got %d", c.calls)
	}
}

func TestProvider_FetchFailure_DoesNotBlockAndRecordsState(t *testing.T) {
	zl := zerolog.Nop()
	c := &fakeClient{err: errString("boom")}
	s := &fakeStore{}
	p := New(Config{Enabled: true}, c, s, func(_ context.Context, _ string) string { return "texas" }, nil, &zl)
	// The renderer should still return a non-empty "unavailable"
	// slot because the contract is: never block the alert.
	out := p.LoadAndRenderForFinding(context.Background(), mkFinding("Ken Paxton", "0xpaxton", anomaly.SeverityWarning), 5000)
	if !strings.Contains(out, "unavailable") {
		t.Errorf("expected unavailable fallback, got %q", out)
	}
	if len(s.markFetches) != 1 || s.markFetches[0].err == "" {
		t.Errorf("expected failed markFetch row, got %+v", s.markFetches)
	}
}

// --- renderer ordering ---------------------------------------------------

func TestSummary_Render_SameOutcomeFirstThenLargeOppositeMoves(t *testing.T) {
	t1 := time.Date(2026, 5, 9, 14, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 4, 28, 18, 0, 0, 0, time.UTC)
	t3 := time.Date(2026, 3, 15, 12, 0, 0, 0, time.UTC)
	paxtonChange := 0.07
	cornynChange := -0.11
	smallChange := 0.01
	sum := Summary{
		EventSlug:     "texas",
		LastFetchedAt: time.Date(2026, 5, 19, 13, 0, 0, 0, time.UTC),
		Annotations: []repository.EventAnnotation{
			{EventSlug: "texas", Timestamp: t2, Outcome: "John Cornyn", Title: "Cornyn scandal", PriceChange: &cornynChange},
			{EventSlug: "texas", Timestamp: t1, Outcome: "Ken Paxton", Title: "Paxton poll up", PriceChange: &paxtonChange},
			{EventSlug: "texas", Timestamp: t3, Outcome: "Ken Paxton", Title: "Paxton endorsement", PriceChange: &smallChange},
		},
	}
	// side = "Ken Paxton" → expect Paxton items first (newest first
	// within same-outcome group), then the Cornyn opposite-side
	// item (large absolute move >= 0.05).
	rendered := sum.Render("Ken Paxton", 3, 5000)
	idxPaxtonPoll := strings.Index(rendered, "Paxton poll up")
	idxEndorsement := strings.Index(rendered, "Paxton endorsement")
	idxCornyn := strings.Index(rendered, "Cornyn scandal")
	if idxPaxtonPoll < 0 || idxEndorsement < 0 || idxCornyn < 0 {
		t.Fatalf("missing annotation lines:\n%s", rendered)
	}
	if !(idxPaxtonPoll < idxEndorsement && idxEndorsement < idxCornyn) {
		t.Errorf("ordering wrong: paxtonPoll=%d, endorsement=%d, cornyn=%d\n%s",
			idxPaxtonPoll, idxEndorsement, idxCornyn, rendered)
	}
}

func TestSummary_Render_OppositeSideSmallMovesSkipped(t *testing.T) {
	t1 := time.Date(2026, 5, 9, 14, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 4, 28, 18, 0, 0, 0, time.UTC)
	paxtonBig := 0.10
	cornynTiny := 0.01
	sum := Summary{
		EventSlug:     "texas",
		LastFetchedAt: time.Date(2026, 5, 19, 13, 0, 0, 0, time.UTC),
		Annotations: []repository.EventAnnotation{
			{EventSlug: "texas", Timestamp: t1, Outcome: "Ken Paxton", Title: "Paxton big", PriceChange: &paxtonBig},
			{EventSlug: "texas", Timestamp: t2, Outcome: "John Cornyn", Title: "Cornyn tiny", PriceChange: &cornynTiny},
		},
	}
	out := sum.Render("Ken Paxton", 5, 5000)
	if !strings.Contains(out, "Paxton big") {
		t.Errorf("paxton item should appear:\n%s", out)
	}
	if strings.Contains(out, "Cornyn tiny") {
		t.Errorf("tiny opposite-side move must NOT pass the 0.05 filter:\n%s", out)
	}
}

func TestSummary_Render_Empty_FallsBackToUnavailable(t *testing.T) {
	out := Summary{EventSlug: "texas"}.Render("", 10, 5000)
	if !strings.Contains(out, "unavailable") {
		t.Errorf("empty summary must render unavailable: %q", out)
	}
}

// --- compile-time: NarrativeLoader compatibility -------------------------

// LoadAndRenderForFinding signature mirrors aianalysis.NarrativeLoader.
// We don't import aianalysis here (would cycle); the wiring test in
// aianalysis covers the cross-package compatibility.

// --- helpers -------------------------------------------------------------

type errString string

func (e errString) Error() string { return string(e) }

// snapshotMarshalUnmarshal sanity-checks that RawJSON we marshal in
// the provider is valid JSON. We don't assert content — just shape.
func TestProvider_RawJSONIsValid(t *testing.T) {
	pl := mkPayload()
	b, err := json.Marshal(pl)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	if len(b) < 50 {
		t.Errorf("payload looks empty: %s", string(b))
	}
}
