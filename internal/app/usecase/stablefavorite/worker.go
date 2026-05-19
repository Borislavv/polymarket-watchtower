// Package stablefavorite owns the periodic-sweep worker for the
// late-market-stable-favorite strategy.
//
// Architecture:
//
//   - The detector (internal/app/usecase/analytics/stablefavorite) is
//     pure and consumes a pre-computed Input.
//   - This package wires the DB reads (price window, candidate
//     markets, latest price), constructs the Input, calls Decide, and
//     persists firing verdicts as polymarket_alerts rows with a
//     stable_favorite dedup_key.
//
// This is NOT the whale-flow detector. It runs on a slower cadence
// (default 5m) and scans every late-stage market once per tick — not
// every trade.
package stablefavorite

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/rs/zerolog"

	det "github.com/Borislavv/polymarket-watchtower/internal/app/usecase/analytics/stablefavorite"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/marketcache"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/anomaly"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/vo"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/metrics"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/repository"
)

// Candidates lists active markets past the lifecycle threshold.
// Implemented by *repository.TradeRepository.
type Candidates interface {
	ListLateMarketCandidates(ctx context.Context, minLifecyclePct float64, limit int32) ([]repository.LateMarketCandidate, error)
}

// Prices reads the price-window stats and the latest observed price
// for a (market, outcome). Implemented by *repository.TradeRepository.
type Prices interface {
	PriceWindowStats(ctx context.Context, marketID int64, outcomeToken string, since time.Time) (repository.PriceWindow, error)
	LatestPriceForOutcome(ctx context.Context, marketID int64, outcomeToken string) (float64, error)
}

// AlertCreator is the dedup primitive — same shape detect.Loop uses.
// *repository.AlertRepository satisfies it.
type AlertCreator interface {
	TryCreatePending(ctx context.Context, a repository.NewAlert) (repository.Alert, bool, error)
}

// CrossMarket is the optional related-market price reader. Returns 0
// when no related market is wired or the lookup fails — the detector
// downgrades to "unavailable" confidence without erroring out.
//
// Production has no upstream cross-market integration wired today;
// the interface exists so the contract is explicit and any future
// adapter (Kalshi, sibling Polymarket outcome, CLOB book) plugs in
// without restructuring the worker.
type CrossMarket interface {
	Price(ctx context.Context, marketConditionID, outcomeToken string) float64
}

// Emitter is the realtime fanout (log + Telegram in production).
type Emitter interface {
	Notify(ctx context.Context, f anomaly.Finding) error
}

// MarketCache returns the per-(condition_id) outcome list. The
// adapter in app.go wraps *marketcache.Cache and returns a
// MarketView snapshot.
type MarketCache interface {
	View(id vo.MarketID) (MarketView, bool)
}

// MarketView is the read-only snapshot of a cached market that the
// worker needs. The adapter projects market.Market into this shape.
type MarketView struct {
	Tokens   []vo.TokenID
	Outcomes []string // human label, parallel to Tokens
}

// OutcomeLabel resolves a token id to its human label, or "" on miss.
func (m MarketView) OutcomeLabel(t vo.TokenID) string {
	for i, tok := range m.Tokens {
		if tok == t && i < len(m.Outcomes) {
			return m.Outcomes[i]
		}
	}
	return ""
}

// Config tunes the periodic worker.
type Config struct {
	// Enabled is the worker-side master switch. When false, Run
	// returns immediately. The detector's Enabled flag is checked
	// downstream too — but disabling at this layer also stops the
	// expensive candidate-listing query.
	Enabled bool

	// Interval is the polling cadence. 5m is enough resolution for a
	// strategy that fires on market state, not per-trade.
	Interval time.Duration

	// CandidateLimit caps how many markets we evaluate per tick.
	CandidateLimit int32

	// Clock optionally overrides time.Now (tests).
	Clock func() time.Time

	// StrategyVersion is woven into the dedup key.
	StrategyVersion string
}

func (c *Config) applyDefaults() {
	if c.Interval <= 0 {
		c.Interval = 5 * time.Minute
	}
	if c.CandidateLimit <= 0 {
		c.CandidateLimit = 200
	}
	if c.Clock == nil {
		c.Clock = time.Now
	}
	if c.StrategyVersion == "" {
		c.StrategyVersion = anomaly.StrategyIdentity
	}
}

