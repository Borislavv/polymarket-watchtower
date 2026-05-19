// Package dbbaseline is the Postgres-backed baseline fetcher used by the
// detector in production. It satisfies the same Stats contract as the
// in-memory analytics/baseline package, but reads from polymarket_trades
// via the repository layer.
//
// Trade writes that populate the bucket are performed by
// internal/app/usecase/persist (collect path) and
// internal/app/usecase/backfill (historical fill). This provider never
// writes — it is a pure read adapter, so persistence is the single
// authority on which trades count toward a baseline.
package dbbaseline

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/analytics/baseline"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/vo"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/repository"
)

// MarketResolver maps an upstream condition id to the local market row.
// Implemented by *repository.MarketRepository; abstracted as an interface
// so tests can plug in a fake without standing up a DB.
type MarketResolver interface {
	GetByConditionID(ctx context.Context, conditionID string) (repository.Market, error)
}

// TradeStats is the subset of *repository.TradeRepository the provider
// consumes. Same rationale as MarketResolver: test-friendliness.
type TradeStats interface {
	Distribution(ctx context.Context, q repository.BaselineQuery) (repository.BaselineDistribution, error)
}

// Config tunes the provider. Window is interpreted exactly as the in-memory
// baseline.Config.Window: the MAXIMUM lookback. 0 means "no upper bound"
// (use all stored history).
type Config struct {
	Window time.Duration
	Clock  func() time.Time
}

// Provider implements the BaselineFetcher interface consumed by the detector.
// It is safe for concurrent use.
type Provider struct {
	cfg     Config
	now     func() time.Time
	trades  TradeStats
	markets MarketResolver

	// idCache maps condition_id → local market id. Markets do not change
	// ids, so the cache is process-lifetime; bounded by the active market
	// universe.
	idCache sync.Map // map[vo.MarketID]int64
}

// New constructs a Provider. Both repositories are required.
func New(cfg Config, trades TradeStats, markets MarketResolver) *Provider {
	now := cfg.Clock
	if now == nil {
		now = time.Now
	}
	return &Provider{cfg: cfg, now: now, trades: trades, markets: markets}
}

// Stats returns the per-bucket statistical summary used by the per-trade
// scorer. The behaviour matches in-memory analytics/baseline:
//
//   - Empty bucket → zero Stats (Count=0).
//   - Window > 0 → samples older than (now − Window) are excluded.
//   - Window == 0 → use every stored sample.
//   - SpanActual is the time between the oldest and newest live samples;
//     zero when fewer than two samples remain.
//
// Errors from the underlying repository are propagated; the detector treats
// them as "no baseline available" and skips scoring rather than firing on
// stale data.
func (p *Provider) Stats(ctx context.Context, k baseline.Key) (baseline.Stats, error) {
	mid, ok, err := p.resolveID(ctx, k.Market)
	if err != nil {
		return baseline.Stats{}, fmt.Errorf("resolve market %q: %w", k.Market, err)
	}
	if !ok {
		// Market not yet persisted — discovery hasn't caught up. Treat as
		// "no baseline" so the detector doesn't fire on a missing row.
		return baseline.Stats{}, nil
	}

	since := time.Time{}
	if p.cfg.Window > 0 {
		since = p.now().Add(-p.cfg.Window)
	}
	dist, err := p.trades.Distribution(ctx, repository.BaselineQuery{
		MarketID:     mid,
		OutcomeToken: string(k.OutcomeToken),
		Since:        since,
	})
	if err != nil {
		return baseline.Stats{}, fmt.Errorf("distribution: %w", err)
	}
	if dist.SampleCount == 0 {
		return baseline.Stats{}, nil
	}
	return baseline.Stats{
		Count:      int(dist.SampleCount),
		MeanUSD:    dist.MeanNotionalUSD,
		MedianUSD:  dist.MedianNotionalUSD,
		P95USD:     dist.P95NotionalUSD,
		P99USD:     dist.P99NotionalUSD,
		TotalUSD:   dist.TotalNotionalUSD,
		SpanActual: dist.Span(),
		OldestAt:   dist.OldestAt,
	}, nil
}

// resolveID returns the local market id for the supplied condition id.
// Returns ok=false (no error) when the market is not yet in the DB.
func (p *Provider) resolveID(ctx context.Context, condID vo.MarketID) (int64, bool, error) {
	if v, ok := p.idCache.Load(condID); ok {
		return v.(int64), true, nil
	}
	m, err := p.markets.GetByConditionID(ctx, string(condID))
	if err != nil {
		// repository.GetByConditionID returns a wrapped pgx.ErrNoRows for
		// "not found"; the caller's contract is "ok=false, no error".
		// Distinguish via error message — string match against the wrapped
		// pgx sentinel is brittle; instead the repo returns a typed sentinel
		// in future work. For now we treat any error as "not yet present"
		// to keep detection robust against transient DB hiccups; the next
		// trade will retry.
		return 0, false, nil //nolint:nilerr // intentional: missing market is not an error here
	}
	p.idCache.Store(condID, m.ID)
	return m.ID, true, nil
}

// Forget clears any cached id for the supplied condition id. Called when
// discovery removes a market from the registry so the provider doesn't
// pin stale ids.
func (p *Provider) Forget(condID vo.MarketID) {
	p.idCache.Delete(condID)
}
