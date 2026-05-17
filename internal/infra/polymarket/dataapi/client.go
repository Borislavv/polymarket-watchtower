// Package dataapi is the adapter for https://data-api.polymarket.com.
// We use it as the authoritative source of public trade events.
package dataapi

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/domain/trade"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/vo"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/polymarket/httpx"
)

type Client struct {
	h *httpx.Client
}

func New(h *httpx.Client) *Client { return &Client{h: h} }

// ListTradesOpts is the filter set for /trades.
type ListTradesOpts struct {
	Market vo.MarketID // condition id, required for our use-case
	Since  time.Time   // inclusive lower bound on trade timestamp
	Limit  int         // page size (Data API caps around 500)
}

// ListTradesSince returns every public trade for Market with timestamp >= Since.
// The API supports an offset-style pagination plus a timestamp filter; we walk
// pages oldest-first until a page returns nothing or every row is before Since.
func (c *Client) ListTradesSince(ctx context.Context, opts ListTradesOpts) ([]trade.Trade, error) {
	if opts.Market == "" {
		return nil, fmt.Errorf("dataapi: market required")
	}
	pageSize := opts.Limit
	if pageSize <= 0 || pageSize > 500 {
		pageSize = 100
	}
	var (
		out    []trade.Trade
		offset int
	)
	for {
		q := url.Values{}
		q.Set("market", string(opts.Market))
		q.Set("limit", strconv.Itoa(pageSize))
		q.Set("offset", strconv.Itoa(offset))
		if !opts.Since.IsZero() {
			q.Set("filterType", "TIMESTAMP")
			q.Set("filterAmount", strconv.FormatInt(opts.Since.Unix(), 10))
		}
		var page []dataTrade
		if err := c.h.GetJSON(ctx, "/trades", q, &page); err != nil {
			return nil, fmt.Errorf("dataapi trades offset=%d: %w", offset, err)
		}
		if len(page) == 0 {
			break
		}
		stop := false
		for _, t := range page {
			ts := time.Unix(t.Timestamp, 0).UTC()
			if !opts.Since.IsZero() && ts.Before(opts.Since) {
				stop = true
				continue
			}
			out = append(out, trade.Trade{
				ID:        t.ID,
				Market:    vo.MarketID(t.Market),
				Token:     vo.TokenID(t.Asset),
				Side:      mapSide(t.Side),
				Price:     t.Price,
				Size:      t.Size,
				Timestamp: ts,
				TxHash:    t.TransactionHash,
				Taker:     t.Taker,
				Maker:     t.Maker,
			})
		}
		if stop || len(page) < pageSize {
			break
		}
		offset += pageSize
	}
	return out, nil
}

func mapSide(s string) trade.Side {
	switch s {
	case "BUY", "buy":
		return trade.SideBuy
	case "SELL", "sell":
		return trade.SideSell
	}
	return trade.Side(s)
}
