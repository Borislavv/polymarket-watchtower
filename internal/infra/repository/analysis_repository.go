// Package repository — AI-analysis persistence wrappers.
//
// Three tables, three repositories of similar shape:
//   - AlertAnalysisRepository — append-only version chain per alert
//   - MarketIntelligenceRepository — 2h scout reports keyed by content hash
//   - AlertOutcomeAnalysisRepository — postmortem rows, one per alert
//
// All three expose insert + read-latest helpers. The usecase layer
// owns refresh/dedup logic; this layer is pure CRUD.
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

// --- AlertAnalysisRepository ----------------------------------------------

// AlertAnalysis is the domain-friendly view of one persisted AI alert
// note.
type AlertAnalysis struct {
	ID               int64
	AlertID          int64
	Version          int32
	TriggerKind      string
	TriggerDetail    string
	Model            string
	PromptChars      int32
	OutputChars      int32
	PromptTokens     int32
	CompletionTokens int32
	EstimatedCostUSD float64
	AnalysisText     string
	Verdict          string
	Status           string
	LastError        string
	CreatedAt        time.Time
}

// NewAlertAnalysis is the insert input.
type NewAlertAnalysis struct {
	AlertID          int64
	Version          int32
	TriggerKind      string
	TriggerDetail    string
	Model            string
	PromptChars      int32
	OutputChars      int32
	PromptTokens     int32
	CompletionTokens int32
	EstimatedCostUSD float64
	AnalysisText     string
	Verdict          string
	Status           string // ok | skipped | error
	LastError        string
}

// AlertAnalysisRepository wraps polymarket_alert_analyses.
type AlertAnalysisRepository struct {
	q *sqlc.Queries
}

func NewAlertAnalysisRepository(pool *pgxpool.Pool) *AlertAnalysisRepository {
	return &AlertAnalysisRepository{q: sqlc.New(pool)}
}

// LatestVersion returns the highest version recorded for the alert,
// or 0 when none. Used by the refresh policy to compute next-version.
func (r *AlertAnalysisRepository) LatestVersion(ctx context.Context, alertID int64) (int32, error) {
	v, err := r.q.LatestAlertAnalysisVersion(ctx, alertID)
	if err != nil {
		return 0, fmt.Errorf("latest alert analysis version: %w", err)
	}
	return v, nil
}

// Latest returns the most recent analysis row for the alert.
// ErrAnalysisNotFound when none.
func (r *AlertAnalysisRepository) Latest(ctx context.Context, alertID int64) (AlertAnalysis, error) {
	row, err := r.q.LatestAlertAnalysis(ctx, alertID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return AlertAnalysis{}, ErrAnalysisNotFound
		}
		return AlertAnalysis{}, fmt.Errorf("latest alert analysis: %w", err)
	}
	return alertAnalysisFromSQLC(row), nil
}

// Insert persists one analysis row. ON CONFLICT (alert_id, version)
// DO NOTHING — returns false when a concurrent writer beat us; true
// otherwise.
func (r *AlertAnalysisRepository) Insert(ctx context.Context, a NewAlertAnalysis) (AlertAnalysis, bool, error) {
	row, err := r.q.InsertAlertAnalysis(ctx, sqlc.InsertAlertAnalysisParams{
		AlertID:          a.AlertID,
		Version:          a.Version,
		TriggerKind:      a.TriggerKind,
		TriggerDetail:    strPtr(a.TriggerDetail),
		Model:            a.Model,
		PromptChars:      a.PromptChars,
		OutputChars:      a.OutputChars,
		PromptTokens:     a.PromptTokens,
		CompletionTokens: a.CompletionTokens,
		EstimatedCostUsd: a.EstimatedCostUSD,
		AnalysisText:     a.AnalysisText,
		Verdict:          strPtr(a.Verdict),
		Status:           a.Status,
		LastError:        strPtr(a.LastError),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return AlertAnalysis{}, false, nil
		}
		return AlertAnalysis{}, false, fmt.Errorf("insert alert analysis: %w", err)
	}
	return alertAnalysisFromSQLC(row), true, nil
}

