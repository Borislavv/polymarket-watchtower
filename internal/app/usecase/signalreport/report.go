package signalreport

import (
	"context"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/infra/repository"
)

// Report is the projected value the renderer consumes. Pure data —
// the aggregator builds it, the renderer formats it.
type Report struct {
	PeriodType  PeriodType
	Window      Window
	GeneratedAt time.Time

	Totals     repository.SignalQualityRow
	ByKind     []repository.SignalQualityBreakdownRow
	BySeverity []repository.SignalQualityBreakdownRow

	// SmallSample is true when the resolved count is below
	// SmallSampleThreshold — the renderer surfaces an explicit
	// "directional only" caveat in that case.
	SmallSample bool
}

// SmallSampleThreshold is the resolved-count below which the report
// surfaces a "sample size is small; treat as directional, not
// statistically stable" caveat. 30 is the conventional minimum-N for
// a binomial confidence interval to be informative.
const SmallSampleThreshold = 30

// SignalQualityFetcher is the read-side dependency. Satisfied by
// *repository.SignalReportRepository.
type SignalQualityFetcher interface {
	SignalQualityAggregate(ctx context.Context, periodStart, periodEnd time.Time) (repository.SignalQualityRow, error)
	SignalQualityByKind(ctx context.Context, periodStart, periodEnd time.Time) ([]repository.SignalQualityBreakdownRow, error)
	SignalQualityBySeverity(ctx context.Context, periodStart, periodEnd time.Time) ([]repository.SignalQualityBreakdownRow, error)
}

// BuildReport runs all three aggregator queries against the supplied
// window and returns the projected Report.
func BuildReport(ctx context.Context, q SignalQualityFetcher, kind PeriodType, window Window, now time.Time) (Report, error) {
	totals, err := q.SignalQualityAggregate(ctx, window.Start, window.End)
	if err != nil {
		return Report{}, err
	}
	byKind, err := q.SignalQualityByKind(ctx, window.Start, window.End)
	if err != nil {
		return Report{}, err
	}
	bySev, err := q.SignalQualityBySeverity(ctx, window.Start, window.End)
	if err != nil {
		return Report{}, err
	}
	resolved := totals.SuccessCount + totals.FailureCount
	return Report{
		PeriodType:  kind,
		Window:      window,
		GeneratedAt: now,
		Totals:      totals,
		ByKind:      byKind,
		BySeverity:  bySev,
		SmallSample: resolved < SmallSampleThreshold,
	}, nil
}

// SuccessRate returns the directional-correctness rate over RESOLVED
// alerts only (denominator excludes pending/ambiguous/unavailable).
// Returns 0 when the denominator is zero.
func SuccessRate(r repository.SignalQualityRow) float64 {
	denom := r.SuccessCount + r.FailureCount
	if denom == 0 {
		return 0
	}
	return float64(r.SuccessCount) / float64(denom)
}

// PositiveCLVRatio returns the share of alerts whose 24h CLV-lite
// drift was positive (i.e. favourable to the alert direction). 0 when
// the sample is empty.
func PositiveCLVRatio(r repository.SignalQualityRow) float64 {
	if r.CLV24hSampleCount == 0 {
		return 0
	}
	return float64(r.PositiveCLV24hCount) / float64(r.CLV24hSampleCount)
}
