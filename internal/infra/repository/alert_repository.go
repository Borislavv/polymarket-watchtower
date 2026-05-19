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
	AlertKindTrade        AlertKind = "trade_anomaly"
	AlertKindCluster      AlertKind = "category_watch"
	AlertKindAccumulation AlertKind = "accumulation"
	// AlertKindOwnership is Strategy E — market-ownership concentration
	// approximated from trade-flow share counts. CHECK constraint added
	// in migration 00006.
	AlertKindOwnership AlertKind = "ownership_concentration"
	// AlertKindStableFavorite is the late-market-convergence
	// strategy. Emitted by internal/app/usecase/stablefavorite.
	AlertKindStableFavorite AlertKind = "stable_favorite"
)

// OutcomeStatus is the typed string used by polymarket_alerts.outcome_status.
type OutcomeStatus string

const (
	OutcomePending     OutcomeStatus = "pending"
	OutcomeCorrect     OutcomeStatus = "resolved_correct"
	OutcomeWrong       OutcomeStatus = "resolved_wrong"
	OutcomeUnknown     OutcomeStatus = "unknown"
	OutcomeUnavailable OutcomeStatus = "unavailable"
)

// DriftStatus is the typed string used by polymarket_alerts.drift_status.
type DriftStatus string

const (
	DriftPending     DriftStatus = "pending"
	DriftAvailable   DriftStatus = "available"
	DriftUnavailable DriftStatus = "unavailable"
)

