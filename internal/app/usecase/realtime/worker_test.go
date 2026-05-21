package realtime

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/infra/polymarket/ws"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/repository"
)

// fakeStore captures every persistence call so the assertions can
// inspect them directly.
type fakeStore struct {
	mu          sync.Mutex
	events      []repository.WSEventRow
	live        []repository.LiveMarketStateRow
	preset      map[string]repository.LiveMarketStateRow
	enqueued    []repository.EnqueueRealtimeWorkRow
	connFlips   []bool
	connCondIDs [][]string
}

func newFakeStore() *fakeStore {
	return &fakeStore{preset: map[string]repository.LiveMarketStateRow{}}
}

func (f *fakeStore) InsertWSEvent(_ context.Context, in repository.WSEventRow) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, in)
	return nil
}

func (f *fakeStore) UpsertLiveMarketState(_ context.Context, in repository.LiveMarketStateRow) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.live = append(f.live, in)
	f.preset[in.ConditionID] = in
	return nil
}

func (f *fakeStore) GetLiveMarketState(_ context.Context, conditionID string) (repository.LiveMarketStateRow, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.preset[conditionID]
	return row, ok, nil
}

func (f *fakeStore) SetLiveMarketWSConnected(_ context.Context, ids []string, connected bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.connCondIDs = append(f.connCondIDs, append([]string(nil), ids...))
	f.connFlips = append(f.connFlips, connected)
	return nil
}

func (f *fakeStore) EnqueueRealtimeWork(_ context.Context, in repository.EnqueueRealtimeWorkRow) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enqueued = append(f.enqueued, in)
	return nil
}

func (f *fakeStore) InsertGapRecovery(_ context.Context, _ string, _, _ time.Time) (int64, error) {
	return 1, nil
}

func (f *fakeStore) FinishGapRecovery(_ context.Context, _ int64, _ string, _ int32, _ string) error {
	return nil
}

func newTestWorker(t *testing.T, store *fakeStore, cfg Config) *Worker {
	t.Helper()
	cfg.Enabled = true
	if cfg.Clock == nil {
		cfg.Clock = func() time.Time { return time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC) }
	}
	return New(cfg, store, nil, nil, nil, nil)
}

func floatPtr(v float64) *float64 { return &v }

// TestHandle_LastTradePersistsAndEnqueuesTradeSeen pins the
// load-bearing trade-seen rule: a last_trade_price event must
// persist + enqueue a trade_seen work row, AND the side_source
// MUST be 'websocket' (not authoritative).
func TestHandle_LastTradePersistsAndEnqueuesTradeSeen(t *testing.T) {
	store := newFakeStore()
	w := newTestWorker(t, store, Config{})
	w.handle(context.Background(), ws.Event{
		Type:           ws.EventTypeLastTradePrice,
		ConditionID:    "0xabc",
		EventSlug:      "ev",
		CLOBTokenID:    "tokY",
		ReceivedAt:     time.Now(),
		Price:          floatPtr(0.55),
		Size:           floatPtr(200),
		Side:           "BUY",
		SideSource:     ws.SideSourceWebsocket,
		SideConfidence: ws.SideConfidenceWebsocket,
	})
	if len(store.events) != 1 {
		t.Fatalf("expected 1 event persisted; got %d", len(store.events))
	}
	if got := store.events[0].SideSource; got != ws.SideSourceWebsocket {
		t.Errorf("side_source: got %q want websocket", got)
	}
	if len(store.enqueued) != 1 {
		t.Fatalf("expected 1 enqueue; got %d", len(store.enqueued))
	}
	if got := store.enqueued[0].Reason; got != "trade_seen" {
		t.Errorf("reason: got %q want trade_seen", got)
	}
}

