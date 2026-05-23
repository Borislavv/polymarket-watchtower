package ws

import (
	"testing"
	"time"
)

// Real payloads from the Polymarket CLOB WS docs, verified
// 2026-05-21. Pin every load-bearing field the worker reads.

const bookSample = `{
  "event_type":"book",
  "asset_id":"65818619657568813474341868652308942079804919287380422192892211131408793125422",
  "market":"0xbd31dc8a20211944f6b70f31557f1001557b59905b7738480ca09bd4532f84af",
  "bids":[{"price":".48","size":"30"},{"price":".49","size":"20"},{"price":".50","size":"15"}],
  "asks":[{"price":".52","size":"25"},{"price":".53","size":"60"},{"price":".54","size":"10"}],
  "timestamp":"1750428146322","hash":"0x0abc"
}`

const priceChangeSample = `{
  "market":"0x5f65177b394277fd294cd75650044e32ba009a95022d88a0c1d565897d72f8f1",
  "price_changes":[{
    "asset_id":"71321045679252212594626385532706912750332728571942532289631379312455583992563",
    "price":"0.5","size":"200","side":"BUY","hash":"56621a121a47","best_bid":"0.5","best_ask":"1"
  }],
  "timestamp":"1757908892351","event_type":"price_change"
}`

const lastTradeSample = `{
  "asset_id":"114122071509644379678018727908709560226618148003371446110114509806601493071694",
  "event_type":"last_trade_price","fee_rate_bps":"0",
  "market":"0x6a67b9d828d53862160e470329ffea5246f338ecfffdf2cab45211ec578b0347",
  "price":"0.456","side":"BUY","size":"219.217767","timestamp":"1750428146322"
}`

const bestBidAskSample = `{
  "event_type":"best_bid_ask",
  "market":"0x0005c0d312de0be897668695bae9f32b624b4a1ae8b140c49f08447fcc74f442",
  "asset_id":"85354956062430465315924116860125388538595433819574542752031640332592237464430",
  "best_bid":"0.73","best_ask":"0.77","spread":"0.04","timestamp":"1766789469958"
}`

func TestDecodeBook_FillsBestBidAskMid(t *testing.T) {
	ev := decode([]byte(bookSample), nil, time.Now())
	if ev.Type != EventTypeBook {
		t.Fatalf("type: got %q want book", ev.Type)
	}
	if ev.BestBid == nil || *ev.BestBid != 0.50 {
		t.Errorf("best_bid: got %v want 0.50", ev.BestBid)
	}
	if ev.BestAsk == nil || *ev.BestAsk != 0.52 {
		t.Errorf("best_ask: got %v want 0.52", ev.BestAsk)
	}
	if ev.Mid == nil {
		t.Fatal("mid should be set from book")
	}
	if !approxEq(*ev.Mid, 0.51, 0.001) {
		t.Errorf("mid: got %v want ~0.51", *ev.Mid)
	}
}

// v11.10: depth arrays must be preserved on the decoded Event so
// bookvacuum / book_feature_bars can read them without re-parsing
// the raw payload.
func TestDecodeBook_PreservesFullDepth(t *testing.T) {
	ev := decode([]byte(bookSample), nil, time.Now())
	if len(ev.BidLevels) != 3 || len(ev.AskLevels) != 3 {
		t.Fatalf("depth counts: bids=%d asks=%d (expected 3+3)", len(ev.BidLevels), len(ev.AskLevels))
	}
	// Bids DESC: 0.50 > 0.49 > 0.48
	if ev.BidLevels[0].Price != 0.50 || ev.BidLevels[2].Price != 0.48 {
		t.Fatalf("bid sort: %+v", ev.BidLevels)
	}
	// Asks ASC: 0.52 < 0.53 < 0.54
	if ev.AskLevels[0].Price != 0.52 || ev.AskLevels[2].Price != 0.54 {
		t.Fatalf("ask sort: %+v", ev.AskLevels)
	}
	if ev.BidLevels[0].Size != 15 || ev.AskLevels[0].Size != 25 {
		t.Fatalf("best-of-book sizes: bid=%v ask=%v", ev.BidLevels[0].Size, ev.AskLevels[0].Size)
	}
}