// ErrAnalysisNotFound is returned by Latest when no row exists.
var ErrAnalysisNotFound = errors.New("alert analysis not found")

func alertAnalysisFromSQLC(row sqlc.PolymarketAlertAnalyses) AlertAnalysis {
	out := AlertAnalysis{
		ID:               row.ID,
		AlertID:          row.AlertID,
		Version:          row.Version,
		TriggerKind:      row.TriggerKind,
		Model:            row.Model,
		PromptChars:      row.PromptChars,
		OutputChars:      row.OutputChars,
		PromptTokens:     row.PromptTokens,
		CompletionTokens: row.CompletionTokens,
		EstimatedCostUSD: row.EstimatedCostUsd,
		AnalysisText:     row.AnalysisText,
		Status:           row.Status,
		CreatedAt:        row.CreatedAt.Time,
	}
	if row.TriggerDetail != nil {
		out.TriggerDetail = *row.TriggerDetail
	}
	if row.Verdict != nil {
		out.Verdict = *row.Verdict
	}
	if row.LastError != nil {
		out.LastError = *row.LastError
	}
	return out
}

// --- MarketIntelligenceRepository -----------------------------------------

// MarketIntelligenceReport mirrors polymarket_market_intelligence_reports.
type MarketIntelligenceReport struct {
	ID                int64
	GeneratedAt       time.Time
	PeriodStart       time.Time
	PeriodEnd         time.Time
	SummaryHash       string
	ReportText        string
	MarketsJSON       []byte
	Model             string
	PromptTokens      int32
	CompletionTokens  int32
	EstimatedCostUSD  float64
	TelegramMessageID *int64
	TelegramChatID    string
	DeliveryStatus    string
}

// NewMarketIntelligenceReport — insert input.
type NewMarketIntelligenceReport struct {
	// PeriodKey is the load-bearing dedup column. The worker computes
	// it from the bucketed period boundary so two ticks inside the
	// same window collapse to one row.
	PeriodKey         string
	PeriodStart       time.Time
	PeriodEnd         time.Time
	SummaryHash       string
	ReportText        string
	MarketsJSON       []byte
	Model             string
	PromptTokens      int32
	CompletionTokens  int32
	EstimatedCostUSD  float64
	TelegramMessageID *int64
	TelegramChatID    string
	DeliveryStatus    string
	LastDeliveryError string
}

type MarketIntelligenceRepository struct {
	q *sqlc.Queries
}

func NewMarketIntelligenceRepository(pool *pgxpool.Pool) *MarketIntelligenceRepository {
	return &MarketIntelligenceRepository{q: sqlc.New(pool)}
}

// Insert persists one report. Returns (report, true) on fresh insert,
// (zero, false) on summary_hash conflict (dedup hit).
func (r *MarketIntelligenceRepository) Insert(ctx context.Context, rpt NewMarketIntelligenceReport) (MarketIntelligenceReport, bool, error) {
	row, err := r.q.InsertMarketIntelligenceReport(ctx, sqlc.InsertMarketIntelligenceReportParams{
		PeriodKey:         rpt.PeriodKey,
		PeriodStart:       tsFromTime(rpt.PeriodStart),
		PeriodEnd:         tsFromTime(rpt.PeriodEnd),
		SummaryHash:       rpt.SummaryHash,
		ReportText:        rpt.ReportText,
		MarketsJson:       rpt.MarketsJSON,
		Model:             rpt.Model,
		PromptTokens:      rpt.PromptTokens,
		CompletionTokens:  rpt.CompletionTokens,
		EstimatedCostUsd:  rpt.EstimatedCostUSD,
		TelegramMessageID: rpt.TelegramMessageID,
		TelegramChatID:    strPtr(rpt.TelegramChatID),
		DeliveryStatus:    rpt.DeliveryStatus,
		LastDeliveryError: strPtr(rpt.LastDeliveryError),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return MarketIntelligenceReport{}, false, nil
		}
		return MarketIntelligenceReport{}, false, fmt.Errorf("insert market intelligence report: %w", err)
	}
	return marketIntelFromSQLC(row), true, nil
}

