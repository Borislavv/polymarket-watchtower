// holders.go — Polymarket Data API /holders client.
//
// Endpoint contract (verified against live API on 2026-05-23):
//
//	GET /holders?market=<conditionId>[&limit=N]
//
// Response is an array (one entry per outcome token) of:
//
//	[
//	  {
//	    "token": "<clob_token_id>",
//	    "holders": [
//	      {
//	        "proxyWallet": "0x…",
//	        "asset": "<clob_token_id>",
//	        "amount": 944.5,
//	        "outcomeIndex": 1,
//	        "pseudonym": "…",
//	        ...
//	      }
//	    ]
//	  }, …
//	]
//
// Auth: none for the holders feed. Limit defaults around 14-16 per
// token; pagination via repeat ?limit= isn't supported the way
// /trades supports offset — the holders feed is bounded server-side
// to the top-N holders per token.
//
// Open Interest derivation: Polymarket does NOT expose a separate
// /open-interest endpoint. The honest derivation is
// SUM(holders.amount) per token. Callers that need OI must compute
// it from this response — we never invent a denominator.
package dataapi

import (
	"context"
	"errors"
	"net/url"
	"strconv"

	"github.com/Borislavv/polymarket-watchtower/internal/domain/vo"
)

// HolderRow is one row of (token, wallet, shares, outcome_index) the
// caller stores in polymarket_holder_snapshots.
type HolderRow struct {
	Token        string  // CLOB token id
	Wallet       string  // proxyWallet
	Amount       float64 // shares
	OutcomeIndex int     // 0 / 1 typically
	Pseudonym    string
}

// HoldersByToken groups HolderRow by token + carries a derived OI
// total per token (SUM(amount)) — caller may use this as denominator
// for pct_oi calculations without inventing data.
type HoldersByToken struct {
	Token        string
	OpenInterest float64 // SUM(amount) over Holders below
	Holders      []HolderRow
}

// ListHoldersOpts controls the /holders call.
type ListHoldersOpts struct {
	Market vo.MarketID // condition id; required
	Limit  int         // server-side cap per token; clamped [1, 200]
}

// wire shape for one holder entry.
type wireHolder struct {
	ProxyWallet  string  `json:"proxyWallet"`
	Asset        string  `json:"asset"`
	Amount       float64 `json:"amount"`
	OutcomeIndex int     `json:"outcomeIndex"`
	Pseudonym    string  `json:"pseudonym"`
}

// wire shape for one (token, holders[]) group.
type wireHoldersByToken struct {
	Token   string       `json:"token"`
	Holders []wireHolder `json:"holders"`
}

// ListHolders returns the per-token holder snapshot for a market.
// Returned slices are sorted by Amount DESC per token so the caller
// can compute rank directly. Empty response is NOT an error — it
// is reported as zero rows so the worker can record honest stale
// state instead of synthetic data.
func (c *Client) ListHolders(ctx context.Context, opts ListHoldersOpts) ([]HoldersByToken, error) {
	if opts.Market == "" {
		return nil, errors.New("dataapi.ListHolders: market is required")
	}
	q := url.Values{}
	q.Set("market", string(opts.Market))
	if opts.Limit > 0 {
		l := opts.Limit
		if l > 200 {
			l = 200
		}
		q.Set("limit", strconv.Itoa(l))
	}

	var raw []wireHoldersByToken
	if err := c.h.GetJSON(ctx, "/holders", q, &raw); err != nil {
		return nil, err
	}
	out := make([]HoldersByToken, 0, len(raw))
	for _, g := range raw {
		group := HoldersByToken{Token: g.Token}
		// Aggregate + sort DESC by amount.
		for _, h := range g.Holders {
			if h.ProxyWallet == "" || h.Amount <= 0 {
				continue
			}
			group.Holders = append(group.Holders, HolderRow{
				Token:        g.Token,
				Wallet:       h.ProxyWallet,
				Amount:       h.Amount,
				OutcomeIndex: h.OutcomeIndex,
				Pseudonym:    h.Pseudonym,
			})
			group.OpenInterest += h.Amount
		}
		// Insertion sort DESC by Amount — n is tiny (top-N).
		for i := 1; i < len(group.Holders); i++ {
			for j := i; j > 0 && group.Holders[j-1].Amount < group.Holders[j].Amount; j-- {
				group.Holders[j-1], group.Holders[j] = group.Holders[j], group.Holders[j-1]
			}
		}
		out = append(out, group)
	}
	return out, nil
}
