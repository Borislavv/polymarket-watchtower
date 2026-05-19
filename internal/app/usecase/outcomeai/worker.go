// Package outcomeai owns the postmortem loop: when a market resolves
// and Watchtower's outcomes worker stamps `resolved_correct/wrong`
// on an alert, this worker generates the AI postmortem, persists
// it, and updates the original Telegram message (edit + reaction).
//
// Design notes:
//   - Deliberately a SEPARATE worker, not a hook on outcomes.Worker:
//     the postmortem path may call an external LLM (slow + costly)
//     and should not slow down the verdict-stamping loop. Decoupling
//     also means a deploy without the AI key still runs verdicts to
//     completion; postmortems simply queue up.
//   - Idempotent: one row per alert in polymarket_alert_outcome_analyses
//     (UNIQUE on alert_id). The claim query LEFT JOINs the table so
//     processed rows fall out of the candidate list.
//   - Graceful degrade: AI errors are recorded (status="error"); the
//     reaction is still applied and a follow-up message is still
//     sent so the operator always sees the result.
package outcomeai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/analysis"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/anomaly"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/repository"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/telegram"
)

// AlertSource exposes the read+write surface against polymarket_alerts.
// *repository.AlertRepository satisfies it.
type AlertSource interface {
	ListResolvedForPostmortem(ctx context.Context, limit int32) ([]repository.Alert, error)
	GetByID(ctx context.Context, id int64) (repository.Alert, error)
}

// PostmortemStore wraps polymarket_alert_outcome_analyses inserts.
// *repository.AlertOutcomeAnalysisRepository satisfies it.
type PostmortemStore interface {
	Insert(ctx context.Context, a repository.NewAlertOutcomeAnalysis) (repository.AlertOutcomeAnalysis, bool, error)
}

// Bot is the Telegram surface this worker needs.
// *telegram.Bot satisfies it.
type Bot interface {
	EditMessageText(ctx context.Context, chatID string, messageID int64, text string) error
	SendHTML(ctx context.Context, chatID, text string) (telegram.SendResult, error)
	SetMessageReaction(ctx context.Context, chatID string, messageID int64, emoji string) error
}

// Analyzer is the AI postmortem entry point.
// internal/domain/model/analysis.Analyzer satisfies it.
type Analyzer interface {
	AnalyzeOutcome(ctx context.Context, req analysis.OutcomeAnalysisRequest) (analysis.OutcomeAnalysis, error)
}

// Config tunes the worker.
type Config struct {
	Enabled    bool
	Interval   time.Duration
	ClaimLimit int32
	ChatID     string // Telegram chat to edit / reply in
	// Reaction emoji map. Tunable per spec; defaults below.
	SuccessReaction string // resolved_correct → e.g. "✅"
	FailureReaction string // resolved_wrong → e.g. "❌"

	Clock func() time.Time
}

func (c *Config) applyDefaults() {
	if c.Interval <= 0 {
		c.Interval = 10 * time.Minute
	}
	if c.ClaimLimit <= 0 {
		c.ClaimLimit = 16
	}
	if c.SuccessReaction == "" {
		c.SuccessReaction = "✅"
	}
	if c.FailureReaction == "" {
		c.FailureReaction = "❌"
	}
	if c.Clock == nil {
		c.Clock = time.Now
	}
}

// Worker is the periodic postmortem loop.
type Worker struct {
	cfg      Config
	alerts   AlertSource
	store    PostmortemStore
	analyzer Analyzer
	bot      Bot
	log      *zerolog.Logger
}

// New wires the worker. All deps required; pass analysis.NoopAnalyzer
// to disable the AI call (the worker still applies reactions and
// still records an empty postmortem row).
func New(cfg Config, alerts AlertSource, store PostmortemStore, analyzer Analyzer, bot Bot, log *zerolog.Logger) *Worker {
	cfg.applyDefaults()
	return &Worker{cfg: cfg, alerts: alerts, store: store, analyzer: analyzer, bot: bot, log: log}
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
	rows, err := w.alerts.ListResolvedForPostmortem(ctx, w.cfg.ClaimLimit)
	if err != nil {
		w.log.Err(err).Msg("outcomeai: list resolved failed")
		return
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for _, a := range rows {
		a := a
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			w.processOne(ctx, a)
		}()
	}
	wg.Wait()
}

