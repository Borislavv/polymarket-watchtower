//go:build integration

// External integration smoke tests that hit the live public Polymarket Gamma
// API. Opt-in only:
//
//	POLYMARKET_INTEGRATION=1 go test -tags=integration ./internal/core/infra/polymarket/gamma/...
package gamma

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/core/infra/polymarket/httpx"
	"github.com/Borislavv/polymarket-watchtower/internal/core/infra/ratelimit"
)

func skipUnlessEnabled(t *testing.T) {
	if os.Getenv("POLYMARKET_INTEGRATION") != "1" {
		t.Skip("set POLYMARKET_INTEGRATION=1 to run live API tests")
	}
}

func liveClient(t *testing.T) *Client {
	t.Helper()
	h, err := httpx.New(httpx.Config{
		BaseURL:  "https://gamma-api.polymarket.com",
		Timeout:  10 * time.Second,
		Limiter:  ratelimit.New(2, 4),
		MaxRetry: 2,
	})
	if err != nil {
		t.Fatalf("httpx: %v", err)
	}
	return New(h)
}

func TestLive_ListMarketsActiveOne(t *testing.T) {
	skipUnlessEnabled(t)
	c := liveClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	got, err := c.ListMarkets(ctx, ListMarketsOpts{ActiveOnly: true, MaxRows: 1})
	if err != nil {
		t.Fatalf("ListMarkets: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("expected at least one active market")
	}
	if got[0].ID == "" {
		t.Errorf("first market missing id: %+v", got[0])
	}
}

func TestLive_ListTagsAtLeastOne(t *testing.T) {
	skipUnlessEnabled(t)
	c := liveClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	got, err := c.ListTags(ctx)
	if err != nil {
		t.Fatalf("ListTags: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("expected at least one tag")
	}
}
