// Package analysis defines the request/response types and the
// Analyzer interface that decouple the AI provider (OpenAI etc.)
// from the usecase layer. No OpenAI DTO leakage past this boundary.
//
// Three analyzer entry points:
//
//   - AnalyzeAlert      — one alert → one analyst note
//   - AnalyzeMarketReport — top-N candidate markets → 2h report
//   - AnalyzeOutcome    — a resolved alert → postmortem
//
// The service operates without an AI key: a noop Analyzer
// implementation (returns Status="skipped" with no text) keeps every
// downstream call path defined and the binary functional.
package analysis

import (
	"context"
	"time"
)

// Status names the analyzer's outcome. Persisted alongside the text
// so an operator can distinguish "analysis suppressed by policy"
// from "model raised an error".
type Status string

const (
	StatusOK      Status = "ok"
	StatusSkipped Status = "skipped"
	StatusError   Status = "error"
)

// --- Alert analysis -------------------------------------------------------

// AlertAnalysisRequest is the structured context the prompt builder
// receives. The fields are intentionally narrow and English-keyed so
// the prompt is small and predictable.
type AlertAnalysisRequest struct {
	// Identity
	AlertID  int64
	Kind     string // "trade_anomaly" | "accumulation" | "stable_favorite" | ...
	Severity string // info | warning | critical | hard
	Reason   string // canonical alert reason

	// Market context
	Title        string
	Category     string
	OutcomeLabel string
	LifecyclePct float64
	EndsAt       time.Time
	NowAt        time.Time

	// Trade / position numbers (zero when not applicable)
	Side               string
	NotionalUSD        float64
	Price              float64
	Odds               float64
	ProfitIfWinUSD     float64
	RemainingReturnPct float64
	Score              float64
	Confidence         float64

	// Structured reason codes — the model gets these verbatim so it
	// doesn't have to guess the firing shape.
	Reasons []string

	// Optional auxiliary context — empty when not available. The
	// prompt builder elides empty fields rather than printing zeros.
	MarketP95Ratio   float64
	TraderP95Ratio   float64
	AccumulationNote string // human-readable accumulation summary
	OwnershipNote    string
	QuietMarketNote  string
	NewWalletNote    string
	OutcomeStatus    string

	// v8 cross-flow context: when the same market has had multiple
	// alerts in the last 24h, the model needs to distinguish a
	// clean one-sided whale signal from conflicting flow. The
	// detector populates these from recent rows in polymarket_alerts.
	SameMarketRecentAlerts            int     // count last 24h
	SameMarketSameSideNotionalUSD     float64 // notional summed on this side
	SameMarketOppositeSideNotionalUSD float64 // notional summed on opposite side
	SameWalletBidirectional           bool    // wallet bought AND sold same outcome inside window

	// NoveltyOrMemeGuess flags markets the detector or operator
	// classified as joke/novelty (low informational value). When
	// true the prompt asks the model to bias toward Avoid/Watch.
	NoveltyOrMemeGuess bool

	// PublicContextEnabled tells the model whether web_search was
	// run for this request. When false the model must include a
	// "Live context was not checked." sentence in Risk or Next so
	// the operator never confuses an offline note with a researched one.
	PublicContextEnabled bool
}

// AlertAnalysis is what the analyzer returns.
type AlertAnalysis struct {
	Status           Status
	Model            string
	AnalysisText     string
	Verdict          string // operator-facing verdict: actionable | watchlist | avoid | n/a
	PromptChars      int
	OutputChars      int
	PromptTokens     int
	CompletionTokens int
	EstimatedCostUSD float64
	LastError        string // populated when Status == error
}

// --- Market intelligence (2h) --------------------------------------------

// MarketReportMarket is one row in the 2h scout dataset.
type MarketReportMarket struct {
	Title              string
	Category           string
	LifecyclePct       float64
	Probability        float64
	RemainingReturnPct float64
	Volume24hUSD       float64
	RecentTrades24h    int
	StabilityScore     float64 // 0..1 normalized, optional
	AlertsLast24h      int
	Notes              string // free-text hints (e.g. "stable favorite candidate")
}

