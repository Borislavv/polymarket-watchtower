package signalreport

import (
	"context"
	"time"

	"github.com/rs/zerolog"
)

// Config wires the worker. SendAt maps period → HH:MM at which the
// report is due in the reporting timezone. YearlyDelay is added on top
// for the yearly report (the spec wants late-resolution settlement to
// settle, so 72h is the default).
type Config struct {
	Enabled      bool
	Location     *time.Location
	ChatID       string
	TickInterval time.Duration            // default 1m
	SendAt       map[PeriodType]TimeOfDay // per-period HH:MM
	YearlyDelay  time.Duration
	Clock        func() time.Time
}

func (c Config) applyDefaults() Config {
	if c.Location == nil {
		c.Location = time.UTC
	}
	if c.TickInterval <= 0 {
		c.TickInterval = time.Minute
	}
	if c.SendAt == nil {
		c.SendAt = map[PeriodType]TimeOfDay{}
	}
	for _, p := range AllPeriods {
		if _, ok := c.SendAt[p]; !ok {
			c.SendAt[p] = TimeOfDay{Hour: 8, Minute: 0}
		}
	}
	if c.Clock == nil {
		c.Clock = time.Now
	}
	return c
}

// Store is the persistence dependency the worker drives. Satisfied by
// *repository.SignalReportRepository.
type Store interface {
	SignalQualityFetcher
	TryCreateSignalReportPending(ctx context.Context, periodType, dedupKey string, periodStart, periodEnd, scheduledAt time.Time) (int64, bool, error)
	MarkSignalReportSent(ctx context.Context, id int64, telegramMessageID int64) error
	MarkSignalReportFailed(ctx context.Context, id int64, lastError string) error
}

// Sender posts the report body to Telegram. Returns the upstream
// message_id so the worker can persist it.
type Sender interface {
	SendHTML(ctx context.Context, chatID, text string) (int64, error)
}

// Metrics is the optional Prometheus surface. nil disables telemetry.
type Metrics interface {
	ObserveReportSent(periodType, status string)
}

// Worker is the long-running scheduler. One ticker drives all five
// period types; per-tick it evaluates each period and emits at most
// one report per period per tick.
type Worker struct {
	cfg     Config
	store   Store
	sender  Sender
	metrics Metrics
	log     *zerolog.Logger
}

// New constructs the worker.
func New(cfg Config, store Store, sender Sender, metrics Metrics, log *zerolog.Logger) *Worker {
	return &Worker{cfg: cfg.applyDefaults(), store: store, sender: sender, metrics: metrics, log: log}
}

// Run blocks until ctx is cancelled. The ticker fires every
// TickInterval; at each tick the worker evaluates whether ANY period
// is due (current time ≥ DueAt for that period's completed window AND
// the dedup_key insert succeeds).
func (w *Worker) Run(ctx context.Context) error {
	if !w.cfg.Enabled {
		w.log.Info().Msg("signalreport: disabled")
		return nil
	}
	t := time.NewTicker(w.cfg.TickInterval)
	defer t.Stop()
	w.tick(ctx) // immediate sweep on startup
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			w.tick(ctx)
		}
	}
}

// Tick runs one scheduler sweep; exposed for tests.
func (w *Worker) Tick(ctx context.Context) { w.tick(ctx) }

func (w *Worker) tick(ctx context.Context) {
	now := w.cfg.Clock()
	for _, p := range AllPeriods {
		if ctx.Err() != nil {
			return
		}
		w.evaluatePeriod(ctx, now, p)
	}
}

// evaluatePeriod is the per-period scheduler decision: compute the
// most recently completed window, decide if the report is due, and
// claim+send. Claim is idempotent via the dedup_key UNIQUE constraint.
func (w *Worker) evaluatePeriod(ctx context.Context, now time.Time, kind PeriodType) {
	window, err := CompletedWindow(now, kind, w.cfg.Location)
	if err != nil {
		w.log.Err(err).Str("period", string(kind)).Msg("signalreport: window math failed")
		return
	}
	delay := time.Duration(0)
	if kind == PeriodYearly {
		delay = w.cfg.YearlyDelay
	}
	due := DueAt(window, w.cfg.SendAt[kind], delay, w.cfg.Location)
	if now.Before(due) {
		return
	}
	dedup := DedupKey(kind, window)
	id, claimed, err := w.store.TryCreateSignalReportPending(ctx, string(kind), dedup, window.Start, window.End, due)
	if err != nil {
		w.log.Err(err).Str("period", string(kind)).Msg("signalreport: claim failed")
		return
	}
	if !claimed {
		// Another worker (or a previous tick) already inserted this
		// period's report row — idempotency wins.
		return
	}
	report, err := BuildReport(ctx, w.store, kind, window, now)
	if err != nil {
		w.log.Err(err).Str("period", string(kind)).Msg("signalreport: build failed")
		_ = w.store.MarkSignalReportFailed(ctx, id, err.Error())
		w.observe(kind, "failed")
		return
	}
	body := FormatTelegram(report)
	msgID, err := w.sender.SendHTML(ctx, w.cfg.ChatID, body)
	if err != nil {
		w.log.Err(err).Str("period", string(kind)).Msg("signalreport: send failed")
		_ = w.store.MarkSignalReportFailed(ctx, id, err.Error())
		w.observe(kind, "failed")
		return
	}
	if err := w.store.MarkSignalReportSent(ctx, id, msgID); err != nil {
		w.log.Err(err).Int64("id", id).Msg("signalreport: mark sent failed")
		// Don't undo the send — the message went out, the row is just
		// stuck in pending. The next tick won't double-send because
		// the dedup_key UNIQUE constraint still holds.
	}
	w.observe(kind, "sent")
	w.log.Info().
		Str("period", string(kind)).
		Time("period_start", window.Start).
		Time("period_end", window.End).
		Int64("message_id", msgID).
		Msg("signalreport: sent")
}

func (w *Worker) observe(kind PeriodType, status string) {
	if w.metrics != nil {
		w.metrics.ObserveReportSent(string(kind), status)
	}
}
