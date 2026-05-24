// Package realtime is the hybrid WebSocket fast-lane worker. It
// owns:
//
//   - selecting the subscription set (active predictions + alerts +
//     catalysts + repricing surface — see selector.go);
//   - starting the infra/polymarket/ws Client;
//   - consuming normalised events (book / price_change /
//     last_trade_price / best_bid_ask / market_resolved);
//   - persisting them to polymarket_ws_events (audit) and
//     polymarket_live_market_state (top-of-book / mid / last_price);
//   - enqueuing realtime work into polymarket_realtime_work_queue;
//   - periodic reconciliation via the existing polling path.
//
// Non-negotiables (verified against the v10.4 spec):
//
//   - WS NEVER triggers Telegram directly.
//   - WS NEVER triggers AI directly.
//   - WS NEVER raises severity.
//   - WS side data is stored with side_source=websocket /
//     side_confidence=0.5 so the strategy layer can downgrade.
//   - Polling + backfill + alertsender remain the canonical
//     pipeline; WS is a trigger accelerator.
//   - WS_ENABLED=false by default.
package realtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/Borislavv/polymarket-watchtower/internal/infra/metrics"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/polymarket/ws"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/repository"
)

// Config tunes the worker. Defaults match the v10.4 spec env knobs.
type Config struct {
	Enabled             bool
	MarketStreamEnabled bool
	// SubscriptionMode is the selector mode. Prediction-driven
	// modes were removed in v11.12-insider-prior.
	//   off    — empty set; worker no-op.
	//   hot    — default; insider-prior bucket scheme.
	//   alerts — recent alerts only (test/CLI convenience).
	SubscriptionMode string
	MaxMarkets       int
	// MaxTokensHardCap is the WS-client circuit-breaker. The
	// realtime worker forwards it to ws.Config.MaxTokensHardCap.
	// NOT a tuning knob — tune by MaxMarkets instead.
	MaxTokensHardCap          int
	ReconnectMin              time.Duration
	ReconnectMax              time.Duration
	PingInterval              time.Duration
	ReadTimeout               time.Duration
	WriteTimeout              time.Duration
	EventBuffer               int
	DropPolicy                string // drop_low_priority | drop_oldest | drop_none
	RawCaptureEnabled         bool
	RawCaptureMaxBytes        int
	ReconcileEnabled          bool
	ReconcileInterval         time.Duration
	GapRecoveryLookback       time.Duration
	HealthStaleAfter          time.Duration
	StartupSubscribeDelay     time.Duration
	PriceMoveTrigger          float64
	RepricingTriggerCooldown  time.Duration
	PredictionRefreshCooldown time.Duration
	SubscriptionRefreshEvery  time.Duration

	Endpoint string
	Clock    func() time.Time
}

func (c *Config) applyDefaults() {
	if c.SubscriptionMode == "" {
		c.SubscriptionMode = "hot"
	}
	if c.MaxMarkets <= 0 {
		c.MaxMarkets = 250
	}
	if c.MaxTokensHardCap <= 0 {
		c.MaxTokensHardCap = 50_000
	}
	if c.ReconnectMin <= 0 {
		c.ReconnectMin = 1 * time.Second
	}
	if c.ReconnectMax <= 0 {
		c.ReconnectMax = 30 * time.Second
	}
	if c.PingInterval <= 0 {
		c.PingInterval = 10 * time.Second
	}
	if c.ReadTimeout <= 0 {
		c.ReadTimeout = 45 * time.Second
	}
	if c.WriteTimeout <= 0 {
		c.WriteTimeout = 10 * time.Second
	}
	if c.EventBuffer <= 0 {
		c.EventBuffer = 10_000
	}
	if c.DropPolicy == "" {
		c.DropPolicy = "drop_low_priority"
	}
	if c.RawCaptureMaxBytes <= 0 {
		c.RawCaptureMaxBytes = 4096
	}
	if c.ReconcileInterval <= 0 {
		c.ReconcileInterval = 2 * time.Minute
	}
	if c.GapRecoveryLookback <= 0 {
		c.GapRecoveryLookback = 10 * time.Minute
	}
	if c.HealthStaleAfter <= 0 {
		c.HealthStaleAfter = 60 * time.Second
	}
	if c.StartupSubscribeDelay <= 0 {
		c.StartupSubscribeDelay = 10 * time.Second
	}
	if c.PriceMoveTrigger <= 0 {
		c.PriceMoveTrigger = 0.03
	}
	if c.RepricingTriggerCooldown <= 0 {
		c.RepricingTriggerCooldown = 60 * time.Second
	}
	if c.PredictionRefreshCooldown <= 0 {
		c.PredictionRefreshCooldown = 5 * time.Minute
	}
	if c.SubscriptionRefreshEvery <= 0 {
		c.SubscriptionRefreshEvery = 5 * time.Minute
	}
	if c.Clock == nil {
		c.Clock = time.Now
	}
}

