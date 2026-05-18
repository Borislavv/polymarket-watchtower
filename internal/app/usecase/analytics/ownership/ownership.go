// Package ownership is the market-ownership concentration detector
// (Strategy E). It evaluates whether a single wallet has accumulated a
// significant fraction of an outcome's trade flow, expressed as a
// percentage with three severity tiers (Info / Warning / Critical) plus
// an optional Hard at the very top.
//
// IMPORTANT — this is a TRADE-FLOW APPROXIMATION, not a real holders
// read. The watchtower currently has no Polymarket holders endpoint
// wired (CLOB_API_URL is in config but unused). The percentage produced
// here is:
//
//	SharePct = wallet_net_shares / market_total_shares
//
// where `wallet_net_shares` is the wallet's BUY-side size_shares minus
// SELL-side size_shares on this (market, outcome), and
// `market_total_shares` is SUM(size_shares) for BUY trades on the
// outcome across all wallets. The wallet may have transferred shares
// off-chain, may have sold to other wallets we don't track, or may have
// accumulated at very different prices — so the percentage is
// directional, not authoritative. The detector flags this on every
// Verdict (Approximate=true) and the Telegram renderer surfaces it.
//
// The detector is pure: no I/O. The caller fetches the trade-flow
// totals through the repository and hands the detector a Shares value.
package ownership

import (
	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/anomaly"
)

// ReasonCode is a structured tag attached to the Finding so an operator
// can see at a glance which tier and shape fired.
type ReasonCode string

const (
	// ReasonConcentration is always attached to a fired Verdict.
	ReasonConcentration ReasonCode = "MARKET_OWNERSHIP_CONCENTRATION"
	// ReasonDominates is added when the wallet's share crossed the
	// highest (Critical) tier — the wallet effectively dominates the
	// outcome's recorded flow.
	ReasonDominates ReasonCode = "WALLET_DOMINATES_OUTCOME"
)

// Shares carries the per-(wallet, market, outcome) flow totals the
// detector needs. The repository computes these server-side; the
// detector only does the math.
//
// All counts are share counts (the trade size_shares field), not USD.
// Using shares keeps the percentage stable against price drift — a
// position worth $1000 at $0.10 is the same 10000 shares regardless of
// what the price moves to.
type Shares struct {
	// WalletBuyShares is SUM(size_shares) WHERE side='BUY' for this
	// (wallet, market, outcome).
	WalletBuyShares float64
	// WalletSellShares is SUM(size_shares) WHERE side='SELL' for this
	// (wallet, market, outcome).
	WalletSellShares float64
	// MarketBuyShares is SUM(size_shares) WHERE side='BUY' across ALL
	// wallets on this (market, outcome). The denominator of the
	// percentage.
	MarketBuyShares float64
	// PriceHint is the last observed trade price for the outcome (used
	// only to convert net shares into a USD notional estimate for the
	// alert payload). 0 disables the notional estimate.
	PriceHint float64
}

// Net is WalletBuyShares − WalletSellShares.
func (s Shares) Net() float64 { return s.WalletBuyShares - s.WalletSellShares }

// SharePct is Net / MarketBuyShares × 100. Returns 0 when the
// denominator is zero (no recorded BUYs on the outcome — should never
// happen if the detector is invoked alongside accumulation).
func (s Shares) SharePct() float64 {
	if s.MarketBuyShares <= 0 {
		return 0
	}
	return s.Net() / s.MarketBuyShares * 100.0
}

// NotionalUSD is a coarse estimate of the wallet's position dollar
// value: net_shares × PriceHint. The wallet may have built the
// position at very different prices, so this is order-of-magnitude
// only — useful as a sanity check, not as an exact figure.
func (s Shares) NotionalUSD() float64 {
	if s.PriceHint <= 0 {
		return 0
	}
	return s.Net() * s.PriceHint
}

// Config tunes the detector. Defaults applied by New.
type Config struct {
	// Enabled is the master switch.
	Enabled bool
	// InfoPct is the lower of the three severity tiers (default 10).
	// Crossing this with a non-trivial absolute position triggers Info.
	InfoPct float64
	// WarningPct is the middle tier (default 15).
	WarningPct float64
	// CriticalPct is the top tier (default 25). Wallet effectively
	// dominates the outcome's recorded flow.
	CriticalPct float64
	// MinNotionalUSD is the absolute-position floor. The detector
	// suppresses fires on tiny positions even when they technically
	// cross a percentage tier — a wallet owning 30% of a market with
	// $50 of recorded flow is noise, not signal.
	MinNotionalUSD float64
}

func (c Config) applyDefaults() Config {
	if c.InfoPct <= 0 {
		c.InfoPct = 10
	}
	if c.WarningPct <= 0 {
		c.WarningPct = 15
	}
	if c.CriticalPct <= 0 {
		c.CriticalPct = 25
	}
	if c.MinNotionalUSD <= 0 {
		c.MinNotionalUSD = 10_000
	}
	return c
}

// Verdict is the structured outcome.
type Verdict struct {
	// Fired is true when the wallet's share crossed at least the Info
	// tier AND the absolute-position floor.
	Fired bool
	// Severity is the highest tier the share cleared.
	Severity anomaly.Severity
	// SharePct is the wallet's percentage of the outcome's recorded
	// BUY-side flow. Always populated for diagnostic purposes, even
	// when Fired=false.
	SharePct float64
	// NetShares is WalletBuy − WalletSell.
	NetShares float64
	// NotionalUSD is the coarse dollar estimate (Net × PriceHint).
	NotionalUSD float64
	// Reasons surfaces the structured tags for the alert payload.
	Reasons []ReasonCode
}

// Detector evaluates a Shares value against the configured tiers.
// Pure and concurrency-safe (no internal state).
type Detector struct {
	cfg Config
}

// New constructs a Detector with defaults applied.
func New(cfg Config) *Detector {
	return &Detector{cfg: cfg.applyDefaults()}
}

// Config returns the applied configuration.
func (d *Detector) Config() Config { return d.cfg }

// Decide produces a Verdict. Disabled config or below-floor inputs
// return Fired=false with diagnostic SharePct still populated.
func (d *Detector) Decide(s Shares) Verdict {
	v := Verdict{
		SharePct:    s.SharePct(),
		NetShares:   s.Net(),
		NotionalUSD: s.NotionalUSD(),
	}
	if !d.cfg.Enabled {
		return v
	}
	// Absolute-position floor: avoids firing on dust markets where one
	// wallet "owns 50%" of $200 of flow.
	if v.NotionalUSD < d.cfg.MinNotionalUSD {
		return v
	}
	switch {
	case v.SharePct >= d.cfg.CriticalPct:
		v.Fired = true
		v.Severity = anomaly.SeverityCritical
		v.Reasons = []ReasonCode{ReasonConcentration, ReasonDominates}
	case v.SharePct >= d.cfg.WarningPct:
		v.Fired = true
		v.Severity = anomaly.SeverityWarning
		v.Reasons = []ReasonCode{ReasonConcentration}
	case v.SharePct >= d.cfg.InfoPct:
		v.Fired = true
		v.Severity = anomaly.SeverityInfo
		v.Reasons = []ReasonCode{ReasonConcentration}
	}
	return v
}
