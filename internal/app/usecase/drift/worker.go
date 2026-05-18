// Package drift is the CLV-lite post-trade drift enrichment worker. For
// each sent alert it persists four signed-favourable price-drift values
// computed from the cheap public-data proxy "first trade on the same
// (market, outcome) at or after T+window":
//
//	clv_15m   — drift at T+15m
//	clv_1h    — drift at T+1h
//	clv_6h    — drift at T+6h
//	clv_24h   — drift at T+24h
//
// Sign convention: the values are stored already aligned to the alert's
// direction, so a POSITIVE drift always means "the price moved in the
// alert's favour".
//
//	BUY  outcome → favourable = laterPrice > tradePrice → drift > 0
//	SELL outcome → favourable = laterPrice < tradePrice → drift > 0
//
// Each value is the *fractional* price move (-1.0 .. +1.0), not basis
// points or percent. Persisting raw fractions avoids unit confusion
// when this lands in Grafana.
//
// The worker NEVER uses future data for the firing decision — by
// construction it only runs on alerts whose sent_at is at least
// minWindow ago (the 15m window must have elapsed; the 6h/24h windows
// may still be pending and are stamped NULL until the data exists).
// drift_status flips to 'available' the moment AT LEAST one window
// has a number; 'unavailable' when the row is older than the longest
// window but none yielded a reference price.
package drift

import (
	"context"
	"encoding/json"
	"time"

	"github.com/rs/zerolog"

	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/anomaly"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/trade"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/repository"
)

// AlertStore is the subset of *repository.AlertRepository the worker uses.
type AlertStore interface {
	ListSentForDrift(ctx context.Context, minWindow time.Duration, claimLimit int32) ([]repository.Alert, error)
	MarkDrift(ctx context.Context, u repository.DriftUpdate) error
}

// PriceLookup returns the price of the first trade on (market, outcome)
// at or after the supplied timestamp. Satisfied by
// *repository.TradeRepository.TradePriceAtOrAfter.
type PriceLookup interface {
	TradePriceAtOrAfter(ctx context.Context, marketID int64, outcomeToken string, at time.Time) (float64, bool, error)
}

// Config tunes the worker.
type Config struct {
	Interval   time.Duration
	ClaimLimit int32
	// LongestWindow is used to decide when to flip drift_status from
	// 'pending' to 'unavailable' for rows that have been waiting longer
	// than the largest window but still produced no reference price.
	// Defaults to 24h to match the longest CLV window.
	LongestWindow time.Duration
	Clock         func() time.Time
}

func (c Config) applyDefaults() Config {
	if c.Interval <= 0 {
		c.Interval = 5 * time.Minute
	}
	if c.ClaimLimit <= 0 {
		c.ClaimLimit = 64
	}
	if c.LongestWindow <= 0 {
		c.LongestWindow = 24 * time.Hour
	}
	if c.Clock == nil {
		c.Clock = time.Now
	}
	return c
}

// windows is the fixed CLV-lite window set. Mirrors the schema column
// order in 00005 (clv_15m, clv_1h, clv_6h, clv_24h).
var windows = []time.Duration{
	15 * time.Minute,
	1 * time.Hour,
	6 * time.Hour,
	24 * time.Hour,
}

// minWindow is the shortest of `windows`; the worker filters candidates
// to those that have at least one window elapsed.
var minWindow = windows[0]

type Worker struct {
	cfg    Config
	alerts AlertStore
	prices PriceLookup
	log    *zerolog.Logger
}

func New(cfg Config, alerts AlertStore, prices PriceLookup, log *zerolog.Logger) *Worker {
	return &Worker{cfg: cfg.applyDefaults(), alerts: alerts, prices: prices, log: log}
}

// Run blocks until ctx is cancelled. Initial tick fires immediately.
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

// Tick runs one drift-enrichment sweep; exposed for tests.
func (w *Worker) Tick(ctx context.Context) { w.tick(ctx) }

