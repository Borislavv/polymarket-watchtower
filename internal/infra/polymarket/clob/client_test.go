package clob

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const bookFixture = `{
  "market": "0xCID",
  "asset_id": "TOK",
  "timestamp": "1779514708646",
  "hash": "abc",
  "last_trade_price": "0.50",
  "bids": [
    {"price":"0.01","size":"30000"},
    {"price":"0.49","size":"100"},
    {"price":"0.48","size":"200"}
  ],
  "asks": [
    {"price":"0.99","size":"500"},
    {"price":"0.51","size":"77"},
    {"price":"0.52","size":"123"}
  ]
}`

func TestGetBook_FullDepth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/book" {
			t.Errorf("path: %s", r.URL.Path)
		}
		if r.URL.Query().Get("token_id") != "TOK" {
			t.Errorf("token_id missing")
		}
		_, _ = w.Write([]byte(bookFixture))
	}))
	defer srv.Close()
	c, err := New(Config{BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	b, err := c.GetBook(context.Background(), "TOK")
	if err != nil {
		t.Fatalf("GetBook: %v", err)
	}
	if b.Market != "0xCID" || b.AssetID != "TOK" {
		t.Fatalf("ids: %+v", b)
	}
	if len(b.Bids) != 3 || len(b.Asks) != 3 {
		t.Fatalf("level counts: bids=%d asks=%d", len(b.Bids), len(b.Asks))
	}
	// Bids DESC: 0.49 > 0.48 > 0.01
	if b.Bids[0].Price != 0.49 || b.Bids[1].Price != 0.48 || b.Bids[2].Price != 0.01 {
		t.Fatalf("bid sort: %+v", b.Bids)
	}
	// Asks ASC: 0.51 < 0.52 < 0.99
	if b.Asks[0].Price != 0.51 || b.Asks[1].Price != 0.52 || b.Asks[2].Price != 0.99 {
		t.Fatalf("ask sort: %+v", b.Asks)
	}
	// TopNDepth — top 2 asks: 77 + 123 = 200.
	if d := TopNDepth(b.Asks, 2); d != 200 {
		t.Fatalf("topN: got %f want 200", d)
	}
	if b.LastTradePrice != 0.5 {
		t.Fatalf("last_trade_price: got %f", b.LastTradePrice)
	}
}

func TestGetBooks_Batch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/books" || r.Method != http.MethodPost {
			t.Errorf("path/method: %s %s", r.Method, r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		var req []wireBooksReq
		_ = json.Unmarshal(body, &req)
		if len(req) != 2 {
			t.Errorf("payload size: %d", len(req))
		}
		_, _ = w.Write([]byte("[" + bookFixture + "," + bookFixture + "]"))
	}))
	defer srv.Close()
	c, _ := New(Config{BaseURL: srv.URL})
	books, err := c.GetBooks(context.Background(), []string{"TOK", "TOK2"})
	if err != nil {
		t.Fatalf("GetBooks: %v", err)
	}
	if len(books) != 2 {
		t.Fatalf("books len: %d", len(books))
	}
}

func TestGetBook_InvalidDecimalsSkipped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{
"market":"X","asset_id":"T","timestamp":"0","hash":"","last_trade_price":"",
"bids":[{"price":"abc","size":"100"},{"price":"0.5","size":"-1"},{"price":"0.4","size":"50"}],
"asks":[{"price":"","size":"100"}]
}`))
	}))
	defer srv.Close()
	c, _ := New(Config{BaseURL: srv.URL})
	b, err := c.GetBook(context.Background(), "T")
	if err != nil {
		t.Fatalf("GetBook: %v", err)
	}
	if len(b.Bids) != 1 || b.Bids[0].Price != 0.4 {
		t.Fatalf("expected only 0.4/50 bid: %+v", b.Bids)
	}
	if len(b.Asks) != 0 {
		t.Fatalf("expected zero asks (empty price): %+v", b.Asks)
	}
}

func TestGetBook_RequiresTokenID(t *testing.T) {
	c, _ := New(Config{BaseURL: "http://example"})
	_, err := c.GetBook(context.Background(), "")
	if err == nil {
		t.Fatalf("expected error for empty tokenID")
	}
}

func TestGetBook_Non200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(500)
	}))
	defer srv.Close()
	c, _ := New(Config{BaseURL: srv.URL})
	_, err := c.GetBook(context.Background(), "T")
	if err == nil || !strings.Contains(err.Error(), "status 500") {
		t.Fatalf("non-200 must surface: %v", err)
	}
}

func TestMidpoint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"mid":"0.855"}`))
	}))
	defer srv.Close()
	c, _ := New(Config{BaseURL: srv.URL})
	m, err := c.Midpoint(context.Background(), "T")
	if err != nil {
		t.Fatalf("Midpoint: %v", err)
	}
	if m != 0.855 {
		t.Fatalf("mid: got %f", m)
	}
}