// Store is the persistence seam — the realtime worker writes to
// polymarket_ws_events / polymarket_live_market_state /
// polymarket_realtime_work_queue / polymarket_ws_gap_recoveries.
type Store interface {
	InsertWSEvent(ctx context.Context, in repository.WSEventRow) error
	UpsertLiveMarketState(ctx context.Context, in repository.LiveMarketStateRow) error
	GetLiveMarketState(ctx context.Context, conditionID string) (repository.LiveMarketStateRow, bool, error)
	SetLiveMarketWSConnected(ctx context.Context, conditionIDs []string, connected bool) error
	EnqueueRealtimeWork(ctx context.Context, in repository.EnqueueRealtimeWorkRow) error
	InsertGapRecovery(ctx context.Context, conditionID string, lookbackStart, lookbackEnd time.Time) (int64, error)
	FinishGapRecovery(ctx context.Context, id int64, status string, recovered int32, lastError string) error
}

// SelectorFunc returns the up-to-date subscription set for the
// worker. Pluggable so tests can inject fixtures and prod can wire
// the SQL-backed candidate selector.
type SelectorFunc func(ctx context.Context, mode string, maxMarkets int) (ws.SubscriptionSet, error)

// WSClient is the small slice of *ws.Client the worker depends on.
// Defined as an interface so tests can stub.
type WSClient interface {
	Subscribe(set ws.SubscriptionSet)
	Run(ctx context.Context, out chan<- ws.Event) error
	Status() string
	LastEventAge(now time.Time) time.Duration
}

// Worker is the per-process WS realtime ingestion loop.
type Worker struct {
	cfg      Config
	store    Store
	client   WSClient
	selector SelectorFunc
	met      *metrics.Metrics
	log      *zerolog.Logger

	// Per-condition cooldown for trigger emission. In-memory;
	// restart resets to "fresh" which is acceptable per the v10.4
	// spec — worst case is one extra refresh per condition_id
	// after a restart.
	cooldownMu      sync.Mutex
	lastRepriceTrig map[string]time.Time
	lastPredictTrig map[string]time.Time
}

// New constructs the Worker. nil metrics + nil logger tolerated.
func New(cfg Config, store Store, client WSClient, selector SelectorFunc, met *metrics.Metrics, log *zerolog.Logger) *Worker {
	cfg.applyDefaults()
	return &Worker{
		cfg:             cfg,
		store:           store,
		client:          client,
		selector:        selector,
		met:             met,
		log:             log,
		lastRepriceTrig: map[string]time.Time{},
		lastPredictTrig: map[string]time.Time{},
	}
}

