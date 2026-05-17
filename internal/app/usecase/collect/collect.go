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

	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/aggregate"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/market"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/trade"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/vo"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/metrics"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/polymarket/dataapi"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/polymarket/httpx"
	"github.com/rs/zerolog"
)

// TradeObserver is the per-trade hook into the detection pipeline. The detect
// package implements it; we depend on the interface so collect tests can fake
// it without dragging the whole analytics graph in.
type TradeObserver interface {
	Observe(ctx context.Context, m market.Market, t trade.Trade)
}

type Config struct {
	Interval     time.Duration
	Concurrency  int
	LookbackBoot time.Duration    // initial pull window per market on first sight
	Clock        func() time.Time // optional; defaults to time.Now
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
	since := l.lookback(m.ID)
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

func (l *Loop) lookback(id vo.MarketID) time.Time {
	l.lastTsMu.Lock()
	defer l.lastTsMu.Unlock()
	if ts, ok := l.lastTs[id]; ok {
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
