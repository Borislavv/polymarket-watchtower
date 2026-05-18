package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Borislavv/polymarket-watchtower/internal/infra/postgres/sqlc"
)

// AlertStatus is the typed string used by polymarket_alerts.status.
type AlertStatus string

const (
	AlertPending AlertStatus = "pending"
	AlertSending AlertStatus = "sending"
	AlertSent    AlertStatus = "sent"
	AlertFailed  AlertStatus = "failed"
)

// AlertKind is the typed string used by polymarket_alerts.kind.
type AlertKind string

const (
	AlertKindTrade   AlertKind = "trade_anomaly"
	AlertKindCluster AlertKind = "category_watch"
)

// Alert is the repository view of a polymarket_alerts row.
type Alert struct {
	ID                int64
	DedupKey          string
	StrategyVersion   string
	Kind              AlertKind
	Reason            string
	Severity          string
	MarketID          *int64
	TraderID          *int64
	TradeID           *int64
	Payload           []byte
	Status            AlertStatus
	TelegramMessageID *int64
	SendAttempts      int32
	LastSendError     string
	SentAt            time.Time
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// NewAlert is the per-insert input for TryCreatePending. The repo derives
// nothing here — the caller supplies a fully-formed dedup key (see
// `cluster:<v>:…` and `single:<v>:…` formats in doc/persistence.md).
type NewAlert struct {
	DedupKey        string
	StrategyVersion string
	Kind            AlertKind
	Reason          string
	Severity        string
	MarketID        *int64
	TraderID        *int64
	TradeID         *int64
	Payload         []byte
}

// AlertRepository owns reads and writes for polymarket_alerts.
type AlertRepository struct {
	q *sqlc.Queries
}

func NewAlertRepository(pool *pgxpool.Pool) *AlertRepository {
	return &AlertRepository{q: sqlc.New(pool)}
}

// TryCreatePending is the dedup primitive. Returns (alert, true, nil)
// when this caller inserted a fresh row; (zero, false, nil) when the
// dedup_key already exists. Errors are real DB errors only.
//
// Callers map `created=false` to "skip send" — the previous owner of
// this dedup_key has either already sent the alert or is about to.
func (r *AlertRepository) TryCreatePending(ctx context.Context, a NewAlert) (Alert, bool, error) {
	row, err := r.q.TryCreatePendingAlert(ctx, sqlc.TryCreatePendingAlertParams{
		DedupKey:        a.DedupKey,
		StrategyVersion: a.StrategyVersion,
		Kind:            string(a.Kind),
		Reason:          a.Reason,
		Severity:        a.Severity,
		MarketID:        a.MarketID,
		TraderID:        a.TraderID,
		TradeID:         a.TradeID,
		Payload:         a.Payload,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// ON CONFLICT DO NOTHING + RETURNING → no row when conflict.
			return Alert{}, false, nil
		}
		return Alert{}, false, fmt.Errorf("try create alert %q: %w", a.DedupKey, err)
	}
	return alertFromSQLC(row), true, nil
}

// ClaimPending atomically transitions up to `limit` pending alerts into
// the `sending` state and returns them. The transition uses the standard
// queue-table pattern: UPDATE … WHERE id IN (SELECT … FOR UPDATE SKIP
// LOCKED). Once flipped to `sending`, a row is invisible to any
// subsequent ClaimPending until MarkSent / MarkFailed / ResetStaleSending
// advances it — so concurrent senders cannot double-send.
func (r *AlertRepository) ClaimPending(ctx context.Context, limit int32) ([]Alert, error) {
	rows, err := r.q.ClaimPendingAlertsForSend(ctx, limit)
	if err != nil {
		return nil, fmt.Errorf("claim pending alerts: %w", err)
	}
	out := make([]Alert, 0, len(rows))
	for _, row := range rows {
		out = append(out, alertFromSQLC(row))
	}
	return out, nil
}

// MarkSent records a successful delivery.
func (r *AlertRepository) MarkSent(ctx context.Context, id int64, telegramMessageID int64) error {
	msgID := telegramMessageID
	return r.q.MarkAlertSent(ctx, sqlc.MarkAlertSentParams{
		ID:                id,
		TelegramMessageID: &msgID,
	})
}

// MarkFailed records a failed delivery without flipping status; the row
// stays pending so the next sender tick can retry. The attempt counter
// bumps so callers can implement bounded-retry policies above the repo.
func (r *AlertRepository) MarkFailed(ctx context.Context, id int64, errMsg string) error {
	return r.q.MarkAlertSendFailed(ctx, sqlc.MarkAlertSendFailedParams{
		ID:            id,
		LastSendError: strPtr(errMsg),
	})
}

// Exists reports whether an alert with the given dedup_key already exists.
// Cheaper than TryCreatePending when the caller just wants to short-
// circuit alert building before computing the full payload.
func (r *AlertRepository) Exists(ctx context.Context, dedupKey string) (bool, error) {
	got, err := r.q.AlertExistsByDedupKey(ctx, dedupKey)
	if err != nil {
		return false, fmt.Errorf("alert exists %q: %w", dedupKey, err)
	}
	return got, nil
}

// ResetStaleSending recovers alerts wedged in the `sending` state by a
// crashed previous sender. Any row whose updated_at predates `cutoff` is
// moved back to `pending` so the next ClaimPending tick re-issues it.
func (r *AlertRepository) ResetStaleSending(ctx context.Context, cutoff time.Time) error {
	return r.q.ResetStaleSendingAlerts(ctx, tsFromTime(cutoff))
}

// LatestClusterForMarket returns the most recent cluster alert for the
// given market under the supplied strategy version. Used by the cluster
// cooldown gate. Returns (Alert{}, false, nil) when none exists.
func (r *AlertRepository) LatestClusterForMarket(ctx context.Context, marketID int64, strategyVersion string) (Alert, bool, error) {
	id := marketID
	row, err := r.q.LatestClusterAlertForCategory(ctx, sqlc.LatestClusterAlertForCategoryParams{
		MarketID:        &id,
		StrategyVersion: strategyVersion,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Alert{}, false, nil
		}
		return Alert{}, false, fmt.Errorf("latest cluster alert: %w", err)
	}
	return alertFromSQLC(row), true, nil
}

func alertFromSQLC(row sqlc.PolymarketAlerts) Alert {
	return Alert{
		ID:                row.ID,
		DedupKey:          row.DedupKey,
		StrategyVersion:   row.StrategyVersion,
		Kind:              AlertKind(row.Kind),
		Reason:            row.Reason,
		Severity:          row.Severity,
		MarketID:          row.MarketID,
		TraderID:          row.TraderID,
		TradeID:           row.TradeID,
		Payload:           row.Payload,
		Status:            AlertStatus(row.Status),
		TelegramMessageID: row.TelegramMessageID,
		SendAttempts:      row.SendAttempts,
		LastSendError:     derefStr(row.LastSendError),
		SentAt:            tsTime(row.SentAt),
		CreatedAt:         row.CreatedAt.Time,
		UpdatedAt:         row.UpdatedAt.Time,
	}
}
