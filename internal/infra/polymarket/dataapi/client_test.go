package dataapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
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

func TestListTradesSincePaginatesAndStopsOnEmpty(t *testing.T) {
	all := []dataTrade{
		{ID: "1", Market: "0xa", Asset: "1", Side: "BUY", Size: 10, Price: 0.5, Timestamp: 1000},
		{ID: "2", Market: "0xa", Asset: "1", Side: "SELL", Size: 5, Price: 0.6, Timestamp: 1100},
		{ID: "3", Market: "0xa", Asset: "1", Side: "BUY", Size: 1, Price: 0.55, Timestamp: 1200},
	}
	c, done := newDataClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		if limit == 0 {
			limit = 100
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
	}))
	defer done()
	trades, err := c.ListTradesSince(context.Background(), ListTradesOpts{Market: vo.MarketID("0xa"), Limit: 2})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(trades) != 3 {
		t.Fatalf("got %d", len(trades))
	}
	if trades[0].Size != 10 || trades[2].Size != 1 {
		t.Fatalf("trades: %+v", trades)
	}
}

func TestListTradesSinceFiltersBeforeTimestamp(t *testing.T) {
	page := []dataTrade{
		{ID: "1", Market: "0xa", Timestamp: 1000, Size: 1, Price: 0.5, Side: "BUY"},
		{ID: "2", Market: "0xa", Timestamp: 2000, Size: 1, Price: 0.5, Side: "BUY"},
		{ID: "3", Market: "0xa", Timestamp: 500, Size: 1, Price: 0.5, Side: "BUY"},
	}
	c, done := newDataClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		if offset > 0 {
			_ = json.NewEncoder(w).Encode([]dataTrade{})
			return
		}
		_ = json.NewEncoder(w).Encode(page)
	}))
	defer done()
	got, err := c.ListTradesSince(context.Background(), ListTradesOpts{
		Market: "0xa",
		Since:  time.Unix(800, 0),
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
