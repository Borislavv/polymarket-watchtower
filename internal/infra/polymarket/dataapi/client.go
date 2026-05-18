// Package dataapi is the adapter for https://data-api.polymarket.com.
// It is the authoritative source of public trade events.
//
// Endpoint contract (verified against the live API on 2026-05-17):
//
//	GET /trades?market=<conditionId>&limit=N&offset=K
//	    [&filterType=CASH|TOKENS&filterAmount=X]
//	    [&takerOnly=true|false]
//
// Per-market results are ordered by timestamp DESC (newest first). There is
// NO server-side "since"/"from"/"until" timestamp parameter — recent-window
// filtering must happen client-side by paging newest→oldest and stopping at
// the cutoff.
//
// The "filterType" param is a value filter (CASH = USD notional, TOKENS =
// share count), NOT a sort key. Passing filterType=TIMESTAMP returns 400.
package dataapi

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/trade"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/vo"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/polymarket/httpx"
)

// FilterType is the typed enum of legal /trades filterType values. The Data
// API rejects anything else with HTTP 400.
type FilterType string

const (
	FilterCash   FilterType = "CASH"   // filterAmount = USD notional threshold
	FilterTokens FilterType = "TOKENS" // filterAmount = token quantity threshold
)

func (f FilterType) valid() bool {
	switch f {
	case FilterCash, FilterTokens:
		return true
	}
	return false
}

const (
	defaultPageSize = 100
	maxPageSize     = 500 // Data API hard cap on ?limit
	// MaxUpstreamOffset is the documented (and observed in 400 responses)
	// historical-activity offset cap: "max historical activity offset of 3000
	// exceeded". A request with ?offset>3000 returns HTTP 400. Pagination must
	// stop before crossing this boundary. Exposed because the BackfillWorker
	// uses it to classify "complete" vs "partial_api_limit" outcomes.
	MaxUpstreamOffset = 3000
	maxUpstreamOffset = MaxUpstreamOffset // internal alias to keep diff minimal
	defaultMaxPages   = 50
)

type Client struct {
	h *httpx.Client
}

func New(h *httpx.Client) *Client { return &Client{h: h} }

// ListTradesOpts is the filter set for /trades.
type ListTradesOpts struct {
	// Market is the condition id; required.
	Market vo.MarketID
	// Since is the inclusive client-side cutoff. Pagination stops once a page
	// returns a trade older than Since (per-market order is DESC).
	Since time.Time
	// Limit is the page size; clamped to [1, maxPageSize]; 0 => default.
	Limit int
	// MaxPages caps total pages walked per call; 0 => default.
	MaxPages int
	// MinValue is an optional server-side value filter. Partial config returns
	// an error rather than being silently dropped.
	MinValue *MinValueFilter
}

// MinValueFilter expresses the (filterType, filterAmount) pair.
type MinValueFilter struct {
	Type   FilterType
	Amount float64
}

// ListTradesSince returns trades for opts.Market with timestamp >= opts.Since.
// Pages are walked offset 0..N (newest-to-oldest by time within a market)
// until any of:
//   - a page comes back empty,
//   - a page contains a trade older than opts.Since,
//   - the per-call page cap is reached.
//
// Returned trades have Market overwritten to the requested condition id so
// downstream code is independent of the upstream JSON field name.
func (c *Client) ListTradesSince(ctx context.Context, opts ListTradesOpts) ([]trade.Trade, error) {
	if opts.Market == "" {
		return nil, errors.New("dataapi: market required")
	}
	pageSize := opts.Limit
	if pageSize <= 0 {
		pageSize = defaultPageSize
	}
	if pageSize > maxPageSize {
		pageSize = maxPageSize
	}
	maxPages := opts.MaxPages
	if maxPages <= 0 {
		maxPages = defaultMaxPages
	}

	var (
		out    []trade.Trade
		offset int
	)
	for page := 0; page < maxPages; page++ {
		if offset > maxUpstreamOffset {
			// Upstream rejects offset > 3000; surface the cap as a clean stop
			// rather than letting it become a 400 on the next request.
			break
		}
		q, err := buildTradesQuery(opts, pageSize, offset)
		if err != nil {
			return nil, err
		}

		var batch []dataTrade
		if err := c.h.GetJSON(ctx, "/trades", q, &batch); err != nil {
			return nil, fmt.Errorf("dataapi /trades market=%s offset=%d: %w", opts.Market, offset, err)
		}
		if len(batch) == 0 {
			break
		}

		stop := false
		for _, t := range batch {
			ts := time.Unix(t.Timestamp, 0).UTC()
			if !opts.Since.IsZero() && ts.Before(opts.Since) {
				stop = true
				continue
			}
			out = append(out, trade.Trade{
				ID:        t.ID,
				Market:    opts.Market, // canonical: what we asked for
				Token:     vo.TokenID(t.Asset),
				Side:      mapSide(t.Side),
				Price:     t.Price,
				Size:      t.Size,
				Timestamp: ts,
				TxHash:    t.TransactionHash,
				Taker:     t.ProxyWallet,
			})
		}
		if stop || len(batch) < pageSize {
			break
		}
		offset += pageSize
	}
	return out, nil
}