// MarketReportRequest is the analyst's input.
type MarketReportRequest struct {
	GeneratedAt time.Time
	PeriodStart time.Time
	PeriodEnd   time.Time

	// Pre-bucketed signal counters — gives the AI an at-a-glance
	// shape of the period without us asking it to do arithmetic.
	WhaleFlowCandidates int
	StableFavorites     int
	AsymmetricSetups    int
	DevelopingSignals   int

	Markets []MarketReportMarket

	// Free-text upcoming-events hint: e.g. "Iran nuclear talks
	// continue; UK summer recess". When empty the AI is told to say
	// "no external context provided".
	UpcomingEventsNote string
}

// MarketReportAnalysis is the analyzer's response. Telegram body
// composition happens in the usecase layer; the analyzer returns the
// free-text "analyst summary" portion only.
type MarketReportAnalysis struct {
	Status           Status
	Model            string
	ReportText       string
	SummaryHash      string // computed by usecase from canonicalised content
	PromptTokens     int
	CompletionTokens int
	EstimatedCostUSD float64
	LastError        string
}

// --- Outcome analysis (postmortem) ---------------------------------------

type OutcomeAnalysisRequest struct {
	AlertID      int64
	Kind         string
	Severity     string
	Title        string
	Category     string
	OutcomeLabel string

	// What the alert SAID at fire time.
	AlertText          string // the original Telegram body, escaped
	NotionalUSD        float64
	Probability        float64
	ProfitIfWinUSD     float64
	RemainingReturnPct float64
	Score              float64
	Confidence         float64
	Reasons            []string

	// What ACTUALLY happened.
	OutcomeStatus  string // resolved_correct | resolved_wrong | unknown | unavailable
	WinningOutcome string
	CLV15m         float64
	CLV1h          float64
	CLV6h          float64
	CLV24h         float64
	ResolvedAt     time.Time
}

type OutcomeAnalysis struct {
	Status           Status
	Model            string
	ReasonText       string  // why did this resolve this way
	LessonsText      string  // future-lessons takeaway
	WonExpected      *bool   // nil = uncertain; true = Watchtower expected this; false = surprised
	Confidence       float64 // 0..1
	PromptTokens     int
	CompletionTokens int
	EstimatedCostUSD float64
	LastError        string
}

// --- Analyzer interface ---------------------------------------------------

// Analyzer is the single seam between the AI provider and the rest
// of the system. *openai.Client and *noop.Analyzer both satisfy it.
type Analyzer interface {
	AnalyzeAlert(ctx context.Context, req AlertAnalysisRequest) (AlertAnalysis, error)
	AnalyzeMarketReport(ctx context.Context, req MarketReportRequest) (MarketReportAnalysis, error)
	AnalyzeOutcome(ctx context.Context, req OutcomeAnalysisRequest) (OutcomeAnalysis, error)
}

// NoopAnalyzer satisfies Analyzer when the operator has not wired an
// OpenAI key. Every call returns StatusSkipped with empty text — the
// usecase layer renders the "AI analysis disabled" path cleanly.
type NoopAnalyzer struct{}

func (NoopAnalyzer) AnalyzeAlert(_ context.Context, _ AlertAnalysisRequest) (AlertAnalysis, error) {
	return AlertAnalysis{Status: StatusSkipped, Model: "noop"}, nil
}
func (NoopAnalyzer) AnalyzeMarketReport(_ context.Context, _ MarketReportRequest) (MarketReportAnalysis, error) {
	return MarketReportAnalysis{Status: StatusSkipped, Model: "noop"}, nil
}
func (NoopAnalyzer) AnalyzeOutcome(_ context.Context, _ OutcomeAnalysisRequest) (OutcomeAnalysis, error) {
	return OutcomeAnalysis{Status: StatusSkipped, Model: "noop"}, nil
}
