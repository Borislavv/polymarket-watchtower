package dataapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/domain/vo"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/polymarket/httpx"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/ratelimit"
)

func newDataClient(t *testing.T, handler http.Handler) (*Client, func()) {
	t.Helper()
	srv := httptest.NewServer(handler)
	h, err := httpx.New(httpx.Config{BaseURL: srv.URL, Limiter: ratelimit.Noop{}})
	if err != nil {
		srv.Close()
		t.Fatalf("httpx: %v", err)
	}
	return New(h), srv.Close
}

// pages serves a newest-first feed for one market, slicing by ?offset&limit.
func pagedHandler(all []dataTrade) http.HandlerFunc {
	sort.SliceStable(all, func(i, j int) bool { return all[i].Timestamp > all[j].Timestamp })
	return func(w http.ResponseWriter, r *http.Request) {
		offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		if limit == 0 {
			limit = defaultPageSize
		}
		if offset >= len(all) {
			_ = json.NewEncoder(w).Encode([]dataTrade{})
			return
		}
		end := offset + limit
		if end > len(all) {
			end = len(all)
		}
		_ = json.NewEncoder(w).Encode(all[offset:end])
	}
}

func TestListTradesSincePaginatesAndStopsOnEmpty(t *testing.T) {
	all := []dataTrade{
		{ID: "1", ConditionID: "0xa", Asset: "1", Side: "BUY", Size: 10, Price: 0.5, Timestamp: 1000},
		{ID: "2", ConditionID: "0xa", Asset: "1", Side: "SELL", Size: 5, Price: 0.6, Timestamp: 1100},
		{ID: "3", ConditionID: "0xa", Asset: "1", Side: "BUY", Size: 1, Price: 0.55, Timestamp: 1200},
	}
	c, done := newDataClient(t, pagedHandler(all))
	defer done()
	trades, err := c.ListTradesSince(context.Background(), ListTradesOpts{Market: vo.MarketID("0xa"), Limit: 2})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(trades) != 3 {
		t.Fatalf("got %d", len(trades))
	}
	// Market stamped from request, not echo.
	for _, tr := range trades {
		if tr.Market != "0xa" {
			t.Fatalf("market: %s", tr.Market)
		}
	}
}