// Run blocks until ctx cancels. Disabled mode short-circuits without
// goroutines or DB writes.
func (w *Worker) Run(ctx context.Context) {
	if !w.cfg.Enabled || w.cfg.SubscriptionMode == "off" {
		if w.log != nil {
			w.log.Info().Bool("enabled", w.cfg.Enabled).Str("mode", w.cfg.SubscriptionMode).Msg("realtime ws: disabled")
		}
		return
	}
	if w.cfg.StartupSubscribeDelay > 0 {
		select {
		case <-ctx.Done():
			return
		case <-time.After(w.cfg.StartupSubscribeDelay):
		}
	}
	// Initial subscription set.
	set, err := w.selector(ctx, w.cfg.SubscriptionMode, w.cfg.MaxMarkets)
	if err != nil || len(set.Markets) == 0 {
		if w.log != nil {
			w.log.Warn().Err(err).Int("markets", len(set.Markets)).Msg("realtime ws: empty subscription set, sleeping")
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(w.cfg.SubscriptionRefreshEvery):
		}
		w.Run(ctx) // recursion is fine — `Run` is the only entry.
		return
	}
	w.client.Subscribe(set)
	w.markLiveConnected(ctx, conditionsOf(set), true)

	// Drain channel.
	out := make(chan ws.Event, w.cfg.EventBuffer)
	wg := sync.WaitGroup{}
	wg.Add(1)
	go func() {
		defer wg.Done()
		w.consume(ctx, out)
	}()

	// Subscription refresh + reconcile tickers.
	refreshT := time.NewTicker(w.cfg.SubscriptionRefreshEvery)
	defer refreshT.Stop()
	go w.subscriptionRefreshLoop(ctx, refreshT.C)

	// Run the WS client. Errors propagate (with workerguard
	// wrapping at the app level) — context cancellation is the
	// only clean stop.
	_ = w.client.Run(ctx, out)
	close(out)
	wg.Wait()
	w.markLiveConnected(ctx, conditionsOf(set), false)
	if w.log != nil {
		w.log.Info().Msg("realtime ws: stopped")
	}
}

// consume is the per-event hot loop. Every inbound Event goes
// through:
//
//  1. Optional persistence to polymarket_ws_events.
//  2. polymarket_live_market_state upsert when the event carries
//     book / mid / last_price data.
//  3. Trigger evaluation — price-move / trade-seen / market-status
//     → enqueue realtime work with cooldown.
//
// Per-event failures log + continue. The loop NEVER calls AI /
// Telegram / strategy severity directly.
func (w *Worker) consume(ctx context.Context, in <-chan ws.Event) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-in:
			if !ok {
				return
			}
			w.observeBufferDepth(len(in))
			w.handle(ctx, ev)
		}
	}
}

// HandleForSmoke is the test/CLI entry into handle(). Production
// code uses Run() — this is the seam the ws-smoke CLI uses to drive
// one Event through persist → trigger → live-state without standing
// up the full worker loop.
func (w *Worker) HandleForSmoke(ctx context.Context, ev ws.Event) {
	w.handle(ctx, ev)
}

func (w *Worker) handle(ctx context.Context, ev ws.Event) {
	if ev.Type == ws.EventTypeHeartbeat || ev.Type == ws.EventTypeUnknown {
		// Persist for audit only when raw capture is enabled.
		if w.cfg.RawCaptureEnabled {
			w.persistEvent(ctx, ev)
		}
		return
	}
	w.persistEvent(ctx, ev)
	// Trigger evaluation must read the PRIOR live snapshot before
	// the upsert overwrites it — otherwise priceMoveExceedsThreshold
	// always compares the new mid to itself and never fires.
	w.maybeEnqueueTriggers(ctx, ev)
	w.updateLiveState(ctx, ev)
}

func (w *Worker) persistEvent(ctx context.Context, ev ws.Event) {
	raw := ev.Raw
	if !w.cfg.RawCaptureEnabled {
		raw = nil
	} else if w.cfg.RawCaptureMaxBytes > 0 && len(raw) > w.cfg.RawCaptureMaxBytes {
		raw = raw[:w.cfg.RawCaptureMaxBytes]
	}
	// raw_json is a JSONB column — Postgres rejects non-JSON bytes
	// with SQLSTATE 22P02. The WS stream contains heartbeat / pong /
	// "unknown" frames whose Raw payload is plain text. Validate
	// before persisting; on invalid input drop the raw bytes (the
	// other typed columns still land).
	if len(raw) > 0 && !json.Valid(raw) {
		raw = nil
	}
	row := repository.WSEventRow{
		ReceivedAt:        ev.ReceivedAt,
		ExchangeTimestamp: ev.ExchangeTimestamp,
		EventType:         ev.Type,
		EventSlug:         ev.EventSlug,
		ConditionID:       ev.ConditionID,
		MarketSlug:        ev.MarketSlug,
		CLOBTokenID:       ev.CLOBTokenID,
		Outcome:           ev.Outcome,
		Price:             ev.Price,
		Size:              ev.Size,
		Side:              ev.Side,
		SideSource:        emptyIfBlank(ev.SideSource, ws.SideSourceUnknown),
		SideConfidence:    ev.SideConfidence,
		BestBid:           ev.BestBid,
		BestAsk:           ev.BestAsk,
		Mid:               ev.Mid,
		TxHash:            ev.TxHash,
		TradeID:           ev.TradeID,
		Wallet:            ev.Wallet,
		Sequence:          ev.Sequence,
		RawJSON:           raw,
		RawHash:           ev.RawHash,
	}
	if err := w.store.InsertWSEvent(ctx, row); err != nil && w.log != nil {
		w.log.Warn().Err(err).Str("type", ev.Type).Msg("realtime ws: persist event failed")
	}
}

