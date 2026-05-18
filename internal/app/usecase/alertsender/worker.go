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
	"math/rand"
	"strings"
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
	// MarkFailed records a failed attempt. nextRetryAt is the wall-clock
	// time at which the row becomes eligible for re-claim. Pass a zero
	// time.Time to signal "exhausted / permanent failure" — the row will
	// stay in 'failed' state forever (until an operator intervenes).
	MarkFailed(ctx context.Context, id int64, errMsg string, nextRetryAt time.Time) error
	ResetStaleSending(ctx context.Context, cutoff time.Time) error
}

// Telegram is the transport contract — internal/infra/telegram.Bot
// satisfies it. The sender depends on the interface, never the concrete
// type, so tests can fake the network round-trip and so the Telegram
// package can evolve without dragging alertsender along.
type Telegram interface {
	SendHTML(ctx context.Context, chatID, text string) (telegram.SendResult, error)
}

// RetryPolicy controls how MarkFailed schedules subsequent attempts. Zero-
// value RetryPolicy disables retry entirely (every failure is permanent).
type RetryPolicy struct {
	// Enabled gates the entire retry behaviour. When false, MarkFailed is
	// always called with nextRetryAt=zero and the row stays in 'failed'.
	Enabled bool
	// MaxAttempts is the hard cap on total send attempts (including the
	// first). 0 → no retry. 1 → first attempt only. 5 (default) →
	// initial + 4 retries.
	MaxAttempts int
	// InitialBackoff is the first retry delay. Subsequent retries double
	// up to MaxBackoff. Default 30s.
	InitialBackoff time.Duration
	// MaxBackoff caps the per-retry delay. Default 30m.
	MaxBackoff time.Duration
	// JitterFraction is the +/- random fraction applied to each backoff
	// to spread concurrent retries. 0.2 = ±20%. Default 0.2.
	JitterFraction float64
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
	// Retry controls failed-alert retry. When Retry.Enabled is false the
	// worker behaves the same as the pre-v4 sender (every failure is
	// permanent).
	Retry RetryPolicy
	// Clock optionally overrides time.Now (tests).
	Clock func() time.Time
	// Rand optionally overrides the jitter source (tests).
	Rand *rand.Rand
}

// Worker is the long-running sender loop.
type Worker struct {
	cfg     Config
	store   AlertStore
	tg      Telegram
	metrics *metrics.Metrics
	log     *zerolog.Logger
	now     func() time.Time
	rng     *rand.Rand
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
	if cfg.Retry.Enabled {
		if cfg.Retry.MaxAttempts <= 0 {
			cfg.Retry.MaxAttempts = 5
		}
		if cfg.Retry.InitialBackoff <= 0 {
			cfg.Retry.InitialBackoff = 30 * time.Second
		}
		if cfg.Retry.MaxBackoff <= 0 {
			cfg.Retry.MaxBackoff = 30 * time.Minute
		}
		if cfg.Retry.JitterFraction <= 0 {
			cfg.Retry.JitterFraction = 0.2
		}
	}
	now := cfg.Clock
	if now == nil {
		now = time.Now
	}
	rng := cfg.Rand
	if rng == nil {
		rng = rand.New(rand.NewSource(time.Now().UnixNano()))
	}
	return &Worker{cfg: cfg, store: store, tg: tg, metrics: m, log: log, now: now, rng: rng}
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
	nextRetryAt, exhausted := w.scheduleRetry(a, sendErr)
	ev := w.log.Err(sendErr).Int64("alert_id", a.ID).Int32("attempts", a.SendAttempts+1)
	if exhausted {
		ev.Msg("alertsender: send failed (permanent / retry exhausted)")
	} else {
		ev.Time("next_retry_at", nextRetryAt).Msg("alertsender: send failed (will retry)")
	}
	if err := w.store.MarkFailed(ctx, a.ID, sendErr.Error(), nextRetryAt); err != nil {
		w.log.Err(err).Int64("alert_id", a.ID).Msg("alertsender: mark failed update failed")
	}
	w.observeErr(a.Severity)
}

// scheduleRetry computes the next retry time for a failed alert.
// Returns (zero, true) when the failure is permanent — retry disabled,
// max attempts reached, or the error is structurally unrecoverable
// (render error, Telegram HTML parse error). Otherwise returns the
// jittered exponential-backoff target and (target, false).
func (w *Worker) scheduleRetry(a repository.Alert, sendErr error) (time.Time, bool) {
	if !w.cfg.Retry.Enabled {
		return time.Time{}, true
	}
	if isPermanentError(sendErr) {
		return time.Time{}, true
	}
	// SendAttempts is the count BEFORE this attempt; the next claim sees
	// SendAttempts+1, so retry budget compares against that.
	nextAttemptNumber := a.SendAttempts + 1
	if int(nextAttemptNumber) >= w.cfg.Retry.MaxAttempts {
		return time.Time{}, true
	}
	delay := backoff(w.cfg.Retry, int(nextAttemptNumber), w.rng)
	return w.now().Add(delay), false
}

// backoff returns the unjittered exponential backoff for the given retry
// attempt index, capped at MaxBackoff, then applies a symmetric jitter
// of ±JitterFraction.
func backoff(p RetryPolicy, attempt int, rng *rand.Rand) time.Duration {
	// attempt=1 → initial; attempt=2 → 2×initial; …
	delay := p.InitialBackoff
	for i := 1; i < attempt; i++ {
		delay *= 2
		if delay > p.MaxBackoff {
			delay = p.MaxBackoff
			break
		}
	}
	if p.JitterFraction > 0 {
		// rng.Float64() in [0,1); shift to [-1,+1).
		jit := (rng.Float64()*2 - 1) * p.JitterFraction
		delay += time.Duration(float64(delay) * jit)
	}
	if delay < 0 {
		delay = 0
	}
	return delay
}

// isPermanentError reports whether the supplied transport error is
// structurally unrecoverable, so retry would just burn quota. The list is
// intentionally narrow — anything ambiguous is treated as transient.
//
// Permanent classes:
//   - HTML parse rejections (Telegram 400 "can't parse entities")
//   - JSON render errors from the local Finding payload (caller bug)
//   - "chat not found" and "bot kicked" — operator must fix config
func isPermanentError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	// Local payload bug: the persisted JSON cannot be deserialised. Will
	// never succeed without code/data change.
	if strings.HasPrefix(msg, "render:") {
		return true
	}
	for _, marker := range []string{
		"can't parse entities",   // Telegram HTML parse rejection
		"chat not found",         // bad chat id
		"bot was kicked",         // chat removed bot
		"bot is not a member",    // bot lacks permission
		"have no rights to send", // bot lacks permission
		"message is too long",    // payload exceeds Telegram limit
	} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
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
