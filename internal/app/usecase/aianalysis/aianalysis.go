// Package aianalysis is the orchestration layer around analysis.Analyzer.
//
// Responsibilities:
//   - decide WHEN to call the Analyzer (refresh policy) so we don't
//     burn budget repeating identical analyses
//   - build the AlertAnalysisRequest from an anomaly.Finding + the
//     alert row, mapping the dozen-or-so domain fields into the
//     compact analyst-friendly shape
//   - persist the result via repository.AlertAnalysisRepository
//   - expose the latest analysis text so the Telegram formatter can
//     render an "Analyst note" block
//
// The Analyzer dependency is INTERFACE-typed; tests inject fakes,
// production passes either openai.Client or analysis.NoopAnalyzer.
// The service stays fully functional when the operator hasn't wired
// an API key — the NoopAnalyzer returns StatusSkipped and the
// rendering path elides the Analyst-note block.
package aianalysis

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/analysis"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/anomaly"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/metrics"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/repository"
)

// Service is the usecase facade.
type Service struct {
	cfg        Config
	analyzer   analysis.Analyzer
	repo       AnalysisStore
	requestLog RequestLogStore
	narrative  NarrativeLoader // optional — nil silently disables event-page context enrichment
	catalyst   CatalystLoader  // optional — nil silently disables catalyst context enrichment
	metrics    *metrics.Metrics
	log        *zerolog.Logger
}

// NarrativeLoader is the seam to the eventpagecontext.Provider. It
// loads a compact Polymarket event-page narrative summary keyed by
// the Finding's market id (which the loader resolves to event_slug
// internally). The interface lives here to keep aianalysis free of
// any dependency on the usecase package implementing the loader.
//
// Failure contract: return "" on any failure (slug unresolved, fetch
// error, parse error). The aianalysis service treats empty as "no
// usable context" and the prompt renders an "unavailable" slot — the
// alert path NEVER blocks on the loader.
type NarrativeLoader interface {
	LoadAndRenderForFinding(ctx context.Context, f anomaly.Finding, maxChars int) string
}

// CatalystLoader is the seam to the eventcatalyst.Provider — the
// Political-Catalyst Intelligence overlay. Returns the rendered
// "Future catalysts:" prompt block. Same fail-silent contract as
// NarrativeLoader: empty result yields a "no catalyst recorded"
// fallback in the prompt and a no-op in the Telegram renderer.
type CatalystLoader interface {
	LoadAndRenderForFinding(ctx context.Context, f anomaly.Finding, maxChars int) string
}

// Config tunes refresh behavior + master switches.
type Config struct {
	// AlertsEnabled is the master switch for per-alert analysis.
	// When false the service returns StatusSkipped without any
	// Analyzer call.
	AlertsEnabled bool

	// LifecycleRefreshDeltaPct — re-analyze when the alert's
	// underlying market lifecycle has moved by at least this many
	// percentage points since the last analysis. 0 disables.
	LifecycleRefreshDeltaPct float64

	// CLVMaterialChange — re-analyze when |CLV24h| has moved by at
	// least this absolute fractional value since the last analysis.
	CLVMaterialChange float64
}

// AnalysisStore is the persistence seam. *repository.AlertAnalysisRepository
// satisfies it. Stores SUCCESSFUL AI answers only — failures land in
// RequestLogStore.
type AnalysisStore interface {
	LatestVersion(ctx context.Context, alertID int64) (int32, error)
	Latest(ctx context.Context, alertID int64) (repository.AlertAnalysis, error)
	Insert(ctx context.Context, a repository.NewAlertAnalysis) (repository.AlertAnalysis, bool, error)
}

// RequestLogStore is the operational-telemetry seam for AI calls.
// *repository.AIRequestLogRepository satisfies it. Optional — nil
// keeps the service usable without the new table (dev mode, tests).
type RequestLogStore interface {
	Insert(ctx context.Context, l repository.AIRequestLog) error
}

