// Package gamma is the adapter for https://gamma-api.polymarket.com.
// It returns domain.Market and domain.Category values; raw DTOs do not leak.
package gamma

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/core/domain/market"
	"github.com/Borislavv/polymarket-watchtower/internal/core/domain/vo"
	"github.com/Borislavv/polymarket-watchtower/internal/core/infra/polymarket/httpx"
)

// Client is the Gamma adapter.
type Client struct {
	h *httpx.Client
}

// New wraps the supplied httpx.Client.
func New(h *httpx.Client) *Client { return &Client{h: h} }

// ListTags returns every category-style tag known to Gamma.
func (c *Client) ListTags(ctx context.Context) ([]market.Category, error) {
	const pageSize = 100
	var (
		out    []market.Category
		offset int
	)
	for {
		q := url.Values{}
		q.Set("limit", strconv.Itoa(pageSize))
		q.Set("offset", strconv.Itoa(offset))
		var page []gammaTag
		if err := c.h.GetJSON(ctx, "/tags", q, &page); err != nil {
			return nil, fmt.Errorf("gamma tags page offset=%d: %w", offset, err)
		}
		for _, t := range page {
			out = append(out, market.Category{
				ID:    vo.CategoryID(t.ID),
				Slug:  t.Slug,
				Label: t.Label,
			})
		}
		if len(page) < pageSize {
			break
		}
		offset += pageSize
	}
	return out, nil
}

// ListMarketsOpts narrows ListMarkets. Zero-value means "no filter".
type ListMarketsOpts struct {
	ActiveOnly bool
	OrderBy    string // e.g. "volume_24hr"
	MaxRows    int    // soft cap on total rows fetched (0 = unlimited)
}

// ListMarkets pages through Gamma /markets and returns domain markets.
// Events are fetched in parallel to enrich category (tag) information; if the
// event lookup fails for a given market we still return the market with empty
// categories.
func (c *Client) ListMarkets(ctx context.Context, opts ListMarketsOpts) ([]market.Market, error) {
	const pageSize = 100
	var (
		out    []market.Market
		offset int
	)
	for {
		q := url.Values{}
		q.Set("limit", strconv.Itoa(pageSize))
		q.Set("offset", strconv.Itoa(offset))
		q.Set("closed", "false")
		if opts.ActiveOnly {
			q.Set("active", "true")
		}
		if opts.OrderBy != "" {
			q.Set("order", opts.OrderBy)
			q.Set("ascending", "false")
		}
		var page []gammaMarket
		if err := c.h.GetJSON(ctx, "/markets", q, &page); err != nil {
			return nil, fmt.Errorf("gamma markets page offset=%d: %w", offset, err)
		}
		for _, m := range page {
			dm, err := mapMarket(m)
			if err != nil {
				continue
			}
			out = append(out, dm)
			if opts.MaxRows > 0 && len(out) >= opts.MaxRows {
				return out, nil
			}
		}
		if len(page) < pageSize {
			break
		}
		offset += pageSize
	}
	return out, nil
}

// ListEvents pages Gamma /events. Used to harvest category memberships per
// market: each event carries tags + a list of markets.
func (c *Client) ListEvents(ctx context.Context, opts ListMarketsOpts) ([]gammaEvent, error) {
	const pageSize = 100
	var (
		out    []gammaEvent
		offset int
	)
	for {
		q := url.Values{}
		q.Set("limit", strconv.Itoa(pageSize))
		q.Set("offset", strconv.Itoa(offset))
		q.Set("closed", "false")
		if opts.ActiveOnly {
			q.Set("active", "true")
		}
		if opts.OrderBy != "" {
			q.Set("order", opts.OrderBy)
			q.Set("ascending", "false")
		}
		var page []gammaEvent
		if err := c.h.GetJSON(ctx, "/events", q, &page); err != nil {
			return nil, fmt.Errorf("gamma events page offset=%d: %w", offset, err)
		}
		out = append(out, page...)
		if len(page) < pageSize {
			break
		}
		offset += pageSize
		if opts.MaxRows > 0 && len(out) >= opts.MaxRows {
			break
		}
	}
	return out, nil
}

// MapEventsToMarketCategories indexes event tags by market condition id, so a
// caller that holds a []market.Market can backfill Categories without another
// round-trip.
func MapEventsToMarketCategories(events []gammaEvent) map[string][]vo.CategoryID {
	out := make(map[string][]vo.CategoryID, len(events))
	for _, e := range events {
		tags := make([]vo.CategoryID, 0, len(e.Tags))
		for _, t := range e.Tags {
			tags = append(tags, vo.CategoryID(int64(t.ID)))
		}
		for _, m := range e.Markets {
			if m.ConditionID == "" {
				continue
			}
			out[m.ConditionID] = append(out[m.ConditionID], tags...)
		}
	}
	return out
}

func mapMarket(m gammaMarket) (market.Market, error) {
	if m.ConditionID == "" {
		return market.Market{}, errors.New("gamma: missing conditionId")
	}
	tokens, _ := parseStringArray(m.ClobTokenIDsRaw)
	tokenIDs := make([]vo.TokenID, 0, len(tokens))
	for _, t := range tokens {
		tokenIDs = append(tokenIDs, vo.TokenID(t))
	}
	start, _ := parseTime(m.StartDate)
	end, _ := parseTime(m.EndDate)
	return market.Market{
		ID:          vo.MarketID(m.ConditionID),
		Slug:        m.Slug,
		Question:    m.Question,
		ConditionID: m.ConditionID,
		TokenIDs:    tokenIDs,
		Active:      m.Active,
		Closed:      m.Closed,
		StartDate:   start,
		EndDate:     end,
		Volume:      m.Volume,
		Volume24h:   m.Volume24h,
		Liquidity:   m.Liquidity,
	}, nil
}

func parseStringArray(raw string) ([]string, error) {
	if raw == "" {
		return nil, nil
	}
	var s []string
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return nil, err
	}
	return s, nil
}

func parseTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339, s)
}
