// Package collect pulls public trades from the Data API for every market in
// the registry on a fixed cadence and folds them into the aggregate engine.
package collect

import (
	"context"
	"sync"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/core/domain/vo"
	"github.com/Borislavv/polymarket-watchtower/internal/core/infra/metrics"
	"github.com/Borislavv/polymarket-watchtower/internal/core/infra/polymarket/dataapi"
	"github.com/Borislavv/polymarket-watchtower/internal/core/usecase/aggregate"
	"github.com/rs/zerolog"
)

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
		go func(id vo.MarketID) {
			defer wg.Done()
			defer func() { <-sem }()
			l.pull(ctx, id)
		}(m.ID)
	}
	wg.Wait()
}

func (l *Loop) pull(ctx context.Context, id vo.MarketID) {
	since := l.lookback(id)
	trades, err := l.client.ListTradesSince(ctx, dataapi.ListTradesOpts{Market: id, Since: since})
	if err != nil {
		l.log.Err(err).Str("market", string(id)).Msg("collect: pull failed")
		return
	}
	if len(trades) == 0 {
		return
	}
	l.engine.IngestBatch(trades)

	var notional float64
	var newest time.Time
	for _, t := range trades {
		notional += t.NotionalUSD()
		if t.Timestamp.After(newest) {
			newest = t.Timestamp
		}
	}
	l.metrics.TradesIngested.WithLabelValues(string(id)).Add(float64(len(trades)))
	l.metrics.NotionalIngested.WithLabelValues(string(id)).Add(notional)
	l.setLastTs(id, newest)
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