// IntelligenceCandidate is the per-market row the worker hands the
// AI analyzer.
type IntelligenceCandidate struct {
	ConditionID  string
	Question     string
	Category     string
	LifecyclePct float64
	Trades24h    int64
	Volume24hUSD float64
	LastPrice    float64
	Alerts24h    int64
}

// ListIntelligenceCandidates returns up to `limit` top-N markets
// for the 2h intelligence report.
func (r *MarketIntelligenceRepository) ListIntelligenceCandidates(ctx context.Context, limit int32) ([]IntelligenceCandidate, error) {
	rows, err := r.q.ListMarketIntelligenceCandidates(ctx, limit)
	if err != nil {
		return nil, fmt.Errorf("list intelligence candidates: %w", err)
	}
	out := make([]IntelligenceCandidate, 0, len(rows))
	for _, row := range rows {
		c := IntelligenceCandidate{
			ConditionID:  row.ConditionID,
			Question:     row.Question,
			Trades24h:    row.Trades24h,
			Volume24hUSD: row.Volume24hUsd,
			LastPrice:    row.LastPrice,
			Alerts24h:    row.Alerts24h,
		}
		if row.Category != nil {
			c.Category = *row.Category
		}
		c.LifecyclePct = row.LifecyclePct
		out = append(out, c)
	}
	return out, nil
}

func (r *MarketIntelligenceRepository) Latest(ctx context.Context) (MarketIntelligenceReport, error) {
	row, err := r.q.LatestMarketIntelligenceReport(ctx)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return MarketIntelligenceReport{}, ErrAnalysisNotFound
		}
		return MarketIntelligenceReport{}, fmt.Errorf("latest market intelligence report: %w", err)
	}
	return marketIntelFromSQLC(row), nil
}

func marketIntelFromSQLC(row sqlc.PolymarketMarketIntelligenceReports) MarketIntelligenceReport {
	out := MarketIntelligenceReport{
		ID:               row.ID,
		GeneratedAt:      row.GeneratedAt.Time,
		PeriodStart:      row.PeriodStart.Time,
		PeriodEnd:        row.PeriodEnd.Time,
		SummaryHash:      row.SummaryHash,
		ReportText:       row.ReportText,
		MarketsJSON:      row.MarketsJson,
		Model:            row.Model,
		PromptTokens:     row.PromptTokens,
		CompletionTokens: row.CompletionTokens,
		EstimatedCostUSD: row.EstimatedCostUsd,
		DeliveryStatus:   row.DeliveryStatus,
	}
	if row.TelegramMessageID != nil {
		v := *row.TelegramMessageID
		out.TelegramMessageID = &v
	}
	if row.TelegramChatID != nil {
		out.TelegramChatID = *row.TelegramChatID
	}
	return out
}

// --- AlertOutcomeAnalysisRepository ---------------------------------------

type AlertOutcomeAnalysis struct {
	ID                int64
	AlertID           int64
	OutcomeStatus     string
	WonExpected       *bool
	AIReasonText      string
	AILessonsText     string
	Confidence        float64
	Model             string
	PromptTokens      int32
	CompletionTokens  int32
	EstimatedCostUSD  float64
	TelegramMessageID *int64
	TelegramChatID    string
	DeliveryStatus    string
	CreatedAt         time.Time
}

type NewAlertOutcomeAnalysis struct {
	AlertID           int64
	OutcomeStatus     string
	WonExpected       *bool
	AIReasonText      string
	AILessonsText     string
	Confidence        float64
	Model             string
	PromptTokens      int32
	CompletionTokens  int32
	EstimatedCostUSD  float64
	TelegramMessageID *int64
	TelegramChatID    string
	DeliveryStatus    string
	LastDeliveryError string
}

type AlertOutcomeAnalysisRepository struct {
	q *sqlc.Queries
}

func NewAlertOutcomeAnalysisRepository(pool *pgxpool.Pool) *AlertOutcomeAnalysisRepository {
	return &AlertOutcomeAnalysisRepository{q: sqlc.New(pool)}
}