func (w *Worker) tick(ctx context.Context) {
	candidates, err := w.alerts.ListSentForDrift(ctx, minWindow, w.cfg.ClaimLimit)
	if err != nil {
		w.log.Err(err).Msg("drift: list candidates failed")
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
		_ = w.alerts.MarkDrift(ctx, repository.DriftUpdate{
			AlertID: a.ID,
			Status:  repository.DriftUnavailable,
		})
		return
	}

	// Decode the persisted Finding to recover trade.Token + side + price.
	// The detector copies the trade timestamp and price into TradeRef; we
	// also need the outcome token, which is NOT on TradeRef. Recover it
	// from the alert's payload via the (typed) trade field — wait, it
	// IS recoverable: the Finding's Accumulation reference (when present)
	// carries OutcomeToken; for single-trade Findings we fall back to
	// the alert's market_id + the in-payload outcome label. To keep this
	// path simple and robust, we read the raw trade from the payload's
	// `Trade` substructure and rely on the `OutcomeToken` we persist via
	// the AccumulationRef when available, otherwise we skip.
	var f anomaly.Finding
	if err := json.Unmarshal(a.Payload, &f); err != nil || f.Trade == nil {
		_ = w.alerts.MarkDrift(ctx, repository.DriftUpdate{
			AlertID: a.ID,
			Status:  repository.DriftUnavailable,
		})
		return
	}
	outcomeToken := outcomeTokenFromFinding(f)
	if outcomeToken == "" {
		// No usable token in the payload — drift cannot be computed.
		_ = w.alerts.MarkDrift(ctx, repository.DriftUpdate{
			AlertID: a.ID,
			Status:  repository.DriftUnavailable,
		})
		return
	}

	tradePrice := f.Trade.Price
	tradeAt := f.Trade.At
	side := f.Trade.Side

	clv := make([]*float64, len(windows))
	availableCount := 0
	for i, win := range windows {
		// Skip the lookup when the window hasn't elapsed yet — leaves the
		// column NULL and the row eligible for re-check next tick.
		if w.cfg.Clock().Sub(tradeAt) < win {
			continue
		}
		later, ok, err := w.prices.TradePriceAtOrAfter(ctx, *a.MarketID, outcomeToken, tradeAt.Add(win))
		if err != nil {
			w.log.Err(err).
				Int64("alert_id", a.ID).
				Dur("window", win).
				Msg("drift: price lookup failed; leaving window null")
			continue
		}
		if !ok || tradePrice <= 0 {
			continue
		}
		value := favorableDrift(side, tradePrice, later)
		clv[i] = &value
		availableCount++
	}

	status := decideStatus(w.cfg.Clock(), tradeAt, availableCount, w.cfg.LongestWindow)
	_ = w.alerts.MarkDrift(ctx, repository.DriftUpdate{
		AlertID: a.ID,
		Status:  status,
		CLV15m:  clv[0],
		CLV1h:   clv[1],
		CLV6h:   clv[2],
		CLV24h:  clv[3],
	})
}

// favorableDrift returns the signed fractional drift in the alert's
// favour. Sign convention:
//
//	BUY  : positive when laterPrice > tradePrice
//	SELL : positive when laterPrice < tradePrice
//
// Drift is normalised to the trade price so a $0.05→$0.06 BUY move
// becomes +0.2 (a 20% move in the alert's favour); a $0.06→$0.05
// move becomes -0.16... (negative).
func favorableDrift(side trade.Side, tradePrice, laterPrice float64) float64 {
	if tradePrice <= 0 {
		return 0
	}
	raw := (laterPrice - tradePrice) / tradePrice
	if side == trade.SideSell {
		raw = -raw
	}
	return raw
}

// outcomeTokenFromFinding returns the outcome CLOB token id from the
// alert's persisted Finding. Accumulation Findings carry it directly on
// the AccumulationRef; single-trade Findings do not (TradeRef has the
// outcome LABEL only). For single-trade Findings we fall through to "",
// which marks the row unavailable.
func outcomeTokenFromFinding(f anomaly.Finding) string {
	if f.Accumulation != nil && f.Accumulation.OutcomeToken != "" {
		return f.Accumulation.OutcomeToken
	}
	// TODO(post-cleanup): expose vo.TokenID on TradeRef so single-trade
	// drift can be computed without a DB roundtrip. The migration to add
	// it is trivial; deferred to avoid churning the dedup-key payload.
	return ""
}

// decideStatus returns the drift_status to persist on this update.
//
//   - available   : at least one window produced a number
//   - unavailable : row is older than the longest window AND no window
//     produced a number (no later trade exists; the
//     bucket is dead)
//   - pending     : earlier in the day; the worker re-checks next tick
func decideStatus(now, tradeAt time.Time, available int, longest time.Duration) repository.DriftStatus {
	if available > 0 {
		return repository.DriftAvailable
	}
	if now.Sub(tradeAt) >= longest {
		return repository.DriftUnavailable
	}
	return repository.DriftPending
}