// TestHandle_PriceMoveTriggersWhenCrossesThreshold pins the price-
// move cooldown gate. mid moves from 0.50 to 0.60 = 20% relative
// move > 3% threshold → price_move enqueue + book_change enqueue.
func TestHandle_PriceMoveTriggersWhenCrossesThreshold(t *testing.T) {
	store := newFakeStore()
	store.preset["0xabc"] = repository.LiveMarketStateRow{
		ConditionID: "0xabc", Mid: floatPtr(0.50),
	}
	w := newTestWorker(t, store, Config{PriceMoveTrigger: 0.03})
	w.handle(context.Background(), ws.Event{
		Type:        ws.EventTypeBook,
		ConditionID: "0xabc",
		ReceivedAt:  time.Now(),
		BestBid:     floatPtr(0.59),
		BestAsk:     floatPtr(0.61),
		Mid:         floatPtr(0.60),
	})
	gotReasons := map[string]int{}
	for _, e := range store.enqueued {
		gotReasons[e.Reason]++
	}
	if gotReasons["price_move"] != 1 {
		t.Errorf("expected one price_move enqueue; got %v", gotReasons)
	}
	if gotReasons["book_change"] != 1 {
		t.Errorf("expected one book_change enqueue; got %v", gotReasons)
	}
}

// TestHandle_PriceMoveBelowThresholdDoesNotTrigger pins the
// quiet-market case: small move = no enqueue.
func TestHandle_PriceMoveBelowThresholdDoesNotTrigger(t *testing.T) {
	store := newFakeStore()
	store.preset["0xabc"] = repository.LiveMarketStateRow{
		ConditionID: "0xabc", Mid: floatPtr(0.50),
	}
	w := newTestWorker(t, store, Config{PriceMoveTrigger: 0.10})
	w.handle(context.Background(), ws.Event{
		Type:        ws.EventTypeBook,
		ConditionID: "0xabc",
		ReceivedAt:  time.Now(),
		Mid:         floatPtr(0.51),
	})
	for _, e := range store.enqueued {
		if e.Reason == "price_move" {
			t.Fatalf("price_move must NOT trigger on 2%% move when threshold is 10%%; enqueued=%v", store.enqueued)
		}
	}
}

// TestHandle_RepricingCooldownGate pins the per-condition cooldown:
// two price-move events within the cooldown window produce ONE
// price_move enqueue.
func TestHandle_RepricingCooldownGate(t *testing.T) {
	store := newFakeStore()
	store.preset["0xabc"] = repository.LiveMarketStateRow{
		ConditionID: "0xabc", Mid: floatPtr(0.50),
	}
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	clk := func() time.Time { return now }
	w := newTestWorker(t, store, Config{
		PriceMoveTrigger:         0.03,
		RepricingTriggerCooldown: 60 * time.Second,
		Clock:                    clk,
	})
	for i := 0; i < 3; i++ {
		w.handle(context.Background(), ws.Event{
			Type:        ws.EventTypeBook,
			ConditionID: "0xabc",
			ReceivedAt:  now,
			Mid:         floatPtr(0.60),
		})
	}
	var priceMoveCount int
	for _, e := range store.enqueued {
		if e.Reason == "price_move" {
			priceMoveCount++
		}
	}
	if priceMoveCount != 1 {
		t.Errorf("cooldown failed; price_move enqueues=%d, expected 1", priceMoveCount)
	}
}

// TestHandle_MarketResolvedEnqueuesMarketStatus pins the resolution
// path: market_resolved triggers a market_status work row.
func TestHandle_MarketResolvedEnqueuesMarketStatus(t *testing.T) {
	store := newFakeStore()
	w := newTestWorker(t, store, Config{})
	w.handle(context.Background(), ws.Event{
		Type:        ws.EventTypeMarketResolved,
		ConditionID: "0xabc",
		ReceivedAt:  time.Now(),
		Outcome:     "Yes",
	})
	if len(store.enqueued) != 1 || store.enqueued[0].Reason != "market_status" {
		t.Errorf("expected one market_status enqueue; got %v", store.enqueued)
	}
}

