// Package create is the prediction-creation worker. Without this
// loop the v9.9 evolution worker has nothing to evolve — predictions
// were "born and forgotten" because the cold-start path never
// existed. The PART 1 v10.0 spec calls this the missing fuel
// injector of the engine.
//
// Architecture (deterministic-first, AI second):
//
//  1. Pull intelligence candidates from the existing
//     MarketIntelligenceRepository.ListIntelligenceCandidates query.
//     Apply category whitelist + signal-density filter to shortlist
//     ~15–25 markets out of 150.
//  2. Drop candidates that already have an active prediction or
//     fall inside the per-event dedupe window (default 24h).
//  3. Stage 1 AI: call PredictionRanker with SHORT candidate
//     summaries. The model returns the top {{MaxSelected}} markets
//     worth a full deep-dive.
//  4. Stage 2 AI: for each ranker pick, build the full prediction
//     context (annotations, catalysts, repricing, flow, matched
//     alerts) and call PredictionCreator. The response is parsed,
//     normalised, and persisted.
//  5. Persist via the same Applier the evolution worker uses, so
//     the state machine + audit trail stay consistent.
//
// Failure isolation: per-candidate panics / errors NEVER stop the
// batch. AI budget governor short-circuits the cycle when the
// prediction_creation bucket runs out; deterministic refresh of
// event-page snapshots still happens.
//
// Per-day cap: MaxPerDay caps how many new predictions can be
// created in a UTC day. Counted from the DB (cheap COUNT against
// the partial index).
package create

