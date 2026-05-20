// Package evolution is the heartbeat of the prediction engine.
//
// Every Interval (default 15m) the Worker:
//
//  1. Selects active predictions whose last_evolved_at exceeds the
//     refresh-eligibility age, ordered by state priority (blocked /
//     active_catalyst first, then repricing / confirmed /
//     contradicted / watching / already_priced).
//  2. For each prediction (bounded fan-out), refreshes the
//     deterministic intelligence layers:
//     - event-page snapshot (annotations + market pricing)
//     - active catalysts
//     - repricing signal per recent annotation
//     - event flow summary (alerts + trades)
//     - re-runs the alert/prediction matcher
//  3. Computes the deterministic state transition via
//     marketprediction.Decide and persists the change.
//  4. Applies confidence decay when nothing material changed.
//  5. Conditionally invokes the AI thesis-refresh path (gated by
//     MeaningfulChange + AIMinInterval + AIMaxPerRun).
//  6. Sends a "PREDICTION UPDATE" Telegram message only when a
//     material change occurred and the per-prediction cooldown has
//     elapsed.
//
// Failure isolation: per-prediction panics / errors NEVER stop the
// batch. The alertsender path is fully decoupled — this worker is
// purely intelligence-layer work.
package evolution

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/eventflow"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/eventpagecontext"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/marketprediction"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/repricing"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/analysis"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/aibudget"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/alerting"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/metrics"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/repository"
)

// Config tunes the worker.
type Config struct {
	Enabled     bool
	Interval    time.Duration
	BatchSize   int
	Concurrency int
	Timeout     time.Duration

	AIEnabled     bool
	AIMinInterval time.Duration
	AIMaxPerRun   int

	StaleAfter    time.Duration
	DecayEnabled  bool
	DecayPerDay   float64
	MinConfidence float64

	MajorPriceMove     float64
	CatalystNearWindow time.Duration

	SendTelegram     bool
	TelegramCooldown time.Duration
	TelegramChatID   string

	// Clock + clock-dependent overrides for tests.
	Clock func() time.Time
}

func (c *Config) applyDefaults() {
	if c.Interval <= 0 {
		c.Interval = 15 * time.Minute
	}
	if c.BatchSize <= 0 {
		c.BatchSize = 100
	}
	if c.Concurrency <= 0 {
		c.Concurrency = 4
	}
	if c.Timeout <= 0 {
		c.Timeout = 60 * time.Second
	}
	if c.AIMinInterval <= 0 {
		c.AIMinInterval = 6 * time.Hour
	}
	if c.AIMaxPerRun <= 0 {
		c.AIMaxPerRun = 10
	}
	if c.StaleAfter <= 0 {
		c.StaleAfter = 24 * time.Hour
	}
	if c.DecayPerDay <= 0 {
		c.DecayPerDay = 0.15
	}
	if c.MinConfidence <= 0 {
		c.MinConfidence = 0.10
	}
	if c.MajorPriceMove <= 0 {
		c.MajorPriceMove = 0.08
	}
	if c.CatalystNearWindow <= 0 {
		c.CatalystNearWindow = 12 * time.Hour
	}
	if c.TelegramCooldown <= 0 {
		c.TelegramCooldown = 6 * time.Hour
	}
	if c.Clock == nil {
		c.Clock = time.Now
	}
}

// PredictionStore is the seam to the predictions repository.
type PredictionStore interface {
	ListPredictionsForEvolution(ctx context.Context, maxAge time.Time, limit int32) ([]repository.MarketPrediction, error)
	UpsertPrediction(ctx context.Context, p repository.NewMarketPrediction) (int64, error)
	RecordStateTransition(ctx context.Context, t repository.NewMarketPredictionStateTransition) error
	TouchPredictionEvolution(ctx context.Context, id int64) error
	ApplyPredictionDecay(ctx context.Context, id int64, delta, floor float64, reason string) error
	ListRepricingSignals(ctx context.Context, eventSlug string, limit int32) ([]repository.RepricingSignal, error)
	UpsertRepricingSignal(ctx context.Context, s repository.NewRepricingSignal) error
}

// CatalystSource exposes active catalyst rows per event_slug.
type CatalystSource interface {
	ListActive(ctx context.Context, eventSlug string) ([]repository.EventCatalyst, error)
}

