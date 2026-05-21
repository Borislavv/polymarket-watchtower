package ws

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// decode is the cheap, allocation-conscious normalisation step the
// read loop calls per inbound message. It NEVER fails the connection
// — an unknown shape collapses to EventTypeUnknown with the raw bytes
// preserved so the worker can persist it for audit.
//
// `sub` is the MarketSubscription whose CLOB token id matches the
// incoming asset_id; the decoder uses it to fill event_slug /
// market_slug / outcome without re-querying the mapper.
type subResolver func(assetID, conditionID string) (MarketSubscription, bool)

func decode(raw []byte, resolver subResolver, now time.Time) Event {
	ev := Event{
		Source:     "polymarket_clob_ws",
		Type:       EventTypeUnknown,
		ReceivedAt: now,
	}
	if len(raw) == 0 {
		return ev
	}
	ev.Raw = append(json.RawMessage(nil), raw...)
	ev.RawHash = hashRaw(raw)
	// Peek at event_type without unmarshalling the whole payload —
	// avoids allocating the union-of-shapes when only the type is
	// needed.
	var head struct {
		EventType string `json:"event_type"`
		AssetID   string `json:"asset_id"`
		Market    string `json:"market"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		// Polymarket's literal "PONG" replies arrive as plain text
		// frames, not JSON. Treat as heartbeat so the read loop
		// keeps its timer alive without an error.
		if isPong(raw) {
			ev.Type = EventTypeHeartbeat
			return ev
		}
		return ev
	}
	ev.Type = normaliseType(head.EventType)
	ev.ConditionID = head.Market
	ev.CLOBTokenID = head.AssetID
	if resolver != nil {
		if sub, ok := resolver(head.AssetID, head.Market); ok {
			ev.EventSlug = sub.EventSlug
			ev.MarketSlug = sub.MarketSlug
			if sub.OutcomeByToken != nil {
				ev.Outcome = sub.OutcomeByToken[head.AssetID]
			}
		}
	}
	switch ev.Type {
	case EventTypeBook:
		decodeBook(raw, &ev)
	case EventTypePriceChange:
		decodePriceChange(raw, &ev)
	case EventTypeLastTradePrice:
		decodeLastTrade(raw, &ev)
	case EventTypeBestBidAsk:
		decodeBestBidAsk(raw, &ev)
	case EventTypeTickSizeChange:
		// No price/size to carry — type + raw is sufficient.
	case EventTypeMarketResolved:
		decodeMarketResolved(raw, &ev)
	}
	return ev
}

func decodeBook(raw []byte, ev *Event) {
	var b wireBook
	if err := json.Unmarshal(raw, &b); err != nil {
		return
	}
	if t := parseUnixMillis(b.Timestamp); !t.IsZero() {
		ev.ExchangeTimestamp = &t
	}
	bid := bestLevel(b.Bids, true)
	ask := bestLevel(b.Asks, false)
	if bid != nil {
		ev.BestBid = bid
	}
	if ask != nil {
		ev.BestAsk = ask
	}
	if bid != nil && ask != nil {
		mid := (*bid + *ask) / 2
		ev.Mid = &mid
	}
	ev.Sequence = b.Hash
}

func decodePriceChange(raw []byte, ev *Event) {
	var p wirePriceChange
	if err := json.Unmarshal(raw, &p); err != nil {
		return
	}
	if t := parseUnixMillis(p.Timestamp); !t.IsZero() {
		ev.ExchangeTimestamp = &t
	}
	if len(p.PriceChanges) == 0 {
		return
	}
	// The "summary" the caller cares about is the first row — the
	// asset id should already match the subscription we routed to.
	first := p.PriceChanges[0]
	if px := parseFloatStr(first.Price); px != nil {
		ev.Price = px
	}
	if sz := parseFloatStr(first.Size); sz != nil {
		ev.Size = sz
	}
	if bid := parseFloatStr(first.BestBid); bid != nil {
		ev.BestBid = bid
	}
	if ask := parseFloatStr(first.BestAsk); ask != nil {
		ev.BestAsk = ask
	}
	if ev.BestBid != nil && ev.BestAsk != nil {
		mid := (*ev.BestBid + *ev.BestAsk) / 2
		ev.Mid = &mid
	}
	ev.Side = normaliseSide(first.Side)
	ev.SideSource = SideSourceWebsocket
	ev.SideConfidence = SideConfidenceWebsocket
	ev.Sequence = first.Hash
}

func decodeLastTrade(raw []byte, ev *Event) {
	var t wireLastTrade
	if err := json.Unmarshal(raw, &t); err != nil {
		return
	}
	if ts := parseUnixMillis(t.Timestamp); !ts.IsZero() {
		ev.ExchangeTimestamp = &ts
	}
	if px := parseFloatStr(t.Price); px != nil {
		ev.Price = px
	}
	if sz := parseFloatStr(t.Size); sz != nil {
		ev.Size = sz
	}
	ev.Side = normaliseSide(t.Side)
	// CLOB ws does NOT carry on-chain wallet / tx_hash — strategy
	// layer must not treat this side as authoritative. Side source
	// stays websocket / low confidence.
	ev.SideSource = SideSourceWebsocket
	ev.SideConfidence = SideConfidenceWebsocket
}

func decodeBestBidAsk(raw []byte, ev *Event) {
	var b wireBestBidAsk
	if err := json.Unmarshal(raw, &b); err != nil {
		return
	}
	if ts := parseUnixMillis(b.Timestamp); !ts.IsZero() {
		ev.ExchangeTimestamp = &ts
	}
	if bid := parseFloatStr(b.BestBid); bid != nil {
		ev.BestBid = bid
	}
	if ask := parseFloatStr(b.BestAsk); ask != nil {
		ev.BestAsk = ask
	}
	if ev.BestBid != nil && ev.BestAsk != nil {
		mid := (*ev.BestBid + *ev.BestAsk) / 2
		ev.Mid = &mid
	}
}

func decodeMarketResolved(raw []byte, ev *Event) {
	var r wireMarketResolved
	if err := json.Unmarshal(raw, &r); err != nil {
		return
	}
	if ts := parseUnixMillis(r.Timestamp); !ts.IsZero() {
		ev.ExchangeTimestamp = &ts
	}
	ev.TradeID = r.ID
	ev.Outcome = r.WinningOutcome
}

// bestLevel returns the best-side price across the supplied levels.
// `descending=true` for bids (largest price wins), false for asks
// (smallest wins). nil on empty / unparseable input.
func bestLevel(levels []wireBookLevel, descending bool) *float64 {
	var best *float64
	for _, lv := range levels {
		px := parseFloatStr(lv.Price)
		if px == nil {
			continue
		}
		if best == nil {
			b := *px
			best = &b
			continue
		}
		if (descending && *px > *best) || (!descending && *px < *best) {
			b := *px
			best = &b
		}
	}
	return best
}

func normaliseType(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "book":
		return EventTypeBook
	case "price_change":
		return EventTypePriceChange
	case "last_trade_price":
		return EventTypeLastTradePrice
	case "best_bid_ask":
		return EventTypeBestBidAsk
	case "tick_size_change":
		return EventTypeTickSizeChange
	case "market_resolved":
		return EventTypeMarketResolved
	case "new_market":
		return EventTypeNewMarket
	}
	return EventTypeUnknown
}

func normaliseSide(s string) string {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "BUY":
		return "BUY"
	case "SELL":
		return "SELL"
	}
	return ""
}

func parseFloatStr(s string) *float64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	if v, err := strconv.ParseFloat(s, 64); err == nil {
		return &v
	}
	return nil
}

// parseUnixMillis accepts Polymarket's millisecond-since-epoch
// string. Empty / unparseable input returns zero time.
func parseUnixMillis(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil || v <= 0 {
		return time.Time{}
	}
	// Some payloads use seconds (10-digit), others ms (13). Detect
	// by magnitude so the heuristic survives both.
	if v < 1_000_000_000_000 {
		return time.Unix(v, 0).UTC()
	}
	return time.UnixMilli(v).UTC()
}

func isPong(raw []byte) bool {
	s := strings.TrimSpace(string(raw))
	return strings.EqualFold(s, "PONG")
}

func hashRaw(raw []byte) string {
	h := sha256.Sum256(raw)
	return hex.EncodeToString(h[:8])
}