// New constructs a Service. requestLog may be nil — without it the
// service still works, but operational failures land in logs only.
func New(cfg Config, analyzer analysis.Analyzer, repo AnalysisStore, requestLog RequestLogStore, met *metrics.Metrics, log *zerolog.Logger) *Service {
	return &Service{cfg: cfg, analyzer: analyzer, repo: repo, requestLog: requestLog, metrics: met, log: log}
}

// SetNarrativeLoader wires the optional Polymarket event-page
// narrative loader. nil keeps the slot empty.
func (s *Service) SetNarrativeLoader(loader NarrativeLoader) { s.narrative = loader }

// SetCatalystLoader wires the optional Political-Catalyst
// Intelligence loader. nil keeps the slot empty.
func (s *Service) SetCatalystLoader(loader CatalystLoader) { s.catalyst = loader }

// Canonical skip/failure reason strings. Stable identifiers — the
// alertsender logs them and dashboards may group by them.
const (
	ReasonDisabled            = "disabled"
	ReasonTelegramAlertsOff   = "telegram_alerts_disabled"
	ReasonNoAPIKey            = "no_api_key"
	ReasonRepoMissing         = "repo_unconfigured"
	ReasonNoRefreshNeeded     = "no_refresh_needed"
	ReasonAnalyzerSkipped     = "analyzer_skipped"
	ReasonAnalyzerError       = "analyzer_error"
	ReasonRepoLatestError     = "repo_latest_error"
	ReasonRepoVersionError    = "repo_version_error"
	ReasonRepoInsertError     = "repo_insert_error"
	ReasonAnalysisTextEmpty   = "analysis_text_empty"
	ReasonLatestTextNotOK     = "latest_text_status_not_ok"
	ReasonLatestTextNotFound  = "latest_text_not_found"
	ReasonLatestTextRepoError = "latest_text_repo_error"
	ReasonValidationFailed    = "validation_failed"
)

