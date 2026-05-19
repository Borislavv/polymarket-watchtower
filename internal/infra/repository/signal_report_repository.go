// Package repository — signal_report_repository.go owns persistence
// for the scheduled signal-quality reports.
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

// SignalReportRepository wraps the sqlc-generated queries on the
// polymarket_signal_reports table.
type SignalReportRepository struct {
	q *sqlc.Queries
}

func NewSignalReportRepository(pool *pgxpool.Pool) *SignalReportRepository {
	return &SignalReportRepository{q: sqlc.New(pool)}
}

// SignalQualityRow is the per-period roll-up used by the renderer. All
// counts are bounded to a single (period_start, period_end) window.
type SignalQualityRow struct {
	TotalAlerts         int64
	SuccessCount        int64
	FailureCount        int64
	AmbiguousCount      int64
	UnavailableCount    int64
	PendingCount        int64
	AvgCLV24h           float64
	PositiveCLV24hCount int64
	CLV24hSampleCount   int64
}

// SignalQualityBreakdownRow is one row in a by-kind / by-severity
// breakdown.
type SignalQualityBreakdownRow struct {
	Label      string // "single_trade" / "info" / etc.
	Total      int64
	Success    int64
	Failure    int64
	Unresolved int64
}

// SignalQualityAggregate computes the period-wide totals.
func (r *SignalReportRepository) SignalQualityAggregate(ctx context.Context, periodStart, periodEnd time.Time) (SignalQualityRow, error) {
	row, err := r.q.SignalQualityAggregate(ctx, sqlc.SignalQualityAggregateParams{
		PeriodStart: tsFromTime(periodStart),
		PeriodEnd:   tsFromTime(periodEnd),
	})
	if err != nil {
		return SignalQualityRow{}, fmt.Errorf("signal quality aggregate: %w", err)
	}
	return SignalQualityRow{
		TotalAlerts:         row.TotalAlerts,
		SuccessCount:        row.SuccessCount,
		FailureCount:        row.FailureCount,
		AmbiguousCount:      row.AmbiguousCount,
		UnavailableCount:    row.UnavailableCount,
		PendingCount:        row.PendingCount,
		AvgCLV24h:           row.AvgClv24h,
		PositiveCLV24hCount: row.PositiveClv24hCount,
		CLV24hSampleCount:   row.Clv24hSampleCount,
	}, nil
}

// SignalQualityByKind returns the per-alert-kind breakdown.
func (r *SignalReportRepository) SignalQualityByKind(ctx context.Context, periodStart, periodEnd time.Time) ([]SignalQualityBreakdownRow, error) {
	rows, err := r.q.SignalQualityByKind(ctx, sqlc.SignalQualityByKindParams{
		PeriodStart: tsFromTime(periodStart),
		PeriodEnd:   tsFromTime(periodEnd),
	})
	if err != nil {
		return nil, fmt.Errorf("signal quality by kind: %w", err)
	}
	out := make([]SignalQualityBreakdownRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, SignalQualityBreakdownRow{Label: r.Kind, Total: r.Total, Success: r.Success, Failure: r.Failure, Unresolved: r.Unresolved})
	}
	return out, nil
}

// SignalQualityBySeverity returns the per-severity breakdown.
func (r *SignalReportRepository) SignalQualityBySeverity(ctx context.Context, periodStart, periodEnd time.Time) ([]SignalQualityBreakdownRow, error) {
	rows, err := r.q.SignalQualityBySeverity(ctx, sqlc.SignalQualityBySeverityParams{
		PeriodStart: tsFromTime(periodStart),
		PeriodEnd:   tsFromTime(periodEnd),
	})
	if err != nil {
		return nil, fmt.Errorf("signal quality by severity: %w", err)
	}
	out := make([]SignalQualityBreakdownRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, SignalQualityBreakdownRow{Label: r.Severity, Total: r.Total, Success: r.Success, Failure: r.Failure, Unresolved: r.Unresolved})
	}
	return out, nil
}

// SignalReport is the persistence view of one polymarket_signal_reports
// row. Used by the scheduler at startup to determine "have we already
// sent this period's report?".
type SignalReport struct {
	ID                int64
	PeriodType        string
	PeriodStart       time.Time
	PeriodEnd         time.Time
	ScheduledAt       time.Time
	SentAt            time.Time
	Status            string
	TelegramMessageID *int64
	LastError         string
	DedupKey          string
}

// TryCreateSignalReportPending inserts a pending row keyed by
// dedup_key. Returns (id, true) on insert, (0, false) on conflict.
func (r *SignalReportRepository) TryCreateSignalReportPending(
	ctx context.Context,
	periodType, dedupKey string,
	periodStart, periodEnd, scheduledAt time.Time,
) (int64, bool, error) {
	id, err := r.q.TryCreatePendingSignalReport(ctx, sqlc.TryCreatePendingSignalReportParams{
		PeriodType:  periodType,
		PeriodStart: tsFromTime(periodStart),
		PeriodEnd:   tsFromTime(periodEnd),
		ScheduledAt: tsFromTime(scheduledAt),
		Payload:     nil,
		DedupKey:    dedupKey,
	})
	switch {
	case err == nil:
		return id, true, nil
	case errors.Is(err, pgx.ErrNoRows):
		// ON CONFLICT DO NOTHING — another worker already inserted.
		return 0, false, nil
	default:
		return 0, false, fmt.Errorf("try create signal report: %w", err)
	}
}

// MarkSignalReportSent finalizes a successful send.
func (r *SignalReportRepository) MarkSignalReportSent(ctx context.Context, id int64, telegramMessageID int64) error {
	msgID := telegramMessageID
	return r.q.MarkSignalReportSent(ctx, sqlc.MarkSignalReportSentParams{
		ID:                id,
		TelegramMessageID: &msgID,
	})
}

// MarkSignalReportFailed records a send error.
func (r *SignalReportRepository) MarkSignalReportFailed(ctx context.Context, id int64, lastError string) error {
	return r.q.MarkSignalReportFailed(ctx, sqlc.MarkSignalReportFailedParams{
		ID: id, LastError: lastError,
	})
}

// LatestSignalReportByPeriodType returns the most recent row of the
// given period_type, or (zero, false, nil) when none exists.
func (r *SignalReportRepository) LatestSignalReportByPeriodType(ctx context.Context, periodType string) (SignalReport, bool, error) {
	row, err := r.q.LatestSignalReportByPeriodType(ctx, periodType)
	switch {
	case err == nil:
		return SignalReport{
			ID:                row.ID,
			PeriodType:        row.PeriodType,
			PeriodStart:       tsTime(row.PeriodStart),
			PeriodEnd:         tsTime(row.PeriodEnd),
			ScheduledAt:       tsTime(row.ScheduledAt),
			SentAt:            tsTime(row.SentAt),
			Status:            row.Status,
			TelegramMessageID: row.TelegramMessageID,
			LastError:         derefStr(row.LastError),
			DedupKey:          row.DedupKey,
		}, true, nil
	case errors.Is(err, pgx.ErrNoRows):
		return SignalReport{}, false, nil
	default:
		return SignalReport{}, false, fmt.Errorf("latest signal report: %w", err)
	}
}
