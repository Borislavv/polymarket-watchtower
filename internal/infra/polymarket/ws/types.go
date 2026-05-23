// Package ws is the Polymarket CLOB WebSocket fast-lane.
//
// API reference (verified 2026-05-21):
//
//   - Endpoint: wss://ws-subscriptions-clob.polymarket.com/ws/market
//   - Auth: none for the market channel.
//   - Subscribe: {"assets_ids":["<token_id>",…], "type":"market"}.
//   - Ping: client sends literal text "PING" every 10s, server "PONG".
//   - Message types: book / price_change / tick_size_change /
//     last_trade_price / best_bid_ask / new_market / market_resolved.
//
// IMPORTANT contract — this package does INFRA ONLY:
//
//   - connect, subscribe, decode, normalise, persist via the worker
//     callback, reconnect with backoff;
//   - never calls AI, never calls Telegram, never raises severity;
//   - emits Events into a bounded output channel; the worker
//     consumes + persists.
//
// The polling / backfill / reconciliation paths remain authoritative.
// WS is a low-latency trigger accelerator, not a source of truth.
package ws

import (
	"encoding/json"
	"time"
)

// EventType is the normalised classification we surface to the
// realtime worker. Verbatim Polymarket strings on the wire are
// mapped here so downstream code never sees raw API enums.
const (
	EventTypeBook           = "book"
	EventTypePriceChange    = "price_change"
	EventTypeLastTradePrice = "last_trade_price"
	EventTypeBestBidAsk     = "best_bid_ask"
	EventTypeTickSizeChange = "tick_size_change"
	EventTypeMarketResolved = "market_resolved"
	EventTypeNewMarket      = "new_market"
	EventTypeHeartbeat      = "heartbeat"
	EventTypeUnknown        = "unknown"
)

// SideSource records WHICH layer determined the side of a trade-like
// event. Strategy / detection paths MUST prefer onchain/data_api over
// websocket — the WS side is informational, not authoritative.
const (
	SideSourceWebsocket = "websocket"
	SideSourceDataAPI   = "data_api"
	SideSourceOnchain   = "onchain"
	SideSourceInferred  = "inferred"
	SideSourceUnknown   = "unknown"
)

// SideConfidence helpers — keep magic numbers off call sites.
const (
	SideConfidenceWebsocket float64 = 0.5
	SideConfidenceDataAPI   float64 = 0.95
	SideConfidenceOnchain   float64 = 1.0
)

// MarketSubscription is one (event_slug, condition_id, market_slug,
// clob_token_ids) tuple. The worker fans these out to per-token
// subscription messages on the wire.
type MarketSubscription struct {
	EventSlug    string
	ConditionID  string
	MarketSlug   string
	CLOBTokenIDs []string
	// OutcomeByToken maps token id → outcome label (e.g.
	// "Yes"/"No" or the candidate name) so the event-emit path
	// can fill `Event.Outcome` without re-resolving via the mapper.
	OutcomeByToken map[string]string
}

// SubscriptionSet is the per-cycle world view the worker hands to
// the client. The diff between cycles drives subscribe / unsubscribe.
type SubscriptionSet struct {
	Markets []MarketSubscription
}

// Event is the normalised view the worker consumes. Side / SideSource /
// SideConfidence are deliberately tagged so the strategy layer can
// downgrade severity when only WS evidence is available.
//
// All time.Time values are UTC. nil pointers ⇒ "not present on this
// message"; downstream code never invents values.
type Event struct {
	Source            string
	Type              string
	ReceivedAt        time.Time
	ExchangeTimestamp *time.Time

	EventSlug   string
	ConditionID string
	MarketSlug  string
	CLOBTokenID string
	Outcome     string

	Price *float64
	Size  *float64
	Side  string

	SideSource     string
	SideConfidence float64

	BestBid *float64
	BestAsk *float64
	Mid     *float64

	// v11.10: depth from the `book` event. nil for non-book events.
	// BidLevels is sorted DESC by price (best bid first); AskLevels
	// ASC (best ask first). Empty array means the event payload had
	// no levels; nil means "depth was not present on the wire".
	BidLevels []BookLevel
	AskLevels []BookLevel

	TxHash   string
	TradeID  string
	Wallet   string
	Sequence string

	Raw     json.RawMessage
	RawHash string
}

// Subscribe payload shape sent to the market channel.
type subscribeMsg struct {
	AssetsIDs []string `json:"assets_ids"`
	Type      string   `json:"type"`
}

// --- wire payload shapes ---------------------------------------------------
//
// These mirror the JSON the Polymarket CLOB WS emits today. Keep them
// permissive — Polymarket has historically added new fields without
// notice. The decoder treats unknown event_type values as
// EventTypeUnknown rather than failing the read loop.

type wireBookLevel struct {
	Price string `json:"price"`
	Size  string `json:"size"`
}

// BookLevel is the public-facing decoded level (price + size as
// float64). Exposed because the realtime worker / bookbars producer
// consume the depth array directly.
type BookLevel struct {
	Price float64
	Size  float64
}

type wireBook struct {
	EventType string          `json:"event_type"`
	AssetID   string          `json:"asset_id"`
	Market    string          `json:"market"`
	Bids      []wireBookLevel `json:"bids"`
	Asks      []wireBookLevel `json:"asks"`
	Timestamp string          `json:"timestamp"`
	Hash      string          `json:"hash"`
}

type wirePriceChangeRow struct {
	AssetID string `json:"asset_id"`
	Price   string `json:"price"`
	Size    string `json:"size"`
	Side    string `json:"side"`
	Hash    string `json:"hash"`
	BestBid string `json:"best_bid"`
	BestAsk string `json:"best_ask"`
}

type wirePriceChange struct {
	EventType    string               `json:"event_type"`
	Market       string               `json:"market"`
	PriceChanges []wirePriceChangeRow `json:"price_changes"`
	Timestamp    string               `json:"timestamp"`
}

type wireLastTrade struct {
	EventType  string `json:"event_type"`
	AssetID    string `json:"asset_id"`
	Market     string `json:"market"`
	Price      string `json:"price"`
	Side       string `json:"side"`
	Size       string `json:"size"`
	FeeRateBps string `json:"fee_rate_bps"`
	Timestamp  string `json:"timestamp"`
}

type wireBestBidAsk struct {
	EventType string `json:"event_type"`
	AssetID   string `json:"asset_id"`
	Market    string `json:"market"`
	BestBid   string `json:"best_bid"`
	BestAsk   string `json:"best_ask"`
	Spread    string `json:"spread"`
	Timestamp string `json:"timestamp"`
}

type wireTickSize struct {
	EventType   string `json:"event_type"`
	AssetID     string `json:"asset_id"`
	Market      string `json:"market"`
	OldTickSize string `json:"old_tick_size"`
	NewTickSize string `json:"new_tick_size"`
	Timestamp   string `json:"timestamp"`
}

type wireMarketResolved struct {
	EventType      string   `json:"event_type"`
	ID             string   `json:"id"`
	Market         string   `json:"market"`
	AssetsIDs      []string `json:"assets_ids"`
	WinningAssetID string   `json:"winning_asset_id"`
	WinningOutcome string   `json:"winning_outcome"`
	Timestamp      string   `json:"timestamp"`
}
