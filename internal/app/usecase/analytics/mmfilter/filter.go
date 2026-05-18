// Package mmfilter suppresses single-trade alerts on wallets whose recent
// activity on the same (market, outcome) looks like market-making or
// arbitrage rather than informed flow.
//
// The signal we are trying to find — an informed-flow candidate — is
// directional: a wallet buys YES and holds, or sells YES and holds, because
// they believe the price is wrong. A wallet that has been doing roughly
// balanced BUY+SELL on the same outcome over the last day is almost
// certainly a liquidity provider or arbitrageur. We want to filter that
// noise out before paging an operator.
//
// Mechanism: over a configurable lookback (default 24h), pull the wallet's
// per-side activity on the current (market, outcome). The wallet is judged
// "two-sided" (and the alert is suppressed) when both:
//
//  1. count(BUY)  ≥ MinTradesPerSide  AND  count(SELL) ≥ MinTradesPerSide
//  2. abs(buyNotional − sellNotional) / max(buyNotional, sellNotional) ≤ NeutralityTol
//
// (1) ensures we are not suppressing on noise — a single profit-take SELL
// after a directional BUY does not flip the classification. (2) is the
// "balanced book" check: the larger of the two notionals must be within
// (1+NeutralityTol)× the smaller.
//
// Cluster alerts are deliberately NOT filtered here. Even if some of the
// participating wallets are MMs, multiple wallets converging on one side of
// a category is a signal worth paging — the cluster detector's own
// gates (unique-wallets, total-notional, cooldown) make that judgement.
package mmfilter

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/infra/repository"
)

// SideActivityFetcher is the slice of *repository.TraderRepository the
// filter consumes. Abstracted as an interface for tests.
type SideActivityFetcher interface {
	SideActivity(ctx context.Context, traderID, marketID int64, outcomeToken string, since time.Time) (repository.TraderSideActivity, error)
}

// TraderResolver resolves wallet → trader id, used to skip the filter when
// the wallet has not been persisted yet (in which case "no MM signal" is
// the safe default — do not suppress).
type TraderResolver interface {
	GetByWallet(ctx context.Context, wallet string) (repository.Trader, error)
}

// Config tunes the filter.
//
//   - Enabled: master switch. When false, Filter.Decide always returns a
//     "pass" verdict — useful for local exploration or when a recent
//     misclassification is being investigated.
//   - Lookback: how far back we examine activity. 0 means "no lookback bound"
//     (the trader's full stored history on the bucket).
//   - MinTradesPerSide: minimum BUY and minimum SELL trades for the wallet to
//     even be considered two-sided. Default 4.
//   - NeutralityTol: how close the buy/sell notionals must be (as a fraction
//     of the larger side) to count as balanced. Default 0.3 = 30%.
type Config struct {
	Enabled          bool
	Lookback         time.Duration
	MinTradesPerSide int
	NeutralityTol    float64
	// Clock optionally overrides time.Now (tests).
	Clock func() time.Time
}

// Verdict is the structured outcome of Decide. Suppress=true means the
// detector should drop the single-trade alert; the Reason captures
// human-readable rationale for the metric label and the alert payload (in
// case operators inspect a suppressed alert later).
type Verdict struct {
	Suppress bool
	Reason   string
	// Diagnostic fields surfaced for alert payload + metrics.
	BuyCount        int64
	SellCount       int64
	BuyNotionalUSD  float64
	SellNotionalUSD float64
	Imbalance       float64 // |buy − sell| / max(buy, sell); 0 when one side is 0
}

// Filter is the runtime object. Safe for concurrent use; no internal state.
type Filter struct {
	cfg      Config
	now      func() time.Time
	traders  TraderResolver
	activity SideActivityFetcher
}

// New constructs a Filter. traders and activity may be nil only when
// cfg.Enabled=false (the filter degenerates to a pass-through). When
// cfg.Enabled=true both repositories are required.
func New(cfg Config, traders TraderResolver, activity SideActivityFetcher) *Filter {
	if cfg.MinTradesPerSide <= 0 {
		cfg.MinTradesPerSide = 4
	}
	if cfg.NeutralityTol <= 0 {
		cfg.NeutralityTol = 0.3
	}
	now := cfg.Clock
	if now == nil {
		now = time.Now
	}
	return &Filter{cfg: cfg, now: now, traders: traders, activity: activity}
}

// Decide classifies a candidate trade. Returns Suppress=false (pass) when:
//   - the filter is disabled;
//   - the wallet is unknown (never persisted — caller treats unknown as
//     "no MM evidence");
//   - the wallet's per-side activity does not clear both the MinTradesPerSide
//     gates;
//   - the wallet's two-sided notional imbalance exceeds NeutralityTol
//     (i.e. it is directional enough to remain a candidate).
//
// Repository errors are non-fatal: the filter defaults to a pass-through
// verdict so that DB hiccups don't accidentally squash real alerts.
func (f *Filter) Decide(ctx context.Context, wallet string, marketID int64, outcomeToken string) (Verdict, error) {
	if f == nil || !f.cfg.Enabled {
		return Verdict{}, nil
	}
	if wallet == "" || marketID == 0 || outcomeToken == "" || f.traders == nil || f.activity == nil {
		return Verdict{}, nil
	}

	t, err := f.traders.GetByWallet(ctx, wallet)
	if err != nil {
		// Unknown trader: no MM evidence by definition.
		return Verdict{Reason: "trader-unknown"}, nil
	}

	since := time.Time{}
	if f.cfg.Lookback > 0 {
		since = f.now().Add(-f.cfg.Lookback)
	}
	act, err := f.activity.SideActivity(ctx, t.ID, marketID, outcomeToken, since)
	if err != nil {
		// Fail-open: a DB hiccup must not suppress a legitimate alert.
		return Verdict{Reason: fmt.Sprintf("activity-error: %v", err)}, err
	}

	v := Verdict{
		BuyCount:        act.BuyCount,
		SellCount:       act.SellCount,
		BuyNotionalUSD:  act.BuyNotionalUSD,
		SellNotionalUSD: act.SellNotionalUSD,
		Imbalance:       imbalance(act.BuyNotionalUSD, act.SellNotionalUSD),
	}

	if act.BuyCount < int64(f.cfg.MinTradesPerSide) || act.SellCount < int64(f.cfg.MinTradesPerSide) {
		v.Reason = "one-sided-or-thin"
		return v, nil
	}
	if v.Imbalance > f.cfg.NeutralityTol {
		v.Reason = fmt.Sprintf("imbalance %.2f > %.2f", v.Imbalance, f.cfg.NeutralityTol)
		return v, nil
	}

	v.Suppress = true
	v.Reason = fmt.Sprintf("balanced two-sided activity (buy=%d sell=%d imbalance=%.2f)",
		act.BuyCount, act.SellCount, v.Imbalance)
	return v, nil
}

// imbalance returns |buy − sell| / max(buy, sell), or 0 when both sides are
// zero. A perfectly balanced book yields 0; a fully one-sided book yields 1.
func imbalance(buy, sell float64) float64 {
	if buy <= 0 && sell <= 0 {
		return 0
	}
	hi := math.Max(buy, sell)
	if hi <= 0 {
		return 0
	}
	return math.Abs(buy-sell) / hi
}