// buildTradesQuery centralises query construction so the filterType=TIMESTAMP
// regression cannot reappear: only fields explicitly handled here are emitted.
func buildTradesQuery(opts ListTradesOpts, pageSize, offset int) (url.Values, error) {
	q := url.Values{}
	q.Set("market", string(opts.Market))
	q.Set("limit", strconv.Itoa(pageSize))
	q.Set("offset", strconv.Itoa(offset))
	if opts.MinValue != nil {
		if !opts.MinValue.Type.valid() {
			return nil, fmt.Errorf("dataapi: invalid filterType %q (allowed: CASH, TOKENS)", opts.MinValue.Type)
		}
		q.Set("filterType", string(opts.MinValue.Type))
		q.Set("filterAmount", strconv.FormatFloat(opts.MinValue.Amount, 'f', -1, 64))
	}
	return q, nil
}

// ListTradesPage fetches exactly one offset-paged page of trades, newest
// first. Limit is clamped to [1, MaxPageSize]; offset above MaxUpstreamOffset
// returns ErrOffsetCapExceeded so the BackfillWorker can flip the market
// status to partial_api_limit without first absorbing a 400.
//
// Unlike ListTradesSince, this method does NOT walk pagination internally —
// callers (backfill worker) drive it so they can persist each page and
// commit progress between calls.
func (c *Client) ListTradesPage(ctx context.Context, market vo.MarketID, offset, limit int) ([]trade.Trade, error) {
	if market == "" {
		return nil, errors.New("dataapi: market required")
	}
	if offset < 0 {
		return nil, fmt.Errorf("dataapi: offset must be >= 0, got %d", offset)
	}
	if offset > MaxUpstreamOffset {
		return nil, ErrOffsetCapExceeded
	}
	if limit <= 0 {
		limit = defaultPageSize
	}
	if limit > maxPageSize {
		limit = maxPageSize
	}
	q := url.Values{}
	q.Set("market", string(market))
	q.Set("limit", strconv.Itoa(limit))
	q.Set("offset", strconv.Itoa(offset))

	var batch []dataTrade
	if err := c.h.GetJSON(ctx, "/trades", q, &batch); err != nil {
		return nil, fmt.Errorf("dataapi /trades market=%s offset=%d: %w", market, offset, err)
	}
	out := make([]trade.Trade, 0, len(batch))
	for _, t := range batch {
		out = append(out, trade.Trade{
			ID:        t.ID,
			Market:    market,
			Token:     vo.TokenID(t.Asset),
			Side:      mapSide(t.Side),
			Price:     t.Price,
			Size:      t.Size,
			Timestamp: time.Unix(t.Timestamp, 0).UTC(),
			TxHash:    t.TransactionHash,
			Taker:     t.ProxyWallet,
		})
	}
	return out, nil
}

// ErrOffsetCapExceeded is returned by ListTradesPage when the requested
// offset is beyond the Data API's documented 3000-row historical cap. The
// BackfillWorker maps this to BackfillPartialAPILimit.
var ErrOffsetCapExceeded = errors.New("dataapi: offset exceeds upstream cap")

func mapSide(s string) trade.Side {
	switch s {
	case "BUY", "buy":
		return trade.SideBuy
	case "SELL", "sell":
		return trade.SideSell
	}
	return trade.Side(s)
}
