package gamma

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/Borislavv/polymarket-watchtower/internal/infra/polymarket/httpx"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/ratelimit"
)

// fakeGamma serves canned /markets, /events, /tags responses with pagination.
type fakeGamma struct {
	markets []gammaMarket
	events  []gammaEvent
	tags    []gammaTag
}

func (f *fakeGamma) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/markets", func(w http.ResponseWriter, r *http.Request) {
		page := paginate(r, f.markets)
		_ = json.NewEncoder(w).Encode(page)
	})
	mux.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
		page := paginate(r, f.events)
		_ = json.NewEncoder(w).Encode(page)
	})
	mux.HandleFunc("/tags", func(w http.ResponseWriter, r *http.Request) {
		page := paginate(r, f.tags)
		_ = json.NewEncoder(w).Encode(page)
	})
	return mux
}

func paginate[T any](r *http.Request, all []T) []T {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))
	if limit <= 0 {
		limit = 100
	}
	if offset >= len(all) {
		return []T{}
	}
	end := offset + limit
	if end > len(all) {
		end = len(all)
	}
	return all[offset:end]
}

func newGammaClient(t *testing.T, fake *fakeGamma) (*Client, func()) {
	t.Helper()
	srv := httptest.NewServer(fake.handler())
	h, err := httpx.New(httpx.Config{BaseURL: srv.URL, Limiter: ratelimit.Noop{}})
	if err != nil {
		srv.Close()
		t.Fatalf("httpx: %v", err)
	}
	return New(h), srv.Close
}

func TestListMarketsPaginates(t *testing.T) {
	var markets []gammaMarket
	for i := 0; i < 250; i++ {
		markets = append(markets, gammaMarket{
			ConditionID: "0x" + strconv.Itoa(i),
			Slug:        "m" + strconv.Itoa(i),
			Active:      true,
		})
	}
	c, done := newGammaClient(t, &fakeGamma{markets: markets})
	defer done()
	got, err := c.ListMarkets(context.Background(), ListMarketsOpts{ActiveOnly: true})
	if err != nil {
		t.Fatalf("ListMarkets: %v", err)
	}
	if len(got) != 250 {
		t.Fatalf("got %d markets, want 250", len(got))
	}
}

func TestListMarketsHonoursMaxRows(t *testing.T) {
	var markets []gammaMarket
	for i := 0; i < 250; i++ {
		markets = append(markets, gammaMarket{ConditionID: "0x" + strconv.Itoa(i)})
	}
	c, done := newGammaClient(t, &fakeGamma{markets: markets})
	defer done()
	got, err := c.ListMarkets(context.Background(), ListMarketsOpts{MaxRows: 50})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(got) != 50 {
		t.Fatalf("got %d, want 50", len(got))
	}
}

func TestListTagsDecodesStringIDs(t *testing.T) {
	// Gamma /tags returns id as a JSON string in production. Use a raw fixture.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		if offset > 0 {
			_, _ = w.Write([]byte(`[]`))
			return
		}
		_, _ = w.Write([]byte(`[{"id":"100381","slug":"crypto","label":"Crypto"},
                              {"id":"100382","slug":"sports","label":"Sports"}]`))
	}))
	defer srv.Close()
	h, _ := httpx.New(httpx.Config{BaseURL: srv.URL, Limiter: ratelimit.Noop{}})
	c := New(h)
	tags, err := c.ListTags(context.Background())
	if err != nil {
		t.Fatalf("ListTags: %v", err)
	}
	if len(tags) != 2 || tags[0].ID != 100381 || tags[1].Slug != "sports" {
		t.Fatalf("decoded: %+v", tags)
	}
}

func TestMapMarketPropagatesEventSlug(t *testing.T) {
	// Mirrors the production failure: a market that lives inside a parent
	// "winner" event. The market slug itself is NOT a valid Polymarket URL —
	// only the event slug is.
	raw := gammaMarket{
		ID:          "514929",
		Slug:        "will-tunisia-win-the-2026-fifa-world-cup-165",
		Question:    "Will Tunisia win the 2026 FIFA World Cup?",
		ConditionID: "0xff0cfa9506cfa95759e4c7591654195bd26e3011f9882b51439135e04f2b69f1",
		Events: []gammaEventRef{{
			ID: "30615", Slug: "2026-fifa-world-cup-winner-595", Title: "2026 FIFA World Cup Winner",
		}},
	}
	got, err := mapMarket(raw)
	if err != nil {
		t.Fatalf("mapMarket: %v", err)
	}
	if got.EventSlug != "2026-fifa-world-cup-winner-595" {
		t.Fatalf("EventSlug: got %q want %q", got.EventSlug, "2026-fifa-world-cup-winner-595")
	}
	if got.EventTitle != "2026 FIFA World Cup Winner" {
		t.Fatalf("EventTitle: %q", got.EventTitle)
	}
	if got.Slug != "will-tunisia-win-the-2026-fifa-world-cup-165" {
		t.Fatalf("market slug should still round-trip, got %q", got.Slug)
	}
}

func TestMapMarketWithoutEventsLeavesSlugEmpty(t *testing.T) {
	raw := gammaMarket{
		ConditionID: "0xabc",
		Slug:        "standalone-market",
		// Events omitted — represents markets that aren't grouped under an event.
	}
	got, err := mapMarket(raw)
	if err != nil {
		t.Fatalf("mapMarket: %v", err)
	}
	if got.EventSlug != "" || got.EventTitle != "" {
		t.Fatalf("expected empty event fields, got slug=%q title=%q", got.EventSlug, got.EventTitle)
	}
}

func TestMapEventsToMarketCategories(t *testing.T) {
	evs := []gammaEvent{
		{Tags: []gammaTag{{ID: 1}, {ID: 2}}, Markets: []gammaMarket{{ConditionID: "0xa"}, {ConditionID: "0xb"}}},
		{Tags: []gammaTag{{ID: 3}}, Markets: []gammaMarket{{ConditionID: "0xb"}}},
	}
	idx := MapEventsToMarketCategories(evs)
	if len(idx["0xa"]) != 2 {
		t.Errorf("0xa: %v", idx["0xa"])
	}
	if len(idx["0xb"]) != 3 {
		t.Errorf("0xb: %v", idx["0xb"])
	}
}
