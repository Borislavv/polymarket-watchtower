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
	"strings"
	"time"

	market2 "github.com/Borislavv/polymarket-watchtower/internal/domain/model/market"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/vo"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/polymarket/httpx"
)

// MaxListOffset is the Gamma-side cap on /markets and /events offset.
// Going past this returns a 422 validation error with body
//
//	{"type":"validation error","error":"offset exceeds maximum allowed for markets list queries"}
//
// The pagers stop at this cap and return whatever they have so far —
// the discover sweep is best-effort, not all-or-nothing, and a cap-hit
// must not poison the whole tick (otherwise persistence, baseline and
// alerting downstream see zero new state).
const MaxListOffset = 10000

// Client is the Gamma adapter.
type Client struct {
	h *httpx.Client
}

// New wraps the supplied httpx.Client.
func New(h *httpx.Client) *Client { return &Client{h: h} }

// isOffsetCapError recognises the Gamma 422 returned when the requested
// offset is past MaxListOffset. We match on both the status code AND a
// body substring so a different 422 (a real validation error in the
// query) still surfaces as a fatal upstream error. The pagers guard
// against issuing such a request, but the upstream cap can move over
// time — this is the second line of defence.
func isOffsetCapError(err error) bool {
	var apiErr *httpx.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	if apiErr.Status != 422 && apiErr.Status != 400 {
		return false
	}
	return strings.Contains(apiErr.Body, "offset exceeds maximum")
}

// ListTags returns every category-style tag known to Gamma.
func (c *Client) ListTags(ctx context.Context) ([]market2.Category, error) {
	const pageSize = 100
	var (
		out    []market2.Category
		offset int
	)
	for {
		if offset >= MaxListOffset {
			break
		}
		q := url.Values{}
		q.Set("limit", strconv.Itoa(pageSize))
		q.Set("offset", strconv.Itoa(offset))
		var page []gammaTag
		if err := c.h.GetJSON(ctx, "/tags", q, &page); err != nil {
			if isOffsetCapError(err) {
				break
			}
			return nil, fmt.Errorf("gamma tags page offset=%d: %w", offset, err)
		}
		for _, t := range page {
			out = append(out, market2.Category{
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
func (c *Client) ListMarkets(ctx context.Context, opts ListMarketsOpts) ([]market2.Market, error) {
	const pageSize = 100
	var (
		out    []market2.Market
		offset int
	)
	for {
		if offset >= MaxListOffset {
			// Reached the documented upstream cap. Treat as a clean stop and
			// return what we have rather than letting the next request 422.
			break
		}
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
			if isOffsetCapError(err) {
				break
			}
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
		if offset >= MaxListOffset {
			break
		}
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
			if isOffsetCapError(err) {
				break
			}
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

// MarketResolution is the resolution snapshot of a single market —
// enough for the outcomes worker to decide if an alert was correct.
// Returned by GetMarketResolution; never leaked outside the gamma
// adapter when this method is used elsewhere.
type MarketResolution struct {
	ConditionID   string
	Closed        bool
	Archived      bool
	EndDate       time.Time
	TokenIDs      []string
	OutcomeLabels []string
	OutcomePrices []float64 // index-aligned with TokenIDs / OutcomeLabels
}

// GetMarketResolution returns the resolution snapshot for one market.
// Found=false when Gamma doesn't return the market (typically: archived
// > 90d, or unknown conditionID). The caller distinguishes "not yet
// resolved" (Closed=false) from "resolved" (Closed=true) by inspecting
// the returned MarketResolution.
func (c *Client) GetMarketResolution(ctx context.Context, conditionID string) (MarketResolution, bool, error) {
	if conditionID == "" {
		return MarketResolution{}, false, errors.New("gamma: conditionId required")
	}
	q := url.Values{}
	q.Set("condition_ids", conditionID)
	q.Set("limit", "1")
	var page []gammaMarket
	if err := c.h.GetJSON(ctx, "/markets", q, &page); err != nil {
		return MarketResolution{}, false, fmt.Errorf("gamma markets resolution %s: %w", conditionID, err)
	}
	if len(page) == 0 {
		return MarketResolution{}, false, nil
	}
	raw := page[0]
	tokens, _ := parseStringArray(raw.ClobTokenIDsRaw)
	labels, _ := parseStringArray(raw.OutcomesJSON)
	prices, _ := parseFloatArray(raw.OutcomePricesRaw)
	endDate, _ := parseTime(raw.EndDate)
	return MarketResolution{
		ConditionID:   raw.ConditionID,
		Closed:        raw.Closed,
		Archived:      raw.Archived,
		EndDate:       endDate,
		TokenIDs:      tokens,
		OutcomeLabels: labels,
		OutcomePrices: prices,
	}, true, nil
}

// parseFloatArray decodes Gamma's JSON-encoded numeric arrays. They land
// as quoted strings like `["1","0"]` or `["0.5","0.5"]` so we strip
// quotes and parse each element.
func parseFloatArray(raw string) ([]float64, error) {
	if raw == "" {
		return nil, nil
	}
	var s []string
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return nil, err
	}
	out := make([]float64, 0, len(s))
	for _, v := range s {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
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

func mapMarket(m gammaMarket) (market2.Market, error) {
	if m.ConditionID == "" {
		return market2.Market{}, errors.New("gamma: missing conditionId")
	}
	tokens, _ := parseStringArray(m.ClobTokenIDsRaw)
	tokenIDs := make([]vo.TokenID, 0, len(tokens))
	for _, t := range tokens {
		tokenIDs = append(tokenIDs, vo.TokenID(t))
	}
	outcomes, _ := parseStringArray(m.OutcomesJSON)
	start, _ := parseTime(m.StartDate)
	end, _ := parseTime(m.EndDate)
	var eventSlug, eventTitle string
	for _, e := range m.Events {
		if e.Slug != "" {
			eventSlug = e.Slug
			eventTitle = e.Title
			break
		}
	}
	return market2.Market{
		ID:          vo.MarketID(m.ConditionID),
		Slug:        m.Slug,
		Question:    m.Question,
		ConditionID: m.ConditionID,
		TokenIDs:    tokenIDs,
		Outcomes:    outcomes,
		EventSlug:   eventSlug,
		EventTitle:  eventTitle,
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
