package marketclosereview

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/Borislavv/polymarket-watchtower/internal/infra/ai/openai"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/metrics"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/repository"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/telegram"
)

// --- Seams (interfaces over infra) ---------------------------------------

// CandidateSource lists markets that recently closed and have no
// succeeded review row. *repository.MarketCloseReviewRepository
// satisfies it.
type CandidateSource interface {
	ListCandidates(ctx context.Context, closedSince, closedUntil time.Time, limit int32) ([]repository.MarketCloseReviewCandidate, error)
	HasSucceededReview(ctx context.Context, conditionID string) (bool, error)
	InsertRunning(ctx context.Context, marketID int64, conditionID, eventSlug string, closedAt, resolvedAt time.Time) (int64, error)
	FinishSucceeded(ctx context.Context, in repository.MarketCloseReviewFinish) error
	FinishFailed(ctx context.Context, id int64, errMsg string, nextRetryAt time.Time) error
	FinishSkipped(ctx context.Context, id int64, reason string) error
	ListAlertsForReview(ctx context.Context, marketID int64, sentSince time.Time, limit int32) ([]repository.Alert, error)
}

// Analyzer dispatches the AI call. *openai.Client satisfies it.
type Analyzer interface {
	ReviewMarketClose(ctx context.Context, req openai.MarketCloseReviewRequest) (openai.MarketCloseReviewResponse, error)
}

// Budget is the AI-budget gate. *aibudget.Manager satisfies it
// via the same Allow/Charge shape every v11 worker uses.
type Budget interface {
	Allow(bucket string, estCost float64) (bool, string)
	Charge(bucket string, actualCost float64)
}

// Telegram delivers the typed admin body via *telegram.Router.
type Telegram interface {
	Send(ctx context.Context, msg telegram.Message) (telegram.SendResult, error)
}

// Reactioner sets a single reaction on a previously-sent signal
// alert. *telegram.Annotation satisfies it.
type Reactioner interface {
	SetOutcomeReaction(ctx context.Context, surface telegram.Surface, chatID string, messageID int64, emoji string) error
}

// MarketCloseReviewBucket is the aibudget bucket name. Exported
// so the wiring layer can pass it into the cfg.
const MarketCloseReviewBucket = "market_close_review"

// estPerReviewUSD is a conservative pre-flight cost estimate. The
// post-call ledger.consume() in the openai bridge records the
// actual cost; this number is only used for Budget.Allow's
// gating decision.
const estPerReviewUSD = 0.05

// --- Worker --------------------------------------------------------------

// Worker is the periodic Market Close Review loop.
type Worker struct {
	cfg        Config
	store      CandidateSource
	analyzer   Analyzer
	budget     Budget
	telegram   Telegram
	reactioner Reactioner
	met        *metrics.Metrics
	log        *zerolog.Logger
}

// New wires the worker. Any nil dependency degrades gracefully:
//   - nil analyzer → AI calls are skipped (status=skipped_ai_disabled).
//   - nil budget → fail-open (all calls allowed; legacy behaviour).
//   - nil telegram → AI runs, persistence happens, no admin send.
//   - nil reactioner → AI runs, no reactions applied.
func New(
	cfg Config,
	store CandidateSource,
	analyzer Analyzer,
	budget Budget,
	tg Telegram,
	reactioner Reactioner,
	met *metrics.Metrics,
	log *zerolog.Logger,
) *Worker {
	cfg.applyDefaults()
	return &Worker{
		cfg:        cfg,
		store:      store,
		analyzer:   analyzer,
		budget:     budget,
		telegram:   tg,
		reactioner: reactioner,
		met:        met,
		log:        log,
	}
}

// Run blocks until ctx cancels. Ticks at cfg.Interval; first
// tick fires immediately so a redeploy can resume the queue
// without waiting one full window.
func (w *Worker) Run(ctx context.Context) {
	if !w.cfg.Enabled {
		if w.log != nil {
			w.log.Info().Msg("market close review: disabled by config")
		}
		return
	}
	w.Tick(ctx)
	t := time.NewTicker(w.cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.Tick(ctx)
		}
	}
}

