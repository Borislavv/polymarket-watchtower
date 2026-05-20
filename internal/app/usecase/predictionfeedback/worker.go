// Package predictionfeedback is the v10.2 prediction calibration
// worker. It periodically measures whether predictions moved the
// market in the predicted direction at fixed horizons (1h / 6h /
// 24h) so the operator can see whether the engine is producing
// useful theses or noise.
//
// The worker is deterministic, lightweight, and fail-open:
//
//   - Reads polymarket_market_predictions for active rows older
//     than the smallest horizon.
//   - Resolves each prediction's market_id + outcome_token via the
//     existing repositories.
//   - Pulls the first trade price at-or-after T_pred and
//     T_pred + horizon for each missing horizon.
//   - Writes one polymarket_prediction_feedback row per
//     (prediction_id, horizon) with direction_correct + delta.
//   - NEVER blocks alert delivery. Per-prediction failures log and
//     move on.
package predictionfeedback

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/outcomemapping"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/predictionevaluation"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/metrics"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/repository"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/workerguard"
)

// Config tunes the worker.
type Config struct {
	Enabled  bool
	Interval time.Duration
	// Horizons is the list of look-ahead durations to measure.
	// Default ["1h","6h","24h"]; parsed from the
	// PREDICTION_FEEDBACK_HORIZONS env var by app.go.
	Horizons []time.Duration
	// BatchSize bounds the candidates pulled per Tick.
	BatchSize int

	// --- v10.3 evaluation classifier knobs ---
	// MinMaterialDelta is the |price_delta| threshold below which a
	// horizon is treated as "no movement" rather than "direction
	// correct/wrong". Default 0.03 (3 percentage points of probability).
	MinMaterialDelta float64
	// UsefulEarlyWindow is the horizon at or below which a correct
	// call is classified "useful_early" rather than "useful_correct".
	// Default "6h".
	UsefulEarlyWindow string

	// Clock is overridable for tests.
	Clock func() time.Time
}

func (c *Config) applyDefaults() {
	if c.Interval <= 0 {
		c.Interval = 15 * time.Minute
	}
	if len(c.Horizons) == 0 {
		c.Horizons = []time.Duration{1 * time.Hour, 6 * time.Hour, 24 * time.Hour}
	}
	if c.BatchSize <= 0 {
		c.BatchSize = 100
	}
	if c.Clock == nil {
		c.Clock = time.Now
	}
}

// Seams.

type FeedbackStore interface {
	ListPredictionsForFeedback(ctx context.Context, oldest time.Time, limit int32) ([]repository.FeedbackCandidate, error)
	HorizonsRecorded(ctx context.Context, predictionID int64) (map[string]bool, error)
	UpsertFeedback(ctx context.Context, in repository.FeedbackRow) error
}

// EvaluationStore is the v10.3 seam to the
// PredictionIntelligenceRepository's evaluation upsert. nil =
// disabled (the feedback worker still writes feedback rows; only
// the evaluation classification step is skipped).
type EvaluationStore interface {
	UpsertEvaluation(ctx context.Context, in repository.PredictionEvaluationRow) error
}

type MarketResolver interface {
	GetByConditionID(ctx context.Context, conditionID string) (repository.Market, error)
}

type EventPageMarketLister interface {
	ListLatestEventMarkets(ctx context.Context, eventSlug string) ([]repository.EventPageMarketRow, error)
}

type TradePriceLookup interface {
	TradePriceAtOrAfter(ctx context.Context, marketID int64, outcomeToken string, at time.Time) (float64, bool, error)
}

// Worker is the periodic feedback loop.
type Worker struct {
	cfg       Config
	evaluator EvaluationStore
	store     FeedbackStore
	markets   MarketResolver
	pages     EventPageMarketLister
	trades    TradePriceLookup
	met       *metrics.Metrics
	log       *zerolog.Logger
	startMu   sync.Mutex
	started   bool
}

