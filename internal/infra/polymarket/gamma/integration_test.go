//go:build integration

// External integration smoke tests that hit the live public Polymarket Gamma
// API. Opt-in only:
//
//	POLYMARKET_INTEGRATION=1 go test -tags=integration ./internal/core/infra/polymarket/gamma/...
package gamma

import (
	"context"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/infra/polymarket/httpx"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/ratelimit"
)

type netHTTPClient struct{ timeout time.Duration }

func (c *netHTTPClient) head(ctx context.Context, url string) int {
	cl := &http.Client{Timeout: c.timeout}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := cl.Do(req)
	if err != nil {
		return 0
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode
}

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

// TestLive_EventSlugProducesReachableURL proves the contract that backs
// alert link generation: the EVENT slug from Gamma's /markets[].events[]
// resolves to a live page, while the market slug alone does not.
func TestLive_EventSlugProducesReachableURL(t *testing.T) {
	skipUnlessEnabled(t)
	c := liveClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	markets, err := c.ListMarkets(ctx, ListMarketsOpts{ActiveOnly: true, MaxRows: 20})
	if err != nil {
		t.Fatalf("ListMarkets: %v", err)
	}
	var withEvent *struct{ marketSlug, eventSlug string }
	for _, m := range markets {
		if m.EventSlug != "" && m.EventSlug != m.Slug {
			withEvent = &struct{ marketSlug, eventSlug string }{m.Slug, m.EventSlug}
			break
		}
	}
	if withEvent == nil {
		t.Skip("none of the top live markets are grouped under a multi-outcome event")
	}

	http := &netHTTPClient{timeout: 10 * time.Second}
	if status := http.head(ctx, "https://polymarket.com/event/"+withEvent.eventSlug); status != 200 {
		t.Fatalf("event URL must return 200, got %d for slug=%s", status, withEvent.eventSlug)
	}
	// The known-broken pattern. We don't *require* this to 404 (Polymarket
	// could change behavior), but the test documents what we observed and
	// why we route through the event slug.
	t.Logf("market-slug URL (informational): /event/%s", withEvent.marketSlug)
}