import (
	"context"
	"fmt"
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

// Config tunes the worker. Defaults are moderate + safe.
type Config struct {
	Enabled      bool
	Interval     time.Duration
	BatchSize    int           // upper bound on candidates pulled before filtering
	MaxSelected  int           // AI ranker output cap → max creations per cycle
	MaxPerDay    int           // per-day creation cap (UTC midnight reset)
	MinScore     float64       // ranker score floor (0..1) — picks below are dropped
	DedupeWindow time.Duration // per-event minimum gap between creations
	AIEnabled    bool
	AITimeout    time.Duration
	SendTelegram bool
	Concurrency  int
	Categories   []string // category whitelist (case-insensitive substring on category label)

	// --- v10.1 Telegram polish (PART 1/3/5/7) -----------------
	// Annotations block (under AI thesis, above Links).
	AnnotationsEnabled        bool
	AnnotationsLimit          int
	AnnotationsMaxTitleChars  int
	AnnotationsMaxSourceNames int
	// Links block (under annotations).
	LinksEnabled   bool
	PolymarketBase string // e.g. "https://polymarket.com"
	GrafanaBaseURL string // empty → Grafana link elided
	GrafanaDashUID string // dashboard uid for deep-linking
	// Telegram throttling.
	TelegramCooldown  time.Duration
	MaxTelegramPerRun int
	SendOnStartup     bool
	// Quality gate.
	SendNeutral       bool
	PersistLowQuality bool
	MinConfidence     float64
	RequireSignal     bool
	MinSummaryChars   int

	Clock func() time.Time
}

func (c *Config) applyDefaults() {
	if c.Interval <= 0 {
		c.Interval = 30 * time.Minute
	}
	if c.BatchSize <= 0 {
		c.BatchSize = 150
	}
	if c.MaxSelected <= 0 {
		c.MaxSelected = 10
	}
	if c.MaxPerDay <= 0 {
		c.MaxPerDay = 40
	}
	if c.MinScore <= 0 {
		c.MinScore = 0.55
	}
	if c.DedupeWindow <= 0 {
		c.DedupeWindow = 24 * time.Hour
	}
	if c.AITimeout <= 0 {
		c.AITimeout = 60 * time.Second
	}
	if c.Concurrency <= 0 {
		c.Concurrency = 2
	}
	if len(c.Categories) == 0 {
		c.Categories = []string{"politics", "geopolitics", "elections"}
	}
	// v10.1 Telegram polish defaults.
	if c.AnnotationsLimit <= 0 {
		c.AnnotationsLimit = 5
	}
	if c.AnnotationsMaxTitleChars <= 0 {
		c.AnnotationsMaxTitleChars = 160
	}
	if c.AnnotationsMaxSourceNames <= 0 {
		c.AnnotationsMaxSourceNames = 3
	}
	if c.PolymarketBase == "" {
		c.PolymarketBase = "https://polymarket.com"
	}
	if c.TelegramCooldown <= 0 {
		c.TelegramCooldown = 6 * time.Hour
	}
	if c.MaxTelegramPerRun <= 0 {
		c.MaxTelegramPerRun = 3
	}
	if c.MinConfidence < 0 {
		c.MinConfidence = 0
	}
	if c.MinSummaryChars <= 0 {
		c.MinSummaryChars = 300
	}
	if c.Clock == nil {
		c.Clock = time.Now
	}
}

// CandidateSource is the seam to the existing intelligence-candidate
// query. *repository.MarketIntelligenceRepository satisfies it.
type CandidateSource interface {
	ListIntelligenceCandidates(ctx context.Context, limit int32) ([]repository.IntelligenceCandidate, error)
}

// MarketResolver maps conditionID → market row so we can attach
// event_slug + category + start/end dates to the candidate.
type MarketResolver interface {
	GetByConditionID(ctx context.Context, conditionID string) (repository.Market, error)
}

// PredictionStore is the persistence seam (subset of
// *repository.RepricingPredictionsRepository).
type PredictionStore interface {
	GetPrediction(ctx context.Context, eventSlug, conditionID string) (repository.MarketPrediction, error)
	UpsertPrediction(ctx context.Context, p repository.NewMarketPrediction) (int64, error)
	RecordStateTransition(ctx context.Context, t repository.NewMarketPredictionStateTransition) error
	CountPredictionsCreatedSince(ctx context.Context, since time.Time) (int64, error)
	CountPredictionsForEventSince(ctx context.Context, eventSlug string, since time.Time) (int64, error)
	TouchPredictionEvolution(ctx context.Context, id int64) error
}

// EventPageRefresher is the seam to refresh annotations + per-market
// snapshot before we generate the thesis.
type EventPageRefresher interface {
	Load(ctx context.Context, eventSlug string, sev eventpagecontext.Severity) eventpagecontext.Summary
}

// CatalystSource serves the catalysts block in the thesis context.
// Mirrors the evolution worker's seam shape.
type CatalystSource interface {
	ListActive(ctx context.Context, eventSlug string) ([]repository.EventCatalyst, error)
}

// FlowLoader returns the deterministic event-flow summary. Mirrors
// the evolution worker's seam shape.
type FlowLoader interface {
	LoadEventFlowSummary(ctx context.Context, eventSlug string, lookback time.Duration) (eventflow.EventFlowSummary, error)
}

// RepricingComputer computes the repricing signal for one
// annotation. Optional; nil disables the repricing context block.
// Same shape as the evolution worker uses.
type RepricingComputer interface {
	Compute(ctx context.Context, in repricing.AnnotationInput, persist bool) (repricing.Signal, error)
}

// BudgetGuard mirrors evolution.BudgetGuard. nil = fail-open.
type BudgetGuard interface {
	Allow(bucket string, estCost float64) (bool, string)
	Charge(bucket string, actualCost float64)
}

// Telegram is the seam the worker uses for "PREDICTION CREATED"
// notifications. nil disables Telegram.
type Telegram interface {
	SendHTML(ctx context.Context, body string) (int64, error)
}

// Worker is the long-running creation loop.
type Worker struct {
	cfg         Config
	candidates  CandidateSource
	markets     MarketResolver
	predictions PredictionStore
	pages       EventPageRefresher
	catalysts   CatalystSource
	flow        FlowLoader
	repricing   RepricingComputer
	ranker      analysis.PredictionRanker
	creator     analysis.PredictionCreator
	budget      BudgetGuard
	tg          Telegram
	met         *metrics.Metrics
	log         *zerolog.Logger

	// startupDone flips after the first Tick — gates the
	// SendOnStartup=false suppression so subsequent cycles can send
	// normally even when the very first cycle was muted.
	tgMu          sync.Mutex
	startupDone   bool
	tgSentThisRun int                  // reset at the top of every Tick
	lastTGSentAt  map[string]time.Time // event_slug → last send (per-event cooldown)
}

// Pre-flight cost estimates (USD) per AI stage. Conservative;
// Charge() after the call uses the real token-based cost so the
// running total stays accurate.
const (
	estPerRankingUSD  = 0.04
	estPerCreationUSD = 0.06
)

// New wires the worker.
func New(
	cfg Config,
	candidates CandidateSource,
	markets MarketResolver,
	predictions PredictionStore,
	pages EventPageRefresher,
	catalysts CatalystSource,
	flow FlowLoader,
	repricingComp RepricingComputer,
	ranker analysis.PredictionRanker,
	creator analysis.PredictionCreator,
	tg Telegram,
	met *metrics.Metrics,
	log *zerolog.Logger,
) *Worker {
	cfg.applyDefaults()
	return &Worker{
		cfg:          cfg,
		candidates:   candidates,
		markets:      markets,
		predictions:  predictions,
		pages:        pages,
		catalysts:    catalysts,
		flow:         flow,
		repricing:    repricingComp,
		ranker:       ranker,
		creator:      creator,
		tg:           tg,
		met:          met,
		log:          log,
		lastTGSentAt: make(map[string]time.Time),
	}
}

// SetBudget attaches the AI budget governor. nil = fail-open.
func (w *Worker) SetBudget(b BudgetGuard) { w.budget = b }

// Run blocks until ctx is done. Immediate first tick so a fresh
// process doesn't wait one full interval to surface predictions.
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

// Summary is the per-cycle result the CLI prints.
type Summary struct {
	Candidates int
	Filtered   int
	Selected   int
	Created    int
	Skipped    map[string]int
	Errors     []string
}

// Tick exposes one cycle for tests + CLI smoke. Always returns a
// Summary (never blocks on errors).
func (w *Worker) Tick(ctx context.Context) Summary {
	start := w.cfg.Clock()
	defer func() { w.observeLatency(time.Since(start)) }()
	// Reset the per-run Telegram counter so MaxTelegramPerRun is an
	// exact per-tick cap, not a cumulative-since-startup one.
	w.resetPerRunCounter()
	// Stamp startupDone AFTER the tick so the very first cycle's
	// canSendTelegram() decisions see startupDone=false.
	defer w.markStartupDone()
	sum := Summary{Skipped: map[string]int{}}

	// Per-day cap check FIRST — cheap; avoids any AI work when full.
	todayStart := w.cfg.Clock().UTC().Truncate(24 * time.Hour)
	created, err := w.predictions.CountPredictionsCreatedSince(ctx, todayStart)
	if err == nil && int(created) >= w.cfg.MaxPerDay {
		w.observeCycle("daily_cap_reached")
		if w.log != nil {
			w.log.Info().Int("created_today", int(created)).Int("cap", w.cfg.MaxPerDay).Msg("prediction creation: daily cap reached, skipping cycle")
		}
		return sum
	}

	cands, err := w.collectCandidates(ctx)
	sum.Candidates = len(cands)
	if err != nil {
		w.observeCycle("candidates_failed")
		sum.Errors = append(sum.Errors, err.Error())
		return sum
	}
	if len(cands) == 0 {
		w.observeCycle("empty")
		return sum
	}
	w.observeCandidates(len(cands))

	dedupedCands := w.filterDedupe(ctx, cands, &sum)
	sum.Filtered = len(dedupedCands)
	if len(dedupedCands) == 0 {
		w.observeCycle("all_deduped")
		return sum
	}

	if !w.cfg.AIEnabled {
		w.observeCycle("ai_disabled")
		return sum
	}

	picks := w.rankWithAI(ctx, dedupedCands, &sum)
	sum.Selected = len(picks)
	if len(picks) == 0 {
		w.observeCycle("no_picks")
		return sum
	}

	// Concurrent thesis generation, bounded fan-out. One panic
	// must not stop the batch (per-pick recover).
	sem := make(chan struct{}, w.cfg.Concurrency)
	var mu sync.Mutex
	var wg sync.WaitGroup
	remaining := w.cfg.MaxPerDay - int(created)
	for _, p := range picks {
		if remaining <= 0 {
			sum.Skipped["daily_cap_hit_mid_cycle"]++
			break
		}
		p := p
		remaining--
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			defer func() {
				if r := recover(); r != nil && w.log != nil {
					w.log.Error().Interface("panic", r).Str("event_slug", p.EventSlug).Msg("prediction creation: per-pick panic recovered")
				}
			}()
			status := w.createOne(ctx, p, dedupedCands)
			mu.Lock()
			if status == "created" {
				sum.Created++
			} else {
				sum.Skipped[status]++
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	w.observeCycle("ok")
	if w.log != nil {
		w.log.Info().
			Int("candidates", sum.Candidates).
			Int("filtered", sum.Filtered).
			Int("selected", sum.Selected).
			Int("created", sum.Created).
			Dur("duration", time.Since(start)).
			Msg("prediction creation: cycle completed")
	}
	return sum
}

// collectCandidates pulls and filters by category. The query already
// orders by signal density (alerts24h + volume + lifecycle); we just
// cap and apply the category whitelist here.
func (w *Worker) collectCandidates(ctx context.Context) ([]analysis.PredictionCandidate, error) {
	rows, err := w.candidates.ListIntelligenceCandidates(ctx, int32(w.cfg.BatchSize))
	if err != nil {
		return nil, fmt.Errorf("list candidates: %w", err)
	}
	out := make([]analysis.PredictionCandidate, 0, len(rows))
	for _, r := range rows {
		if !w.categoryAllowed(r.Category) {
			continue
		}
		mk, mkErr := w.markets.GetByConditionID(ctx, r.ConditionID)
		if mkErr != nil || strings.TrimSpace(mk.EventSlug) == "" {
			continue
		}
		out = append(out, analysis.PredictionCandidate{
			EventSlug:       mk.EventSlug,
			ConditionID:     r.ConditionID,
			Question:        firstNonEmpty(r.Question, mk.Question),
			Category:        r.Category,
			LastTradePrice:  r.LastPrice,
			LifecyclePct:    r.LifecyclePct,
			RecentAlerts24h: int(r.Alerts24h),
			VolumeUSD24h:    r.Volume24hUSD,
			// Per-candidate outcome / strongest side / catalyst count
			// are filled below from the event-page provider; keeping
			// these light here avoids fan-out on dropped candidates.
		})
	}
	return out, nil
}

// filterDedupe drops candidates that would create a redundant
// prediction. The rules are deterministic and the worker logs every
// skip with its reason code so the operator can audit cycles.
//
// Rules in priority order:
//
//  1. active_prediction — a prediction row already exists for this
//     exact (event_slug, condition_id). The evolution worker will
//     refresh it; we don't create a sibling.
//  2. dedupe_window — the same event_slug had a prediction created
//     within DedupeWindow (default 24h), regardless of condition_id
//     or side_bias. Prevents spamming a single event with N
//     predictions for N markets in one cycle.
//
// (1) is stricter than (2). The same event with a DIFFERENT
// condition_id still trips (2) inside the window — by design, so
// "Texas senate primary" doesn't yield 5 simultaneous predictions.
func (w *Worker) filterDedupe(ctx context.Context, in []analysis.PredictionCandidate, sum *Summary) []analysis.PredictionCandidate {
	out := make([]analysis.PredictionCandidate, 0, len(in))
	cutoff := w.cfg.Clock().Add(-w.cfg.DedupeWindow)
	seenEventSlugInCycle := map[string]bool{}
	for _, c := range in {
		// Active prediction exists?
		_, err := w.predictions.GetPrediction(ctx, c.EventSlug, c.ConditionID)
		if err == nil {
			sum.Skipped["active_prediction"]++
			w.observeDedupeSkipped("active_prediction")
			if w.log != nil {
				w.log.Debug().Str("event_slug", c.EventSlug).Str("condition_id", c.ConditionID).Msg("prediction creation: skipped_active_prediction")
			}
			continue
		}
		// Already shortlisted another condition under the same
		// event in THIS cycle? Suppress so a multi-market event
		// produces one prediction per dedupe window, not N.
		if seenEventSlugInCycle[c.EventSlug] {
			sum.Skipped["dedupe_same_event_cycle"]++
			w.observeDedupeSkipped("dedupe_same_event_cycle")
			if w.log != nil {
				w.log.Debug().Str("event_slug", c.EventSlug).Msg("prediction creation: skipped_dedupe_same_event_cycle")
			}
			continue
		}
		// Recent creation for the same event?
		n, err := w.predictions.CountPredictionsForEventSince(ctx, c.EventSlug, cutoff)
		if err != nil {
			// fail-open: counting failure should not block creation.
			out = append(out, c)
			seenEventSlugInCycle[c.EventSlug] = true
			continue
		}
		if n > 0 {
			sum.Skipped["dedupe_window"]++
			w.observeDedupeSkipped("dedupe_window")
			if w.log != nil {
				w.log.Debug().Str("event_slug", c.EventSlug).Msg("prediction creation: skipped_dedupe_window")
			}
			continue
		}
		seenEventSlugInCycle[c.EventSlug] = true
		out = append(out, c)
	}
	return out
}

// rankWithAI calls the AI ranker. Budget denial returns no picks +
// records the metric. Picks below MinScore are dropped here.
func (w *Worker) rankWithAI(ctx context.Context, cands []analysis.PredictionCandidate, sum *Summary) []analysis.PredictionRankingPick {
	if w.budget != nil {
		if ok, reason := w.budget.Allow(aibudget.BucketPredictionCreate, estPerRankingUSD); !ok {
			w.observeAISkipped("ranker_" + reason)
			sum.Skipped["budget_denied_rank"]++
			if w.log != nil {
				w.log.Warn().Str("reason", reason).Msg("prediction creation: ranker denied by budget")
			}
			return nil
		}
	}
	aiCtx, cancel := context.WithTimeout(ctx, w.cfg.AITimeout)
	defer cancel()
	res, err := w.ranker.RankCandidates(aiCtx, analysis.PredictionRankingRequest{
		AnalysisTimeUTC: w.cfg.Clock().UTC(),
		Candidates:      cands,
		MaxSelected:     w.cfg.MaxSelected,
	})
	if err != nil || res.Status != analysis.StatusOK {
		w.observeAI("ranker_failed")
		sum.Skipped["ranker_failed"]++
		if w.log != nil && err != nil {
			w.log.Warn().Err(err).Msg("prediction creation: ranker call failed")
		}
		return nil
	}
	if w.budget != nil {
		w.budget.Charge(aibudget.BucketPredictionCreate, res.EstimatedCostUSD)
	}
	w.observeAI("ranker_ok")
	picks := make([]analysis.PredictionRankingPick, 0, len(res.Picks))
	for _, p := range res.Picks {
		if p.Score < w.cfg.MinScore {
			sum.Skipped["below_min_score"]++
			continue
		}
		picks = append(picks, p)
	}
	if len(picks) > w.cfg.MaxSelected {
		picks = picks[:w.cfg.MaxSelected]
	}
	return picks
}

// createOne builds full context for ONE pick, calls the deep-dive
// AI, parses + normalises the response, and persists via the
// Applier (so the state machine + audit row write the same way the
// evolution worker writes). Returns a short status for the metric.
func (w *Worker) createOne(ctx context.Context, pick analysis.PredictionRankingPick, candPool []analysis.PredictionCandidate) string {
	cand := matchCandidate(pick, candPool)
	if cand.EventSlug == "" {
		return "no_candidate_match"
	}
	if w.budget != nil {
		if ok, reason := w.budget.Allow(aibudget.BucketPredictionCreate, estPerCreationUSD); !ok {
			w.observeAISkipped("creator_" + reason)
			if w.log != nil {
				w.log.Warn().Str("event_slug", cand.EventSlug).Str("reason", reason).Msg("prediction creation: creator denied by budget")
			}
			return "budget_denied"
		}
	}

	// Deterministic context refresh — same seam contracts the
	// evolution worker uses, kept consistent so tests don't drift.
	page := w.pages.Load(ctx, cand.EventSlug, eventpagecontext.SeverityInfo)
	cats, _ := w.catalysts.ListActive(ctx, cand.EventSlug)
	flow, _ := w.flow.LoadEventFlowSummary(ctx, cand.EventSlug, 24*time.Hour)
	var sig *repricing.Signal
	if w.repricing != nil && len(page.Annotations) > 0 {
		newest := newestAnnotation(page.Annotations)
		s, err := w.repricing.Compute(ctx, repricing.AnnotationInput{
			EventSlug:      cand.EventSlug,
			ConditionID:    cand.ConditionID,
			Outcome:        newest.Outcome,
			AnnotationHash: newest.ItemHash,
			Title:          newest.Title,
			Timestamp:      newest.Timestamp,
			PriceBefore:    newest.PriceBefore,
			PriceAfter:     newest.PriceAfter,
			PriceChange:    newest.PriceChange,
			CurrentPrice:   currentPriceFor(page, cand.ConditionID),
		}, false /* persist=false; the creation pass is a one-shot read */)
		if err == nil {
			sig = &s
		}
	}

	req := analysis.PredictionCreationRequest{
		EventSlug:          cand.EventSlug,
		ConditionID:        cand.ConditionID,
		Outcome:            firstNonEmpty(cand.Outcome, ""),
		Question:           cand.Question,
		Category:           cand.Category,
		MarketSnapshot:     renderMarketSnapshot(cand, page),
		AnnotationsBlock:   renderAnnotationsBlock(page),
		CatalystsBlock:     renderCatalystsBlock(cats),
		RepricingBlock:     renderRepricingBlock(sig),
		FlowSummaryBlock:   flow.RenderPromptBlock(),
		MatchedAlertsBlock: "(matched alerts: 0 — first creation)",
	}
	aiCtx, cancel := context.WithTimeout(ctx, w.cfg.AITimeout)
	defer cancel()
	res, err := w.creator.CreatePrediction(aiCtx, req)
	if err != nil || res.Status != analysis.StatusOK {
		w.observeAI("creator_failed")
		if w.log != nil && err != nil {
			w.log.Warn().Err(err).Str("event_slug", cand.EventSlug).Msg("prediction creation: creator call failed")
		}
		return "creator_failed"
	}
	if w.budget != nil {
		w.budget.Charge(aibudget.BucketPredictionCreate, res.EstimatedCostUSD)
	}
	w.observeAI("creator_ok")

	// --- Quality gate (PART 7) ----------------------------------
	// The deterministic gate runs BEFORE persist so we can both
	// (a) decide whether to persist at all (PersistLowQuality knob)
	// and (b) make the Telegram-suppression decision visible in
	// metrics. The gate is intentionally simple: confidence floor +
	// neutral-skip + minimum-summary-length + at-least-one-signal.
	qualityOK, gateReason := w.passesQuality(res, page, cats, flow, sig)
	if !qualityOK {
		w.observeQualityGate("low_" + gateReason)
		if !w.cfg.PersistLowQuality {
			if w.log != nil {
				w.log.Info().Str("event_slug", cand.EventSlug).Str("reason", gateReason).Msg("prediction creation: quality gate skipped persist")
			}
			return "low_quality_skipped"
		}
	} else {
		w.observeQualityGate("ok")
	}

	// Persist via UpsertPrediction. We INSERT as state="watching" so
	// the next evolution tick runs Decide() and immediately picks
	// the correct state (blocked / active_catalyst / etc).
	newPred := repository.NewMarketPrediction{
		EventSlug:    cand.EventSlug,
		ConditionID:  cand.ConditionID,
		Outcome:      cand.Outcome,
		SideBias:     res.SideBias,
		Summary:      strings.TrimSpace(res.Summary),
		CurrentState: marketprediction.StateWatching,
		StateReason:  "created by prediction creation worker",
		Confidence:   res.Confidence,
	}
	id, upErr := w.predictions.UpsertPrediction(ctx, newPred)
	if upErr != nil {
		if w.log != nil {
			w.log.Warn().Err(upErr).Str("event_slug", cand.EventSlug).Msg("prediction creation: upsert failed")
		}
		return "upsert_failed"
	}
	// Record an audit row for the cold-start so the timeline is
	// continuous in polymarket_market_prediction_states.
	_ = w.predictions.RecordStateTransition(ctx, repository.NewMarketPredictionStateTransition{
		PredictionID:  id,
		PreviousState: "",
		NewState:      marketprediction.StateWatching,
		Reason:        "creation worker: " + truncate(res.RiskFactors, 200),
		EvidenceJSON:  nil,
	})
	// Touch so the row drops to the back of the evolution queue
	// rather than being re-picked immediately.
	_ = w.predictions.TouchPredictionEvolution(ctx, id)

	w.observeCreated(cand.Category)

	// --- Telegram gates (PART 5 throttling + PART 7 quality) ----
	// The row is persisted at this point — the gates below only
	// decide whether Telegram ships. A skipped send still returns
	// "created" because the prediction itself exists; the Telegram
	// outcome is tracked separately via the *_telegram_* metrics.
	// Reasons NOT to ship:
	//  * worker disabled / Telegram unwired.
	//  * quality gate failed (regardless of PersistLowQuality).
	//  * very first cycle and SendOnStartup=false.
	//  * per-event cooldown still ticking.
	//  * already shipped MaxTelegramPerRun in this cycle.
	if !w.cfg.SendTelegram || w.tg == nil {
		return "created"
	}
	if !qualityOK {
		w.observeTelegramSkipped("low_quality")
		w.logTelegramSkip(cand.EventSlug, "low_quality")
		return "created"
	}
	if reason := w.canSendTelegram(cand.EventSlug); reason != "" {
		w.observeTelegramSkipped(reason)
		w.logTelegramSkip(cand.EventSlug, reason)
		return "created"
	}

	body := RenderCreationTelegram(w.buildRenderInput(cand, res, page, 0))
	// Safe-split on the 4000-char Telegram cap. Short bodies
	// pass through unchanged; long bodies (catalyst-dense
	// markets) are split on paragraph boundaries with HTML
	// tag pairs preserved.
	chunks := alerting.SafeSplitForTelegram(body)
	w.observeMessageChunks("prediction_creation", len(chunks))
	anySent := false
	for _, chunk := range chunks {
		if _, err := w.tg.SendHTML(ctx, chunk); err != nil {
			w.observeTelegram("failed")
			if w.log != nil {
				w.log.Warn().Err(err).Str("event_slug", cand.EventSlug).Msg("prediction creation: telegram send failed")
			}
			break
		}
		anySent = true
	}
	if anySent {
		w.observeTelegram("sent")
		w.observeTelegramSent()
		w.markTelegramSent(cand.EventSlug)
	}
	return "created"
}

func (w *Worker) categoryAllowed(category string) bool {
	if len(w.cfg.Categories) == 0 {
		return true
	}
	c := strings.ToLower(category)
	for _, w := range w.cfg.Categories {
		if strings.Contains(c, strings.ToLower(w)) {
			return true
		}
	}
	return false
}

func matchCandidate(pick analysis.PredictionRankingPick, pool []analysis.PredictionCandidate) analysis.PredictionCandidate {
	for _, c := range pool {
		if c.EventSlug == pick.EventSlug && (pick.ConditionID == "" || c.ConditionID == pick.ConditionID) {
			return c
		}
	}
	// Slug-only fallback when the model didn't echo condition_id.
	for _, c := range pool {
		if c.EventSlug == pick.EventSlug {
			return c
		}
	}
	return analysis.PredictionCandidate{}
}

func newestAnnotation(rows []repository.EventAnnotation) repository.EventAnnotation {
	if len(rows) == 0 {
		return repository.EventAnnotation{}
	}
	newest := rows[0]
	for _, r := range rows[1:] {
		if r.Timestamp.After(newest.Timestamp) {
			newest = r
		}
	}
	return newest
}

func currentPriceFor(page eventpagecontext.Summary, conditionID string) *float64 {
	for _, m := range page.Markets {
		if m.ConditionID == conditionID && m.LastTradePrice != nil {
			v := *m.LastTradePrice
			return &v
		}
	}
	return nil
}

func firstNonEmpty(opts ...string) string {
	for _, o := range opts {
		if strings.TrimSpace(o) != "" {
			return o
		}
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// --- metrics adapters ------------------------------------------------------

func (w *Worker) observeCycle(status string) {
	if w.met == nil || w.met.PredictionCreationRuns == nil {
		return
	}
	w.met.PredictionCreationRuns.WithLabelValues(status).Inc()
}

func (w *Worker) observeCandidates(n int) {
	if w.met == nil || w.met.PredictionCreationCandidates == nil {
		return
	}
	w.met.PredictionCreationCandidates.Add(float64(n))
}

func (w *Worker) observeCreated(category string) {
	if w.met == nil || w.met.PredictionCreationCreated == nil {
		return
	}
	w.met.PredictionCreationCreated.WithLabelValues(category).Inc()
}

func (w *Worker) observeAI(status string) {
	if w.met == nil || w.met.PredictionCreationAIRequests == nil {
		return
	}
	w.met.PredictionCreationAIRequests.WithLabelValues(status).Inc()
}

func (w *Worker) observeAISkipped(reason string) {
	if w.met == nil || w.met.PredictionCreationAISkipped == nil {
		return
	}
	w.met.PredictionCreationAISkipped.WithLabelValues(reason).Inc()
}

func (w *Worker) observeTelegram(status string) {
	if w.met == nil || w.met.PredictionCreationTelegram == nil {
		return
	}
	w.met.PredictionCreationTelegram.WithLabelValues(status).Inc()
}

func (w *Worker) observeLatency(d time.Duration) {
	if w.met == nil || w.met.PredictionCreationLatency == nil {
		return
	}
	w.met.PredictionCreationLatency.Observe(d.Seconds())
}