func New(cfg Config, store FeedbackStore, markets MarketResolver, pages EventPageMarketLister, trades TradePriceLookup, met *metrics.Metrics, log *zerolog.Logger) *Worker {
	cfg.applyDefaults()
	return &Worker{cfg: cfg, store: store, markets: markets, pages: pages, trades: trades, met: met, log: log}
}

// Run blocks until ctx cancels. Immediate first tick is intentional;
// the worker is read-only on the prediction row + writes one
// feedback row per (prediction, horizon) — no Telegram, no AI, no
// burst risk.
func (w *Worker) Run(ctx context.Context) {
	if !w.cfg.Enabled {
		return
	}
	guard := workerguard.New("prediction_feedback", w.met, w.log)
	t := time.NewTicker(w.cfg.Interval)
	defer t.Stop()
	guard.Run(ctx, func(ctx context.Context) { w.Tick(ctx) })
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			guard.Run(ctx, func(ctx context.Context) { w.Tick(ctx) })
		}
	}
}

// Tick processes one batch. Returns the number of feedback rows
// written (handy for the CLI).
func (w *Worker) Tick(ctx context.Context) int {
	start := w.cfg.Clock()
	now := start.UTC()
	// "Smallest horizon" gating — a prediction younger than the
	// shortest horizon has nothing for us to measure yet.
	minHorizon := w.cfg.Horizons[0]
	for _, h := range w.cfg.Horizons {
		if h < minHorizon {
			minHorizon = h
		}
	}
	oldest := now.Add(-minHorizon)
	cands, err := w.store.ListPredictionsForFeedback(ctx, oldest, int32(w.cfg.BatchSize))
	if err != nil {
		if w.log != nil {
			w.log.Warn().Err(err).Msg("prediction feedback: list candidates failed")
		}
		w.observeCycle("failed")
		return 0
	}
	if len(cands) == 0 {
		w.observeCycle("empty")
		return 0
	}
	written := 0
	for _, c := range cands {
		written += w.measureOne(ctx, c, now)
	}
	w.observeCycle("ok")
	if w.log != nil {
		w.log.Debug().
			Int("candidates", len(cands)).
			Int("written", written).
			Dur("duration", time.Since(start)).
			Msg("prediction feedback: cycle complete")
	}
	return written
}

// measureOne computes + persists feedback for one candidate across
// every horizon that's eligible AND not yet recorded.
func (w *Worker) measureOne(ctx context.Context, c repository.FeedbackCandidate, now time.Time) int {
	recorded, err := w.store.HorizonsRecorded(ctx, c.ID)
	if err != nil {
		recorded = map[string]bool{} // fail-open
	}
	// Resolve market_id + outcome_token once per candidate.
	mk, err := w.markets.GetByConditionID(ctx, c.ConditionID)
	if err != nil {
		w.observePred("market_lookup_failed")
		return 0
	}
	pages, _ := w.pages.ListLatestEventMarkets(ctx, c.EventSlug)
	mapper := outcomemapping.NewMapper(pages)
	mapping, ok := mapper.ResolveByConditionAndOutcome(c.ConditionID, c.Outcome)
	if !ok || mapping.CLOBTokenID == "" {
		w.observePred("outcome_token_unknown")
		return 0
	}
	// Price at prediction creation.
	priceAtPred, hasPred, _ := w.trades.TradePriceAtOrAfter(ctx, mk.ID, mapping.CLOBTokenID, c.CreatedAt)
	written := 0
	for _, horizon := range w.cfg.Horizons {
		label := horizonLabel(horizon)
		if label == "" || recorded[label] {
			continue
		}
		due := c.CreatedAt.Add(horizon)
		if due.After(now) {
			continue // not yet eligible
		}
		row := repository.FeedbackRow{
			PredictionID: c.ID,
			Horizon:      label,
		}
		if hasPred {
			p := priceAtPred
			row.PriceAtPrediction = &p
		}
		if px, hasH, _ := w.trades.TradePriceAtOrAfter(ctx, mk.ID, mapping.CLOBTokenID, due); hasH {
			ph := px
			row.PriceAtHorizon = &ph
			if hasPred {
				delta := px - priceAtPred
				row.PriceDelta = &delta
				row.DirectionCorrect = directionCorrect(c.SideBias, delta)
			}
		}
		row.StateAtHorizon = c.CurrentState
		if err := w.store.UpsertFeedback(ctx, row); err != nil {
			if w.log != nil {
				w.log.Warn().Err(err).Int64("id", c.ID).Str("horizon", label).Msg("prediction feedback: upsert failed")
			}
			w.observePred("upsert_failed")
			continue
		}
		written++
		w.observePred("ok")
		w.observeHorizon(label)
		// v10.3: write a deterministic evaluation classification
		// row beside the feedback row.
		w.evaluateAndStore(ctx, c, row)
	}
	return written
}