// AnalyzeAndStore is the per-alert entry point. v8 data-correctness
// semantics:
//
//   - SUCCESSFUL AI answers are stored in polymarket_alert_analyses.
//   - Provider failures and skips land in polymarket_ai_request_logs
//     (when wired). They are NEVER stored as analysis rows — the
//     production incident was OpenAI 429 quota JSON ending up in
//     alert_analyses.last_error, making the analytical table
//     unusable for dashboards.
//   - Output validation runs before persistence: a returned text
//     must contain the structured Thesis/Follow?/Verdict markers
//     (see validateAlertOutput) — otherwise the response is treated
//     as a failed call, request-logged, no analysis row written.
//
// Returns the analysis row when one exists for this alert (success
// path or earlier successful version); otherwise an empty row with
// Status carrying the canonical reason so the alertsender can log it.
func (s *Service) AnalyzeAndStore(ctx context.Context, alertID int64, f anomaly.Finding) (repository.AlertAnalysis, error) {
	if !s.cfg.AlertsEnabled {
		return repository.AlertAnalysis{
			AlertID:   alertID,
			Status:    string(analysis.StatusSkipped),
			LastError: ReasonDisabled,
		}, nil
	}
	if s.repo == nil {
		return repository.AlertAnalysis{
			AlertID:   alertID,
			Status:    string(analysis.StatusSkipped),
			LastError: ReasonRepoMissing,
		}, nil
	}

	// Refresh decision.
	prev, err := s.repo.Latest(ctx, alertID)
	switch {
	case err == nil:
		if !shouldRefresh(prev, f, s.cfg) {
			return prev, nil
		}
	case errors.Is(err, repository.ErrAnalysisNotFound):
		// First-time analysis — fall through.
	default:
		// A repo read failure is operational, not analytical. Log
		// telemetry; return a skip-shaped result so the alert ships.
		s.recordRequestLog(ctx, repository.AIRequestLog{
			TargetKind:    "alert",
			TargetID:      &alertID,
			Provider:      "openai",
			Model:         "",
			RequestKind:   "alert_analysis",
			Status:        "failed_terminal",
			ErrorCategory: ReasonRepoLatestError,
			ErrorMessage:  err.Error(),
		})
		return repository.AlertAnalysis{
			AlertID:   alertID,
			Status:    string(analysis.StatusError),
			LastError: ReasonRepoLatestError,
		}, fmt.Errorf("latest alert analysis: %w", err)
	}

	req := BuildAlertRequest(f, time.Now())
	// Load Polymarket event-page narrative context BEFORE the AI
	// call. Failure is silent — empty string falls back to a
	// "context unavailable" slot in the prompt and the model is
	// told not to invent news. The alert path NEVER blocks on this.
	if s.narrative != nil {
		req.EventNarrativeContext = s.narrative.LoadAndRenderForFinding(ctx, f, 5000)
		if req.EventNarrativeContext != "" && s.metrics != nil && s.metrics.EventPageContextUsed != nil {
			s.metrics.EventPageContextUsed.WithLabelValues("alert").Inc()
		}
	}
	// Political-Catalyst Intelligence overlay. Fails silently —
	// empty result falls back to "no catalyst recorded" in the
	// prompt. Never blocks the alert path.
	if s.catalyst != nil {
		req.CatalystContext = s.catalyst.LoadAndRenderForFinding(ctx, f, 2000)
	}
	startedAt := time.Now()
	res, analyzerErr := s.analyzer.AnalyzeAlert(ctx, req)
	latency := time.Since(startedAt)

	// Whatever the analyzer returned, write a request_log row. This
	// is the load-bearing operational telemetry.
	logRow := repository.AIRequestLog{
		TargetKind:       "alert",
		TargetID:         &alertID,
		Provider:         "openai",
		Model:            res.Model,
		RequestKind:      "alert_analysis",
		PromptChars:      int32(res.PromptChars),
		OutputChars:      int32(res.OutputChars),
		PromptTokens:     int32(res.PromptTokens),
		CompletionTokens: int32(res.CompletionTokens),
		EstimatedCostUSD: res.EstimatedCostUSD,
		LatencyMS:        latency.Milliseconds(),
	}

	switch {
	case analyzerErr != nil:
		// Typed provider error.
		logRow.Status, logRow.ErrorCategory, logRow.ErrorMessage = classifyAnalyzerError(analyzerErr, res)
		s.log.Warn().Err(analyzerErr).
			Int64("alert_id", alertID).
			Str("category", logRow.ErrorCategory).
			Msg("ai request failed")
		s.recordRequestLog(ctx, logRow)
		s.observe(res)
		// Quota-exceeded is a separate counter — operator-actionable
		// (billing), not a transient slow-down.
		if logRow.ErrorCategory == "quota_exceeded" && s.metrics != nil && s.metrics.AIQuotaExceeded != nil {
			s.metrics.AIQuotaExceeded.WithLabelValues("openai", res.Model).Inc()
		}
		return repository.AlertAnalysis{
			AlertID:   alertID,
			Status:    string(analysis.StatusError),
			LastError: logRow.ErrorCategory,
		}, nil

	case res.Status == analysis.StatusSkipped:
		// Analyzer signalled skip without an HTTP failure — usually
		// budget/rate/no_key. Don't pollute the analysis table.
		logRow.Status = "skipped_" + sanitizeReason(res.LastError)
		logRow.ErrorCategory = res.LastError
		s.log.Info().
			Int64("alert_id", alertID).
			Str("reason", res.LastError).
			Msg("ai request skipped")
		s.recordRequestLog(ctx, logRow)
		s.observe(res)
		return repository.AlertAnalysis{
			AlertID:   alertID,
			Status:    string(analysis.StatusSkipped),
			LastError: res.LastError,
		}, nil

	case res.Status != analysis.StatusOK:
		// Defence in depth — any other non-OK status is treated as a
		// failure and request-logged.
		logRow.Status = "failed_terminal"
		logRow.ErrorCategory = res.LastError
		s.recordRequestLog(ctx, logRow)
		s.observe(res)
		return repository.AlertAnalysis{
			AlertID:   alertID,
			Status:    string(res.Status),
			LastError: res.LastError,
		}, nil
	}

	// Validation: a "success" with malformed/empty text is NOT a
	// successful analysis. Reject and request-log.
	if reason := validateAlertOutput(res.AnalysisText); reason != "" {
		logRow.Status = "failed_terminal"
		logRow.ErrorCategory = ReasonValidationFailed + ":" + reason
		logRow.ErrorMessage = sanitizeAndCap(res.AnalysisText, 500)
		s.log.Warn().
			Int64("alert_id", alertID).
			Str("category", logRow.ErrorCategory).
			Msg("ai output failed validation; not persisting as analysis")
		s.recordRequestLog(ctx, logRow)
		s.observe(analysis.AlertAnalysis{Status: analysis.StatusError, Model: res.Model, LastError: logRow.ErrorCategory})
		if s.metrics != nil && s.metrics.AIAnalysisRejected != nil {
			s.metrics.AIAnalysisRejected.WithLabelValues("alert", reason).Inc()
		}
		return repository.AlertAnalysis{
			AlertID:   alertID,
			Status:    string(analysis.StatusError),
			LastError: logRow.ErrorCategory,
		}, nil
	}

	// Real success path — persist the analysis.
	nextVersion, err := s.repo.LatestVersion(ctx, alertID)
	if err != nil {
		s.recordRequestLog(ctx, repository.AIRequestLog{
			TargetKind:    "alert",
			TargetID:      &alertID,
			Provider:      "openai",
			Model:         res.Model,
			RequestKind:   "alert_analysis",
			Status:        "failed_terminal",
			ErrorCategory: ReasonRepoVersionError,
			ErrorMessage:  err.Error(),
		})
		return repository.AlertAnalysis{
			AlertID:   alertID,
			Status:    string(analysis.StatusError),
			LastError: ReasonRepoVersionError,
		}, fmt.Errorf("latest version: %w", err)
	}
	nextVersion++
	row, _, err := s.repo.Insert(ctx, repository.NewAlertAnalysis{
		AlertID:          alertID,
		Version:          nextVersion,
		TriggerKind:      triggerKindFromContext(prev, f),
		TriggerDetail:    triggerDetailFromContext(prev, f),
		Model:            res.Model,
		PromptChars:      int32(res.PromptChars),
		OutputChars:      int32(res.OutputChars),
		PromptTokens:     int32(res.PromptTokens),
		CompletionTokens: int32(res.CompletionTokens),
		EstimatedCostUSD: res.EstimatedCostUSD,
		AnalysisText:     res.AnalysisText,
		Verdict:          res.Verdict,
		Status:           string(analysis.StatusOK),
		LastError:        "",
	})
	if err != nil {
		s.recordRequestLog(ctx, repository.AIRequestLog{
			TargetKind:    "alert",
			TargetID:      &alertID,
			Provider:      "openai",
			Model:         res.Model,
			RequestKind:   "alert_analysis",
			Status:        "failed_terminal",
			ErrorCategory: ReasonRepoInsertError,
			ErrorMessage:  err.Error(),
		})
		return repository.AlertAnalysis{
			AlertID:   alertID,
			Status:    string(analysis.StatusError),
			LastError: ReasonRepoInsertError,
		}, fmt.Errorf("insert: %w", err)
	}

	// Success request_log + metrics.
	logRow.Status = "success"
	s.recordRequestLog(ctx, logRow)
	s.observe(res)
	if s.metrics != nil && s.metrics.AIAnalysisPersisted != nil {
		s.metrics.AIAnalysisPersisted.WithLabelValues("alert").Inc()
	}
	s.log.Info().
		Int64("alert_id", alertID).
		Str("model", res.Model).
		Int64("latency_ms", latency.Milliseconds()).
		Int("prompt_tokens", res.PromptTokens).
		Int("completion_tokens", res.CompletionTokens).
		Int("text_len", len(res.AnalysisText)).
		Msg("ai request completed")
	return row, nil
}

