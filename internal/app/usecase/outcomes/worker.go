// Package outcomes is the post-alert resolution tracker. It runs as a
// background worker that periodically scans sent alerts whose markets
// are (or should be) resolved upstream and stamps a verdict on each
// alert row:
//
//	pending           — the worker has not seen this row yet OR the
//	                    upstream market is still open
//	resolved_correct  — the market resolved AND the alert direction
//	                    matched the winning outcome
//	resolved_wrong    — the market resolved AND the alert direction
//	                    did NOT match the winning outcome
//	unknown           — the market closed but Gamma's outcomePrices
//	                    are inconclusive (no clear 1.0 winner)
//	unavailable       — the upstream lookup failed (Gamma returned a
//	                    not-found or a transient error); the worker
//	                    bumps outcome_checked_at and re-tries next tick
//
// This is signal-quality measurement only. We never re-emit alerts, we
// never reverse the dedup namespace, and we never make legal claims
// from this data. Operators use the persisted verdicts offline to tune
// thresholds and gauge worker accuracy.
package outcomes

import (
	"context"
	"encoding/json"
	"math"
	"time"

	"github.com/rs/zerolog"

	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/anomaly"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/trade"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/polymarket/gamma"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/repository"
)

// AlertStore is the subset of *repository.AlertRepository the worker uses.
type AlertStore interface {
	ListSentForOutcomeCheck(ctx context.Context, claimLimit int32) ([]repository.Alert, error)
	MarkOutcome(ctx context.Context, u repository.OutcomeUpdate) error
	TouchOutcomeUnavailable(ctx context.Context, alertID int64) error
}

// MarketResolver returns the upstream resolution snapshot for one
// market. Satisfied by *gamma.Client.GetMarketResolution.
type MarketResolver interface {
	GetMarketResolution(ctx context.Context, conditionID string) (gamma.MarketResolution, bool, error)
}

// MarketLookup maps a DB market_id (from polymarket_alerts.market_id) to
// its upstream condition_id. Satisfied by *repository.MarketRepository.
type MarketLookup interface {
	GetByID(ctx context.Context, marketID int64) (repository.Market, error)
}

// Config tunes the worker.
type Config struct {
	Interval   time.Duration
	ClaimLimit int32
	// WinningPriceThreshold is the price above which a token is treated
	// as the winning outcome. Polymarket resolutions are typically
	// {1.0, 0.0} but odd dust can show up; 0.99 catches the canonical
	// case while ignoring late-trading noise.
	WinningPriceThreshold float64
	Clock                 func() time.Time
}

func (c Config) applyDefaults() Config {
	if c.Interval <= 0 {
		c.Interval = 15 * time.Minute
	}
	if c.ClaimLimit <= 0 {
		c.ClaimLimit = 64
	}
	if c.WinningPriceThreshold <= 0 {
		c.WinningPriceThreshold = 0.99
	}
	if c.Clock == nil {
		c.Clock = time.Now
	}
	return c
}

// Worker is the long-running outcomes loop.
type Worker struct {
	cfg     Config
	alerts  AlertStore
	markets MarketLookup
	gamma   MarketResolver
	log     *zerolog.Logger
}

// New wires the worker.
func New(cfg Config, alerts AlertStore, markets MarketLookup, gamma MarketResolver, log *zerolog.Logger) *Worker {
	return &Worker{cfg: cfg.applyDefaults(), alerts: alerts, markets: markets, gamma: gamma, log: log}
}

// Run blocks until ctx is cancelled. Fires an immediate tick so a freshly-
// started process drains any backlog without waiting one interval.
func (w *Worker) Run(ctx context.Context) error {
	t := time.NewTicker(w.cfg.Interval)
	defer t.Stop()
	w.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			w.tick(ctx)
		}
	}
}

// Tick runs one outcome-check sweep; exposed for tests.
func (w *Worker) Tick(ctx context.Context) { w.tick(ctx) }

func (w *Worker) tick(ctx context.Context) {
	candidates, err := w.alerts.ListSentForOutcomeCheck(ctx, w.cfg.ClaimLimit)
	if err != nil {
		w.log.Err(err).Msg("outcomes: list candidates failed")
		return
	}
	if len(candidates) == 0 {
		return
	}
	for _, a := range candidates {
		if ctx.Err() != nil {
			return
		}
		w.processOne(ctx, a)
	}
}