// EventPageRefresher refreshes event-page payloads.
type EventPageRefresher interface {
	Load(ctx context.Context, eventSlug string, sev eventpagecontext.Severity) eventpagecontext.Summary
}

// FlowLoader is the deterministic event-flow aggregator.
type FlowLoader interface {
	LoadEventFlowSummary(ctx context.Context, eventSlug string, lookback time.Duration) (eventflow.EventFlowSummary, error)
}

// RepricingComputer is the deterministic repricing-signal computer.
type RepricingComputer interface {
	Compute(ctx context.Context, in repricing.AnnotationInput, persist bool) (repricing.Signal, error)
}

// Telegram is the delivery seam (matches dailypoliticalintel pattern).
type Telegram interface {
	SendHTML(ctx context.Context, chatID, text string) (TelegramResult, error)
}

// TelegramResult mirrors infra/telegram.SendResult.
type TelegramResult struct {
	MessageID int64
}

// BudgetGuard is the subset of *aibudget.Manager the evolution
// worker uses. Defined here so the evolution package doesn't take
// a hard dependency on the budget package internals; nil is allowed
// (fail-open).
type BudgetGuard interface {
	Allow(bucket string, estCost float64) (bool, string)
	Charge(bucket string, actualCost float64)
}

// Worker is the long-running evolution loop.
type Worker struct {
	cfg         Config
	predictions PredictionStore
	pages       EventPageRefresher
	catalysts   CatalystSource
	flow        FlowLoader
	repricing   RepricingComputer
	aiGenerator analysis.PredictionEvolutionGenerator
	budget      BudgetGuard
	tg          Telegram
	metrics     *metrics.Metrics
	log         *zerolog.Logger

	// lastTelegramAt tracks per-prediction cooldown so a flapping
	// state doesn't spam Telegram. In-memory; restart resets — a
	// freshly-started worker may emit one initial post per flapping
	// row, which is acceptable.
	mu             sync.Mutex
	lastTelegramAt map[int64]time.Time
}

// estPerEvolutionRefreshUSD is the conservative pre-flight estimate
// per thesis-refresh AI call. ~12k input + 2k output tokens at
// gpt-4.1 rates ($2/$8 per million) = ~$0.040. Charge() after the
// call corrects with the real token count.
const estPerEvolutionRefreshUSD = 0.05

// New wires the worker.
func New(
	cfg Config,
	predictions PredictionStore,
	pages EventPageRefresher,
	catalysts CatalystSource,
	flow FlowLoader,
	repricingComp RepricingComputer,
	aiGen analysis.PredictionEvolutionGenerator,
	tg Telegram,
	met *metrics.Metrics,
	log *zerolog.Logger,
) *Worker {
	cfg.applyDefaults()
	return &Worker{
		cfg:            cfg,
		predictions:    predictions,
		pages:          pages,
		catalysts:      catalysts,
		flow:           flow,
		repricing:      repricingComp,
		aiGenerator:    aiGen,
		tg:             tg,
		metrics:        met,
		log:            log,
		lastTelegramAt: map[int64]time.Time{},
	}
}

// SetBudget attaches the global AI budget guard. nil is allowed
// (fail-open). Called once at startup from app.go.
func (w *Worker) SetBudget(b BudgetGuard) { w.budget = b }

// Run blocks until ctx cancels, ticking every Interval. Immediate
// first tick on startup so a freshly-deployed worker doesn't sit
// idle for one Interval before evolving.
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

