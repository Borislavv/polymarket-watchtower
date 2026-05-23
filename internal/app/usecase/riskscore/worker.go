// Package riskscore computes deterministic resolution-ambiguity /
// dispute-risk scores and persists them to
// polymarket_market_risk_scores. The score itself is delegated to
// internal/app/usecase/analytics/rulesrisk (pure detector); this
// worker is the orchestration layer.
//
// Failure semantics: per-market failure is logged + counted; the
// batch continues. Worker NEVER touches Telegram or AI.
package riskscore

import (
	"context"
	"sync"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/analytics/rulesrisk"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/metrics"
)

// MarketFacts is the raw text-bearing payload the worker pulls
// from the markets repository and feeds to rulesrisk.
type MarketFacts struct {
	ConditionID      string
	Title            string
	Description      string
	ResolutionRules  string
	CatalystKindHint string
}

// MarketsLister returns the next batch of markets due for scoring.
type MarketsLister interface {
	ListRiskScoreCandidates(ctx context.Context, limit int, refreshOlderThan time.Duration) ([]MarketFacts, error)
}

// RiskSink persists one risk row.
type RiskSink interface {
	UpsertRiskScore(ctx context.Context, row RiskRow) error
}

// RiskRow is the persisted row.
type RiskRow struct {
	ConditionID    string
	ScoreVersion   int
	AmbiguityScore float64
	DisputeRisk    float64
	Reasons        []string
	ComputedAt     time.Time
}

// Logger keeps the worker dependency-free.
type Logger interface {
	Info(msg string, kv ...any)
	Warn(msg string, kv ...any)
	Error(msg string, err error, kv ...any)
}

type noopLogger struct{}

func (noopLogger) Info(string, ...any)         {}
func (noopLogger) Warn(string, ...any)         {}
func (noopLogger) Error(string, error, ...any) {}

// Config controls the worker.
type Config struct {
	Enabled      bool
	Interval     time.Duration
	BatchSize    int
	ScoreVersion int
	RefreshOlder time.Duration
}

// Worker computes + persists rules-risk scores.
type Worker struct {
	cfg      Config
	markets  MarketsLister
	sink     RiskSink
	detector *rulesrisk.Detector
	met      *metrics.Metrics
	log      Logger
	clock    func() time.Time
	mu       sync.Mutex
}

func New(cfg Config, markets MarketsLister, sink RiskSink, detector *rulesrisk.Detector, met *metrics.Metrics, log Logger) *Worker {
	if log == nil {
		log = noopLogger{}
	}
	return &Worker{
		cfg:      cfg,
		markets:  markets,
		sink:     sink,
		detector: detector,
		met:      met,
		log:      log,
		clock:    time.Now,
	}
}

func (w *Worker) WithClock(fn func() time.Time) *Worker {
	if fn != nil {
		w.clock = fn
	}
	return w
}

func (w *Worker) Run(ctx context.Context) {
	if !w.cfg.Enabled {
		return
	}
	t := time.NewTicker(w.cfg.Interval)
	defer t.Stop()
	w.Tick(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.Tick(ctx)
		}
	}
}

// Tick scores one batch.
func (w *Worker) Tick(ctx context.Context) {
	start := w.clock()
	defer func() {
		if r := recover(); r != nil {
			w.observeRun("panic")
			w.log.Error("riskscore: panic", nil, "panic", r)
		}
		w.observeLatency(time.Since(start))
	}()

	if w.markets == nil || w.sink == nil || w.detector == nil {
		w.observeRun("skipped")
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()

	facts, err := w.markets.ListRiskScoreCandidates(ctx, w.cfg.BatchSize, w.cfg.RefreshOlder)
	if err != nil {
		w.observeRun("failed")
		w.log.Error("riskscore: list failed", err)
		return
	}
	if len(facts) == 0 {
		w.observeRun("empty")
		return
	}

	var persisted, errored int
	now := w.clock()
	for _, f := range facts {
		in := rulesrisk.Input{
			ConditionID:      f.ConditionID,
			Title:            f.Title,
			Description:      f.Description,
			ResolutionRules:  f.ResolutionRules,
			CatalystKindHint: f.CatalystKindHint,
		}
		v := w.detector.Decide(in)
		row := RiskRow{
			ConditionID:    f.ConditionID,
			ScoreVersion:   w.cfg.ScoreVersion,
			AmbiguityScore: v.AmbiguityScore,
			DisputeRisk:    v.DisputeRisk,
			Reasons:        v.Reasons,
			ComputedAt:     now,
		}
		if err := w.sink.UpsertRiskScore(ctx, row); err != nil {
			errored++
			w.log.Warn("riskscore: upsert failed", "condition_id", f.ConditionID, "err", err)
			continue
		}
		persisted++
	}
	w.observeItems(persisted, errored)
	w.observeRun("ok")
}

func (w *Worker) observeRun(status string) {
	if w.met == nil || w.met.StrategyWorkerRuns == nil {
		return
	}
	w.met.StrategyWorkerRuns.WithLabelValues("riskscore", status).Inc()
}

func (w *Worker) observeItems(persisted, errored int) {
	if w.met == nil || w.met.StrategyWorkerItems == nil {
		return
	}
	if persisted > 0 {
		w.met.StrategyWorkerItems.WithLabelValues("riskscore", "persisted").Add(float64(persisted))
	}
	if errored > 0 {
		w.met.StrategyWorkerItems.WithLabelValues("riskscore", "errored").Add(float64(errored))
	}
}

func (w *Worker) observeLatency(d time.Duration) {
	if w.met == nil || w.met.StrategyWorkerLatency == nil {
		return
	}
	w.met.StrategyWorkerLatency.WithLabelValues("riskscore").Observe(d.Seconds())
}