func TestDecodePriceChange_FillsSideSourceWebsocket(t *testing.T) {
	ev := decode([]byte(priceChangeSample), nil, time.Now())
	if ev.Type != EventTypePriceChange {
		t.Fatalf("type: got %q want price_change", ev.Type)
	}
	if ev.Side != "BUY" {
		t.Errorf("side: got %q want BUY", ev.Side)
	}
	if ev.SideSource != SideSourceWebsocket {
		t.Errorf("side_source must be websocket; got %q", ev.SideSource)
	}
	if ev.SideConfidence > SideConfidenceDataAPI {
		t.Errorf("ws side confidence must be < data_api; got %v", ev.SideConfidence)
	}
	if ev.Price == nil || *ev.Price != 0.5 {
		t.Errorf("price: got %v", ev.Price)
	}
}

func TestDecodeLastTradePrice_HasSideButLowConfidence(t *testing.T) {
	ev := decode([]byte(lastTradeSample), nil, time.Now())
	if ev.Type != EventTypeLastTradePrice {
		t.Fatalf("type: got %q want last_trade_price", ev.Type)
	}
	if ev.Side != "BUY" {
		t.Errorf("side: got %q", ev.Side)
	}
	if ev.SideSource != SideSourceWebsocket {
		t.Errorf("side_source: got %q", ev.SideSource)
	}
	if ev.SideConfidence >= SideConfidenceOnchain {
		t.Errorf("WS-only trade must NOT have on-chain confidence; got %v", ev.SideConfidence)
	}
	// CLOB WS does NOT carry wallet/tx_hash.
	if ev.Wallet != "" || ev.TxHash != "" {
		t.Errorf("WS payload must not invent wallet/tx_hash: wallet=%q tx=%q", ev.Wallet, ev.TxHash)
	}
}

func TestDecodeBestBidAsk_FillsBookView(t *testing.T) {
	ev := decode([]byte(bestBidAskSample), nil, time.Now())
	if ev.Type != EventTypeBestBidAsk {
		t.Fatalf("type: got %q want best_bid_ask", ev.Type)
	}
	if ev.BestBid == nil || *ev.BestBid != 0.73 {
		t.Errorf("best_bid: %v", ev.BestBid)
	}
	if ev.Mid == nil || !approxEq(*ev.Mid, 0.75, 0.001) {
		t.Errorf("mid: %v", ev.Mid)
	}
}

func TestDecodeUnknownTypeKeptForAudit(t *testing.T) {
	ev := decode([]byte(`{"event_type":"some_future_event","market":"0xab","x":1}`), nil, time.Now())
	if ev.Type != EventTypeUnknown {
		t.Errorf("unknown type expected; got %q", ev.Type)
	}
	if len(ev.Raw) == 0 {
		t.Error("raw payload must be preserved for audit")
	}
}

func TestDecodeMalformedReturnsZero(t *testing.T) {
	ev := decode([]byte("{not json"), nil, time.Now())
	if ev.Type != EventTypeUnknown {
		t.Errorf("malformed must collapse to unknown; got %q", ev.Type)
	}
}

func TestDecodePongHeartbeat(t *testing.T) {
	ev := decode([]byte("PONG"), nil, time.Now())
	if ev.Type != EventTypeHeartbeat {
		t.Errorf("PONG text frame must map to heartbeat; got %q", ev.Type)
	}
}

func TestSubscriptionResolverFillsEventSlugAndOutcome(t *testing.T) {
	resolver := func(assetID, conditionID string) (MarketSubscription, bool) {
		return MarketSubscription{
			EventSlug:      "ev-x",
			MarketSlug:     "m-x",
			OutcomeByToken: map[string]string{assetID: "Yes"},
		}, true
	}
	ev := decode([]byte(bookSample), resolver, time.Now())
	if ev.EventSlug != "ev-x" {
		t.Errorf("event_slug: got %q", ev.EventSlug)
	}
	if ev.Outcome != "Yes" {
		t.Errorf("outcome: got %q", ev.Outcome)
	}
}

func approxEq(a, b, eps float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d <= eps
}