// Tick runs one cycle. Exposed for the CLI dry-run.
func (w *Worker) Tick(ctx context.Context) Summary {
	start := w.cfg.Clock()
	defer func() { w.observeLatency(time.Since(start)) }()

	// Selection: predictions whose last_evolved_at is older than
	// now - Interval/2. The half-interval guard avoids re-processing
	// rows we just touched, even if the Interval timer drifts.
	maxAge := w.cfg.Clock().Add(-w.cfg.Interval / 2)
	rows, err := w.predictions.ListPredictionsForEvolution(ctx, maxAge, int32(w.cfg.BatchSize))
	if err != nil {
		w.observeRun("failed")
		if w.log != nil {
			w.log.Err(err).Msg("prediction evolution: list failed")
		}
		return Summary{Error: err}
	}
	w.observeSelected(len(rows))
	if w.log != nil {
		w.log.Info().Int("selected", len(rows)).Msg("prediction evolution: cycle started")
	}

	sem := make(chan struct{}, w.cfg.Concurrency)
	var wg sync.WaitGroup
	var aiBudget atomicInt
	aiBudget.Set(int32(w.cfg.AIMaxPerRun))

	summary := Summary{
		StartedAt: start,
		Selected:  len(rows),
		Results:   make([]EvolutionResult, 0, len(rows)),
	}
	var summaryMu sync.Mutex

	for _, pred := range rows {
		pred := pred
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			defer func() {
				if r := recover(); r != nil && w.log != nil {
					w.log.Error().Interface("panic", r).Int64("id", pred.ID).Msg("prediction evolution: panic recovered")
				}
			}()
			ctxOne, cancel := context.WithTimeout(ctx, w.cfg.Timeout)
			defer cancel()
			res := w.processOne(ctxOne, pred, &aiBudget, false)
			summaryMu.Lock()
			summary.Results = append(summary.Results, res)
			summaryMu.Unlock()
			w.observeProcessed(res.Status)
		}()
	}
	wg.Wait()
	summary.FinishedAt = w.cfg.Clock()
	w.observeRun("ok")
	if w.log != nil {
		w.log.Info().Int("processed", len(summary.Results)).Dur("duration", summary.FinishedAt.Sub(start)).
			Msg("prediction evolution: cycle completed")
	}
	return summary
}

// TickOne processes exactly one prediction by id — used by the CLI
// dry-run command. Returns the EvolutionResult for printing.
func (w *Worker) TickOne(ctx context.Context, pred repository.MarketPrediction, dryRun bool) EvolutionResult {
	var budget atomicInt
	budget.Set(int32(w.cfg.AIMaxPerRun))
	return w.processOne(ctx, pred, &budget, dryRun)
}

// EvolutionResult is the per-prediction outcome for one cycle.
type EvolutionResult struct {
	PredictionID    int64
	EventSlug       string
	ConditionID     string
	OldState        string
	NewState        string
	StateChanged    bool
	RepricingStatus string
	FlowEmpty       bool
	StrongestSide   string
	MatchedAlerts   int
	AIRefreshed     bool
	AISkipReason    string
	DecayApplied    bool
	TelegramSent    bool
	Status          string // ok / failed / skipped
	Error           error
}

// Summary is the cycle-level outcome.
type Summary struct {
	StartedAt  time.Time
	FinishedAt time.Time
	Selected   int
	Results    []EvolutionResult
	Error      error
}

