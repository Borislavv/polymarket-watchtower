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
	"errors"
	"math"
	"time"

	"github.com/rs/zerolog"

	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/anomaly"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/trade"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/polymarket/gamma"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/repository"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/telegram"
)

// AlertStore is the subset of *repository.AlertRepository the worker uses.
type AlertStore interface {
	ListSentForOutcomeCheck(ctx context.Context, claimLimit int32) ([]repository.Alert, error)
	MarkOutcome(ctx context.Context, u repository.OutcomeUpdate) error
	TouchOutcomeUnavailable(ctx context.Context, alertID int64) error
	// ListAlertsForReaction returns sent alerts with a known outcome
	// still awaiting a Telegram reaction. Used by the reaction pass.
	ListAlertsForReaction(ctx context.Context, claimLimit int32) ([]repository.Alert, error)
	// MarkReaction persists the verdict of the setMessageReaction call.
	MarkReaction(ctx context.Context, alertID int64, status repository.ReactionStatus, emoji string) error
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

	// Reactions configures the post-classify pass that calls Telegram
	// setMessageReaction on every newly-resolved alert. When
	// Reactions.Enabled is false, the worker still classifies outcomes
	// — only the upstream reaction call is skipped (rows are stamped
	// telegram_reaction_status='disabled' so the partial index stays
	// small).
	Reactions ReactionsConfig

	// OutcomeMetrics observes every classified verdict. nil disables.
	OutcomeMetrics OutcomeMetrics
}

// ReactionsConfig controls the Telegram reaction pass.
type ReactionsConfig struct {
	Enabled          bool
	ChatID           string
	SuccessEmoji     string // e.g. "✅"
	FailureEmoji     string // e.g. "💭"
	AmbiguousEmoji   string // e.g. "⚠️"
	ClaimLimit       int32  // rows per tick; 0 -> uses Worker.ClaimLimit
	Bot              ReactionSender
	Metrics          ReactionMetrics
	DisableAmbiguous bool // set true to skip reactions on 'unknown' outcomes
}

// ReactionSender is the subset of *telegram.Bot needed by the reactor.
// Satisfied by *telegram.Bot itself; the interface keeps the worker
// testable without spinning up an HTTP fake.
type ReactionSender interface {
	SetMessageReaction(ctx context.Context, chatID string, messageID int64, emoji string) error
}

// ReactionMetrics is a tiny shim over the global Prometheus collector
// so the worker doesn't transitively depend on the metrics package.
// Set to nil to disable.
type ReactionMetrics interface {
	ObserveReaction(status, reaction string)
}

// OutcomeMetrics is the shim for the alert-outcome counter. Stamped
// once per classified alert so Grafana can graph signal-quality
// without re-running the aggregate SQL on every dashboard refresh.
//
// The interface is intentionally narrow: ObserveOutcome counts the
// verdict; ObservePAL emits the proof-of-alert-value family (edge,
// weighted success, calibration bucket). Two methods so the
// implementation can fan out to different Prometheus collectors
// without forcing the caller to know about the labels.
type OutcomeMetrics interface {
	ObserveOutcome(status, severity, kind string)
	// ObservePAL records one classified alert against the PAL metric
	// family. snap.EdgeValid=false means edge/weighted metrics are
	// skipped (still a calibration counter contribution). Implementers
	// must tolerate nil-safe calls when the metrics handle is nil.
	ObservePAL(snap PALSnapshot)
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
	for _, a := range candidates {
		if ctx.Err() != nil {
			return
		}
		w.processOne(ctx, a)
	}
	// Reaction pass — independent claim list, so a tick that finds zero
	// candidates above can still react to alerts a previous tick
	// classified. Per-row idempotent via the dedup_status state machine.
	w.reactionPass(ctx)
}

// reactionPass claims sent alerts with a known outcome whose Telegram
// reaction is still pending (or previously failed) and applies the
// configured emoji via setMessageReaction. The pass is best-effort —
// individual failures do not block other alerts.
func (w *Worker) reactionPass(ctx context.Context) {
	if !w.cfg.Reactions.Enabled {
		// Reactions are off — leave rows in their default 'pending'
		// state so flipping the flag back on can resume processing.
		return
	}
	if w.cfg.Reactions.Bot == nil || w.cfg.Reactions.ChatID == "" {
		w.log.Warn().Msg("outcomes: reaction pass enabled but bot/chat unwired; skipping")
		return
	}
	limit := w.cfg.Reactions.ClaimLimit
	if limit <= 0 {
		limit = w.cfg.ClaimLimit
	}
	rows, err := w.alerts.ListAlertsForReaction(ctx, limit)
	if err != nil {
		w.log.Err(err).Msg("outcomes: list reaction candidates failed")
		return
	}
	for _, a := range rows {
		if ctx.Err() != nil {
			return
		}
		w.applyReaction(ctx, a)
	}
}

