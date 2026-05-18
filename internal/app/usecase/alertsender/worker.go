// Package alertsender owns the Telegram delivery pipeline. It does ONE job:
// claim pending alert rows from polymarket_alerts and post them to
// Telegram, then mark each row sent or failed.
//
// Why this is its own worker (not synchronous from detect):
//   - Telegram is a flaky external dependency. A 5-second timeout in the
//     detection hot path stalls a collect tick.
//   - The DB queue lets us survive restarts: pending rows are picked up
//     by whichever sender instance runs next. Combined with the alerts
//     dedup_key UNIQUE constraint, no alert is ever sent twice.
//   - Multiple senders can run in parallel without coordination because
//     ClaimPending uses FOR UPDATE SKIP LOCKED.
package alertsender

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/anomaly"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/alerting"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/metrics"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/repository"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/telegram"
)

// AlertStore is the subset of *repository.AlertRepository the sender uses.
// Abstracted so the tests can plug in an in-memory fake.
type AlertStore interface {
	ClaimPending(ctx context.Context, limit int32) ([]repository.Alert, error)
	MarkSent(ctx context.Context, id int64, telegramMessageID int64) error
	MarkFailed(ctx context.Context, id int64, errMsg string) error
	ResetStaleSending(ctx context.Context, cutoff time.Time) error
}

// Telegram is the transport contract — internal/infra/telegram.Bot
// satisfies it. The sender depends on the interface, never the concrete
// type, so tests can fake the network round-trip and so the Telegram
// package can evolve without dragging alertsender along.
type Telegram interface {
	SendHTML(ctx context.Context, chatID, text string) (telegram.SendResult, error)
}

// Config tunes the sender.
type Config struct {
	// Interval is the claim cadence. Default 5s.
	Interval time.Duration
	// ClaimLimit caps how many pending rows are pulled per tick. Default 16.
	ClaimLimit int32
	// Workers is the number of parallel sender goroutines per tick. Each
	// worker claims its own batch via the atomic UPDATE … FOR UPDATE SKIP
	// LOCKED queue pattern, so contention is zero. Default 1.
	Workers int
	// ChatID is the single Telegram recipient.
	ChatID string
	// StaleSendingAfter is the cutoff for ResetStaleSending: rows stuck in
	// the transient `sending` state for longer than this are returned to
	// `pending` so a crashed previous process doesn't wedge them. Default
	// 5m. Must be > the longest plausible Telegram round-trip + claim
	// interval.
	StaleSendingAfter time.Duration
	// Clock optionally overrides time.Now (tests).
	Clock func() time.Time
}

// Worker is the long-running sender loop.
type Worker struct {
	cfg     Config
	store   AlertStore
	tg      Telegram
	metrics *metrics.Metrics
	log     *zerolog.Logger
	now     func() time.Time
}

// New wires the worker. Telegram and the store are required; metrics is
// optional (per-severity send counters).
func New(cfg Config, store AlertStore, tg Telegram, m *metrics.Metrics, log *zerolog.Logger) *Worker {
	if cfg.Interval <= 0 {
		cfg.Interval = 5 * time.Second
	}
	if cfg.ClaimLimit <= 0 {
		cfg.ClaimLimit = 16
	}
	if cfg.Workers <= 0 {
		cfg.Workers = 1
	}
	if cfg.StaleSendingAfter <= 0 {
		cfg.StaleSendingAfter = 5 * time.Minute
	}
	now := cfg.Clock
	if now == nil {
		now = time.Now
	}
	return &Worker{cfg: cfg, store: store, tg: tg, metrics: m, log: log, now: now}
}

// Run blocks until ctx is cancelled. Fires an initial drain immediately
// so alerts queued during startup don't wait one full interval.
func (w *Worker) Run(ctx context.Context) error {
	if w.cfg.ChatID == "" {
		// Sender wired but no recipient. Treat as soft-disabled: the row
		// remains pending; the operator can fix config without losing
		// alerts.
		w.log.Warn().Msg("alertsender: no chat id configured, idling")
		<-ctx.Done()
		return nil
	}
	t := time.NewTicker(w.cfg.Interval)
	defer t.Stop()
	w.drain(ctx)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			w.drain(ctx)
		}
	}
}

// Drain runs one claim-and-send pass; exposed for tests.
func (w *Worker) Drain(ctx context.Context) { w.drain(ctx) }

func (w *Worker) drain(ctx context.Context) {
	// Recover any rows wedged in `sending` by a crashed previous process
	// before claiming new work. Cheap when no rows match.
	if err := w.store.ResetStaleSending(ctx, w.now().Add(-w.cfg.StaleSendingAfter)); err != nil {
		w.log.Err(err).Msg("alertsender: reset stale sending failed")
	}
	var wg sync.WaitGroup
	for i := 0; i < w.cfg.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.drainOne(ctx)
		}()
	}
	wg.Wait()
}

func (w *Worker) drainOne(ctx context.Context) {
	alerts, err := w.store.ClaimPending(ctx, w.cfg.ClaimLimit)
	if err != nil {
		w.log.Err(err).Msg("alertsender: claim failed")
		return
	}
	for _, a := range alerts {
		if ctx.Err() != nil {
			return
		}
		w.send(ctx, a)
	}
}

func (w *Worker) send(ctx context.Context, a repository.Alert) {
	text, err := renderText(a)
	if err != nil {
		// A row that can't be rendered will keep failing forever; we still
		// record the failure but do not stop the worker.
		w.markFailed(ctx, a, fmt.Errorf("render: %w", err))
		return
	}
	res, err := w.tg.SendHTML(ctx, w.cfg.ChatID, text)
	if err != nil {
		// Context cancellation is graceful: leave the row pending. Any
		// other error bumps send_attempts and stays pending for retry.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return
		}
		w.markFailed(ctx, a, err)
		return
	}
	if err := w.store.MarkSent(ctx, a.ID, res.MessageID); err != nil {
		w.log.Err(err).Int64("alert_id", a.ID).Msg("alertsender: mark sent failed")
		return
	}
	w.observeOK(a.Severity)
}

func (w *Worker) markFailed(ctx context.Context, a repository.Alert, sendErr error) {
	w.log.Err(sendErr).Int64("alert_id", a.ID).Msg("alertsender: send failed")
	if err := w.store.MarkFailed(ctx, a.ID, sendErr.Error()); err != nil {
		w.log.Err(err).Int64("alert_id", a.ID).Msg("alertsender: mark failed update failed")
	}
	w.observeErr(a.Severity)
}

// renderText unmarshals the persisted Finding payload and renders the
// HTML message via the alerting package's formatter. The formatter is the
// single source of truth for message layout — the sender owns delivery,
// not composition.
func renderText(a repository.Alert) (string, error) {
	var f anomaly.Finding
	if err := json.Unmarshal(a.Payload, &f); err != nil {
		return "", err
	}
	return alerting.FormatTelegramMessage(f), nil
}

func (w *Worker) observeOK(severity string) {
	if w.metrics != nil {
		w.metrics.TelegramAlertsSent.WithLabelValues(severity).Inc()
	}
}

func (w *Worker) observeErr(severity string) {
	if w.metrics != nil {
		w.metrics.TelegramAlertErrors.WithLabelValues(severity).Inc()
	}
}
