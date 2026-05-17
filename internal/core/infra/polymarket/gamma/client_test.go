package gamma

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/Borislavv/polymarket-watchtower/internal/core/infra/polymarket/httpx"
	"github.com/Borislavv/polymarket-watchtower/internal/core/infra/ratelimit"
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