func (w *Worker) processOne(ctx context.Context, a repository.Alert) {
	// 1. Reconstruct the original Finding to pass to the analyzer.
	var f anomaly.Finding
	if err := json.Unmarshal(a.Payload, &f); err != nil {
		w.log.Err(err).Int64("alert_id", a.ID).Msg("outcomeai: payload unmarshal failed")
		return
	}

	// 2. Build the AI request and call the analyzer.
	req := buildOutcomeRequest(a, f)
	out, err := w.analyzer.AnalyzeOutcome(ctx, req)
	if err != nil {
		w.log.Err(err).Int64("alert_id", a.ID).Msg("outcomeai: analyzer returned error")
		out = analysis.OutcomeAnalysis{Status: analysis.StatusError, Model: "unknown", LastError: err.Error()}
	}

	// 3. Persist the postmortem row first — this is the idempotency
	//    primitive (UNIQUE on alert_id). If we crash mid-flow the
	//    next tick won't double-process this alert.
	stored, _, err := w.store.Insert(ctx, repository.NewAlertOutcomeAnalysis{
		AlertID:          a.ID,
		OutcomeStatus:    string(a.OutcomeStatus),
		WonExpected:      out.WonExpected,
		AIReasonText:     out.ReasonText,
		AILessonsText:    out.LessonsText,
		Confidence:       out.Confidence,
		Model:            out.Model,
		PromptTokens:     int32(out.PromptTokens),
		CompletionTokens: int32(out.CompletionTokens),
		EstimatedCostUSD: out.EstimatedCostUSD,
		TelegramChatID:   w.cfg.ChatID,
		DeliveryStatus:   "pending",
	})
	if err != nil {
		w.log.Err(err).Int64("alert_id", a.ID).Msg("outcomeai: persist postmortem failed")
		return
	}
	_ = stored

	// 4. Telegram update path: edit original message with the
	//    Why WON / Why LOST block, then add a reaction. Failures
	//    are logged but non-fatal — the postmortem is in the DB.
	w.deliver(ctx, a, f, out)
}

func (w *Worker) deliver(ctx context.Context, a repository.Alert, f anomaly.Finding, out analysis.OutcomeAnalysis) {
	if w.bot == nil || w.cfg.ChatID == "" {
		return
	}

	// Rebuild the original alert body, append the resolution block.
	// We use the same formatter the sender used so the surrounding
	// content stays consistent.
	body := renderUpdatedBody(f, a, out)
	if a.TelegramMessageID != nil && *a.TelegramMessageID > 0 {
		err := w.bot.EditMessageText(ctx, w.cfg.ChatID, *a.TelegramMessageID, body)
		if err != nil {
			if errors.Is(err, telegram.ErrEditUnsupported) {
				// Edit not allowed (message too old, etc.) → send a
				// linked follow-up so the operator still sees the
				// resolution.
				if _, sendErr := w.bot.SendHTML(ctx, w.cfg.ChatID, renderFollowupBody(f, a, out)); sendErr != nil {
					w.log.Err(sendErr).Int64("alert_id", a.ID).Msg("outcomeai: follow-up send failed")
				}
			} else {
				w.log.Err(err).Int64("alert_id", a.ID).Msg("outcomeai: edit failed")
			}
		}
	} else {
		// No message id on record (older alert / lost message id) —
		// linked follow-up only.
		if _, err := w.bot.SendHTML(ctx, w.cfg.ChatID, renderFollowupBody(f, a, out)); err != nil {
			w.log.Err(err).Int64("alert_id", a.ID).Msg("outcomeai: follow-up send failed")
		}
	}

	// Reaction. We always try — the failure mode is well-handled
	// (ErrReactionUnsupported terminates silently; transient
	// errors are retryable but we don't retry here because the
	// edit/follow-up already carries the result text).
	if a.TelegramMessageID != nil && *a.TelegramMessageID > 0 {
		emoji := w.pickReaction(string(a.OutcomeStatus))
		if emoji != "" {
			if err := w.bot.SetMessageReaction(ctx, w.cfg.ChatID, *a.TelegramMessageID, emoji); err != nil {
				w.log.Err(err).Int64("alert_id", a.ID).Msg("outcomeai: reaction failed")
			}
		}
	}
}

func (w *Worker) pickReaction(outcomeStatus string) string {
	switch outcomeStatus {
	case "resolved_correct":
		return w.cfg.SuccessReaction
	case "resolved_wrong":
		return w.cfg.FailureReaction
	}
	return ""
}