func (w *Worker) updateLiveState(ctx context.Context, ev ws.Event) {
	if ev.ConditionID == "" {
		return
	}
	row := repository.LiveMarketStateRow{
		ConditionID:   ev.ConditionID,
		EventSlug:     ev.EventSlug,
		MarketSlug:    ev.MarketSlug,
		BestBid:       ev.BestBid,
		BestAsk:       ev.BestAsk,
		Mid:           ev.Mid,
		LastWSEventAt: ptrTime(ev.ReceivedAt),
		WSConnected:   true,
	}
	if ev.Type == ws.EventTypeLastTradePrice {
		row.LastPrice = ev.Price
		if ev.ExchangeTimestamp != nil {
			t := *ev.ExchangeTimestamp
			row.LastTradeAt = &t
		} else {
			row.LastTradeAt = ptrTime(ev.ReceivedAt)
		}
	}
	if ev.Type == ws.EventTypePriceChange {
		row.LastPrice = ev.Price
	}
	if err := w.store.UpsertLiveMarketState(ctx, row); err != nil && w.log != nil {
		w.log.Warn().Err(err).Str("condition_id", ev.ConditionID).Msg("realtime ws: upsert live state failed")
	}
}

// maybeEnqueueTriggers turns the inbound event into 0..N realtime
// work-queue rows. Cooldowns prevent the same condition_id from
// firing the same trigger repeatedly.
func (w *Worker) maybeEnqueueTriggers(ctx context.Context, ev ws.Event) {
	now := w.cfg.Clock()
	if ev.ConditionID == "" {
		return
	}
	// Trade-seen: every trade-like event.
	if ev.Type == ws.EventTypeLastTradePrice {
		w.enqueue(ctx, ev, "trade_seen", 3, now)
	}
	// Market status: persistent state change.
	if ev.Type == ws.EventTypeMarketResolved {
		w.enqueue(ctx, ev, "market_status", 1, now)
	}
	// Price-move trigger: book or price_change crossing the
	// configured threshold relative to the prior live snapshot.
	if ev.Type == ws.EventTypeBook || ev.Type == ws.EventTypePriceChange || ev.Type == ws.EventTypeBestBidAsk {
		if w.priceMoveExceedsThreshold(ctx, ev) {
			if !w.cooldownActive(w.lastRepriceTrig, ev.ConditionID, w.cfg.RepricingTriggerCooldown, now) {
				w.enqueue(ctx, ev, "price_move", 2, now)
				w.stampCooldown(w.lastRepriceTrig, ev.ConditionID, now)
				// Also enqueue a downstream prediction refresh
				// trigger, gated by its own (longer) cooldown.
				if !w.cooldownActive(w.lastPredictTrig, ev.ConditionID, w.cfg.PredictionRefreshCooldown, now) {
					w.enqueue(ctx, ev, "book_change", 4, now)
					w.stampCooldown(w.lastPredictTrig, ev.ConditionID, now)
				}
			}
		}
	}
}

// priceMoveExceedsThreshold computes |newMid - lastKnownMid| /
// lastKnownMid and compares to PriceMoveTrigger. nil/zero
// references treat the event as "first observation" and DO NOT
// trigger.
func (w *Worker) priceMoveExceedsThreshold(ctx context.Context, ev ws.Event) bool {
	if ev.Mid == nil && ev.Price == nil {
		return false
	}
	curr := 0.0
	if ev.Mid != nil {
		curr = *ev.Mid
	} else if ev.Price != nil {
		curr = *ev.Price
	}
	if curr <= 0 {
		return false
	}
	prev, ok, _ := w.store.GetLiveMarketState(ctx, ev.ConditionID)
	if !ok || prev.Mid == nil || *prev.Mid <= 0 {
		return false
	}
	delta := curr - *prev.Mid
	if delta < 0 {
		delta = -delta
	}
	rel := delta / *prev.Mid
	return rel >= w.cfg.PriceMoveTrigger
}

