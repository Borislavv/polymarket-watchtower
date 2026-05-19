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
	// KindAccumulation is a same-trader same-(market,outcome,side) line: one
	// wallet repeatedly building exposure on a single side. Detected by
	// internal/app/usecase/analytics/accumulation.
	KindAccumulation Kind = "accumulation"
	// KindOwnership is a market-ownership concentration alert: one
	// wallet's net (BUY − SELL) share count crossed a percentage of the
	// outcome's total flow. Trade-flow approximation — there is no
	// holders endpoint wired upstream, so the percentage is approximate.
	// Detected alongside KindAccumulation in the same detect tick.
	KindOwnership Kind = "ownership_concentration"
	// KindStableFavorite is a SEPARATE strategy from whale-flow
	// detection: a market is in its final 8% (default), a single
	// outcome trades inside the favorite-probability band (default
	// 55–85%), the price has been stable, no adverse drift, and
	// liquidity is reasonable. Fires on slow-time market state, not
	// per-trade — emitted by internal/app/usecase/stablefavorite.
	//
	// Surveillance read: "late-market convergence candidate with
	// meaningful remaining payout". Never described as risk-free.
	KindStableFavorite Kind = "stable_favorite"
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
	P99USD    float64
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

// QuietMarketRef carries the structured "quiet-market wake-up" context.
// Populated by internal/app/usecase/analytics/quietmarket.Detector when
// the firing single-trade or accumulation event lands on a historically
// quiet (market, outcome). Nil when the gate did not qualify.
type QuietMarketRef struct {
	TradesPerDay      float64
	NotionalPerDayUSD float64
	IdleDuration      time.Duration
	BaselineSpan      time.Duration
}

// AccumulationRef summarises a same-trader accumulation-line alert.
// Carries enough context for the Telegram formatter to render the alert
// without re-querying the DB.
type AccumulationRef struct {
	Wallet            string
	MarketID          string
	OutcomeToken      string
	Outcome           string // "Yes"/"No"/... from market metadata
	Side              string // "BUY" or "SELL"
	TradeCount        int
	TotalNotionalUSD  float64
	MeanNotionalUSD   float64
	MedianNotionalUSD float64
	MaxNotionalUSD    float64
	AvgOdds           float64
	MaxOdds           float64
	Span              time.Duration
	// Line-level multipliers — line total over baseline median.
	MarketMultiplier float64
	TraderMultiplier float64
	// Score is the 0..100 triage heuristic. Confidence is 0..1.
	Score      int
	Confidence float64
	// Reasons is a list of REASON_CODEs surfaced as structured tags in
	// the Telegram payload.
	Reasons []string
	// SizePath names which size-path qualified the line — "meaningful"
	// or "many-smalls". Empty when not fired.
	SizePath string
	// Window names the horizon the line was aggregated over ("recent" or
	// "lifetime"). Surfaces in the alert header so an operator can tell a
	// 24h burst apart from a 90-day slow drip without re-deriving from
	// Span.
	Window string
}

// DormantWalletRef is the context-booster payload attached to single-
// trade and accumulation Findings when the firing wallet has been
// idle for at least DORMANT_WALLET_MIN_IDLE before the firing trade
// and the trade clears DORMANT_WALLET_MIN_NOTIONAL_USD. Surveillance
// read: a wallet that built positions a year ago, sat dormant, then
// woke up the week of a binding event is qualitatively suspicious.
type DormantWalletRef struct {
	// LastSeenAt is the wallet's most recent trade timestamp STRICTLY
	// before the firing trade.
	LastSeenAt time.Time
	// IdleDuration is firingAt − LastSeenAt; zero when LastSeenAt is
	// zero (wallet has no prior history).
	IdleDuration time.Duration
}