// applyReaction picks the emoji for the alert's outcome status, calls
// setMessageReaction, and persists the verdict. Telegram explicit
// "unsupported" errors are terminal (the row will never be retried);
// transient errors are persisted as `failed` so the next tick retries.
func (w *Worker) applyReaction(ctx context.Context, a repository.Alert) {
	if a.TelegramMessageID == nil || *a.TelegramMessageID == 0 {
		// Defensive — the SQL filter already excludes this case.
		_ = w.alerts.MarkReaction(ctx, a.ID, repository.ReactionUnsupported, "")
		w.observeReaction("unsupported", "")
		return
	}
	emoji := w.emojiForOutcome(a.OutcomeStatus)
	if emoji == "" {
		// Configured to skip this outcome class (e.g. ambiguous reactions
		// disabled). Stamp 'disabled' so the row leaves the partial index.
		_ = w.alerts.MarkReaction(ctx, a.ID, repository.ReactionDisabled, "")
		w.observeReaction("disabled", "")
		return
	}
	err := w.cfg.Reactions.Bot.SetMessageReaction(ctx, w.cfg.Reactions.ChatID, *a.TelegramMessageID, emoji)
	switch {
	case err == nil:
		_ = w.alerts.MarkReaction(ctx, a.ID, repository.ReactionApplied, emoji)
		w.observeReaction("applied", emoji)
	case errors.Is(err, telegram.ErrReactionUnsupported):
		_ = w.alerts.MarkReaction(ctx, a.ID, repository.ReactionUnsupported, emoji)
		w.observeReaction("unsupported", emoji)
		w.log.Info().Int64("alert_id", a.ID).Str("emoji", emoji).Msg("outcomes: reaction unsupported on this target; marking terminal")
	default:
		_ = w.alerts.MarkReaction(ctx, a.ID, repository.ReactionFailed, emoji)
		w.observeReaction("failed", emoji)
		w.log.Err(err).Int64("alert_id", a.ID).Str("emoji", emoji).Msg("outcomes: reaction failed; will retry next tick")
	}
}

// emojiForOutcome maps the persisted outcome_status to the configured
// emoji. Empty return signals "skip this outcome class".
func (w *Worker) emojiForOutcome(status repository.OutcomeStatus) string {
	switch status {
	case repository.OutcomeCorrect:
		return w.cfg.Reactions.SuccessEmoji
	case repository.OutcomeWrong:
		return w.cfg.Reactions.FailureEmoji
	case repository.OutcomeUnknown:
		if w.cfg.Reactions.DisableAmbiguous {
			return ""
		}
		return w.cfg.Reactions.AmbiguousEmoji
	default:
		return ""
	}
}

func (w *Worker) observeReaction(status, reaction string) {
	if w.cfg.Reactions.Metrics != nil {
		w.cfg.Reactions.Metrics.ObserveReaction(status, reaction)
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

	verdict, price, side := w.classifyWithTrade(a, res)
	if err := w.alerts.MarkOutcome(ctx, verdict); err != nil {
		w.log.Err(err).Int64("alert_id", a.ID).Msg("outcomes: mark outcome failed")
	}
	if w.cfg.OutcomeMetrics != nil {
		w.cfg.OutcomeMetrics.ObserveOutcome(string(verdict.Status), a.Severity, string(a.Kind))
		// PAL: emit only when we recovered the trade-level price+side
		// from the Finding payload AND the verdict is in a state that
		// admits an edge calculation (or contributes a calibration
		// bucket count). The snapshot builder handles the gating.
		if price > 0 {
			snap := BuildSnapshot(verdict.Status, a.Severity, string(a.Kind), price, side)
			w.cfg.OutcomeMetrics.ObservePAL(snap)
		}
	}
}

// classifyWithTrade is the PAL-enriched wrapper around classify. It
// decodes the Finding twice (once internally in classify, once here
// for the trade details). The cost is a sub-microsecond JSON unmarshal
// per resolved alert — vastly cheaper than restructuring the payload
// to avoid the second pass.
func (w *Worker) classifyWithTrade(a repository.Alert, res gamma.MarketResolution) (repository.OutcomeUpdate, float64, trade.Side) {
	verdict := w.classify(a, res)
	// Best-effort price/side recovery for PAL. A payload that won't
	// parse is rare (the alertsender wrote it) but tolerated.
	var f anomaly.Finding
	if err := json.Unmarshal(a.Payload, &f); err != nil || f.Trade == nil {
		return verdict, 0, ""
	}
	return verdict, f.Trade.Price, f.Trade.Side
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
	// TradeRef carries the outcome LABEL ("Yes"/"No"/...), not the CLOB
	// token id, so we match by label against res.OutcomeLabels.
	var f anomaly.Finding
	if err := json.Unmarshal(a.Payload, &f); err != nil || f.Trade == nil {
		update.Status = repository.OutcomeUnknown
		return update
	}
	alertedLabel := f.Trade.Outcome
	alertedSide := f.Trade.Side
	winningLabel := update.WinningOutcomeLabel

	switch {
	case alertedLabel == "" || winningLabel == "":
		update.Status = repository.OutcomeUnknown
	case alertedSide == trade.SideBuy && alertedLabel == winningLabel:
		update.Status = repository.OutcomeCorrect
	case alertedSide == trade.SideSell && alertedLabel != winningLabel:
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