func (w *Worker) enqueue(ctx context.Context, ev ws.Event, reason string, priority int, now time.Time) {
	dedupe := buildDedupeKey(ev.ConditionID, reason, now)
	row := repository.EnqueueRealtimeWorkRow{
		ConditionID: ev.ConditionID,
		EventSlug:   ev.EventSlug,
		Reason:      reason,
		Priority:    int16(priority),
		DedupeKey:   dedupe,
		AvailableAt: now,
	}
	if err := w.store.EnqueueRealtimeWork(ctx, row); err != nil {
		if w.log != nil {
			w.log.Warn().Err(err).Str("reason", reason).Str("condition_id", ev.ConditionID).Msg("realtime ws: enqueue failed")
		}
		return
	}
	if w.met != nil && w.met.RealtimeWorkEnqueued != nil {
		w.met.RealtimeWorkEnqueued.WithLabelValues(reason).Inc()
	}
}

func (w *Worker) cooldownActive(m map[string]time.Time, conditionID string, cooldown time.Duration, now time.Time) bool {
	w.cooldownMu.Lock()
	defer w.cooldownMu.Unlock()
	last, ok := m[conditionID]
	return ok && now.Sub(last) < cooldown
}

func (w *Worker) stampCooldown(m map[string]time.Time, conditionID string, now time.Time) {
	w.cooldownMu.Lock()
	defer w.cooldownMu.Unlock()
	m[conditionID] = now
}

func (w *Worker) markLiveConnected(ctx context.Context, conditions []string, connected bool) {
	if err := w.store.SetLiveMarketWSConnected(ctx, conditions, connected); err != nil && w.log != nil {
		w.log.Warn().Err(err).Bool("connected", connected).Msg("realtime ws: set ws_connected failed")
	}
}

// subscriptionRefreshLoop periodically re-runs the selector +
// diffs against the current set. Today we Subscribe() the new set
// (which the client uses on the next reconnect); a future
// enhancement would dynamic-subscribe without reconnecting once
// the upstream supports it.
func (w *Worker) subscriptionRefreshLoop(ctx context.Context, tick <-chan time.Time) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick:
			set, err := w.selector(ctx, w.cfg.SubscriptionMode, w.cfg.MaxMarkets)
			if err != nil {
				w.observeSubRefresh("failed")
				continue
			}
			if len(set.Markets) == 0 {
				w.observeSubRefresh("unchanged")
				continue
			}
			w.client.Subscribe(set)
			w.observeSubRefresh("ok")
		}
	}
}

func (w *Worker) observeBufferDepth(n int) {
	if w.met == nil || w.met.WSBufferDepth == nil {
		return
	}
	w.met.WSBufferDepth.Set(float64(n))
}

func (w *Worker) observeSubRefresh(status string) {
	if w.met == nil || w.met.WSSubscriptionRefresh == nil {
		return
	}
	w.met.WSSubscriptionRefresh.WithLabelValues(status).Inc()
}

// buildDedupeKey collapses bursts of the same reason for the same
// condition into one queue row per minute bucket.
func buildDedupeKey(conditionID, reason string, now time.Time) string {
	bucket := now.UTC().Truncate(time.Minute).Format("2006010215040")
	h := sha256.Sum256([]byte(conditionID + "|" + reason + "|" + bucket))
	return reason + ":" + hex.EncodeToString(h[:8])
}

func conditionsOf(set ws.SubscriptionSet) []string {
	out := make([]string, 0, len(set.Markets))
	for _, m := range set.Markets {
		if m.ConditionID != "" {
			out = append(out, m.ConditionID)
		}
	}
	return out
}

func emptyIfBlank(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func ptrTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	v := t
	return &v
}

// ErrSelectorEmpty is the canonical sentinel for "no markets to
// subscribe to". The worker treats it as "back off + retry".
var ErrSelectorEmpty = errors.New("realtime: selector returned empty set")

// ErrUnusedForLinter is a placeholder so the package always has at
// least one fmt usage when other helpers are pruned. Removed by
// the linter when unnecessary.
var _ = fmt.Sprintf