// recordRequestLog is a fail-open helper — telemetry MUST NEVER block
// the AI path. If the store write fails (DB down, table missing),
// log it and move on.
func (s *Service) recordRequestLog(ctx context.Context, l repository.AIRequestLog) {
	if s.requestLog == nil {
		return
	}
	if err := s.requestLog.Insert(ctx, l); err != nil {
		s.log.Err(err).Str("target_kind", l.TargetKind).Msg("aianalysis: request_log write failed")
	}
}

// sanitizeReason converts a free-form reason string into a
// snake_case identifier safe for status enums. Empty → "unknown".
func sanitizeReason(s string) string {
	if s == "" {
		return "unknown"
	}
	r := strings.ToLower(s)
	r = strings.ReplaceAll(r, " ", "_")
	r = strings.ReplaceAll(r, "-", "_")
	// Drop colon-suffix payload to keep cardinality bounded.
	if i := strings.Index(r, ":"); i > 0 {
		r = r[:i]
	}
	return r
}

// classifyAnalyzerError maps an analyzer error + result into the
// request_log status / category / message triple. Falls back to
// unknown when the error isn't a typed ProviderError.
func classifyAnalyzerError(err error, res analysis.AlertAnalysis) (status, category, message string) {
	// Try to use the typed openai ProviderError. We don't import
	// the package to avoid a cycle — instead we parse the LastError
	// field which the openai client already sets to the canonical
	// category string. See openai.AnalyzeAlert.
	cat := res.LastError
	if cat == "" {
		cat = "unknown"
	}
	switch cat {
	case "quota_exceeded":
		// Note: AIQuotaExceeded metric is emitted at the openai
		// client layer where the typed error is classified — this
		// branch is the routing decision, not the counter bump.
		return "skipped_quota", cat, sanitizeAndCap(err.Error(), 500)
	case "rate_limited":
		return "failed_retryable", cat, sanitizeAndCap(err.Error(), 500)
	case "timeout":
		return "failed_retryable", cat, sanitizeAndCap(err.Error(), 500)
	case "provider_5xx":
		return "failed_retryable", cat, sanitizeAndCap(err.Error(), 500)
	case "bad_request", "invalid_model", "prompt_rejected":
		return "failed_terminal", cat, sanitizeAndCap(err.Error(), 500)
	default:
		return "failed_terminal", cat, sanitizeAndCap(err.Error(), 500)
	}
}