// processOne is the per-prediction orchestrator. Failures are
// per-prediction; siblings keep running. Returns the result struct
// for telemetry + CLI rendering.
func (w *Worker) processOne(ctx context.Context, pred repository.MarketPrediction, aiBudget *atomicInt, dryRun bool) EvolutionResult {
	res := EvolutionResult{
		PredictionID: pred.ID,
		EventSlug:    pred.EventSlug,
		ConditionID:  pred.ConditionID,
		OldState:     pred.CurrentState,
		Status:       "ok",
	}

	// 1. Refresh event-page payload (annotations + markets).
	pageSummary := w.pages.Load(ctx, pred.EventSlug, eventpagecontext.SeverityInfo)
	res.FlowEmpty = true

	// 2. Active catalysts.
	cats, _ := w.catalysts.ListActive(ctx, pred.EventSlug)

	// 3. Flow summary.
	flowSum, _ := w.flow.LoadEventFlowSummary(ctx, pred.EventSlug, 24*time.Hour)
	res.FlowEmpty = flowSum.Empty()
	res.StrongestSide = flowSum.StrongestSide

	// 4. Repricing — recompute the freshest signal per latest
	// annotation; the renderer + state machine consume only the
	// newest one. We pick the latest annotation whose price_after
	// is present (the move was observed).
	var topSignal *repricing.Signal
	var newestAnn *repository.EventAnnotation
	for i := range pageSummary.Annotations {
		ann := pageSummary.Annotations[i]
		if ann.PriceAfter == nil {
			continue
		}
		if newestAnn == nil || ann.Timestamp.After(newestAnn.Timestamp) {
			newestAnn = &pageSummary.Annotations[i]
		}
	}
	if newestAnn != nil {
		var current *float64
		// Use the market's latest reference price from the event-page
		// snapshot for the matching condition_id.
		for _, m := range pageSummary.Markets {
			if m.ConditionID == pred.ConditionID && m.LastTradePrice != nil {
				current = m.LastTradePrice
				break
			}
		}
		sig, _ := w.repricing.Compute(ctx, repricing.AnnotationInput{
			EventSlug:      pred.EventSlug,
			ConditionID:    pred.ConditionID,
			Outcome:        pred.Outcome,
			AnnotationHash: newestAnn.ItemHash,
			Title:          newestAnn.Title,
			Timestamp:      newestAnn.Timestamp,
			PriceBefore:    newestAnn.PriceBefore,
			PriceAfter:     newestAnn.PriceAfter,
			PriceChange:    newestAnn.PriceChange,
			CurrentPrice:   current,
		}, !dryRun)
		topSignal = &sig
		res.RepricingStatus = sig.RepricingStatus
	}

	// 5. Matched alerts — replayed from flow summary's TopAlerts.
	// The deterministic scorer reads identity + side; we lift the
	// recent alerts into AlertCandidate shape.
	matches := w.scoreMatches(pred, flowSum)
	res.MatchedAlerts = len(matches)

	// 6. Deterministic state transition.
	inputs := marketprediction.Inputs{
		Now:             w.cfg.Clock(),
		Prediction:      pred,
		ActiveCatalysts: cats,
		RepricingSignal: topSignal,
		FlowSummary:     flowSum,
		MatchedAlerts:   matches,
	}
	stateCfg := marketprediction.Config{
		StaleAfter:              w.cfg.StaleAfter,
		ConfirmAlertScoreFloor:  0.6,
		ContradictFlowImbalance: 0.65,
	}
	dec := marketprediction.Decide(inputs, stateCfg)
	res.NewState = dec.NewState
	res.StateChanged = dec.Changed

	// 7. Persist transition (skip in dry-run).
	if !dryRun && dec.Changed {
		newRow := repository.NewMarketPrediction{
			EventSlug:    pred.EventSlug,
			ConditionID:  pred.ConditionID,
			Outcome:      pred.Outcome,
			SideBias:     pred.SideBias,
			Summary:      pred.Summary,
			CurrentState: dec.NewState,
			StateReason:  dec.Reason,
			Confidence:   pred.Confidence,
		}
		// Stamp the right "last X at" depending on the new state.
		now := w.cfg.Clock()
		switch dec.NewState {
		case marketprediction.StateConfirmedByFlow:
			newRow.LastConfirmedByAlertAt = now
		case marketprediction.StateContradictedByFlow:
			newRow.LastContradictedByAlertAt = now
		case marketprediction.StateRepricing, marketprediction.StateAlreadyPriced:
			newRow.LastRepricedAt = now
		}
		id, err := w.predictions.UpsertPrediction(ctx, newRow)
		if err == nil {
			_ = w.predictions.RecordStateTransition(ctx, repository.NewMarketPredictionStateTransition{
				PredictionID:  id,
				PreviousState: dec.PreviousState,
				NewState:      dec.NewState,
				Reason:        dec.Reason,
				EvidenceJSON:  dec.EvidenceJSON,
			})
			w.observeStateChange(dec.PreviousState, dec.NewState)
		} else if w.log != nil {
			w.log.Warn().Err(err).Int64("id", pred.ID).Msg("prediction evolution: upsert failed")
		}
	}

	// 8. Decay — only when nothing material changed AND configured on.
	if !dryRun && !dec.Changed && w.cfg.DecayEnabled {
		if w.shouldDecay(pred, flowSum, dec) {
			delta := w.decayPerCycle()
			_ = w.predictions.ApplyPredictionDecay(ctx, pred.ID, delta, w.cfg.MinConfidence,
				fmt.Sprintf("deterministic decay −%.3f (no fresh evidence)", delta))
			w.observeDecay(dec.NewState)
			res.DecayApplied = true
		}
	}

	// 9. AI refresh gating.
	aiReason := w.aiSkipReason(pred, dec, topSignal, flowSum)
	if aiReason == "" && aiBudget.Add(-1) >= 0 {
		if !dryRun {
			res.AIRefreshed = w.refreshAI(ctx, pred, dec, pageSummary, cats, topSignal, flowSum, matches)
		} else {
			res.AIRefreshed = true // would-have-refreshed
		}
	} else {
		res.AISkipReason = aiReason
		if aiReason == "" {
			res.AISkipReason = "ai_budget_exhausted"
		}
		w.observeAISkipped(res.AISkipReason)
	}

	// 10. Telegram update (gated on meaningful change + cooldown).
	if !dryRun && w.cfg.SendTelegram && w.tg != nil && w.shouldTelegram(pred, dec) {
		text := RenderEvolutionUpdate(EvolutionRenderInput{
			OldState:    dec.PreviousState,
			NewState:    dec.NewState,
			Reason:      dec.Reason,
			MarketTitle: pred.Summary,
			ConditionID: pred.ConditionID,
			Repricing:   topSignal,
			Flow:        &flowSum,
			Catalysts:   cats,
			Matched:     matches,
			AIText:      "", // The AI refresh stores its own audit row in
			// the analyses table downstream; for the evolution
			// Telegram body we keep it deterministic for the MVP.
		})
		// SafeSplit caps each chunk at Telegram's 4000-char limit
		// while preserving HTML tag pairs. Short bodies are passed
		// through as a single chunk; long catalyst-dense bodies are
		// split on paragraph boundaries.
		chunks := alerting.SafeSplitForTelegram(text)
		anySent := false
		for _, chunk := range chunks {
			if _, err := w.tg.SendHTML(ctx, w.cfg.TelegramChatID, chunk); err != nil {
				w.observeTelegram("failed")
				if w.log != nil {
					w.log.Warn().Err(err).Int64("id", pred.ID).Msg("prediction evolution: telegram send failed")
				}
				break
			}
			anySent = true
		}
		if anySent {
			w.observeTelegram("sent")
			res.TelegramSent = true
			w.mu.Lock()
			w.lastTelegramAt[pred.ID] = w.cfg.Clock()
			w.mu.Unlock()
		}
	}

	// 11. Mark as evolved this cycle (even when nothing changed).
	if !dryRun {
		if err := w.predictions.TouchPredictionEvolution(ctx, pred.ID); err != nil && w.log != nil {
			w.log.Warn().Err(err).Int64("id", pred.ID).Msg("prediction evolution: touch failed")
		}
	}
	return res
}