func TestListTradesSinceStopsAtCutoff(t *testing.T) {
	page := []dataTrade{
		{ID: "1", Timestamp: 2000, Size: 1, Price: 0.5, Side: "BUY"},
		{ID: "2", Timestamp: 1500, Size: 1, Price: 0.5, Side: "BUY"},
		{ID: "3", Timestamp: 1000, Size: 1, Price: 0.5, Side: "BUY"}, // below cutoff
		{ID: "4", Timestamp: 500, Size: 1, Price: 0.5, Side: "BUY"},  // below cutoff
	}
	c, done := newDataClient(t, pagedHandler(page))
	defer done()
	got, err := c.ListTradesSince(context.Background(), ListTradesOpts{
		Market: "0xa",
		Since:  time.Unix(1200, 0),
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d trades (want 2 after cutoff): %+v", len(got), got)
	}
}

func TestListTradesRequiresMarket(t *testing.T) {
	c := New(nil)
	_, err := c.ListTradesSince(context.Background(), ListTradesOpts{})
	if err == nil {
		t.Fatal("expected error for missing market")
	}
}

// Regression: filterType=TIMESTAMP must never be emitted, ever. The Data API
// rejects it with HTTP 400 "must be: [CASH TOKENS]".
func TestQueryBuilderNeverEmitsTimestampFilter(t *testing.T) {
	opts := ListTradesOpts{
		Market: "0xa",
		Since:  time.Unix(1700000000, 0),
		Limit:  100,
	}
	q, err := buildTradesQuery(opts, 100, 0)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got := q.Get("filterType"); got != "" {
		t.Fatalf("filterType must be empty when MinValue is nil, got %q", got)
	}
	if got := q.Get("filterAmount"); got != "" {
		t.Fatalf("filterAmount must be empty when MinValue is nil, got %q", got)
	}
	allowed := map[string]bool{"market": true, "limit": true, "offset": true, "filterType": true, "filterAmount": true}
	for k := range q {
		if !allowed[k] {
			t.Fatalf("unexpected query key %q (only market/limit/offset/filterType/filterAmount allowed)", k)
		}
	}
	if q.Get("market") != "0xa" {
		t.Fatalf("market: %q", q.Get("market"))
	}
	if q.Get("limit") != "100" || q.Get("offset") != "0" {
		t.Fatalf("pagination params: %v", q)
	}
}

func TestQueryBuilderEmitsAllowedFilterTypes(t *testing.T) {
	for _, ft := range []FilterType{FilterCash, FilterTokens} {
		opts := ListTradesOpts{
			Market:   "0xa",
			MinValue: &MinValueFilter{Type: ft, Amount: 100},
		}
		q, err := buildTradesQuery(opts, 100, 0)
		if err != nil {
			t.Fatalf("%s: %v", ft, err)
		}
		if q.Get("filterType") != string(ft) {
			t.Fatalf("filterType: got %q want %q", q.Get("filterType"), ft)
		}
		if q.Get("filterAmount") != "100" {
			t.Fatalf("filterAmount: %q", q.Get("filterAmount"))
		}
	}
}

func TestQueryBuilderRejectsInvalidFilterType(t *testing.T) {
	_, err := buildTradesQuery(ListTradesOpts{
		Market:   "0xa",
		MinValue: &MinValueFilter{Type: "TIMESTAMP", Amount: 1},
	}, 100, 0)
	if err == nil {
		t.Fatal("expected error for filterType=TIMESTAMP")
	}
}

// Regression: live request that previously triggered the 400 must come back as
// a non-retryable APIError, not as a transport error or successful empty page.
func TestListTradesSurfacesAPIErrorOn400(t *testing.T) {
	var calls atomic.Int32
	c, done := newDataClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		// emulate the real upstream response shape
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid filterType TIMESTAMP. must be: [CASH TOKENS]"}`))
	}))
	defer done()
	_, err := c.ListTradesSince(context.Background(), ListTradesOpts{Market: "0xa"})
	if err == nil {
		t.Fatal("expected error")
	}
	var apiErr *httpx.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("want APIError, got %T: %v", err, err)
	}
	if apiErr.Status != http.StatusBadRequest {
		t.Fatalf("status: %d", apiErr.Status)
	}
	if apiErr.Retryable() {
		t.Fatal("400 must not be retryable")
	}
	if calls.Load() != 1 {
		t.Fatalf("400 must be a single shot, got %d calls", calls.Load())
	}
}

func TestListTradesRespectsUpstreamOffsetCap(t *testing.T) {
	var maxOffset atomic.Int32
	c, done := newDataClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		off, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		for {
			cur := maxOffset.Load()
			if int32(off) <= cur || maxOffset.CompareAndSwap(cur, int32(off)) {
				break
			}
		}
		// always serve a full page so pagination only ends at our internal cap
		batch := make([]dataTrade, 100)
		for i := range batch {
			batch[i] = dataTrade{ID: "x", Timestamp: 9_000_000_000, Size: 1, Price: 0.5, Side: "BUY"}
		}
		_ = json.NewEncoder(w).Encode(batch)
	}))
	defer done()
	_, err := c.ListTradesSince(context.Background(), ListTradesOpts{
		Market: "0xa", Limit: 100, MaxPages: 1000,
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	// Must never request offset > 3000 (would 400 with "max historical activity offset of 3000 exceeded").
	if got := maxOffset.Load(); got > maxUpstreamOffset {
		t.Fatalf("offset cap breached: max observed=%d limit=%d", got, maxUpstreamOffset)
	}
}

func TestListTradesStopsAtMaxPages(t *testing.T) {
	var calls atomic.Int32
	c, done := newDataClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		// always serve a full page so pagination never naturally ends
		batch := make([]dataTrade, 10)
		for i := range batch {
			batch[i] = dataTrade{ID: "x", Timestamp: 9_000_000_000, Size: 1, Price: 0.5, Side: "BUY"}
		}
		_ = json.NewEncoder(w).Encode(batch)
	}))
	defer done()
	_, err := c.ListTradesSince(context.Background(), ListTradesOpts{
		Market: "0xa", Limit: 10, MaxPages: 3,
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("expected exactly 3 page calls (MaxPages cap), got %d", got)
	}
}