// buildOutcomeRequest projects a persisted alert + its Finding into
// the analyzer's structured request shape. Only fields that are
// available go into the prompt; missing ones (e.g. clv_*) are 0,
// which the prompt builder elides.
func buildOutcomeRequest(a repository.Alert, f anomaly.Finding) analysis.OutcomeAnalysisRequest {
	req := analysis.OutcomeAnalysisRequest{
		AlertID:        a.ID,
		Kind:           string(a.Kind),
		Severity:       a.Severity,
		OutcomeStatus:  string(a.OutcomeStatus),
		Reasons:        f.Reasons,
		Confidence:     0,
		WinningOutcome: a.WinningOutcomeLabel,
		ResolvedAt:     a.ResolvedAt,
	}
	if f.Trade != nil {
		req.Title = f.Trade.Question
		req.OutcomeLabel = f.Trade.Outcome
		req.NotionalUSD = f.Trade.NotionalUSD
		req.Probability = f.Trade.Price
	}
	if f.StableFavorite != nil {
		if req.Title == "" {
			req.Title = "stable-favorite alert"
		}
		req.Probability = f.StableFavorite.Probability
		req.Score = f.StableFavorite.Score
		req.Confidence = f.StableFavorite.Confidence
		req.RemainingReturnPct = f.StableFavorite.RemainingReturnPct
	}
	if f.Category != nil {
		req.Category = f.Category.Label
	}
	req.ProfitIfWinUSD = f.ProfitIfWinUSD
	if a.CLV15m != nil {
		req.CLV15m = *a.CLV15m
	}
	if a.CLV1h != nil {
		req.CLV1h = *a.CLV1h
	}
	if a.CLV6h != nil {
		req.CLV6h = *a.CLV6h
	}
	if a.CLV24h != nil {
		req.CLV24h = *a.CLV24h
	}
	return req
}

// renderUpdatedBody appends "Why WON" / "Why LOST" + "Expected by
// Watchtower: …" to the original alert's body. Edits replace the
// entire text, so we compose [original ── resolution] in one shot.
//
// Note: we DON'T re-call the alerting formatter on the Finding —
// that would re-render the AI analyst note + render the alert with
// updated AnalystNote which could differ from what was originally
// shown. Operators should see the SAME text they were paged with,
// PLUS the resolution. So we use a marker-based "header + resolution"
// shape that the operator can scan top-down.
func renderUpdatedBody(f anomaly.Finding, a repository.Alert, out analysis.OutcomeAnalysis) string {
	var b strings.Builder
	// Brief header that names the alert + carries the resolution
	// verdict. We don't include the full original because Telegram
	// edits are length-bound; the operator can scroll to the
	// original via the message thread.
	b.WriteString(headerForFinding(f))
	b.WriteString("\n")
	b.WriteString(resolutionBlock(a, out))
	return b.String()
}

func renderFollowupBody(f anomaly.Finding, a repository.Alert, out analysis.OutcomeAnalysis) string {
	var b strings.Builder
	b.WriteString("<b>Alert resolution follow-up</b>\n")
	b.WriteString(headerForFinding(f))
	b.WriteString("\n")
	b.WriteString(resolutionBlock(a, out))
	return b.String()
}

func headerForFinding(f anomaly.Finding) string {
	title := ""
	if f.Trade != nil && f.Trade.Question != "" {
		title = f.Trade.Question
	} else if f.StableFavorite != nil {
		title = "Stable favorite alert"
	} else {
		title = "Alert"
	}
	return fmt.Sprintf("<b>%s · %s</b>", strings.ToUpper(string(f.Severity)), htmlEscape(title))
}

func resolutionBlock(a repository.Alert, out analysis.OutcomeAnalysis) string {
	var b strings.Builder
	switch a.OutcomeStatus {
	case "resolved_correct":
		b.WriteString("\n<b>Why WON</b>\n")
	case "resolved_wrong":
		b.WriteString("\n<b>Why LOST</b>\n")
	default:
		b.WriteString("\n<b>Resolution</b>\n")
	}
	reason := strings.TrimSpace(out.ReasonText)
	if reason == "" {
		reason = "No AI postmortem available."
	}
	for i, line := range strings.Split(reason, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if i == 0 {
			fmt.Fprintf(&b, "• %s\n", htmlEscape(line))
			continue
		}
		fmt.Fprintf(&b, "  %s\n", htmlEscape(line))
	}
	if out.LessonsText != "" {
		b.WriteString("\n<b>Lessons</b>\n")
		for _, line := range strings.Split(out.LessonsText, "\n") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "-"))
			if line == "" {
				continue
			}
			fmt.Fprintf(&b, "• %s\n", htmlEscape(line))
		}
	}
	exp := "uncertain"
	if out.WonExpected != nil {
		if *out.WonExpected {
			exp = "yes"
		} else {
			exp = "no"
		}
	}
	fmt.Fprintf(&b, "\nExpected by Watchtower: %s\n", exp)
	return b.String()
}

// htmlEscape avoids importing the html stdlib in this small module.
// We replace the four characters Telegram's HTML mode actually
// requires escaping.
func htmlEscape(s string) string {
	r := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		`"`, "&quot;",
	)
	return r.Replace(s)
}
