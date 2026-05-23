// Package thesisaccum implements the v11.5 Cross-market Thesis
// Accumulation primary detector. The detector is PURE — no
// repository, network, or AI access. The orchestration layer
// (detect.Loop or a periodic worker) resolves the market-link
// graph + per-wallet directional lines and hands them to Decide().
//
// Detection thesis: a single wallet that builds aligned exposure
// across multiple superficially-distinct markets sharing one
// political thesis is a higher-signal informed-flow pattern than
// any single-market accumulation. The detector measures three
// things — breadth (how many markets), consistency (how aligned
// the directions are after normalising for the link graph), and
// magnitude (the normalised aligned exposure) — and combines them
// into a tiered verdict.
package thesisaccum

import "time"

// LinkDirection mirrors polymarket_market_links.direction: aligned
// means the linked market resolves "with" the source thesis,
// opposed means it resolves "against" it (mirror outcome), unknown
// means we only know they're in the same event/series.
type LinkDirection string

const (
	DirAligned LinkDirection = "aligned"
	DirOpposed LinkDirection = "opposed"
	DirUnknown LinkDirection = "unknown"
)

// Link is one edge from the source market to a related market in
// the thesis graph. Orchestration loads these from
// polymarket_market_links.
type Link struct {
	DstConditionID string
	LinkType       string
	Direction      LinkDirection
	Confidence     float64
}

// WalletLine is a per-(wallet, market, side) directional position
// summary the orchestration layer prepares. The detector treats
// (NetSharesUSD, Trades) as the raw signal; the layer is
// responsible for the SQL aggregate.
type WalletLine struct {
	ConditionID       string
	Side              string  // YES / NO / BUY / SELL — keyed to the alert's side semantics
	NetSharesUSD      float64 // signed USD exposure on the alert side
	Trades            int
	WindowStart       time.Time
	LiquidityFloor    float64 // market liquidity floor used for normalisation
	BaselineMedianUSD float64
}

// Catalyst is a forward-looking catalyst the orchestration layer
// passed in. Only the time-to-catalyst + confidence are needed for
// the boost; the rest is metadata.
type Catalyst struct {
	Kind       string
	ExpectedAt time.Time
	Confidence float64
}

// Input is the pure-Decide payload.
type Input struct {
	// Source market (the one the current trade / signal sits on).
	SourceConditionID string
	SourceEventSlug   string
	Wallet            string
	Side              string
	Now               time.Time

	// Graph + wallet history.
	Links       []Link       // dst markets in the thesis graph (excluding source)
	WalletLines []WalletLine // wallet's directional lines INCLUDING the source line

	// Optional catalyst window context (e.g. SCOTUS opinion drop,
	// election day, debate). The detector uses the soonest
	// high-confidence catalyst, if any.
	Catalysts []Catalyst

	// Maximum lifecycle pct observed across linked markets — when
	// the linked markets are deep in their lifecycle the signal is
	// later-stage and a small boost applies. Bounded [0..100].
	MaxLifecyclePctOnGraph float64
}

// Verdict is the pure-Decide output. The orchestration layer reads
// Fired + Level + Score to decide whether to persist a shadow row
// and whether (post-promotion) to fire a standalone alert.
type Verdict struct {
	Fired           bool
	Level           string  // info | warning | critical | none
	Score           float64 // 0..100; tier thresholds come from cfg
	Confidence      float64 // 0..1
	Breadth         int     // count of aligned markets including source
	Consistency     float64 // aligned / (aligned + opposed)
	AlignedExposure float64 // sum of normalised aligned USD
	OpposedExposure float64 // sum of normalised opposed USD
	AlignedMarkets  []string
	OpposedMarkets  []string
	Reasons         []string
	Features        map[string]any
}