// NewWalletRef is the context-booster payload attached to single-trade
// and accumulation Findings when the firing wallet is new (first seen
// recently, or thin history). Populated by the detector — empty wallet
// means the trader was not persisted yet, so wallet-age cannot be read.
// Surveillance read: a one-week-old wallet placing a $10k bet at 10x
// odds is qualitatively more suspicious than the same trade from a
// wallet with thousands of trades.
type NewWalletRef struct {
	// FirstSeenAt is the earliest persisted trade timestamp for this
	// wallet across all markets.
	FirstSeenAt time.Time
	// AgeAtTrade is now − FirstSeenAt. Zero when FirstSeenAt is zero
	// (wallet not yet persisted).
	AgeAtTrade time.Duration
	// HistoryTrades is the wallet's total trade count in
	// polymarket_trades at the time of the firing trade.
	HistoryTrades int64
	// IsNew is the boolean verdict: AgeAtTrade < NEW_WALLET_MAX_AGE OR
	// HistoryTrades ≤ NEW_WALLET_MAX_HISTORY_TRADES.
	IsNew bool
}

// OwnershipRef summarises a market-ownership concentration alert. The
// alert kind that carries this is the only standalone alert built from
// trade-flow approximation — there is no holders endpoint wired, so the
// percentage is "wallet's net BUY-share count divided by the market's
// total BUY-side share count". Documented limitation; consult the
// strategy doc before drawing inference from the exact percentage.
type OwnershipRef struct {
	Wallet       string
	MarketID     string
	OutcomeToken string
	Outcome      string
	// SharePct is the approximate percentage of the outcome's net-BUY
	// share count the wallet has accumulated (0..100). Approximation —
	// it reads BUYs minus SELLs across all stored trades for this
	// (market, outcome). Real on-chain holdings can differ.
	SharePct float64
	// WalletNetShares is the wallet's (BUY-side − SELL-side) share
	// count for this outcome. Used in the Telegram payload to convey
	// raw position size alongside the percentage.
	WalletNetShares float64
	// MarketTotalShares is the denominator: SUM(size_shares) for BUY
	// trades on this outcome. Useful for operators to sanity-check the
	// percentage.
	MarketTotalShares float64
	// NotionalUSD is the dollar value of the wallet's position
	// estimated as (net shares × current trade price). A weak estimate
	// — the wallet may have accumulated at very different prices — but
	// useful as an order-of-magnitude figure.
	NotionalUSD float64
	// Approximate flags this as a trade-flow approximation, not a true
	// holders read. Surfaced in Telegram so the operator never
	// mistakes the percentage for an authoritative position.
	Approximate bool
}

