// Package collect pulls public trades from the Data API for every market in
// the registry on a fixed cadence, folds them into the supporting aggregate
// engine, and hands each trade to the per-trade detector for scoring.
package collect

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/aggregate"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/market"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/trade"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/vo"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/metrics"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/polymarket/dataapi"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/polymarket/httpx"
)

// TradeObserver is the per-trade hook into the detection pipeline. The detect
// package implements it; we depend on the interface so collect tests can fake
// it without dragging the whole analytics graph in.
type TradeObserver interface {
	Observe(ctx context.Context, m market.Market, t trade.Trade)
}

// Persist is the optional Postgres write-through. Called per pull with the
// freshly-fetched batch and its market. In production wiring the loop calls
// Persist BEFORE Observer.Observe so the DB-backed detector sees the trade
// in its baseline reservoir. Errors are logged; observation still runs so
// the in-memory aggregate engine stays warm.
type Persist func(ctx context.Context, m market.Market, trades []trade.Trade) error

// CursorReader returns the newest traded_at already stored for the supplied
// market, or the zero time when nothing is persisted yet. Optional: when
// nil, the loop uses an in-process map as the cursor (legacy/memory mode).
type CursorReader func(ctx context.Context, conditionID string) (time.Time, error)

type Config struct {
	Interval     time.Duration
	Concurrency  int
	LookbackBoot time.Duration    // initial pull window per market on first sight
	Clock        func() time.Time // optional; defaults to time.Now
	// Persist optionally receives each fetched batch for write-through
	// to PostgreSQL. Nil = no DB writes.
	Persist Persist
	// Cursor optionally sources the per-market "since" cutoff from the DB.
	// Nil = use the in-process lastTs map (memory mode).
	Cursor CursorReader
}

type Loop struct {
	cfg      Config
	client   *dataapi.Client
	engine   *aggregate.Engine
	registry *aggregate.MarketRegistry
	observer TradeObserver
	metrics  *metrics.Metrics
	log      *zerolog.Logger
	now      func() time.Time

	lastTsMu sync.Mutex
	lastTs   map[vo.MarketID]time.Time
}

func New(
	cfg Config,
	c *dataapi.Client,
	eng *aggregate.Engine,
	reg *aggregate.MarketRegistry,
	obs TradeObserver,
	m *metrics.Metrics,
	log *zerolog.Logger,
) *Loop {
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 8
	}
	if cfg.LookbackBoot <= 0 {
		cfg.LookbackBoot = time.Hour
	}
	now := cfg.Clock
	if now == nil {
		now = time.Now
	}
	return &Loop{
		cfg:      cfg,
		client:   c,
		engine:   eng,
		registry: reg,
		observer: obs,
		metrics:  m,
		log:      log,
		now:      now,
		lastTs:   make(map[vo.MarketID]time.Time),
	}
}

// Tick runs one collection pass; exposed for tests.
func (l *Loop) Tick(ctx context.Context) { l.tick(ctx) }

func (l *Loop) Run(ctx context.Context) error {
	t := time.NewTicker(l.cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			l.tick(ctx)
		}
	}
}

func (l *Loop) tick(ctx context.Context) {
	markets := l.registry.Snapshot()
	if len(markets) == 0 {
		return
	}
	sem := make(chan struct{}, l.cfg.Concurrency)
	var wg sync.WaitGroup
	for _, m := range markets {
		if !m.IsTradable(l.now()) {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(m market.Market) {
			defer wg.Done()
			defer func() { <-sem }()
			l.pull(ctx, m)
		}(m)
	}
	wg.Wait()
}

func (l *Loop) pull(ctx context.Context, m market.Market) {
	since := l.lookback(ctx, m)
	trades, err := l.client.ListTradesSince(ctx, dataapi.ListTradesOpts{Market: m.ID, Since: since})
	if err != nil {
		var apiErr *httpx.APIError
		ev := l.log.Err(err).Str("market", string(m.ID)).Time("since", since)
		if errors.As(err, &apiErr) {
			ev.Int("status", apiErr.Status).Bool("retryable", apiErr.Retryable()).Str("body", apiErr.Body)
		}
		ev.Msg("collect: pull failed")
		return
	}
	if len(trades) == 0 {
		return
	}
	// Process trades oldest → newest so the baseline is seeded with historical
	// context before any "recent" trade is scored against it. The Data API
	// returns DESC within a market; reverse client-side.
	sort.SliceStable(trades, func(i, j int) bool { return trades[i].Timestamp.Before(trades[j].Timestamp) })
	l.engine.IngestBatch(trades)

	// Persist BEFORE Observe: in DB-baseline mode the detector queries
	// polymarket_trades on every Observe and must see this batch already
	// written. A persist failure does not block observation — it is logged
	// and the in-memory aggregate engine still gets the data.
	if l.cfg.Persist != nil {
		if err := l.cfg.Persist(ctx, m, trades); err != nil {
			l.log.Err(err).Str("market", string(m.ID)).Msg("collect: persist failed")
		}
	}

	var notional float64
	var newest time.Time
	for _, t := range trades {
		notional += t.NotionalUSD()
		if t.Timestamp.After(newest) {
			newest = t.Timestamp
		}
		if l.observer != nil {
			l.observer.Observe(ctx, m, t)
		}
	}
	l.metrics.TradesIngested.WithLabelValues(string(m.ID)).Add(float64(len(trades)))
	l.metrics.NotionalIngested.WithLabelValues(string(m.ID)).Add(notional)
	l.setLastTs(m.ID, newest)
}

// lookback resolves the per-market "since" cutoff. Order of precedence:
//  1. cfg.Cursor (DB-backed) when wired — survives restarts.
//  2. in-process lastTs map — last seen timestamp this run.
//  3. now − LookbackBoot for a first-sight market.
//
// Cursor read errors fall through to the in-process map, so a transient DB
// hiccup doesn't stall collection.
func (l *Loop) lookback(ctx context.Context, m market.Market) time.Time {
	if l.cfg.Cursor != nil {
		if ts, err := l.cfg.Cursor(ctx, string(m.ID)); err == nil && !ts.IsZero() {
			return ts.Add(time.Second)
		}
	}
	l.lastTsMu.Lock()
	defer l.lastTsMu.Unlock()
	if ts, ok := l.lastTs[m.ID]; ok {
		return ts
	}
	return l.now().Add(-l.cfg.LookbackBoot)
}

func (l *Loop) setLastTs(id vo.MarketID, t time.Time) {
	if t.IsZero() {
		return
	}
	l.lastTsMu.Lock()
	defer l.lastTsMu.Unlock()
	if prev, ok := l.lastTs[id]; !ok || t.After(prev) {
		// move slightly forward to avoid re-fetching the boundary trade
		l.lastTs[id] = t.Add(time.Second)
	}
}
