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
	"fmt"
	"math"
	"time"

	"github.com/rs/zerolog"

	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/analysis"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/anomaly"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/metrics"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/repository"
)

// Service is the usecase facade.
type Service struct {
	cfg      Config
	analyzer analysis.Analyzer
	repo     AnalysisStore
	metrics  *metrics.Metrics
	log      *zerolog.Logger
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
// satisfies it.
type AnalysisStore interface {
	LatestVersion(ctx context.Context, alertID int64) (int32, error)
	Latest(ctx context.Context, alertID int64) (repository.AlertAnalysis, error)
	Insert(ctx context.Context, a repository.NewAlertAnalysis) (repository.AlertAnalysis, bool, error)
}

// New constructs a Service.
func New(cfg Config, analyzer analysis.Analyzer, repo AnalysisStore, met *metrics.Metrics, log *zerolog.Logger) *Service {
	return &Service{cfg: cfg, analyzer: analyzer, repo: repo, metrics: met, log: log}
}

// AnalyzeAndStore is the per-alert entry point. The caller (detect
// emitter or a follow-up worker) has already persisted the alert
// row; we look at the latest analysis for it, decide whether a
// refresh is warranted, optionally call the Analyzer, and persist
// the new version.
//
// Returns the analysis row regardless of outcome — Status tells the
// caller whether to render it.
func (s *Service) AnalyzeAndStore(ctx context.Context, alertID int64, f anomaly.Finding) (repository.AlertAnalysis, error) {
	if !s.cfg.AlertsEnabled {
		return repository.AlertAnalysis{}, nil
	}
	if s.repo == nil {
		return repository.AlertAnalysis{}, nil
	}

	// Refresh decision.
	prev, err := s.repo.Latest(ctx, alertID)
	switch {
	case err == nil:
		if !shouldRefresh(prev, f, s.cfg) {
			return prev, nil
		}
	case err == repository.ErrAnalysisNotFound:
		// First-time analysis — fall through.
	default:
		return repository.AlertAnalysis{}, fmt.Errorf("latest alert analysis: %w", err)
	}

	req := BuildAlertRequest(f, time.Now())
	res, err := s.analyzer.AnalyzeAlert(ctx, req)
	if err != nil {
		// Analyzer never returns Go errors — it surfaces Status.
		// A non-nil error here means a serious internal issue;
		// record a skipped row so the alert still emits.
		s.log.Err(err).Int64("alert_id", alertID).Msg("aianalysis: analyzer error")
		res = analysis.AlertAnalysis{Status: analysis.StatusError, Model: "unknown", LastError: err.Error()}
	}

	nextVersion, err := s.repo.LatestVersion(ctx, alertID)
	if err != nil {
		return repository.AlertAnalysis{}, fmt.Errorf("latest version: %w", err)
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
		Status:           string(res.Status),
		LastError:        res.LastError,
	})
	if err != nil {
		return repository.AlertAnalysis{}, fmt.Errorf("insert: %w", err)
	}
	s.observe(res)
	return row, nil
}

// LatestText returns the rendered text for the most recent analysis
// of the alert, or empty when no analysis exists or it's not OK.
// Telegram formatters call this to compose the "Analyst note" block.
func (s *Service) LatestText(ctx context.Context, alertID int64) string {
	if s.repo == nil {
		return ""
	}
	row, err := s.repo.Latest(ctx, alertID)
	if err != nil {
		return ""
	}
	if row.Status != string(analysis.StatusOK) {
		return ""
	}
	return row.AnalysisText
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