func (w *Worker) processOne(ctx context.Context, a repository.Alert) {
	if a.MarketID == nil {
		// Alerts without a persisted market id can't be verified — mark
		// unavailable so we stop scanning the row.
		_ = w.alerts.MarkOutcome(ctx, repository.OutcomeUpdate{
			AlertID: a.ID,
			Status:  repository.OutcomeUnavailable,
		})
		return
	}
	market, err := w.markets.GetByID(ctx, *a.MarketID)
	if err != nil {
		w.log.Err(err).Int64("alert_id", a.ID).Msg("outcomes: market lookup failed")
		_ = w.alerts.TouchOutcomeUnavailable(ctx, a.ID)
		return
	}
	res, found, err := w.gamma.GetMarketResolution(ctx, market.ConditionID)
	if err != nil {
		w.log.Err(err).Int64("alert_id", a.ID).Str("condition_id", market.ConditionID).Msg("outcomes: gamma lookup failed")
		_ = w.alerts.TouchOutcomeUnavailable(ctx, a.ID)
		return
	}
	if !found {
		// Market gone (archived / very old) — no way to verify. Mark as
		// unavailable so we stop scanning.
		_ = w.alerts.MarkOutcome(ctx, repository.OutcomeUpdate{
			AlertID: a.ID,
			Status:  repository.OutcomeUnavailable,
		})
		return
	}
	if !res.Closed {
		// Eligible for re-check next tick.
		_ = w.alerts.TouchOutcomeUnavailable(ctx, a.ID)
		return
	}

	verdict := w.classify(a, res)
	if err := w.alerts.MarkOutcome(ctx, verdict); err != nil {
		w.log.Err(err).Int64("alert_id", a.ID).Msg("outcomes: mark outcome failed")
	}
}

// classify decodes the alert's persisted Finding and compares its trade
// direction against the resolved winning outcome. Returns an
// OutcomeUpdate ready to persist.
func (w *Worker) classify(a repository.Alert, res gamma.MarketResolution) repository.OutcomeUpdate {
	update := repository.OutcomeUpdate{AlertID: a.ID, ResolvedAt: res.EndDate}

	winningIdx, ok := winningOutcomeIndex(res.OutcomePrices, w.cfg.WinningPriceThreshold)
	if !ok {
		update.Status = repository.OutcomeUnknown
		return update
	}
	if winningIdx < len(res.TokenIDs) {
		update.WinningOutcomeToken = res.TokenIDs[winningIdx]
	}
	if winningIdx < len(res.OutcomeLabels) {
		update.WinningOutcomeLabel = res.OutcomeLabels[winningIdx]
	}

	// Decode the persisted Finding to recover the alert's trade direction.
	var f anomaly.Finding
	if err := json.Unmarshal(a.Payload, &f); err != nil || f.Trade == nil {
		update.Status = repository.OutcomeUnknown
		return update
	}
	alertedToken := string(f.Trade.Token)
	alertedSide := f.Trade.Side
	winningToken := update.WinningOutcomeToken

	switch {
	case alertedToken == "" || winningToken == "":
		update.Status = repository.OutcomeUnknown
	case alertedSide == trade.SideBuy && alertedToken == winningToken:
		update.Status = repository.OutcomeCorrect
	case alertedSide == trade.SideSell && alertedToken != winningToken:
		// Selling a losing outcome is correct (the seller predicted the
		// loss).
		update.Status = repository.OutcomeCorrect
	default:
		update.Status = repository.OutcomeWrong
	}
	return update
}

// winningOutcomeIndex picks the index of the outcomePrices entry whose
// price is ≥ threshold. Returns (index, true) on success, (-1, false)
// when no entry crosses the threshold (typically a degenerate resolution
// or a closed-but-not-resolved market). math.Abs handles tiny
// floating-point drift around 1.0.
func winningOutcomeIndex(prices []float64, threshold float64) (int, bool) {
	for i, p := range prices {
		if math.Abs(p) >= threshold {
			return i, true
		}
	}
	return -1, false
}
