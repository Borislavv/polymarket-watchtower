// Package clob is the Polymarket CLOB REST adapter.
//
// Endpoint contract (verified against live API on 2026-05-23):
//
//	GET  /book?token_id=<clob_token>     — single token full orderbook depth.
//	POST /books, body=[{token_id:…}, …]  — batched call for N tokens.
//	GET  /midpoint?token_id=<clob_token>
//	GET  /price?token_id=<clob_token>&side=BUY|SELL
//	GET  /prices-history?market=<token_id>&interval=...&fidelity=...
//
// Auth: none for public read endpoints.
// /trades requires API key — we deliberately do NOT wrap that endpoint
// here; the public Data API /trades is the project's authoritative
// trade feed.
//
// The Book response carries full depth:
//
//	{"market":"<conditionId>",
//	 "asset_id":"<clob_token>",
//	 "timestamp":"<ms>",
//	 "bids":[{"price":"0.01","size":"30000"}, …],
//	 "asks":[{"price":"0.99","size":"500"}, …],
//	 "last_trade_price":"...",
//	 "min_order_size":"...",
//	 "neg_risk":false,
//	 "tick_size":"...",
//	 "hash":"..."}
//
// Decimals on the wire are strings — clients must parse defensively.
package clob

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DefaultBaseURL is the public CLOB endpoint.
const DefaultBaseURL = "https://clob.polymarket.com"

// Level is one price-level entry in the book (price + size).
type Level struct {
	Price float64
	Size  float64
}

// Book is a parsed orderbook snapshot for one token.
type Book struct {
	Market         string
	AssetID        string
	Timestamp      time.Time
	Hash           string
	LastTradePrice float64
	Bids           []Level // descending by price (best bid first)
	Asks           []Level // ascending by price (best ask first)
}

// Client is the CLOB REST adapter.
type Client struct {
	base    *url.URL
	http    *http.Client
	timeout time.Duration
}

// Config wires the Client.
type Config struct {
	BaseURL string
	Timeout time.Duration
}

// New constructs a Client. BaseURL defaults to the public endpoint.
func New(cfg Config) (*Client, error) {
	base := cfg.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	u, err := url.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("clob: parse base url: %w", err)
	}
	t := cfg.Timeout
	if t <= 0 {
		t = 5 * time.Second
	}
	return &Client{
		base:    u,
		http:    &http.Client{Timeout: t},
		timeout: t,
	}, nil
}

// GetBook returns the orderbook for one token.
func (c *Client) GetBook(ctx context.Context, tokenID string) (Book, error) {
	if tokenID == "" {
		return Book{}, errors.New("clob.GetBook: tokenID is required")
	}
	q := url.Values{}
	q.Set("token_id", tokenID)
	var w wireBook
	if err := c.getJSON(ctx, "/book", q, &w); err != nil {
		return Book{}, err
	}
	return parseBook(w), nil
}

// GetBooks batches N tokens in one POST. Polymarket accepts up to a
// few dozen tokens per call — callers should pre-split via the
// Config.BatchSize knob in the worker.
func (c *Client) GetBooks(ctx context.Context, tokenIDs []string) ([]Book, error) {
	if len(tokenIDs) == 0 {
		return nil, nil
	}
	body := make([]wireBooksReq, 0, len(tokenIDs))
	for _, id := range tokenIDs {
		if id == "" {
			continue
		}
		body = append(body, wireBooksReq{TokenID: id})
	}
	if len(body) == 0 {
		return nil, nil
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("clob.GetBooks: marshal: %w", err)
	}
	endpoint := *c.base
	endpoint.Path = "/books"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("clob.GetBooks: new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("clob.GetBooks: do: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		buf, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("clob.GetBooks: status %d body %q", resp.StatusCode, string(buf))
	}
	var ws []wireBook
	if err := json.NewDecoder(resp.Body).Decode(&ws); err != nil {
		return nil, fmt.Errorf("clob.GetBooks: decode: %w", err)
	}
	out := make([]Book, 0, len(ws))
	for _, w := range ws {
		out = append(out, parseBook(w))
	}
	return out, nil
}

