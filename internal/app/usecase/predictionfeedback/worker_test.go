package predictionfeedback

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/infra/repository"
)

// fakeStore captures every Upsert + advertises which horizons it
// already recorded.
type fakeStore struct {
	cands     []repository.FeedbackCandidate
	recorded  map[int64]map[string]bool
	upserts   []repository.FeedbackRow
	upsertErr error
}

func (f *fakeStore) ListPredictionsForFeedback(_ context.Context, _ time.Time, _ int32) ([]repository.FeedbackCandidate, error) {
	return f.cands, nil
}

func (f *fakeStore) HorizonsRecorded(_ context.Context, predictionID int64) (map[string]bool, error) {
	if f.recorded == nil {
		return map[string]bool{}, nil
	}
	if m, ok := f.recorded[predictionID]; ok {
		out := map[string]bool{}
		for k, v := range m {
			out[k] = v
		}
		return out, nil
	}
	return map[string]bool{}, nil
}

func (f *fakeStore) UpsertFeedback(_ context.Context, in repository.FeedbackRow) error {
	if f.upsertErr != nil {
		return f.upsertErr
	}
	f.upserts = append(f.upserts, in)
	return nil
}

type fakeMarkets map[string]repository.Market

func (f fakeMarkets) GetByConditionID(_ context.Context, cid string) (repository.Market, error) {
	m, ok := f[cid]
	if !ok {
		return repository.Market{}, errors.New("not found")
	}
	return m, nil
}

type fakePages struct {
	rows []repository.EventPageMarketRow
}

func (f *fakePages) ListLatestEventMarkets(_ context.Context, _ string) ([]repository.EventPageMarketRow, error) {
	return f.rows, nil
}

type fakeTrades struct {
	byKey map[string]float64 // "marketID|token|after" → price; "*" wildcard supported on the suffix
}

func (f *fakeTrades) TradePriceAtOrAfter(_ context.Context, marketID int64, token string, at time.Time) (float64, bool, error) {
	// Build sequential keys; tests can register either a specific
	// (market,token,time) tuple or a wildcard (market,token,*).
	key := pricelookupKey(marketID, token, at)
	if v, ok := f.byKey[key]; ok {
		return v, true, nil
	}
	if v, ok := f.byKey[pricelookupKey(marketID, token, time.Time{})]; ok {
		return v, true, nil
	}
	return 0, false, nil
}

func pricelookupKey(marketID int64, token string, at time.Time) string {
	if at.IsZero() {
		return joinKey(marketID, token, "*")
	}
	return joinKey(marketID, token, at.UTC().Format(time.RFC3339))
}

func joinKey(marketID int64, token, t string) string {
	return ts(marketID) + "|" + token + "|" + t
}

func ts(i int64) string { return time.Unix(i, 0).Format("id-15:04:05") }

// TestFeedback_HorizonsDueAndUndue covers the eligibility gate:
// horizons earlier than now produce feedback rows; later horizons
// don't.
func TestFeedback_HorizonsDueAndUndue(t *testing.T) {
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	createdAt := now.Add(-2 * time.Hour)
	store := &fakeStore{cands: []repository.FeedbackCandidate{{
		ID: 1, EventSlug: "ev", ConditionID: "0xa", Outcome: "Yes",
		SideBias: "bullish", CreatedAt: createdAt, CurrentState: "watching",
	}}}
	mk := fakeMarkets{"0xa": {ID: 100, ConditionID: "0xa"}}
	page := &fakePages{rows: []repository.EventPageMarketRow{{
		ConditionID: "0xa", Outcomes: []string{"Yes", "No"},
		OutcomePrices: []string{"0.6", "0.4"},
		CLOBTokenIDs:  []string{"tokY", "tokN"},
	}}}
	trades := &fakeTrades{byKey: map[string]float64{
		joinKey(100, "tokY", "*"): 0.55,
	}}
	w := New(Config{Enabled: true, Horizons: []time.Duration{time.Hour, 6 * time.Hour, 24 * time.Hour}, Clock: func() time.Time { return now }}, store, mk, page, trades, nil, nil)
	wrote := w.Tick(context.Background())
	if wrote != 1 {
		t.Fatalf("expected 1 horizon written; got %d (upserts=%+v)", wrote, store.upserts)
	}
	row := store.upserts[0]
	if row.Horizon != "1h" {
		t.Errorf("expected 1h horizon; got %q", row.Horizon)
	}
	if row.PriceAtPrediction == nil || *row.PriceAtPrediction != 0.55 {
		t.Errorf("price_at_prediction not propagated: %+v", row)
	}
}

// TestFeedback_DirectionCorrectness pins the bullish/bearish/neutral
// mapping.
func TestFeedback_DirectionCorrectness(t *testing.T) {
	// Bullish, delta positive → true.
	got := directionCorrect("bullish", 0.05)
	if got == nil || *got != true {
		t.Errorf("bullish+up: got %v", got)
	}
	got = directionCorrect("bullish", -0.05)
	if got == nil || *got != false {
		t.Errorf("bullish+down: got %v", got)
	}
	got = directionCorrect("bearish", -0.05)
	if got == nil || *got != true {
		t.Errorf("bearish+down: got %v", got)
	}
	got = directionCorrect("neutral", 0.05)
	if got != nil {
		t.Errorf("neutral should be nil; got %v", got)
	}
}

// TestFeedback_MissingPriceSafe ensures we never panic and we
// always write *some* metadata even when trade prices are missing.
func TestFeedback_MissingPriceSafe(t *testing.T) {
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	createdAt := now.Add(-2 * time.Hour)
	store := &fakeStore{cands: []repository.FeedbackCandidate{{
		ID: 1, EventSlug: "ev", ConditionID: "0xa", Outcome: "Yes",
		SideBias: "bullish", CreatedAt: createdAt, CurrentState: "watching",
	}}}
	mk := fakeMarkets{"0xa": {ID: 100, ConditionID: "0xa"}}
	page := &fakePages{rows: []repository.EventPageMarketRow{{
		ConditionID: "0xa", Outcomes: []string{"Yes", "No"},
		CLOBTokenIDs: []string{"tokY", "tokN"},
	}}}
	trades := &fakeTrades{} // no prices registered → all lookups return ok=false
	w := New(Config{Enabled: true, Horizons: []time.Duration{time.Hour}, Clock: func() time.Time { return now }}, store, mk, page, trades, nil, nil)
	wrote := w.Tick(context.Background())
	if wrote != 1 {
		t.Fatalf("expected 1 row even with missing prices; got %d", wrote)
	}
	row := store.upserts[0]
	if row.PriceAtPrediction != nil || row.PriceAtHorizon != nil {
		t.Errorf("missing prices must stay nil: %+v", row)
	}
	if row.DirectionCorrect != nil {
		t.Errorf("direction must be nil when delta is undefined: %v", row.DirectionCorrect)
	}
}
