// Package anomaly models trade-level anomaly detection results and the
// category-cluster ("watch required") alerts derived from them.
//
// The legacy aggregate-rate signals (trade_rate, notional_rate, avg_size) are
// retained as supporting telemetry but no longer emit Findings — they were the
// wrong unit of detection for the product goal, which is to spot individual
// abnormal bets that suggest insider activity.
package anomaly

import (
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/trade"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/vo"
)

// Kind discriminates Finding payload shape. Sinks switch on Kind to render.
type Kind string

const (
	// KindTradeAnomaly is a single trade whose USD notional crossed either the
	// per-bucket multiplier ladder or an absolute USD tier.
	KindTradeAnomaly Kind = "trade_anomaly"
	// KindCategoryWatch is a category cluster: N anomalous trades by M unique
	// wallets in a single category within a sliding window.
	KindCategoryWatch Kind = "category_watch"
)

// Severity is a coarse classification routed by sinks and dashboards.
type Severity string

const (
	SeverityInfo     Severity = "info"     // x30 single trade
	SeverityWarning  Severity = "warning"  // x100 single trade
	SeverityCritical Severity = "critical" // x1000 single trade
	SeverityHard     Severity = "hard"     // category cluster — human review required
)

// Maxer returns the higher of two severities (right-bias on equality).
func MaxSeverity(a, b Severity) Severity {
	if rank(a) >= rank(b) {
		return a
	}
	return b
}

// RankAtLeast returns s if it already ranks at floor or higher, else "".
func RankAtLeast(s, floor Severity) Severity {
	if rank(s) >= rank(floor) {
		return s
	}
	return ""
}

func rank(s Severity) int {
	switch s {
	case SeverityHard:
		return 4
	case SeverityCritical:
		return 3
	case SeverityWarning:
		return 2
	case SeverityInfo:
		return 1
	}
	return 0
}

// TradeRef is the snapshot of the trade that fired the anomaly.
type TradeRef struct {
	ID          string
	TxHash      string
	Wallet      string // proxyWallet from Data API; "" if absent
	Market      vo.MarketID
	Slug        string
	Question    string
	Outcome     string // "Yes"/"No"/... from market metadata; "" if unknown
	Side        trade.Side
	SizeShares  float64
	Price       float64
	Odds        float64 // 1/Price; copied so sinks don't redo the math
	NotionalUSD float64
	At          time.Time
}

// FromTrade builds a TradeRef with market-derived enrichment.
type Marketish interface {
	GetSlug() string
	GetQuestion() string
}

// BaselineRef is the per-bucket baseline that the trade was compared against.
type BaselineRef struct {
	// Scope is a human-readable bucket label, e.g.
	// "category=Politics market=us-pres outcome=Yes".
	Scope     string
	MedianUSD float64
	MeanUSD   float64
	P95USD    float64
	SampleN   int
	// Span is the actual time-span the baseline samples cover (newest minus
	// oldest). This is what sinks should display — operators must see the
	// real data span, not a confusingly-large configured cap.
	Span time.Duration
	// WindowMax is the configured upper bound; 0 means "no cap".
	WindowMax time.Duration
}

// CategoryRef identifies the category that fired a CategoryWatch alert (or the
// primary category for a single-trade anomaly).
type CategoryRef struct {
	ID    vo.CategoryID
	Slug  string
	Label string
}

// ClusterStats summarises a category-cluster alert.
type ClusterStats struct {
	Window          time.Duration
	AnomalousTrades int
	UniqueWallets   int
	TotalUSD        float64
	// Sample is a small head of contributing trades for inline context (capped
	// in the cluster detector to keep payload size bounded).
	Sample []TradeRef
}

// Finding is the envelope every sink receives. Fields that don't apply to the
// Kind are left zero; sinks select what to render.
type Finding struct {
	Kind     Kind
	Severity Severity
	At       time.Time

	// Reason names the rule that fired ("multiplier", "absolute_tier", "cluster").
	Reason string

	// Single-trade anomaly fields.
	Trade          *TradeRef
	Baseline       *BaselineRef
	Multiplier     float64  // observed NotionalUSD / Baseline.MedianUSD (0 if N/A)
	AbsoluteTier   Severity // tier crossed by the (notional, odds) pair
	MultiplierTier Severity // tier crossed by the multiplier ladder

	// Cluster fields.
	Category *CategoryRef
	Cluster  *ClusterStats

	// Lifecycle.
	LifecyclePct float64 // 0..100; 0 when unknown (no start/end on the market)
	Hot          bool    // true when the market is in its final HotFromPct window

	// Cluster context on single-trade alerts (the per-trade signal also
	// reports how many anomalous siblings sit in the current category window).
	InCluster        bool
	ClusterPeerCount int

	// Links — sinks render whichever are populated.
	MarketURL   string
	CategoryURL string
	TraderURL   string
	GrafanaURL  string
}