// Alert is the repository view of a polymarket_alerts row.
type Alert struct {
	ID                  int64
	DedupKey            string
	StrategyVersion     string
	Kind                AlertKind
	Reason              string
	Severity            string
	MarketID            *int64
	TraderID            *int64
	TradeID             *int64
	Payload             []byte
	Status              AlertStatus
	TelegramMessageID   *int64
	SendAttempts        int32
	LastSendError       string
	NextRetryAt         time.Time
	LastAttemptAt       time.Time
	SentAt              time.Time
	OutcomeStatus       OutcomeStatus
	OutcomeCheckedAt    time.Time
	ResolvedAt          time.Time
	WinningOutcomeToken string
	WinningOutcomeLabel string
	DriftStatus         DriftStatus
	DriftCheckedAt      time.Time
	CLV15m              *float64
	CLV1h               *float64
	CLV6h               *float64
	CLV24h              *float64
	CreatedAt           time.Time
	UpdatedAt           time.Time

	// Reaction state (migration 00007). ReactionStatus defaults to
	// `pending` for every freshly-inserted alert.
	ReactionStatus ReactionStatus
	ReactionEmoji  string
	LastReactionAt time.Time
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

// MarkFailed records a failed delivery and schedules (or exhausts) the
// retry. The caller computes nextRetryAt via the retry policy (exponential
// backoff + jitter); pass time.Time{} to signal "exhausted / no retry".
// Status flips to 'failed' regardless — the claim query picks up
// retryable failures on subsequent ticks.
func (r *AlertRepository) MarkFailed(ctx context.Context, id int64, errMsg string, nextRetryAt time.Time) error {
	return r.q.MarkAlertSendFailed(ctx, sqlc.MarkAlertSendFailedParams{
		ID:            id,
		LastSendError: strPtr(errMsg),
		NextRetryAt:   tsFromTime(nextRetryAt),
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

// OutcomeUpdate carries the outcome verdict supplied by the outcomes worker.
type OutcomeUpdate struct {
	AlertID             int64
	Status              OutcomeStatus
	ResolvedAt          time.Time
	WinningOutcomeToken string
	WinningOutcomeLabel string
}

// DriftUpdate carries the CLV-lite values supplied by the drift worker.
// Each *float64 is nil when the reference price for that window was
// unavailable (no later trade on the same market+outcome).
type DriftUpdate struct {
	AlertID int64
	Status  DriftStatus
	CLV15m  *float64
	CLV1h   *float64
	CLV6h   *float64
	CLV24h  *float64
}

// ListSentForOutcomeCheck returns sent alerts whose markets are
// resolved-or-close-enough and whose outcome verdict is still pending.
func (r *AlertRepository) ListSentForOutcomeCheck(ctx context.Context, claimLimit int32) ([]Alert, error) {
	rows, err := r.q.ListSentAlertsForOutcomeCheck(ctx, claimLimit)
	if err != nil {
		return nil, fmt.Errorf("list sent alerts for outcome: %w", err)
	}
	out := make([]Alert, 0, len(rows))
	for _, row := range rows {
		out = append(out, alertFromSQLC(row))
	}
	return out, nil
}

// MarkOutcome persists the verdict computed by the outcomes worker.
func (r *AlertRepository) MarkOutcome(ctx context.Context, u OutcomeUpdate) error {
	return r.q.MarkAlertOutcome(ctx, sqlc.MarkAlertOutcomeParams{
		ID:                  u.AlertID,
		OutcomeStatus:       string(u.Status),
		ResolvedAt:          tsFromTime(u.ResolvedAt),
		WinningOutcomeToken: strPtr(u.WinningOutcomeToken),
		WinningOutcomeLabel: strPtr(u.WinningOutcomeLabel),
	})
}

// TouchOutcomeUnavailable bumps outcome_checked_at without changing the
// verdict — used when the upstream check failed transiently and we want
// the row to come up again on the next tick.
func (r *AlertRepository) TouchOutcomeUnavailable(ctx context.Context, alertID int64) error {
	return r.q.MarkAlertOutcomeUnavailableTouch(ctx, alertID)
}

// ReactionStatus is the typed string used by
// polymarket_alerts.telegram_reaction_status. Defined by migration 00007.
type ReactionStatus string

const (
	ReactionPending     ReactionStatus = "pending"
	ReactionApplied     ReactionStatus = "applied"
	ReactionUnsupported ReactionStatus = "unsupported"
	ReactionFailed      ReactionStatus = "failed"
	ReactionDisabled    ReactionStatus = "disabled"
)

// ListAlertsForReaction returns sent alerts whose outcome is known and
// whose Telegram reaction state is pending (or previously failed and
// thus retry-eligible). The query is backed by
// idx_alerts_reaction_pending so the partial-index scan stays small as
// the alerts table grows.
func (r *AlertRepository) ListAlertsForReaction(ctx context.Context, claimLimit int32) ([]Alert, error) {
	rows, err := r.q.ListAlertsForReaction(ctx, claimLimit)
	if err != nil {
		return nil, fmt.Errorf("list alerts for reaction: %w", err)
	}
	out := make([]Alert, 0, len(rows))
	for _, row := range rows {
		out = append(out, alertFromSQLC(row))
	}
	return out, nil
}

// MarkReaction stamps the setMessageReaction outcome on the alert row.
// status MUST be one of the constants above. emoji is persisted as-is
// when status=ReactionApplied; for other statuses it is informational.
func (r *AlertRepository) MarkReaction(ctx context.Context, alertID int64, status ReactionStatus, emoji string) error {
	return r.q.MarkAlertReactionApplied(ctx, sqlc.MarkAlertReactionAppliedParams{
		ID:     alertID,
		Status: string(status),
		Emoji:  strPtr(emoji),
	})
}

// ListSentForDrift returns sent alerts whose drift is still pending and
// whose oldest reference window (`minWindow`, typically 15m) has elapsed.
func (r *AlertRepository) ListSentForDrift(ctx context.Context, minWindow time.Duration, claimLimit int32) ([]Alert, error) {
	rows, err := r.q.ListSentAlertsForDrift(ctx, sqlc.ListSentAlertsForDriftParams{
		MinWindow:  intervalFromDuration(minWindow),
		ClaimLimit: claimLimit,
	})
	if err != nil {
		return nil, fmt.Errorf("list sent alerts for drift: %w", err)
	}
	out := make([]Alert, 0, len(rows))
	for _, row := range rows {
		out = append(out, alertFromSQLC(row))
	}
	return out, nil
}

// MarkDrift persists the CLV-lite values computed by the drift worker.
func (r *AlertRepository) MarkDrift(ctx context.Context, u DriftUpdate) error {
	return r.q.MarkAlertDrift(ctx, sqlc.MarkAlertDriftParams{
		ID:          u.AlertID,
		DriftStatus: string(u.Status),
		Clv15m:      u.CLV15m,
		Clv1h:       u.CLV1h,
		Clv6h:       u.CLV6h,
		Clv24h:      u.CLV24h,
	})
}

// TradePriceAtOrAfter returns the price of the first trade on the
// supplied (market, outcome) bucket at or after `at`. Returns
// (0, false, nil) when no later trade exists yet — the drift worker
// treats that as "reference price unavailable".
func (r *TradeRepository) TradePriceAtOrAfter(ctx context.Context, marketID int64, outcomeToken string, at time.Time) (float64, bool, error) {
	price, err := r.q.TradePriceAtOrAfter(ctx, sqlc.TradePriceAtOrAfterParams{
		MarketID:     marketID,
		OutcomeToken: outcomeToken,
		AtOrAfter:    tsFromTime(at),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("trade price at-or-after: %w", err)
	}
	return price, true, nil
}

func alertFromSQLC(row sqlc.PolymarketAlerts) Alert {
	return Alert{
		ID:                  row.ID,
		DedupKey:            row.DedupKey,
		StrategyVersion:     row.StrategyVersion,
		Kind:                AlertKind(row.Kind),
		Reason:              row.Reason,
		Severity:            row.Severity,
		MarketID:            row.MarketID,
		TraderID:            row.TraderID,
		TradeID:             row.TradeID,
		Payload:             row.Payload,
		Status:              AlertStatus(row.Status),
		TelegramMessageID:   row.TelegramMessageID,
		SendAttempts:        row.SendAttempts,
		LastSendError:       derefStr(row.LastSendError),
		NextRetryAt:         tsTime(row.NextRetryAt),
		LastAttemptAt:       tsTime(row.LastAttemptAt),
		SentAt:              tsTime(row.SentAt),
		OutcomeStatus:       OutcomeStatus(row.OutcomeStatus),
		OutcomeCheckedAt:    tsTime(row.OutcomeCheckedAt),
		ResolvedAt:          tsTime(row.ResolvedAt),
		WinningOutcomeToken: derefStr(row.WinningOutcomeToken),
		WinningOutcomeLabel: derefStr(row.WinningOutcomeLabel),
		DriftStatus:         DriftStatus(row.DriftStatus),
		DriftCheckedAt:      tsTime(row.DriftCheckedAt),
		CLV15m:              row.Clv15m,
		CLV1h:               row.Clv1h,
		CLV6h:               row.Clv6h,
		CLV24h:              row.Clv24h,
		CreatedAt:           row.CreatedAt.Time,
		UpdatedAt:           row.UpdatedAt.Time,
		ReactionStatus:      ReactionStatus(row.TelegramReactionStatus),
		ReactionEmoji:       derefStr(row.TelegramReactionEmoji),
		LastReactionAt:      tsTime(row.LastReactionAt),
	}
}