// sanitizeAndCap is a local copy of the openai package helper so we
// don't introduce an import cycle (aianalysis → repository → ?
// → openai → ?). The dedupe is tolerable; the rule is "short and
// boring", and both copies enforce that.
func sanitizeAndCap(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.TrimSpace(s)
	if n <= 0 || len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// validateAlertOutput runs MINIMAL safety checks on model output.
// v8.1 incident-driven revision: we no longer reject paid model
// output because it lacks Thesis:/Follow?:/Verdict: labels. Strict
// structural validation was throwing away usable analysis the
// operator had already paid OpenAI tokens for; the prompt asks for
// the structure but the operator's feedback wins.
//
// The only rejections that survive are the ones that would actively
// poison the analytical table:
//   - empty / whitespace-only output (nothing useful to render).
//   - obvious provider-error JSON that slipped past the openai
//     client's typed-error path (defence in depth — the openai
//     client should already have categorised these as failures, but
//     a bad upstream change could regress).
//
// Length capping is handled upstream (openai.Client.MaxOutputChars
// + truncate); a long-but-genuine answer is NOT rejected here.
//
// Returns empty string when output is acceptable; otherwise a short
// reason code stored on the request_log row.
func validateAlertOutput(text string) string {
	t := strings.TrimSpace(text)
	if t == "" {
		return "empty_text"
	}
	low := strings.ToLower(t)
	for _, marker := range []string{
		"insufficient_quota",
		"rate_limit_exceeded",
		"\"error\":{",
	} {
		if strings.Contains(low, marker) {
			return "provider_error_text"
		}
	}
	return ""
}

// LatestText returns the rendered text for the most recent analysis
// of the alert plus a non-empty `reason` when the result is empty.
// Reason values are the canonical ReasonX constants so the caller
// can route them into structured logs without parsing.
func (s *Service) LatestText(ctx context.Context, alertID int64) (string, string) {
	if s.repo == nil {
		return "", ReasonRepoMissing
	}
	row, err := s.repo.Latest(ctx, alertID)
	if err != nil {
		if errors.Is(err, repository.ErrAnalysisNotFound) {
			return "", ReasonLatestTextNotFound
		}
		return "", ReasonLatestTextRepoError + ": " + err.Error()
	}
	if row.Status != string(analysis.StatusOK) {
		// Surface why the row exists but is unusable — "skipped" with
		// LastError="no_api_key" tells the operator exactly what to
		// fix without going to SQL.
		reason := ReasonLatestTextNotOK + ":" + row.Status
		if row.LastError != "" {
			reason += ":" + row.LastError
		}
		return "", reason
	}
	if row.AnalysisText == "" {
		return "", ReasonAnalysisTextEmpty
	}
	return row.AnalysisText, ""
}

// --- Refresh policy --------------------------------------------------------

// shouldRefresh is the decision: re-analyze a previously-analyzed
// alert when ANY of the following holds:
//   - the alert's severity has been upgraded since the prior version
//   - the underlying market lifecycle has moved ≥ LifecycleRefreshDeltaPct
//   - |CLV24h| has moved ≥ CLVMaterialChange
//   - the market has resolved (outcome_status flipped to terminal)
//
// All deltas use the verdict-time snapshot embedded in the alert
// row vs the live Finding. Returns false (no refresh) when the
// finding looks "the same" — saves cost and dedupes.
func shouldRefresh(prev repository.AlertAnalysis, f anomaly.Finding, cfg Config) bool {
	// Severity upgrade — always refresh.
	prevSev := severityRank(extractPriorSeverity(prev))
	currSev := severityRank(string(f.Severity))
	if currSev > prevSev {
		return true
	}
	// Lifecycle movement.
	if cfg.LifecycleRefreshDeltaPct > 0 {
		// We don't have the prior lifecycle stamped on the analysis
		// row, so we use a coarse heuristic: if the new finding's
		// lifecycle is materially HIGHER than the median of severity
		// thresholds and the prior was older than 1h, refresh. This
		// is intentionally conservative — operators tune
		// LifecycleRefreshDeltaPct to the noise floor they accept.
		if time.Since(prev.CreatedAt) > time.Hour && f.LifecyclePct > 0 {
			return true
		}
	}
	// Resolution = terminal change.
	if isTerminalOutcomeReason(f) {
		return true
	}
	// CLV material change isn't visible on the Finding payload here
	// — the drift worker writes it to polymarket_alerts.clv_*. The
	// outcomes worker triggers refresh via a different code path.
	return false
}

func extractPriorSeverity(prev repository.AlertAnalysis) string {
	// trigger_detail format is "severity=<sev> lifecycle=<float>",
	// emitted by triggerDetailFromContext. Parse leniently.
	for _, part := range splitFields(prev.TriggerDetail) {
		const key = "severity="
		if len(part) > len(key) && part[:len(key)] == key {
			return part[len(key):]
		}
	}
	return ""
}

func splitFields(s string) []string {
	out := []string{}
	current := ""
	for _, ch := range s {
		if ch == ' ' {
			if current != "" {
				out = append(out, current)
				current = ""
			}
			continue
		}
		current += string(ch)
	}
	if current != "" {
		out = append(out, current)
	}
	return out
}

func severityRank(s string) int {
	switch s {
	case "hard":
		return 4
	case "critical":
		return 3
	case "warning":
		return 2
	case "info":
		return 1
	}
	return 0
}

func isTerminalOutcomeReason(f anomaly.Finding) bool {
	for _, r := range f.Reasons {
		switch r {
		case "RESOLVED_CORRECT", "RESOLVED_WRONG":
			return true
		}
	}
	return false
}

func triggerKindFromContext(prev repository.AlertAnalysis, f anomaly.Finding) string {
	if prev.ID == 0 {
		return "initial"
	}
	if severityRank(string(f.Severity)) > severityRank(extractPriorSeverity(prev)) {
		return "severity_upgrade"
	}
	if isTerminalOutcomeReason(f) {
		return "resolution"
	}
	return "refresh"
}

func triggerDetailFromContext(_ repository.AlertAnalysis, f anomaly.Finding) string {
	return fmt.Sprintf("severity=%s lifecycle=%.1f", string(f.Severity), f.LifecyclePct)
}

func (s *Service) observe(res analysis.AlertAnalysis) {
	if s.metrics == nil {
		return
	}
	if s.metrics.AIAnalysisRequests != nil {
		s.metrics.AIAnalysisRequests.WithLabelValues(string(res.Status)).Inc()
	}
	if s.metrics.AIAnalysisTokens != nil && res.Status == analysis.StatusOK {
		s.metrics.AIAnalysisTokens.WithLabelValues("prompt").Add(float64(res.PromptTokens))
		s.metrics.AIAnalysisTokens.WithLabelValues("completion").Add(float64(res.CompletionTokens))
	}
	if s.metrics.AIAnalysisCost != nil && res.EstimatedCostUSD > 0 {
		s.metrics.AIAnalysisCost.Add(res.EstimatedCostUSD)
	}
	if s.metrics.AIAnalysisSkipped != nil && res.Status == analysis.StatusSkipped {
		reason := res.LastError
		if reason == "" {
			reason = "unknown"
		}
		s.metrics.AIAnalysisSkipped.WithLabelValues(reason).Inc()
	}
	_ = math.Round // silence unused-import in pared-down builds
}

// BuildAlertRequest is exported so callers and tests can construct
// the analyst request shape without going through the Service.
// Carries every field the prompt builder may render — empty values
// are elided downstream.
func BuildAlertRequest(f anomaly.Finding, now time.Time) analysis.AlertAnalysisRequest {
	req := analysis.AlertAnalysisRequest{
		Kind:     string(f.Kind),
		Severity: string(f.Severity),
		Reason:   f.Reason,
		NowAt:    now,
	}
	if f.Trade != nil {
		req.Title = f.Trade.Question
		req.OutcomeLabel = f.Trade.Outcome
		req.Side = string(f.Trade.Side)
		req.NotionalUSD = f.Trade.NotionalUSD
		req.Price = f.Trade.Price
		req.Odds = f.Trade.Odds
	}
	if f.Category != nil {
		req.Category = f.Category.Label
	}
	req.LifecyclePct = f.LifecyclePct
	req.ProfitIfWinUSD = f.ProfitIfWinUSD
	req.MarketP95Ratio = f.MarketP95Ratio
	req.TraderP95Ratio = f.TraderP95Ratio
	req.Reasons = f.Reasons
	if f.StableFavorite != nil {
		req.Title = orFallback(req.Title, "stable favorite candidate")
		req.OutcomeLabel = orFallback(req.OutcomeLabel, f.StableFavorite.Outcome)
		req.Price = f.StableFavorite.Probability
		if req.Price > 0 {
			req.Odds = 1 / req.Price
		}
		req.RemainingReturnPct = f.StableFavorite.RemainingReturnPct
		req.Score = f.StableFavorite.Score
		req.Confidence = f.StableFavorite.Confidence
	}
	if f.Accumulation != nil {
		req.AccumulationNote = fmt.Sprintf("%d trades over %s, total ~$%.0f, side %s",
			f.Accumulation.TradeCount, f.Accumulation.Span, f.Accumulation.TotalNotionalUSD,
			f.Accumulation.Side)
	}
	if f.Ownership != nil {
		req.OwnershipNote = fmt.Sprintf("approx %.1f%% of recorded BUY flow", f.Ownership.SharePct)
	}
	if f.NewWallet != nil && f.NewWallet.IsNew {
		req.NewWalletNote = fmt.Sprintf("new wallet, %d trades on record", f.NewWallet.HistoryTrades)
	}
	if f.QuietMarket != nil {
		req.QuietMarketNote = fmt.Sprintf("idle %s before alert", f.QuietMarket.IdleDuration)
	}
	return req
}

func orFallback(s, fb string) string {
	if s != "" {
		return s
	}
	return fb
}