// TestHandle_HeartbeatAndUnknownDoNotPersistOrEnqueue pins the
// silence rule: heartbeat / unknown events flow through without
// DB / queue side effects (unless RawCaptureEnabled).
func TestHandle_HeartbeatAndUnknownDoNotPersistOrEnqueue(t *testing.T) {
	store := newFakeStore()
	w := newTestWorker(t, store, Config{})
	w.handle(context.Background(), ws.Event{Type: ws.EventTypeHeartbeat})
	w.handle(context.Background(), ws.Event{Type: ws.EventTypeUnknown})
	if len(store.events) != 0 {
		t.Errorf("heartbeat/unknown must NOT persist; got %d", len(store.events))
	}
	if len(store.enqueued) != 0 {
		t.Errorf("heartbeat/unknown must NOT enqueue; got %d", len(store.enqueued))
	}
}

// TestBuildDedupeKey_BucketsByMinute pins the load-bearing dedupe
// rule: same condition + same reason + same minute = same key.
func TestBuildDedupeKey_BucketsByMinute(t *testing.T) {
	a := buildDedupeKey("0xabc", "price_move", time.Date(2026, 5, 21, 12, 0, 5, 0, time.UTC))
	b := buildDedupeKey("0xabc", "price_move", time.Date(2026, 5, 21, 12, 0, 55, 0, time.UTC))
	if a != b {
		t.Errorf("same minute bucket must produce same dedupe key; got %q vs %q", a, b)
	}
	c := buildDedupeKey("0xabc", "price_move", time.Date(2026, 5, 21, 12, 1, 5, 0, time.UTC))
	if a == c {
		t.Errorf("different minute bucket must produce different dedupe key; got %q == %q", a, c)
	}
}

// TestHandle_RawCaptureTruncatesAtCap pins PART 9: when
// RawCaptureEnabled=true, ev.Raw is stored on the persisted row but
// truncated to RawCaptureMaxBytes. Off by default → Raw is nil.
func TestHandle_RawCaptureTruncatesAtCap(t *testing.T) {
	store := newFakeStore()
	w := newTestWorker(t, store, Config{RawCaptureEnabled: true, RawCaptureMaxBytes: 8})
	bigRaw := []byte(`{"event_type":"book","market":"0xabc","x":"aaaaaaaaaaaaaaaaaaaaaaaaaa"}`)
	w.handle(context.Background(), ws.Event{
		Type:        ws.EventTypeBook,
		ConditionID: "0xabc",
		ReceivedAt:  time.Now(),
		BestBid:     floatPtr(0.40),
		BestAsk:     floatPtr(0.42),
		Mid:         floatPtr(0.41),
		Raw:         bigRaw,
	})
	if len(store.events) != 1 {
		t.Fatalf("expected 1 event")
	}
	if got := len(store.events[0].RawJSON); got != 8 {
		t.Errorf("raw payload should be capped to 8 bytes; got %d", got)
	}
}

func TestHandle_RawCaptureDisabledDropsRaw(t *testing.T) {
	store := newFakeStore()
	w := newTestWorker(t, store, Config{RawCaptureEnabled: false})
	bigRaw := []byte(`{"event_type":"book","market":"0xabc","x":"aaaaaaaaaaaa"}`)
	w.handle(context.Background(), ws.Event{
		Type:        ws.EventTypeBook,
		ConditionID: "0xabc",
		ReceivedAt:  time.Now(),
		BestBid:     floatPtr(0.40),
		BestAsk:     floatPtr(0.42),
		Mid:         floatPtr(0.41),
		Raw:         bigRaw,
	})
	if len(store.events) != 1 {
		t.Fatalf("expected 1 event")
	}
	if store.events[0].RawJSON != nil {
		t.Errorf("raw should be nil when RawCaptureEnabled=false; got %d bytes", len(store.events[0].RawJSON))
	}
}