// scoreMatches replays the flow summary's TopAlerts through the
// deterministic match scorer + retains scores above 0.5.
func (w *Worker) scoreMatches(pred repository.MarketPrediction, flow eventflow.EventFlowSummary) []marketprediction.MatchedAlert {
	predRef := marketprediction.PredictionRef{
		EventSlug:   pred.EventSlug,
		ConditionID: pred.ConditionID,
		Outcome:     pred.Outcome,
		SideBias:    pred.SideBias,
		CreatedAt:   pred.UpdatedAt,
	}
	out := make([]marketprediction.MatchedAlert, 0, len(flow.TopAlerts))
	for _, a := range flow.TopAlerts {
		cand := marketprediction.AlertCandidate{
			AlertID:     a.ID,
			Kind:        anomalyKind(a.Kind),
			ConditionID: a.ConditionID,
			EventSlug:   pred.EventSlug,
			Outcome:     pred.Outcome,
			At:          a.CreatedAt,
		}
		// Severity passes through verbatim — the match-scorer pads
		// confidence by severity.
		switch a.Severity {
		case "info", "warning", "critical", "hard":
			cand.Severity = anomalySeverity(a.Severity)
		}
		m := marketprediction.Score(cand, predRef)
		if m.Score >= 0.5 {
			out = append(out, m)
		}
	}
	// Sort by score desc for the renderer + state machine.
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	for _, m := range out {
		w.observeMatch(m.DirectionAlignment)
	}
	return out
}