// Tick runs one cycle: candidate selection → per-market processing.
// Exposed for tests + a future CLI dry-run.
func (w *Worker) Tick(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			w.observeFailure("panic")
			if w.log != nil {
				w.log.Error().Interface("panic", r).Msg("market close review: tick panic")
			}
		}
	}()

	now := w.cfg.Clock()
	closedSince := now.Add(-w.cfg.MarketMaxAgeAfterClose)
	closedUntil := now

	candidates, err := w.store.ListCandidates(ctx, closedSince, closedUntil, int32(w.cfg.MaxMarketsPerRun))
	if err != nil {
		w.observeFailure("list_candidates")
		if w.log != nil {
			w.log.Warn().Err(err).Msg("market close review: list candidates")
		}
		return
	}
	if len(candidates) == 0 {
		return
	}

	for _, c := range candidates {
		w.processOne(ctx, c)
	}
}

// processOne is the per-market state machine. Each step records
// a per-candidate metric so dashboards can show selection vs
// processing outcomes separately.
func (w *Worker) processOne(ctx context.Context, c repository.MarketCloseReviewCandidate) {
	// Already reviewed? The candidate list joins on the partial
	// unique index, but a concurrent insert race could slip a row
	// through — re-check before opening a running row.
	hasSucceeded, err := w.store.HasSucceededReview(ctx, c.ConditionID)
	if err != nil {
		w.observeCandidate("rejected", "lookup_failed")
		return
	}
	if hasSucceeded {
		w.observeCandidate("rejected", "already_succeeded")
		return
	}

	// Evidence pack — fully bounded.
	alerts, evidenceErr := w.buildEvidence(ctx, c)
	if evidenceErr != nil {
		w.observeCandidate("rejected", "evidence_failed")
		return
	}

	if w.cfg.RequireAlertOrNews && len(alerts) < w.cfg.MinAlerts {
		// Skip path — record an explicit "no evidence" row so
		// future runs don't reconsider it on every tick.
		w.recordSkipped(ctx, c, "no_evidence")
		w.observeCandidate("skipped", "no_evidence")
		return
	}

	// Budget gate (fail-open if no budget wired).
	if w.budget != nil {
		ok, reason := w.budget.Allow(MarketCloseReviewBucket, estPerReviewUSD)
		if !ok {
			w.recordSkipped(ctx, c, "budget_"+reason)
			w.observeCandidate("skipped", "budget_exhausted")
			return
		}
	}

	// AI disabled — record skipped + move on.
	if w.analyzer == nil || !w.cfg.AIEnabled {
		w.recordSkipped(ctx, c, "ai_disabled")
		w.observeCandidate("skipped", "ai_disabled")
		return
	}

	// Open a running row.
	now := w.cfg.Clock()
	closedAt := c.EndDate
	if closedAt.IsZero() {
		closedAt = now
	}
	rowID, err := w.store.InsertRunning(ctx, c.MarketID, c.ConditionID, c.EventSlug, closedAt, time.Time{})
	if err != nil {
		w.observeCandidate("rejected", "insert_running_failed")
		return
	}
	w.observeCandidate("accepted", "")

	// Run the review.
	aiCtx, cancel := context.WithTimeout(ctx, w.cfg.AITimeout)
	defer cancel()

	market := openai.MarketCloseReviewMarketSummary{
		ConditionID:    c.ConditionID,
		EventSlug:      c.EventSlug,
		Title:          c.Question,
		ClosedAt:       closedAt,
		WinningOutcome: "",
	}
	aiAlerts := projectAlerts(alerts)
	req := openai.MarketCloseReviewRequest{
		Market:            market,
		Alerts:            aiAlerts,
		MaxAlertsInPrompt: w.cfg.MaxAlertsPerMarket,
		MaxEventsInPrompt: w.cfg.MaxEventsPerMarket,
	}

	resp, aiErr := w.analyzer.ReviewMarketClose(aiCtx, req)
	if w.budget != nil && resp.EstimatedCostUSD > 0 {
		w.budget.Charge(MarketCloseReviewBucket, resp.EstimatedCostUSD)
	}
	if w.met != nil {
		status := "ok"
		if aiErr != nil {
			status = "failed"
		}
		w.met.MarketCloseReviewAICalls.WithLabelValues(status, resp.Model).Inc()
		if resp.EstimatedCostUSD > 0 {
			w.met.MarketCloseReviewAICost.WithLabelValues(resp.Model).Add(resp.EstimatedCostUSD)
		}
	}
	if aiErr != nil {
		w.recordFailed(ctx, rowID, aiErr)
		w.observeRun(string(resp.Verdict), "failed")
		return
	}

	// Persist + admin Telegram + reactions.
	ev := buildEvidenceJSON(c, alerts)
	aiJSON, _ := json.Marshal(resp)

	conf := resp.Confidence
	if err := w.store.FinishSucceeded(ctx, repository.MarketCloseReviewFinish{
		ID:               rowID,
		Verdict:          resp.Verdict,
		Confidence:       &conf,
		AdminSummary:     resp.AdminSummary,
		AIJSON:           aiJSON,
		EvidenceJSON:     ev,
		AIModel:          resp.Model,
		InputTokens:      intToPtr32(resp.PromptTokens),
		OutputTokens:     intToPtr32(resp.CompletionTokens),
		EstimatedCostUSD: &resp.EstimatedCostUSD,
	}); err != nil {
		w.recordFailed(ctx, rowID, err)
		w.observeRun(resp.Verdict, "failed_persist")
		return
	}
	w.observeRun(resp.Verdict, "succeeded")

	// Admin Telegram (non-fatal).
	if w.cfg.SendAdminTelegram && w.telegram != nil {
		body := RenderAdminTelegram(market, aiAlerts, resp)
		if _, err := w.telegram.Send(ctx, telegram.Message{
			Surface: telegram.SurfaceMarketCloseReview,
			HTML:    body,
		}); err != nil && w.log != nil {
			w.log.Warn().Err(err).Int64("review_id", rowID).Msg("market close review: admin telegram failed")
		}
	}

	// Reactions (non-fatal).
	if w.cfg.SetReactions && w.reactioner != nil && w.cfg.SignalChatID != "" {
		w.applyReactions(ctx, alerts, resp)
	}
}