func (r *AlertOutcomeAnalysisRepository) Insert(ctx context.Context, a NewAlertOutcomeAnalysis) (AlertOutcomeAnalysis, bool, error) {
	row, err := r.q.InsertAlertOutcomeAnalysis(ctx, sqlc.InsertAlertOutcomeAnalysisParams{
		AlertID:           a.AlertID,
		OutcomeStatus:     a.OutcomeStatus,
		WonExpected:       a.WonExpected,
		AiReasonText:      a.AIReasonText,
		AiLessonsText:     strPtr(a.AILessonsText),
		Confidence:        a.Confidence,
		Model:             a.Model,
		PromptTokens:      a.PromptTokens,
		CompletionTokens:  a.CompletionTokens,
		EstimatedCostUsd:  a.EstimatedCostUSD,
		TelegramMessageID: a.TelegramMessageID,
		TelegramChatID:    strPtr(a.TelegramChatID),
		DeliveryStatus:    a.DeliveryStatus,
		LastDeliveryError: strPtr(a.LastDeliveryError),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return AlertOutcomeAnalysis{}, false, nil
		}
		return AlertOutcomeAnalysis{}, false, fmt.Errorf("insert alert outcome analysis: %w", err)
	}
	return alertOutcomeFromSQLC(row), true, nil
}

func (r *AlertOutcomeAnalysisRepository) Get(ctx context.Context, alertID int64) (AlertOutcomeAnalysis, error) {
	row, err := r.q.GetAlertOutcomeAnalysis(ctx, alertID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return AlertOutcomeAnalysis{}, ErrAnalysisNotFound
		}
		return AlertOutcomeAnalysis{}, fmt.Errorf("get alert outcome analysis: %w", err)
	}
	return alertOutcomeFromSQLC(row), nil
}

// --- StrategyDimensionsRepository ------------------------------------------

// StrategyDimensions is the bucketed attribution row written for every
// alert. Optional buckets are empty strings; the writer maps "" to a
// SQL NULL so dashboards can `WHERE odds_bucket IS NOT NULL` cleanly.
//
// Cardinality discipline: every bucket below is a coarse label
// (lifecycle bands, log10 notional bands, return bands). The
// strategy_family column is THE primary group-by axis for
// "which-setups-actually-win" panels.
type StrategyDimensions struct {
	AlertID              int64
	StrategyFamily       string
	LifecycleBucket      string
	OddsBucket           string
	NotionalBucket       string
	ReturnBucket         string
	Category             string
	AccumulationWindow   string
	OwnershipShareBucket string
	VolatilityRegime     string
	NewWallet            bool
	QuietMarket          bool
	DormantWallet        bool
	DriftRegime          string
	AIVerdict            string
}

// StrategyDimensionsRepository wraps polymarket_alert_strategy_dimensions.
type StrategyDimensionsRepository struct {
	q *sqlc.Queries
}

func NewStrategyDimensionsRepository(pool *pgxpool.Pool) *StrategyDimensionsRepository {
	return &StrategyDimensionsRepository{q: sqlc.New(pool)}
}

// Upsert writes (or overwrites) the attribution row for an alert.
// Idempotent — multiple calls with identical payload are a no-op,
// and a re-run after a schema fix overwrites any prior bucketing
// rather than accumulating ghosts.
func (r *StrategyDimensionsRepository) Upsert(ctx context.Context, d StrategyDimensions) error {
	if err := r.q.UpsertAlertStrategyDimensions(ctx, sqlc.UpsertAlertStrategyDimensionsParams{
		AlertID:              d.AlertID,
		StrategyFamily:       d.StrategyFamily,
		LifecycleBucket:      d.LifecycleBucket,
		OddsBucket:           strPtr(d.OddsBucket),
		NotionalBucket:       strPtr(d.NotionalBucket),
		ReturnBucket:         strPtr(d.ReturnBucket),
		Category:             strPtr(d.Category),
		AccumulationWindow:   strPtr(d.AccumulationWindow),
		OwnershipShareBucket: strPtr(d.OwnershipShareBucket),
		VolatilityRegime:     strPtr(d.VolatilityRegime),
		NewWallet:            d.NewWallet,
		QuietMarket:          d.QuietMarket,
		DormantWallet:        d.DormantWallet,
		DriftRegime:          strPtr(d.DriftRegime),
		AiVerdict:            strPtr(d.AIVerdict),
	}); err != nil {
		return fmt.Errorf("upsert alert strategy dimensions: %w", err)
	}
	return nil
}

