// Package traderbaseline is the Postgres-backed trader-history fetcher used
// by the detector for the trader-relative leg of the multiplier ladder.
//
// Whereas dbbaseline answers "what does THIS market look like?",
// traderbaseline answers "what does THIS wallet usually do?". A trade is
// flagged as an informed-flow candidate when it is anomalous on EITHER axis
// — see internal/app/usecase/analytics/score for the composition rule.
//
// This provider never writes — persistence is owned by
// internal/app/usecase/persist (collect path) and
// internal/app/usecase/backfill (historical fill).
package traderbaseline

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/analytics/baseline"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/repository"
)

// TraderResolver maps a wallet address to the local trader row.
// Implemented by *repository.TraderRepository.
type TraderResolver interface {
	GetByWallet(ctx context.Context, wallet string) (repository.Trader, error)
}

// TraderStats is the subset of *repository.TraderRepository consumed by the
// provider. Abstracted as an interface so tests can plug a fake without
// standing up a database.
type TraderStats interface {
	Distribution(ctx context.Context, traderID int64, since time.Time) (repository.TraderDistribution, error)
}

// Config tunes the provider. Window is the MAXIMUM lookback over the
// trader's stored history; 0 means "no upper bound" (use the wallet's full
// history). Behaviour matches dbbaseline.Config.
type Config struct {
	Window time.Duration
	Clock  func() time.Time
}

// Provider satisfies the detector's trader-stats interface. Safe for
// concurrent use.
type Provider struct {
	cfg     Config
	now     func() time.Time
	traders TraderResolver
	stats   TraderStats

	// idCache maps wallet → local trader id. Wallets do not change ids, so
	// the cache is process-lifetime; bounded by the active wallet universe
	// (which itself is bounded by the trades we've observed).
	idCache sync.Map // map[string]int64
}

// New constructs a Provider. Both repositories are required.
func New(cfg Config, traders TraderResolver, stats TraderStats) *Provider {
	now := cfg.Clock
	if now == nil {
		now = time.Now
	}
	return &Provider{cfg: cfg, now: now, traders: traders, stats: stats}
}

// Stats returns the wallet's full-history distributional summary, expressed
// in the same baseline.Stats shape the market-side provider uses. This lets
// the detector flow trader stats through the same readiness gates as the
// market baseline.
//
// Behaviour:
//   - Empty/unknown wallet → zero Stats (Count=0). No error.
//   - Unknown trader (wallet never persisted) → zero Stats. No error — the
//     scorer's readiness gate naturally disables the trader axis.
//   - Window > 0 → samples older than (now − Window) are excluded.
//   - Window == 0 → use every stored sample.
//
// Repository errors propagate; the detector treats them as "no trader
// baseline available" and skips the trader axis rather than firing on
// stale data.
func (p *Provider) Stats(ctx context.Context, wallet string) (baseline.Stats, error) {
	if wallet == "" {
		return baseline.Stats{}, nil
	}
	tid, ok, err := p.resolveID(ctx, wallet)
	if err != nil {
		return baseline.Stats{}, fmt.Errorf("resolve trader %q: %w", wallet, err)
	}
	if !ok {
		return baseline.Stats{}, nil
	}

	since := time.Time{}
	if p.cfg.Window > 0 {
		since = p.now().Add(-p.cfg.Window)
	}
	dist, err := p.stats.Distribution(ctx, tid, since)
	if err != nil {
		return baseline.Stats{}, fmt.Errorf("trader distribution %d: %w", tid, err)
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

// resolveID returns the local trader id for the supplied wallet, or
// ok=false when the wallet has not been persisted yet. ErrTraderNotFound is
// silently mapped to ok=false — discovery has simply not caught up.
func (p *Provider) resolveID(ctx context.Context, wallet string) (int64, bool, error) {
	if v, ok := p.idCache.Load(wallet); ok {
		return v.(int64), true, nil
	}
	t, err := p.traders.GetByWallet(ctx, wallet)
	if err != nil {
		if errors.Is(err, repository.ErrTraderNotFound) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("get trader: %w", err)
	}
	p.idCache.Store(wallet, t.ID)
	return t.ID, true, nil
}

// Forget clears any cached id for the wallet. Useful in tests; production
// has no path that retires a wallet.
func (p *Provider) Forget(wallet string) {
	p.idCache.Delete(wallet)
}