// evaluateAndStore wraps the deterministic classifier + persists
// one evaluation row per (prediction, horizon). nil evaluator
// short-circuits silently.
func (w *Worker) evaluateAndStore(ctx context.Context, c repository.FeedbackCandidate, row repository.FeedbackRow) {
	if w.evaluator == nil {
		return
	}
	in := predictionevaluation.Inputs{
		Horizon:                  row.Horizon,
		SideBias:                 c.SideBias,
		PriceAtPrediction:        row.PriceAtPrediction,
		PriceAtHorizon:           row.PriceAtHorizon,
		StateAtHorizon:           c.CurrentState,
		RepricingStatusAtHorizon: row.RepricingStatusAtHorizon,
		FlowConfirmed:            row.FlowConfirmed,
		MinMaterialDelta:         w.cfg.MinMaterialDelta,
		UsefulEarlyWindow:        w.cfg.UsefulEarlyWindow,
	}
	dec := predictionevaluation.Classify(in)
	if err := w.evaluator.UpsertEvaluation(ctx, repository.PredictionEvaluationRow{
		PredictionID: c.ID,
		Horizon:      row.Horizon,
		Evaluation:   string(dec.Class),
		Score:        dec.Score,
		EvidenceJSON: dec.Evidence,
	}); err != nil {
		if w.log != nil {
			w.log.Warn().Err(err).Int64("id", c.ID).Str("horizon", row.Horizon).Msg("prediction evaluation: upsert failed")
		}
		return
	}
	w.observeEvaluation(string(dec.Class), row.Horizon)
}

// SetEvaluator attaches the v10.3 evaluation persistence seam.
func (w *Worker) SetEvaluator(s EvaluationStore) { w.evaluator = s }

func (w *Worker) observeEvaluation(class, horizon string) {
	if w.met == nil || w.met.PredictionEvaluation == nil {
		return
	}
	w.met.PredictionEvaluation.WithLabelValues(class, horizon).Inc()
}

// directionCorrect maps side_bias + signed delta into a boolean.
// neutral predictions return nil (undecidable).
func directionCorrect(sideBias string, delta float64) *bool {
	switch strings.ToLower(sideBias) {
	case "bullish":
		v := delta > 0
		return &v
	case "bearish":
		v := delta < 0
		return &v
	}
	return nil
}

// horizonLabel converts the duration back to the canonical string
// the migration's CHECK constraint expects.
func horizonLabel(h time.Duration) string {
	switch h {
	case time.Hour:
		return "1h"
	case 6 * time.Hour:
		return "6h"
	case 24 * time.Hour:
		return "24h"
	}
	return ""
}

// --- metric adapters ------------------------------------------------------

func (w *Worker) observeCycle(status string) {
	if w.met == nil || w.met.PredictionFeedbackRuns == nil {
		return
	}
	w.met.PredictionFeedbackRuns.WithLabelValues(status).Inc()
}

func (w *Worker) observePred(status string) {
	if w.met == nil || w.met.PredictionFeedbackProcessed == nil {
		return
	}
	w.met.PredictionFeedbackProcessed.WithLabelValues(status).Inc()
}

func (w *Worker) observeHorizon(label string) {
	if w.met == nil || w.met.PredictionFeedbackHorizons == nil {
		return
	}
	w.met.PredictionFeedbackHorizons.WithLabelValues(label).Inc()
}