// Worker is the periodic sweep.
type Worker struct {
	cfg         Config
	detector    *det.Detector
	candidates  Candidates
	prices      Prices
	cache       MarketCache
	crossMarket CrossMarket
	alerts      AlertCreator
	emit        Emitter
	metrics     *metrics.Metrics
	log         *zerolog.Logger
}

// New constructs a Worker.
func New(cfg Config, detector *det.Detector, candidates Candidates, prices Prices, cache MarketCache, cross CrossMarket, alerts AlertCreator, emit Emitter, met *metrics.Metrics, log *zerolog.Logger) *Worker {
	cfg.applyDefaults()
	return &Worker{
		cfg:         cfg,
		detector:    detector,
		candidates:  candidates,
		prices:      prices,
		cache:       cache,
		crossMarket: cross,
		alerts:      alerts,
		emit:        emit,
		metrics:     met,
		log:         log,
	}
}

// Run blocks until ctx cancels.
func (w *Worker) Run(ctx context.Context) {
	if !w.cfg.Enabled {
		return
	}
	t := time.NewTicker(w.cfg.Interval)
	defer t.Stop()
	w.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.tick(ctx)
		}
	}
}

// Tick exposes one cycle to tests.
func (w *Worker) Tick(ctx context.Context) { w.tick(ctx) }

func (w *Worker) tick(ctx context.Context) {
	rows, err := w.candidates.ListLateMarketCandidates(ctx, w.detector.Config().MinLifecyclePct, w.cfg.CandidateLimit)
	if err != nil {
		w.log.Err(err).Msg("stablefavorite: list candidates failed")
		return
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8) // bounded parallelism within a tick
	for _, c := range rows {
		c := c
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			w.processMarket(ctx, c)
		}()
	}
	wg.Wait()
}

func (w *Worker) processMarket(ctx context.Context, m repository.LateMarketCandidate) {
	mv, ok := w.cache.View(vo.MarketID(m.ConditionID))
	if !ok || len(mv.Tokens) == 0 {
		return
	}
	now := w.cfg.Clock()
	cfg := w.detector.Config()
	for _, tok := range mv.Tokens {
		w.evaluateOutcome(ctx, m, tok, mv.OutcomeLabel(tok), now, cfg)
	}
}

func (w *Worker) evaluateOutcome(ctx context.Context, m repository.LateMarketCandidate, tok vo.TokenID, outcomeLabel string, now time.Time, cfg det.Config) {
	price, err := w.prices.LatestPriceForOutcome(ctx, m.ID, string(tok))
	if err != nil || price <= 0 || price >= 1 {
		return
	}
	since24 := now.Add(-cfg.StabilityWindow)
	w24, err := w.prices.PriceWindowStats(ctx, m.ID, string(tok), since24)
	if err != nil {
		w.log.Err(err).Int64("market_id", m.ID).Msg("stablefavorite: 24h stats failed")
		return
	}
	since6 := now.Add(-6 * time.Hour)
	w6, err := w.prices.PriceWindowStats(ctx, m.ID, string(tok), since6)
	if err != nil {
		w.log.Err(err).Int64("market_id", m.ID).Msg("stablefavorite: 6h stats failed")
		return
	}

	cmPrice := 0.0
	if w.crossMarket != nil {
		cmPrice = w.crossMarket.Price(ctx, m.ConditionID, string(tok))
	}

	in := det.Input{
		MarketID:         m.ConditionID,
		OutcomeToken:     string(tok),
		Outcome:          outcomeLabel,
		LifecyclePct:     m.LifecyclePct,
		CurrentPrice:     price,
		Window24h:        toDetWindow(w24),
		Window6h:         toDetWindow(w6),
		CrossMarketPrice: cmPrice,
	}
	v := w.detector.Decide(in)
	if !v.Fired {
		return
	}

	f := w.buildFinding(in, v, m, tok)
	dedup := w.dedupKey(m.ConditionID, string(tok), v.Severity)
	f.DedupKey = dedup
	if !w.persistAlert(ctx, m.ID, dedup, f) {
		// Dedup'd — already alerted at this severity tier.
		return
	}
	if err := w.emit.Notify(ctx, f); err != nil {
		w.log.Err(err).Msg("stablefavorite: emit failed")
	}
}