// StableFavoriteRef carries the structured payload of a
// late-market-stable-favorite alert. This is a DISTINCT signal from
// whale-flow detection — the favorite-probability band, stability
// stats, remaining payout, and cross-market context are surfaced so
// an operator can decide whether the alert is actionable without
// re-querying the source.
//
// Important rendering invariant: NEVER described as "safe" or
// "guaranteed" by any consumer of this struct. Political markets gap
// on late news; the strategy quantifies stability but cannot remove
// underlying event risk.
type StableFavoriteRef struct {
	MarketID     string // condition_id
	OutcomeToken string
	Outcome      string // "Yes" / "No" / label from market metadata

	// Probability is the current price (= implied probability).
	Probability float64
	// RemainingReturnPct is (1 − price) / price expressed as a
	// percentage. Example: 0.65 → 53.8%.
	RemainingReturnPct float64

	// StabilityWindow is the lookback over which the price-stability
	// stats were computed (typically 24h, but the worker may use a
	// shorter window for very-near-resolution markets).
	StabilityWindow time.Duration

	// 24h-window stats: mean / stddev / min / max / first / last
	// price + observed sample count.
	PriceMean    float64
	PriceStddev  float64
	PriceMin     float64
	PriceMax     float64
	PriceFirst   float64
	PriceLast    float64
	PriceSamples int

	// Drawdown is (max − min) / max over the window — bounded in
	// [0, 1].
	Drawdown float64
	// AdverseMove6h is the price drift over the most recent 6h:
	// (price_now − price_6h_ago) for the favored side, so NEGATIVE
	// values indicate adverse drift (favorite weakening).
	AdverseMove6h float64

	// Liquidity proxies.
	RecentVolumeUSD  float64
	RecentTradeCount int

	LifecyclePct float64

	// Score is the 0..100 ranking heuristic — not a probability.
	// Confidence is the 0..1 adjustment driven by sample size and
	// cross-market availability.
	Score      float64
	Confidence float64

	// CrossMarketStatus is one of:
	//   "" / "unavailable" — no related-market data wired
	//   "confirmed"        — related market within MaxDisagreement of this price
	//   "conflict"         — related market disagrees beyond MaxDisagreement
	CrossMarketStatus string
	// CrossMarketDelta is the absolute price-difference between this
	// outcome and its cross-market match. Zero when CrossMarketStatus
	// is "" or "unavailable".
	CrossMarketDelta float64
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

	// Reason names the rule that fired ("LargeRareBet", "WhaleClusterDetected").
	Reason string

	// Single-trade anomaly fields.
	Trade          *TradeRef
	Baseline       *BaselineRef // per-(category,market,outcome) market distribution
	TraderBaseline *BaselineRef // wallet's full-history distribution (nil when trader axis disabled)

	// Payoff (computed from notional and price; surfaced so the Telegram
	// formatter doesn't redo the math).
	GrossPayoutIfWinUSD float64 // notional × odds
	ProfitIfWinUSD      float64 // notional × (odds − 1)

	// Tail ratios — notional divided by the corresponding p95/p99 of
	// each baseline. 0 means "baseline not ready or percentile missing";
	// formatter skips zero-valued rows.
	MarketP95Ratio float64
	MarketP99Ratio float64
	TraderP95Ratio float64
	TraderP99Ratio float64

	// Aggregate gate verdicts. PayoffGatePassed=false on a fired Finding
	// can only happen if every tier had MinProfitUSD=0 (gate disabled).
	// TailGatePassed=false means the trade fired on absolute+payoff with
	// no enforceable tail gate (neither baseline was ready).
	PayoffGatePassed bool
	TailGatePassed   bool

	// Baseline confidence flags (v6). True when the corresponding
	// baseline was unready at scoring time; surfaced in the Telegram
	// payload as warning lines so the operator can see the
	// uncertainty without re-deriving it. SeverityCapped is true when
	// the original tier was lowered because LowBaselineCapEnabled
	// kicked in.
	LowMarketBaselineConfidence bool
	LowTraderBaselineConfidence bool
	SeverityCapped              bool

	// Cluster fields.
	Category *CategoryRef
	Cluster  *ClusterStats

	// Accumulation fields (KindAccumulation only). Populated by
	// internal/app/usecase/analytics/accumulation.Detector.
	Accumulation *AccumulationRef

	// NewWallet is the wallet-age context booster, attached to single-
	// trade and accumulation Findings when the firing wallet matches the
	// "new wallet" gates. Nil when the wallet has substantial history
	// or when the booster is disabled.
	NewWallet *NewWalletRef

	// DormantWallet is the dormant-wallet context booster, attached
	// when the firing wallet has been idle long enough AND the
	// current trade is meaningfully sized. Nil when not dormant or
	// when the booster is disabled.
	DormantWallet *DormantWalletRef

	// Ownership is populated on KindOwnership Findings — never on
	// trade_anomaly / accumulation / category_watch.
	Ownership *OwnershipRef

	// StableFavorite is populated on KindStableFavorite Findings.
	// Distinct from trade-anomaly / accumulation — this is the
	// late-market-convergence strategy.
	StableFavorite *StableFavoriteRef

	// QuietMarket is the context tag attached when the firing event landed
	// on a historically quiet (market, outcome). Nil otherwise. Applies to
	// single-trade and accumulation Findings; cluster Findings do not
	// carry it because a multi-wallet cluster is intrinsically not quiet.
	QuietMarket *QuietMarketRef

	// Reasons is the flat list of structured reason codes that contributed
	// to this Finding. Accumulation Findings populate it from the
	// accumulation detector verdict; quiet-market wake-up appends its
	// canonical code when the QuietMarket gate qualifies. Single-trade
	// Findings carry only the quiet-market reason when applicable.
	Reasons []string

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

	// DedupKey is the alert's idempotency key as inserted into
	// polymarket_alerts. Surfaced to sinks so the rendered payload can
	// carry it in a `Data` block — operators correlating Telegram
	// messages with database rows or with Grafana logs use this string
	// as the primary join key.
	DedupKey string

	// AnalystNote is the AI-generated analysis text. Optional —
	// empty when the AI layer is disabled, missing an API key,
	// rate-limited, or otherwise unavailable. Telegram formatter
	// renders an "Analyst note" block when this is non-empty.
	AnalystNote string
}