// aiSkipReason returns "" when AI refresh is allowed, otherwise a
// short reason string. Gating per PART 5.
func (w *Worker) aiSkipReason(pred repository.MarketPrediction, dec marketprediction.Decision, sig *repricing.Signal, flow eventflow.EventFlowSummary) string {
	if !w.cfg.AIEnabled {
		return "ai_disabled"
	}
	// Allowed conditions — ANY one suffices.
	if dec.Changed {
		return ""
	}
	if sig != nil && (sig.RepricingStatus == repricing.StatusUnderreacting ||
		sig.RepricingStatus == repricing.StatusOverreacting ||
		sig.RepricingStatus == repricing.StatusReversed) {
		return ""
	}
	// Strong matched alert.
	if flow.RecentAlerts >= 3 && flow.HardAlerts+flow.CriticalAlerts >= 1 {
		return ""
	}
	// No prior AI thesis on record (Summary is empty).
	if strings.TrimSpace(pred.Summary) == "" {
		return ""
	}
	// AI staleness.
	if pred.UpdatedAt.IsZero() || w.cfg.Clock().Sub(pred.UpdatedAt) >= w.cfg.AIMinInterval {
		return ""
	}
	return "no_meaningful_change"
}

// refreshAI calls the AI generator and stores the new thesis as the
// prediction Summary. Returns true on success.
func (w *Worker) refreshAI(
	ctx context.Context,
	pred repository.MarketPrediction,
	dec marketprediction.Decision,
	page eventpagecontext.Summary,
	catalysts []repository.EventCatalyst,
	sig *repricing.Signal,
	flow eventflow.EventFlowSummary,
	matched []marketprediction.MatchedAlert,
) bool {
	// Budget governor — block when today's evolution bucket (or
	// global) is exhausted. State + decay still applied above; only
	// the AI thesis refresh is skipped, exactly as if the gating
	// layer had decided "no_strong_alerts".
	if w.budget != nil {
		if ok, reason := w.budget.Allow(aibudget.BucketPredictionEvolve, estPerEvolutionRefreshUSD); !ok {
			w.observeAISkipped(reason)
			if w.log != nil {
				w.log.Warn().Int64("id", pred.ID).Str("reason", reason).Msg("prediction evolution: AI denied by budget")
			}
			return false
		}
	}
	req := analysis.PredictionEvolutionRequest{
		EventSlug:          pred.EventSlug,
		ConditionID:        pred.ConditionID,
		PreviousPrediction: pred.Summary,
		PredictionState:    dec.NewState,
		StateReason:        dec.Reason,
		MarketSnapshot:     renderMarketSnapshot(pred, page),
		AnnotationsBlock:   renderAnnotationsBlock(page),
		CatalystsBlock:     renderCatalystsBlock(catalysts),
		RepricingBlock:     renderRepricingBlock(sig),
		FlowSummaryBlock:   flow.RenderPromptBlock(),
		MatchedAlertsBlock: renderMatchedAlertsBlock(matched),
	}
	res, err := w.aiGenerator.RefreshPredictionThesis(ctx, req)
	if err != nil || res.Status != analysis.StatusOK || strings.TrimSpace(res.ThesisUpdate) == "" {
		w.observeAI("failed")
		if w.log != nil && err != nil {
			w.log.Warn().Err(err).Int64("id", pred.ID).Msg("prediction evolution: AI refresh failed")
		}
		return false
	}
	if w.budget != nil {
		w.budget.Charge(aibudget.BucketPredictionEvolve, res.EstimatedCostUSD)
	}
	w.observeAI("ok")
	// Stamp the new thesis. Upsert preserves state (we don't change
	// state here — the deterministic Decide already did).
	_, _ = w.predictions.UpsertPrediction(ctx, repository.NewMarketPrediction{
		EventSlug:    pred.EventSlug,
		ConditionID:  pred.ConditionID,
		Outcome:      pred.Outcome,
		SideBias:     pred.SideBias,
		Summary:      res.ThesisUpdate,
		CurrentState: dec.NewState,
		StateReason:  dec.Reason,
		Confidence:   pred.Confidence,
	})
	return true
}

