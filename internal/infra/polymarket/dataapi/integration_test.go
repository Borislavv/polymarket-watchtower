//go:build integration

// Live Polymarket Data API smoke tests. Opt-in only — set
// POLYMARKET_INTEGRATION=1 and pass -tags=integration. Hits the public API
// with conservative limits so it cannot accidentally flood the upstream.
package dataapi

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/infra/polymarket/httpx"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/ratelimit"
)

func skipUnlessEnabled(t *testing.T) {
	if os.Getenv("POLYMARKET_INTEGRATION") != "1" {
		t.Skip("set POLYMARKET_INTEGRATION=1 to run live API tests")
	}
}

func liveClient(t *testing.T) *Client {
	t.Helper()
	h, err := httpx.New(httpx.Config{
		BaseURL:  "https://data-api.polymarket.com",
		Timeout:  10 * time.Second,
		Limiter:  ratelimit.New(2, 4),
		MaxRetry: 1,
	})
	if err != nil {
		t.Fatalf("httpx: %v", err)
	}
	return New(h)
}

// Live contract regression: a CASH/TOKENS filterType must not 400; the legacy
// TIMESTAMP value must 400 with the documented error body. If either changes,
// our adapter assumptions need to be revisited.
func TestLive_FilterTypeContract(t *testing.T) {
	skipUnlessEnabled(t)
	c := liveClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// CASH must succeed with a small page.
	_, err := c.ListTradesSince(ctx, ListTradesOpts{
		Market:   "0x34754b217c530433402b30f238fee045760592c93c06f977b0a805911005c723",
		Limit:    1,
		MaxPages: 1,
		MinValue: &MinValueFilter{Type: FilterCash, Amount: 1},
	})
	if err != nil {
		t.Fatalf("CASH filter unexpectedly failed: %v", err)
	}

	// TIMESTAMP must still 400 — proving the regression we fixed.
	q, _ := buildTradesQuery(ListTradesOpts{Market: "0xa"}, 1, 0)
	q.Set("filterType", "TIMESTAMP")
	q.Set("filterAmount", "1700000000")
	var sink []dataTrade
	rawErr := c.h.GetJSON(ctx, "/trades", q, &sink)
	if rawErr == nil {
		t.Fatal("expected 400 for filterType=TIMESTAMP, upstream contract changed")
	}
	var apiErr *httpx.APIError
	if !errors.As(rawErr, &apiErr) || apiErr.Status != 400 {
		t.Fatalf("want APIError(400), got %v", rawErr)
	}
}

func TestLive_ListTradesPerMarketNewestFirst(t *testing.T) {
	skipUnlessEnabled(t)
	c := liveClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	trades, err := c.ListTradesSince(ctx, ListTradesOpts{
		Market:   "0x34754b217c530433402b30f238fee045760592c93c06f977b0a805911005c723",
		Since:    time.Now().Add(-7 * 24 * time.Hour),
		Limit:    50,
		MaxPages: 2,
	})
	if err != nil {
		t.Fatalf("ListTradesSince: %v", err)
	}
	if len(trades) == 0 {
		t.Skip("market has no recent trades — try a different conditionId")
	}
	for i := 1; i < len(trades); i++ {
		if trades[i].Timestamp.After(trades[i-1].Timestamp) {
			t.Fatalf("per-market order is not DESC: trade[%d]=%s newer than trade[%d]=%s",
				i, trades[i].Timestamp, i-1, trades[i-1].Timestamp)
		}
	}
}
