package dataapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Borislavv/polymarket-watchtower/internal/domain/vo"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/polymarket/httpx"
)

const holdersFixture = `[
  {
    "token": "AAA",
    "holders": [
      {"proxyWallet":"0xA","asset":"AAA","amount":500.0,"outcomeIndex":0,"pseudonym":"Misty"},
      {"proxyWallet":"0xB","asset":"AAA","amount":100.5,"outcomeIndex":0,"pseudonym":"Worse"},
      {"proxyWallet":"0xC","asset":"AAA","amount":0,"outcomeIndex":0,"pseudonym":"Skipped"},
      {"proxyWallet":"","asset":"AAA","amount":99,"outcomeIndex":0,"pseudonym":"NoWallet"}
    ]
  },
  {
    "token": "BBB",
    "holders": [
      {"proxyWallet":"0xD","asset":"BBB","amount":250.0,"outcomeIndex":1,"pseudonym":""}
    ]
  }
]`

func newTestClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	h, err := httpx.New(httpx.Config{BaseURL: srv.URL, UserAgent: "test"})
	if err != nil {
		t.Fatalf("httpx.New: %v", err)
	}
	return New(h)
}

func TestListHolders_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/holders" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.URL.Query().Get("market") != "0xCID" {
			t.Errorf("market mismatch: %s", r.URL.Query().Get("market"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(holdersFixture))
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	got, err := c.ListHolders(context.Background(), ListHoldersOpts{Market: vo.MarketID("0xCID"), Limit: 25})
	if err != nil {
		t.Fatalf("ListHolders: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("token groups: got %d want 2", len(got))
	}
	gAAA := got[0]
	if gAAA.Token != "AAA" {
		t.Fatalf("token order: got %s", gAAA.Token)
	}
	// 0xC (amount=0) and "" (no wallet) must be skipped.
	if len(gAAA.Holders) != 2 {
		t.Fatalf("AAA holders: got %d want 2 (Amount=0 + empty wallet skipped)", len(gAAA.Holders))
	}
	// DESC sort: 0xA (500) before 0xB (100.5).
	if gAAA.Holders[0].Wallet != "0xA" || gAAA.Holders[1].Wallet != "0xB" {
		t.Fatalf("sort order: %+v", gAAA.Holders)
	}
	// OI = SUM(amount) = 500 + 100.5 = 600.5
	if gAAA.OpenInterest != 600.5 {
		t.Fatalf("OI: got %f want 600.5", gAAA.OpenInterest)
	}
}

func TestListHolders_EmptyResponseNotError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()
	c := newTestClient(t, srv)
	got, err := c.ListHolders(context.Background(), ListHoldersOpts{Market: "0xX"})
	if err != nil {
		t.Fatalf("empty list must not error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d want 0", len(got))
	}
}

func TestListHolders_RequiresMarket(t *testing.T) {
	c := newTestClient(t, httptest.NewServer(http.NotFoundHandler()))
	_, err := c.ListHolders(context.Background(), ListHoldersOpts{})
	if err == nil {
		t.Fatalf("expected error when market is empty")
	}
}

func TestListHolders_Non200Errors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(500)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	}))
	defer srv.Close()
	c := newTestClient(t, srv)
	_, err := c.ListHolders(context.Background(), ListHoldersOpts{Market: "0xX"})
	if err == nil {
		t.Fatalf("non-200 should surface as error")
	}
}