// shouldDecay applies the decay rule from PART 6.
func (w *Worker) shouldDecay(pred repository.MarketPrediction, flow eventflow.EventFlowSummary, dec marketprediction.Decision) bool {
	if dec.NewState == marketprediction.StateBlocked ||
		dec.NewState == marketprediction.StateActiveCatalyst ||
		dec.NewState == marketprediction.StateResolved ||
		dec.NewState == marketprediction.StateInvalidated {
		return false
	}
	if !flow.Empty() {
		// Any fresh alert flow → keep confidence.
		return false
	}
	return pred.Confidence > w.cfg.MinConfidence
}

// decayPerCycle scales the daily decay into a per-cycle delta.
func (w *Worker) decayPerCycle() float64 {
	cyclesPerDay := float64(24*time.Hour) / float64(w.cfg.Interval)
	if cyclesPerDay <= 0 {
		cyclesPerDay = 1
	}
	return w.cfg.DecayPerDay / cyclesPerDay
}

// shouldTelegram applies the cooldown + meaningfulness gate from
// PART 8.
func (w *Worker) shouldTelegram(pred repository.MarketPrediction, dec marketprediction.Decision) bool {
	if !dec.Changed {
		return false
	}
	w.mu.Lock()
	last, ok := w.lastTelegramAt[pred.ID]
	w.mu.Unlock()
	if ok && w.cfg.Clock().Sub(last) < w.cfg.TelegramCooldown {
		return false
	}
	return true
}

// --- metric helpers -----------------------------------------------------

func (w *Worker) observeRun(status string) {
	if w.metrics == nil || w.metrics.PredictionEvolutionRuns == nil {
		return
	}
	w.metrics.PredictionEvolutionRuns.WithLabelValues(status).Inc()
}

func (w *Worker) observeSelected(n int) {
	if w.metrics == nil || w.metrics.PredictionEvolutionSelected == nil {
		return
	}
	w.metrics.PredictionEvolutionSelected.Add(float64(n))
}

func (w *Worker) observeProcessed(status string) {
	if w.metrics == nil || w.metrics.PredictionEvolutionProcessed == nil {
		return
	}
	w.metrics.PredictionEvolutionProcessed.WithLabelValues(status).Inc()
}

func (w *Worker) observeStateChange(from, to string) {
	if w.metrics == nil || w.metrics.PredictionEvolutionStateChanges == nil {
		return
	}
	w.metrics.PredictionEvolutionStateChanges.WithLabelValues(from, to).Inc()
}

func (w *Worker) observeAI(status string) {
	if w.metrics == nil || w.metrics.PredictionEvolutionAIRequests == nil {
		return
	}
	w.metrics.PredictionEvolutionAIRequests.WithLabelValues(status).Inc()
}

func (w *Worker) observeAISkipped(reason string) {
	if w.metrics == nil || w.metrics.PredictionEvolutionAISkipped == nil {
		return
	}
	w.metrics.PredictionEvolutionAISkipped.WithLabelValues(reason).Inc()
}

func (w *Worker) observeTelegram(status string) {
	if w.metrics == nil || w.metrics.PredictionEvolutionTelegram == nil {
		return
	}
	w.metrics.PredictionEvolutionTelegram.WithLabelValues(status).Inc()
}

func (w *Worker) observeLatency(d time.Duration) {
	if w.metrics == nil || w.metrics.PredictionEvolutionLatency == nil {
		return
	}
	w.metrics.PredictionEvolutionLatency.Observe(d.Seconds())
}

func (w *Worker) observeDecay(state string) {
	if w.metrics == nil || w.metrics.PredictionEvolutionDecay == nil {
		return
	}
	w.metrics.PredictionEvolutionDecay.WithLabelValues(state).Inc()
}

func (w *Worker) observeMatch(alignment string) {
	if w.metrics == nil || w.metrics.MarketPredictionMatches == nil {
		return
	}
	w.metrics.MarketPredictionMatches.WithLabelValues(alignment).Inc()
}

// --- tiny atomic int ---------------------------------------------------

type atomicInt struct {
	mu sync.Mutex
	v  int32
}

func (a *atomicInt) Set(v int32) {
	a.mu.Lock()
	a.v = v
	a.mu.Unlock()
}

func (a *atomicInt) Add(delta int32) int32 {
	a.mu.Lock()
	a.v += delta
	out := a.v
	a.mu.Unlock()
	return out
}

// --- error helper -------------------------------------------------------

var errPredictionFailed = errors.New("prediction evolution failed")
