// Package collect pulls public trades from the Data API for every market
// in the in-memory marketcache on a fixed cadence and hands each trade to
// the per-trade detector for scoring. Trades are also persisted before
// the detector observes them so the DB-backed baseline (the production
// decision source) sees the trade in its statistics on the very next
// query.
package collect

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/marketcache"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/market"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/trade"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/metrics"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/polymarket/dataapi"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/polymarket/httpx"
)

// TradeObserver is the per-trade hook into the detection pipeline.
type TradeObserver interface {
	Observe(ctx context.Context, m market.Market, t trade.Trade)
}

// Persist is the optional Postgres write-through. Called per pull with
// the freshly-fetched batch and its market. In production the loop
// calls Persist BEFORE Observer.Observe so the DB-backed detector sees
// the trade in its baseline reservoir.
type Persist func(ctx context.Context, m market.Market, trades []trade.Trade) error

// CursorReader returns the newest traded_at already stored for the
// supplied market, or the zero time when nothing is persisted yet.
// Required in production; tests inject a fake.
type CursorReader func(ctx context.Context, conditionID string) (time.Time, error)

type Config struct {
	Interval          time.Duration
	Concurrency       int
	BootstrapLookback time.Duration // initial pull window per market on first sight
	Clock             func() time.Time
	Persist           Persist
	Cursor            CursorReader
}

type Loop struct {
	cfg      Config
	client   *dataapi.Client
	cache    *marketcache.Cache
	observer TradeObserver
	metrics  *metrics.Metrics
	log      *zerolog.Logger
	now      func() time.Time
}

func New(
	cfg Config,
	c *dataapi.Client,
	cache *marketcache.Cache,
	obs TradeObserver,
	m *metrics.Metrics,
	log *zerolog.Logger,
) *Loop {
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 8
	}
	if cfg.BootstrapLookback <= 0 {
		cfg.BootstrapLookback = time.Hour
	}
	now := cfg.Clock
	if now == nil {
		now = time.Now
	}
	return &Loop{
		cfg:      cfg,
		client:   c,
		cache:    cache,
		observer: obs,
		metrics:  m,
		log:      log,
		now:      now,
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
	markets := l.cache.Snapshot()
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
	// Process trades oldest → newest so the DB-backed baseline sees them
	// in time order on the very next query — the Data API returns DESC
	// within a market; reverse client-side.
	sort.SliceStable(trades, func(i, j int) bool { return trades[i].Timestamp.Before(trades[j].Timestamp) })

	// Persist BEFORE Observe. A persist failure does not block
	// observation — the next tick will retry the cursor and a fresh
	// page will be persisted.
	if l.cfg.Persist != nil {
		if err := l.cfg.Persist(ctx, m, trades); err != nil {
			l.log.Err(err).Str("market", string(m.ID)).Msg("collect: persist failed")
		}
	}

	var notional float64
	for _, t := range trades {
		notional += t.NotionalUSD()
		// In Postgres mode l.observer is the true nil-interface (see
		// app.go wiring) and we just persist + skip; the detection
		// worker drains the queue. In memory/dev mode the inline
		// observer runs here. Per-trade recover prevents a single
		// bad row from killing the whole collect goroutine (and
		// therefore the process) — root cause must still be fixed
		// downstream; this is last line of defense, not the
		// architectural answer.
		if l.observer != nil {
			l.observeWithRecover(ctx, m, t)
		}
	}
	if l.metrics != nil {
		if l.metrics.TradesIngested != nil {
			l.metrics.TradesIngested.WithLabelValues(string(m.ID)).Add(float64(len(trades)))
		}
		if l.metrics.NotionalIngested != nil {
			l.metrics.NotionalIngested.WithLabelValues(string(m.ID)).Add(notional)
		}
	}
}

// observeWithRecover invokes the inline observer with a per-trade
// panic guard. Memory/dev mode only — in Postgres mode l.observer is
// nil and this function is never reached. The detection worker has
// its own boundary recover (safeObserve) and a DB row to stamp
// `failed` against; here there's no row, so we just log and continue.
func (l *Loop) observeWithRecover(ctx context.Context, m market.Market, t trade.Trade) {
	defer func() {
		if r := recover(); r != nil {
			l.log.Error().
				Interface("panic", r).
				Str("market", string(m.ID)).
				Str("trade_id", t.ID).
				Msg("collect: observer panic (dev inline mode) — trade dropped")
		}
	}()
	l.observer.Observe(ctx, m, t)
}

// lookback resolves the per-market "since" cutoff. Production order:
//  1. cfg.Cursor (DB-backed) — survives restarts.
//  2. now − BootstrapLookback for a first-sight market or when the
//     cursor is empty / read failed.
//
// Cursor read errors fall through to the bootstrap so a transient DB
// hiccup doesn't stall collection.
func (l *Loop) lookback(ctx context.Context, m market.Market) time.Time {
	if l.cfg.Cursor != nil {
		if ts, err := l.cfg.Cursor(ctx, string(m.ID)); err == nil && !ts.IsZero() {
			return ts.Add(time.Second)
		}
	}
	return l.now().Add(-l.cfg.BootstrapLookback)
}