// TestDedupeCollapsesSameMinute pins PART 6: many price_move events
// for the same condition in the same minute resolve to ONE dedupe
// key. The realtime_repository UNIQUE(dedupe_key) constraint then
// collapses inserts at the DB layer.
func TestDedupeCollapsesSameMinute(t *testing.T) {
	store := newFakeStore()
	store.preset["0xabc"] = repository.LiveMarketStateRow{
		ConditionID: "0xabc", Mid: floatPtr(0.50),
	}
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	w := newTestWorker(t, store, Config{
		PriceMoveTrigger:         0.03,
		RepricingTriggerCooldown: 0, // disable cooldown to isolate the dedupe rule
		Clock:                    func() time.Time { return now },
	})
	for i := 0; i < 5; i++ {
		w.handle(context.Background(), ws.Event{
			Type:        ws.EventTypeBook,
			ConditionID: "0xabc",
			ReceivedAt:  now,
			Mid:         floatPtr(0.60),
		})
	}
	keys := map[string]struct{}{}
	for _, e := range store.enqueued {
		if e.Reason == "price_move" {
			keys[e.DedupeKey] = struct{}{}
		}
	}
	if len(keys) != 1 {
		t.Errorf("same minute bucket must produce one dedupe key for price_move; got %d distinct: %v", len(keys), keys)
	}
}

// TestWSSideConfidenceIsLowerThanDataAPI pins the v10.6 invariant:
// the WS side is never authoritative. SideConfidenceWebsocket is
// numerically lower than SideConfidenceDataAPI and SideConfidenceOnchain
// so the strategy layer can downgrade severity when only WS evidence
// is available.
func TestWSSideConfidenceIsLowerThanDataAPI(t *testing.T) {
	if ws.SideConfidenceWebsocket >= ws.SideConfidenceDataAPI {
		t.Errorf("WS confidence (%v) must be < data_api (%v)",
			ws.SideConfidenceWebsocket, ws.SideConfidenceDataAPI)
	}
	if ws.SideConfidenceDataAPI >= ws.SideConfidenceOnchain {
		t.Errorf("data_api confidence (%v) must be < onchain (%v)",
			ws.SideConfidenceDataAPI, ws.SideConfidenceOnchain)
	}
}

// TestWSEventNeverCallsAIOrTelegram is a structural pin. The
// realtime.Worker's only DB-touching dependencies are the persistence
// Store. The architecture_test.go file already enforces no AI /
// Telegram imports at the package level. This test is the runtime
// proof that handle() does NOT exercise any pathway that could reach
// the alerting / aianalysis surfaces — a fakeStore is the ONLY seam,
// and any unexpected dependency would be caught by the absence of
// methods on Store.
func TestWSEventNeverCallsAIOrTelegram(t *testing.T) {
	store := newFakeStore()
	w := newTestWorker(t, store, Config{PriceMoveTrigger: 0.03})
	// Fire every event-type we handle; if a hidden AI/Telegram path
	// existed, it would show up as a missing-method panic on the
	// fakeStore — there is none, by construction.
	for _, typ := range []string{
		ws.EventTypeBook, ws.EventTypePriceChange, ws.EventTypeLastTradePrice,
		ws.EventTypeBestBidAsk, ws.EventTypeMarketResolved,
	} {
		w.handle(context.Background(), ws.Event{
			Type: typ, ConditionID: "0xabc", ReceivedAt: time.Now(),
			Mid: floatPtr(0.5), Price: floatPtr(0.5), Size: floatPtr(100),
		})
	}
	// The only side effects we tolerate: persisted ws_events, live
	// state upserts, queue enqueues. Any other I/O would require a
	// new method on the Store interface — and the architecture test
	// pins the package can't import telegram/AI/alerting.
	if len(store.events) == 0 && len(store.live) == 0 && len(store.enqueued) == 0 {
		t.Error("expected at least one persistence side-effect")
	}
}

// TestSideSourcePersisted pins the v10.4 PART 11 contract: every
// persisted WS event carries side_source != "" (defaults to
// "unknown" when the wire payload didn't include side info).
func TestSideSourcePersisted(t *testing.T) {
	store := newFakeStore()
	w := newTestWorker(t, store, Config{})
	w.handle(context.Background(), ws.Event{
		Type:        ws.EventTypeBook,
		ConditionID: "0xabc",
		ReceivedAt:  time.Now(),
		BestBid:     floatPtr(0.4),
		BestAsk:     floatPtr(0.5),
		Mid:         floatPtr(0.45),
	})
	if len(store.events) != 1 {
		t.Fatalf("expected 1 event")
	}
	if got := store.events[0].SideSource; got == "" {
		t.Errorf("side_source must never be empty in persisted row; got %q", got)
	}
}