// buildEvidence pulls the per-market evidence pack. Today the
// pack is alerts-only — flow aggregates + events join in a
// follow-up iteration. The CandidateSource already bounds the
// alerts read; we just translate to the analyzer shape.
func (w *Worker) buildEvidence(ctx context.Context, c repository.MarketCloseReviewCandidate) ([]repository.Alert, error) {
	if c.MarketID == 0 {
		return nil, errors.New("zero market id")
	}
	sentSince := w.cfg.Clock().Add(-w.cfg.HistoryLookback)
	return w.store.ListAlertsForReview(ctx, c.MarketID, sentSince, int32(w.cfg.MaxAlertsPerMarket))
}

// projectAlerts converts repository.Alert → analyzer evidence
// shape. Strategy / wallet / side data lives inside Alert.Payload
// (anomaly.Finding JSON); for v11.4 the evidence pack carries
// the structured columns directly and the worker leaves
// per-trade detail for a follow-up.
func projectAlerts(alerts []repository.Alert) []openai.MarketCloseReviewAlertEvidence {
	out := make([]openai.MarketCloseReviewAlertEvidence, 0, len(alerts))
	for _, a := range alerts {
		ev := openai.MarketCloseReviewAlertEvidence{
			ID:                a.ID,
			Kind:              string(a.Kind),
			Severity:          a.Severity,
			StrategyVersion:   a.StrategyVersion,
			Reason:            a.Reason,
			Timestamp:         a.SentAt,
			TelegramMessageID: a.TelegramMessageID,
			CLV15m:            a.CLV15m,
			CLV1h:             a.CLV1h,
			CLV6h:             a.CLV6h,
			CLV24h:            a.CLV24h,
			OutcomeStatus:     string(a.OutcomeStatus),
		}
		out = append(out, ev)
	}
	return out
}

// buildEvidenceJSON serialises the evidence pack for audit
// storage in polymarket_market_close_reviews.evidence_json.
func buildEvidenceJSON(c repository.MarketCloseReviewCandidate, alerts []repository.Alert) []byte {
	type alertSnap struct {
		ID                int64    `json:"id"`
		Kind              string   `json:"kind"`
		Severity          string   `json:"severity"`
		StrategyVersion   string   `json:"strategy_version"`
		Reason            string   `json:"reason"`
		Timestamp         string   `json:"timestamp"`
		TelegramMessageID *int64   `json:"telegram_message_id,omitempty"`
		CLV6h             *float64 `json:"clv_6h,omitempty"`
		OutcomeStatus     string   `json:"outcome_status,omitempty"`
	}
	pack := struct {
		ConditionID string      `json:"condition_id"`
		EventSlug   string      `json:"event_slug,omitempty"`
		Title       string      `json:"title,omitempty"`
		ClosedAt    string      `json:"closed_at"`
		Alerts      []alertSnap `json:"alerts"`
	}{
		ConditionID: c.ConditionID,
		EventSlug:   c.EventSlug,
		Title:       c.Question,
		ClosedAt:    c.EndDate.UTC().Format(time.RFC3339),
		Alerts:      make([]alertSnap, 0, len(alerts)),
	}
	for _, a := range alerts {
		pack.Alerts = append(pack.Alerts, alertSnap{
			ID: a.ID, Kind: string(a.Kind), Severity: a.Severity,
			StrategyVersion: a.StrategyVersion, Reason: a.Reason,
			Timestamp:         a.SentAt.UTC().Format(time.RFC3339),
			TelegramMessageID: a.TelegramMessageID,
			CLV6h:             a.CLV6h, OutcomeStatus: string(a.OutcomeStatus),
		})
	}
	b, _ := json.Marshal(pack)
	return b
}