func (w *Worker) buildFinding(in det.Input, v det.Verdict, m repository.LateMarketCandidate, tok vo.TokenID) anomaly.Finding {
	ref := &anomaly.StableFavoriteRef{
		MarketID:           m.ConditionID,
		OutcomeToken:       string(tok),
		Outcome:            in.Outcome,
		Probability:        in.CurrentPrice,
		RemainingReturnPct: v.RemainingReturnPct,
		StabilityWindow:    w.detector.Config().StabilityWindow,
		PriceMean:          in.Window24h.Mean,
		PriceStddev:        in.Window24h.Stddev,
		PriceMin:           in.Window24h.Min,
		PriceMax:           in.Window24h.Max,
		PriceFirst:         in.Window24h.First,
		PriceLast:          in.Window24h.Last,
		PriceSamples:       in.Window24h.Count,
		Drawdown:           safeDrawdown(in.Window24h),
		AdverseMove6h:      in.Window6h.Last - in.Window6h.First,
		RecentVolumeUSD:    in.Window24h.VolumeUSD,
		RecentTradeCount:   in.Window24h.Count,
		LifecyclePct:       in.LifecyclePct,
		Score:              v.Score,
		Confidence:         v.Confidence,
		CrossMarketStatus:  v.CrossMarketStatus,
		CrossMarketDelta:   v.CrossMarketDelta,
	}
	return anomaly.Finding{
		Kind:           anomaly.KindStableFavorite,
		Severity:       v.Severity,
		At:             w.cfg.Clock(),
		Reason:         anomaly.ReasonStableFavorite,
		StableFavorite: ref,
		Reasons:        v.Reasons,
		LifecyclePct:   in.LifecyclePct,
		Hot:            in.LifecyclePct >= w.detector.Config().HotLifecyclePct,
	}
}

// dedupKey: stable_favorite:<strategy>:<market_id>:<outcome>:<severity>.
// One alert per (market, outcome, severity); upgrades emit once at
// each new tier.
func (w *Worker) dedupKey(marketCondID, tok string, sev anomaly.Severity) string {
	return fmt.Sprintf("stable_favorite:%s:%s:%s:%s",
		w.cfg.StrategyVersion, marketCondID, tok, string(sev))
}

// persistAlert wraps TryCreatePending with the JSON payload + the
// right market id. Returns false on dedup hit so the caller skips
// the realtime fanout.
func (w *Worker) persistAlert(ctx context.Context, marketID int64, dedup string, f anomaly.Finding) bool {
	if w.alerts == nil {
		return true
	}
	payload, err := json.Marshal(f)
	if err != nil {
		w.log.Err(err).Msg("stablefavorite: marshal finding failed")
		return false
	}
	mid := marketID
	_, created, err := w.alerts.TryCreatePending(ctx, repository.NewAlert{
		DedupKey:        dedup,
		StrategyVersion: w.cfg.StrategyVersion,
		Kind:            repository.AlertKindStableFavorite,
		Reason:          f.Reason,
		Severity:        string(f.Severity),
		MarketID:        &mid,
		Payload:         payload,
	})
	if err != nil {
		w.log.Err(err).Msg("stablefavorite: persist alert failed")
		return false
	}
	return created
}

// toDetWindow projects the repository PriceWindow into the detector's
// WindowStats shape.
func toDetWindow(w repository.PriceWindow) det.WindowStats {
	return det.WindowStats{
		Count:      int(w.SampleCount),
		Mean:       w.MeanPrice,
		Stddev:     w.StddevPrice,
		Min:        w.MinPrice,
		Max:        w.MaxPrice,
		First:      w.FirstPrice,
		Last:       w.LastPrice,
		VolumeUSD:  w.VolumeUSD,
		BuyVolume:  w.BuyVolumeUSD,
		SellVolume: w.SellVolumeUSD,
	}
}

func safeDrawdown(w det.WindowStats) float64 {
	if w.Count < 2 || w.Max <= 0 {
		return 0
	}
	return (w.Max - w.Min) / w.Max
}

// marketcache is referenced by the production adapter in app.go; we
// keep the import alive so a future move of the adapter into this
// package stays trivial.
var _ = marketcache.New
var _ = errors.Is