// Midpoint returns the midpoint price for one token.
func (c *Client) Midpoint(ctx context.Context, tokenID string) (float64, error) {
	if tokenID == "" {
		return 0, errors.New("clob.Midpoint: tokenID is required")
	}
	q := url.Values{}
	q.Set("token_id", tokenID)
	var w struct {
		Mid string `json:"mid"`
	}
	if err := c.getJSON(ctx, "/midpoint", q, &w); err != nil {
		return 0, err
	}
	return parseFloatStr(w.Mid), nil
}

// getJSON is the GET+decode helper used by Book/Midpoint.
func (c *Client) getJSON(ctx context.Context, path string, query url.Values, out any) error {
	endpoint := *c.base
	endpoint.Path = strings.TrimSuffix(endpoint.Path, "/") + path
	endpoint.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return fmt.Errorf("clob: new request: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("clob: do: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		buf, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("clob: status %d body %q", resp.StatusCode, string(buf))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// --- wire shapes -------------------------------------------------

type wireLevel struct {
	Price string `json:"price"`
	Size  string `json:"size"`
}

type wireBook struct {
	Market         string      `json:"market"`
	AssetID        string      `json:"asset_id"`
	Timestamp      string      `json:"timestamp"`
	Hash           string      `json:"hash"`
	LastTradePrice string      `json:"last_trade_price"`
	Bids           []wireLevel `json:"bids"`
	Asks           []wireLevel `json:"asks"`
}

type wireBooksReq struct {
	TokenID string `json:"token_id"`
}

func parseBook(w wireBook) Book {
	b := Book{
		Market:         w.Market,
		AssetID:        w.AssetID,
		Hash:           w.Hash,
		LastTradePrice: parseFloatStr(w.LastTradePrice),
	}
	if ms, err := strconv.ParseInt(strings.TrimSpace(w.Timestamp), 10, 64); err == nil && ms > 0 {
		// timestamp may be unix ms or unix s; normalize to UTC time
		if ms < 1_000_000_000_000 {
			b.Timestamp = time.Unix(ms, 0).UTC()
		} else {
			b.Timestamp = time.UnixMilli(ms).UTC()
		}
	}
	b.Bids = parseLevels(w.Bids, true)
	b.Asks = parseLevels(w.Asks, false)
	return b
}

// parseLevels normalises decimal strings + sorts:
//   - bidsDescending=true → DESC by price (best bid first).
//   - bidsDescending=false → ASC by price (best ask first).
//
// The Polymarket API returns bids ASC and asks DESC (worst-first);
// we flip both so callers see best-of-book at index 0 regardless.
func parseLevels(raw []wireLevel, bidsDescending bool) []Level {
	out := make([]Level, 0, len(raw))
	for _, r := range raw {
		px := parseFloatStr(r.Price)
		sz := parseFloatStr(r.Size)
		if px <= 0 || sz <= 0 {
			continue
		}
		out = append(out, Level{Price: px, Size: sz})
	}
	// Insertion sort.
	if bidsDescending {
		for i := 1; i < len(out); i++ {
			for j := i; j > 0 && out[j-1].Price < out[j].Price; j-- {
				out[j-1], out[j] = out[j], out[j-1]
			}
		}
	} else {
		for i := 1; i < len(out); i++ {
			for j := i; j > 0 && out[j-1].Price > out[j].Price; j-- {
				out[j-1], out[j] = out[j], out[j-1]
			}
		}
	}
	return out
}

func parseFloatStr(s string) float64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return v
}

// TopNDepth returns the sum of sizes across the top-N levels.
func TopNDepth(levels []Level, topN int) float64 {
	if topN <= 0 || topN > len(levels) {
		topN = len(levels)
	}
	sum := 0.0
	for i := 0; i < topN; i++ {
		sum += levels[i].Size
	}
	return sum
}