// --- AIRequestLogRepository -----------------------------------------------

// AIRequestLog is the operational telemetry row written for every AI
// provider interaction. Distinct from AlertAnalysis (which holds
// successful AI answers ONLY) — see migration 00015 for the
// motivation. Short, queryable, capped error_message.
type AIRequestLog struct {
	TargetKind       string // alert | market_intelligence | outcome
	TargetID         *int64
	Provider         string
	Model            string
	RequestKind      string // alert_analysis | market_report | outcome_postmortem
	Status           string // success | failed_* | skipped_*
	ErrorCategory    string
	ErrorCode        string
	ErrorMessage     string // capped to 500 chars
	HTTPStatus       *int32
	PromptChars      int32
	OutputChars      int32
	PromptTokens     int32
	CompletionTokens int32
	EstimatedCostUSD float64
	LatencyMS        int64
}

// AIRequestLogRepository wraps polymarket_ai_request_logs.
type AIRequestLogRepository struct {
	q *sqlc.Queries
}

func NewAIRequestLogRepository(pool *pgxpool.Pool) *AIRequestLogRepository {
	return &AIRequestLogRepository{q: sqlc.New(pool)}
}

// Insert persists one request-log row. Best-effort — telemetry MUST
// NOT block the AI path; the caller logs the inner failure and moves
// on. error_message is capped at 500 chars before write.
func (r *AIRequestLogRepository) Insert(ctx context.Context, l AIRequestLog) error {
	msg := l.ErrorMessage
	if len(msg) > 500 {
		msg = msg[:499] + "…"
	}
	if err := r.q.InsertAIRequestLog(ctx, sqlc.InsertAIRequestLogParams{
		TargetKind:       l.TargetKind,
		TargetID:         l.TargetID,
		Provider:         l.Provider,
		Model:            l.Model,
		RequestKind:      l.RequestKind,
		Status:           l.Status,
		ErrorCategory:    strPtr(l.ErrorCategory),
		ErrorCode:        strPtr(l.ErrorCode),
		ErrorMessage:     strPtr(msg),
		HttpStatus:       l.HTTPStatus,
		PromptChars:      l.PromptChars,
		OutputChars:      l.OutputChars,
		PromptTokens:     l.PromptTokens,
		CompletionTokens: l.CompletionTokens,
		EstimatedCostUsd: l.EstimatedCostUSD,
		LatencyMs:        l.LatencyMS,
	}); err != nil {
		return fmt.Errorf("insert ai request log: %w", err)
	}
	return nil
}

func alertOutcomeFromSQLC(row sqlc.PolymarketAlertOutcomeAnalyses) AlertOutcomeAnalysis {
	out := AlertOutcomeAnalysis{
		ID:               row.ID,
		AlertID:          row.AlertID,
		OutcomeStatus:    row.OutcomeStatus,
		AIReasonText:     row.AiReasonText,
		Confidence:       row.Confidence,
		Model:            row.Model,
		PromptTokens:     row.PromptTokens,
		CompletionTokens: row.CompletionTokens,
		EstimatedCostUSD: row.EstimatedCostUsd,
		DeliveryStatus:   row.DeliveryStatus,
		CreatedAt:        row.CreatedAt.Time,
	}
	if row.WonExpected != nil {
		v := *row.WonExpected
		out.WonExpected = &v
	}
	if row.AiLessonsText != nil {
		out.AILessonsText = *row.AiLessonsText
	}
	if row.TelegramMessageID != nil {
		v := *row.TelegramMessageID
		out.TelegramMessageID = &v
	}
	if row.TelegramChatID != nil {
		out.TelegramChatID = *row.TelegramChatID
	}
	return out
}