func (w *Worker) recordSkipped(ctx context.Context, c repository.MarketCloseReviewCandidate, reason string) {
	closedAt := c.EndDate
	if closedAt.IsZero() {
		closedAt = w.cfg.Clock()
	}
	id, err := w.store.InsertRunning(ctx, c.MarketID, c.ConditionID, c.EventSlug, closedAt, time.Time{})
	if err != nil {
		return
	}
	_ = w.store.FinishSkipped(ctx, id, reason)
}

func (w *Worker) recordFailed(ctx context.Context, id int64, err error) {
	nextRetry := w.cfg.Clock().Add(w.cfg.RetryInitialBackoff)
	_ = w.store.FinishFailed(ctx, id, err.Error(), nextRetry)
}

// applyReactions walks the AI's reaction_plan and sets the
// configured emoji on each referenced alert message. Failures
// are logged and do NOT fail the review row.
func (w *Worker) applyReactions(ctx context.Context, alerts []repository.Alert, resp openai.MarketCloseReviewResponse) {
	byID := make(map[int64]repository.Alert, len(alerts))
	for _, a := range alerts {
		byID[a.ID] = a
	}
	for _, p := range resp.ReactionPlan {
		if p.Reaction == "none" || p.Reaction == "" {
			continue
		}
		if p.Reaction == "ambiguous" && w.cfg.ReactionSkipAmbiguous {
			continue
		}
		alert, ok := byID[p.AlertID]
		if !ok || alert.TelegramMessageID == nil || *alert.TelegramMessageID <= 0 {
			continue
		}
		emoji := w.pickReactionEmoji(p.Reaction)
		if emoji == "" {
			continue
		}
		err := w.reactioner.SetOutcomeReaction(ctx, telegram.SurfaceOutcomeFollowup, w.cfg.SignalChatID, *alert.TelegramMessageID, emoji)
		status := "sent"
		reactionLabel := "success"
		switch p.Reaction {
		case "failure":
			reactionLabel = "failure"
		case "ambiguous":
			reactionLabel = "ambiguous"
		}
		if err != nil {
			if errors.Is(err, telegram.ErrReactionUnsupported) {
				status = "unsupported"
			} else {
				status = "failed"
				if w.log != nil {
					w.log.Warn().Err(err).Int64("alert_id", p.AlertID).Msg("market close review: reaction failed")
				}
			}
		}
		if w.met != nil {
			w.met.MarketCloseReviewReactions.WithLabelValues(status, reactionLabel).Inc()
		}
	}
}

func (w *Worker) pickReactionEmoji(reaction string) string {
	switch strings.ToLower(reaction) {
	case "success":
		return w.cfg.ReactionSuccess
	case "failure":
		return w.cfg.ReactionFailure
	case "ambiguous":
		return w.cfg.ReactionAmbiguous
	}
	return ""
}

// --- Metric helpers ------------------------------------------------------

func (w *Worker) observeFailure(reason string) {
	if w.met != nil && w.met.MarketCloseReviewFailures != nil {
		w.met.MarketCloseReviewFailures.WithLabelValues(reason).Inc()
	}
}

func (w *Worker) observeCandidate(decision, reason string) {
	if w.met != nil && w.met.MarketCloseReviewCandidates != nil {
		w.met.MarketCloseReviewCandidates.WithLabelValues(decision, reason).Inc()
	}
}

func (w *Worker) observeRun(verdict, status string) {
	if w.met != nil && w.met.MarketCloseReviewRuns != nil {
		w.met.MarketCloseReviewRuns.WithLabelValues(status, verdict).Inc()
	}
}

func intToPtr32(n int) *int32 {
	v := int32(n)
	return &v
}
