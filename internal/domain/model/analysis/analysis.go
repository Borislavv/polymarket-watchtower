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

	// EventNarrativeContext is the rendered Polymarket event-page
	// narrative block (event metadata + event markets + chart
	// annotations). Sourced from
	// internal/app/usecase/eventpagecontext.Provider, which fetches
	// the hydrated /event/<slug>.json payload via the
	// internal/infra/polymarket/eventpage client. Empty when the
	// loader is not wired OR the slug could not be resolved OR the
	// fetch failed — the prompt then renders an "unavailable" slot
	// and tells the model not to invent news.
	EventNarrativeContext string

	// CatalystContext is the rendered Political-Catalyst
	// Intelligence block ("Future catalysts:" prompt slot). Sourced
	// from internal/app/usecase/eventcatalyst.Provider; empty when
	// no active/expected catalysts are registered for the event.
	// Carries the catalyst_type, status, expected_at, and the
	// bullish/bearish/invalidation scenarios the AI uses to reason
	// about pre/post-catalyst flow timing.
	CatalystContext string
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

	// EventNarrativeContext is an OPTIONAL rendered Polymarket
	// event-page narrative block. The market-intelligence worker
	// stamps it for the highest-priority candidate market in the
	// period when the event-page provider is wired. Empty otherwise;
	// the prompt renders an "unavailable" slot.
	EventNarrativeContext string
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

	// EventNarrativeContext is the rendered Polymarket event-page
	// narrative block (annotations + event metadata + event-wide
	// market pricing) as of the resolution time. Used by the
	// postmortem to ask "did the catalyst the market priced in
	// actually happen?". Empty when the provider is not wired.
	EventNarrativeContext string
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

// --- Political-Catalyst extraction (v9.6) -------------------------------

// CatalystExtractionRequest is the structured prompt input the
// eventcatalyst importer hands to the AI for one event slug. The
// importer composes this from the event-page payload + recent
// Watchtower flow + existing catalyst rows; the AI returns a strict
// JSON `CatalystExtractionResponse`.
//
// All Polymarket-authored fields (annotations, event metadata) are
// DATA the AI reasons over, never instructions. The system never
// re-interprets returned strings except by JSON schema validation.
type CatalystExtractionRequest struct {
	EventSlug         string
	AnalysisTimeUTC   time.Time
	EventMetadata     CatalystEventMetadata
	Markets           []CatalystMarket
	Annotations       []CatalystAnnotation
	FlowSummary       CatalystFlowSummary
	ExistingCatalysts []CatalystExistingRow
}

// CatalystEventMetadata is the compact event header the prompt gets.
type CatalystEventMetadata struct {
	Title              string
	Description        string
	ResolutionRules    string
	Category           string
	StartDate          time.Time
	EndDate            time.Time
	ContextDescription string
	ContextUpdatedAt   time.Time
}

// CatalystMarket is one market under the event with current pricing.
type CatalystMarket struct {
	ConditionID        string
	Question           string
	GroupItemTitle     string
	Outcomes           []string
	OutcomePrices      []string
	Volume24hUSD       float64
	Liquidity          float64
	OneHourPriceChange *float64
	OneDayPriceChange  *float64
	OneWeekPriceChange *float64
	LastTradePrice     *float64
	Active             bool
	Closed             bool
	EndDate            time.Time
}

// CatalystAnnotation is one normalised annotation row passed to AI.
type CatalystAnnotation struct {
	Timestamp   time.Time
	Title       string
	Summary     string
	Outcome     string
	PriceBefore *float64
	PriceAfter  *float64
	PriceChange *float64
	SourceNames []string
}

// CatalystFlowSummary is the compact recent-flow rollup.
type CatalystFlowSummary struct {
	RecentAlertsCount       int
	StrongestSide           string
	AccumulationNote        string
	OwnershipNote           string
	ClusterNote             string
	SameSideNotional24h     float64
	OppositeSideNotional24h float64
	LargestRecentTradeUSD   float64
}

// CatalystExistingRow describes a catalyst the system already knows.
// Passed so the model can preserve / update / invalidate it instead
// of duplicating.
type CatalystExistingRow struct {
	CatalystType string
	Title        string
	ExpectedAt   time.Time
	Status       string
	Confidence   float64
}

// CatalystExtractionResponse is the strict JSON shape the model
// returns. JSON tags MUST match PART 4 schema verbatim.
type CatalystExtractionResponse struct {
	EventSlug       string              `json:"event_slug"`
	AnalysisTimeUTC string              `json:"analysis_time_utc"`
	Catalysts       []ExtractedCatalyst `json:"catalysts"`
	// Transport metadata stamped by the analyzer; not part of the
	// JSON contract.
	Status           Status  `json:"-"`
	Model            string  `json:"-"`
	PromptTokens     int     `json:"-"`
	CompletionTokens int     `json:"-"`
	EstimatedCostUSD float64 `json:"-"`
	LastError        string  `json:"-"`
}

// ExtractedCatalyst is one catalyst row from the strict JSON
// response. Nullable upstream fields use pointers so we can
// distinguish "absent" from "empty / zero".
type ExtractedCatalyst struct {
	CatalystType         string   `json:"catalyst_type"`
	Title                string   `json:"title"`
	Description          string   `json:"description"`
	ExpectedAt           *string  `json:"expected_at"`
	Confidence           float64  `json:"confidence"`
	Source               string   `json:"source"`
	SourceURL            *string  `json:"source_url"`
	Status               string   `json:"status"`
	BlockedReason        *string  `json:"blocked_reason"`
	BullishScenario      string   `json:"bullish_scenario"`
	BearishScenario      string   `json:"bearish_scenario"`
	InvalidationScenario string   `json:"invalidation_scenario"`
	FlowInterpretation   string   `json:"flow_interpretation"`
	AffectedOutcomes     []string `json:"affected_outcomes"`
}

// CatalystExtractor is the seam used by the importer. *openai.Client
// satisfies it for production; tests inject fakes. NoopExtractor
// returns an empty response so the importer can run end-to-end
// without an AI key (it will skip the upsert path).
type CatalystExtractor interface {
	ExtractCatalysts(ctx context.Context, req CatalystExtractionRequest) (CatalystExtractionResponse, error)
}

// NoopExtractor returns an empty StatusSkipped response.
type NoopExtractor struct{}

func (NoopExtractor) ExtractCatalysts(_ context.Context, req CatalystExtractionRequest) (CatalystExtractionResponse, error) {
	return CatalystExtractionResponse{
		EventSlug:       req.EventSlug,
		AnalysisTimeUTC: req.AnalysisTimeUTC.UTC().Format(time.RFC3339),
		Catalysts:       nil,
		Status:          StatusSkipped,
		Model:           "noop",
	}, nil
}

// --- v9.7 Annotation ranking + Daily political intel ----------------------

// AnnotationRankingRequest is the input the 2h market-intelligence
// path hands the AI when it needs to pick the most important
// annotations from a candidate batch. The verbatim ranking prompt
// (annotation_ranking_prompt.go) consumes the structured data block
// built around this request.
type AnnotationRankingRequest struct {
	PeriodStart time.Time
	PeriodEnd   time.Time
	OutputLimit int
	Markets     []RankingMarket
	Annotations []RankingAnnotation
	FlowSummary RankingFlowSummary
}

// RankingMarket is one market the ranker reasons about. We keep the
// surface narrow — the ranker only needs market identity + current
// price drift to decide what's "already priced in".
type RankingMarket struct {
	EventSlug         string
	MarketSlug        string
	ConditionID       string
	Question          string
	GroupItemTitle    string
	LastPrice         float64
	OneDayPriceChange *float64
	Volume24hUSD      float64
}

// RankingAnnotation is one candidate annotation to rank.
type RankingAnnotation struct {
	EventSlug      string
	MarketSlug     string
	AnnotationHash string
	Timestamp      time.Time
	Title          string
	Summary        string
	Outcome        string
	PriceBefore    *float64
	PriceAfter     *float64
	PriceChange    *float64
}

// RankingFlowSummary is the compact rollup the ranker sees so it can
// reason about whether flow validates / contradicts an annotation.
// v9.8 added Ownership/Cluster notes + LargestRecentTradeUSD so the
// daily-intel + prediction prompts can see the same shape the
// underlying eventflow.EventFlowSummary computes.
type RankingFlowSummary struct {
	RecentAlertsCount       int
	StrongestSide           string
	SameSideNotional24h     float64
	OppositeSideNotional24h float64
	LargestRecentTradeUSD   float64
	AccumulationNote        string
	OwnershipNote           string
	ClusterNote             string
}

// AnnotationRankingResponse is the strict-JSON shape the AI returns.
type AnnotationRankingResponse struct {
	Selected []SelectedAnnotation `json:"selected"`
	// Transport metadata stamped by the analyzer; not part of the
	// JSON contract.
	Status           Status  `json:"-"`
	Model            string  `json:"-"`
	PromptTokens     int     `json:"-"`
	CompletionTokens int     `json:"-"`
	EstimatedCostUSD float64 `json:"-"`
	LastError        string  `json:"-"`
}

// SelectedAnnotation is one row from `selected`. Pointers + omit-empty
// match the schema's `or null` semantics.
type SelectedAnnotation struct {
	EventSlug           string  `json:"event_slug"`
	MarketSlug          *string `json:"market_slug"`
	Rank                int     `json:"rank"`
	Importance          float64 `json:"importance"`
	VolatilityPotential float64 `json:"volatility_potential"`
	ProbabilityImpact   string  `json:"probability_impact"`
	AffectedOutcome     *string `json:"affected_outcome"`
	Title               string  `json:"title"`
	Reason              string  `json:"reason"`
	MarketRead          string  `json:"market_read"`
	// AnnotationHash is filled by the importer/marketintel worker
	// when matching the ranked title back to the source annotation.
	// Never populated by the AI; not exposed in JSON.
	AnnotationHash string `json:"-"`
}

// AnnotationRanker is the seam used by the marketintel worker.
// *openai.Client satisfies it for production; NoopAnnotationRanker
// satisfies it in dev/test.
type AnnotationRanker interface {
	RankAnnotations(ctx context.Context, req AnnotationRankingRequest) (AnnotationRankingResponse, error)
}

// NoopAnnotationRanker returns an empty StatusSkipped response.
type NoopAnnotationRanker struct{}

func (NoopAnnotationRanker) RankAnnotations(_ context.Context, _ AnnotationRankingRequest) (AnnotationRankingResponse, error) {
	return AnnotationRankingResponse{Status: StatusSkipped, Model: "noop"}, nil
}

// DailyPoliticalIntelRequest is the input for the daily intel report.
// The verbatim PART 5 prompt consumes the structured data block
// built around this request.
type DailyPoliticalIntelRequest struct {
	ReportDate         time.Time
	PeriodStart        time.Time
	PeriodEnd          time.Time
	Markets            []DailyIntelMarket
	FlowSummary        RankingFlowSummary
	KnownCatalysts     []DailyIntelCatalyst
	PreviousReportText string
}

// DailyIntelMarket is one of the 100 selected markets, with its 4
// most relevant annotations attached.
type DailyIntelMarket struct {
	EventSlug         string
	MarketSlug        string
	ConditionID       string
	Question          string
	Category          string
	LifecyclePct      float64
	LastPrice         float64
	OneDayPriceChange *float64
	Volume24hUSD      float64
	AlertsLast24h     int64
	StrongestSide     string
	ActiveCatalyst    string
	Annotations       []RankingAnnotation
}

// DailyIntelCatalyst is one row passed to the daily prompt.
type DailyIntelCatalyst struct {
	EventSlug    string
	CatalystType string
	Title        string
	ExpectedAt   time.Time
	Status       string
	Confidence   float64
}

// DailyPoliticalIntelResponse is the free-text Russian report the
// daily prompt returns. No structured JSON — the model output is
// rendered verbatim into Telegram (after section-aware splitting).
type DailyPoliticalIntelResponse struct {
	ReportText       string
	Status           Status
	Model            string
	PromptTokens     int
	CompletionTokens int
	EstimatedCostUSD float64
	LastError        string
}

// DailyPoliticalIntelGenerator is the seam used by the daily worker.
type DailyPoliticalIntelGenerator interface {
	GenerateDailyPoliticalIntel(ctx context.Context, req DailyPoliticalIntelRequest) (DailyPoliticalIntelResponse, error)
}

// NoopDailyPoliticalIntelGenerator returns an empty StatusSkipped.
type NoopDailyPoliticalIntelGenerator struct{}

func (NoopDailyPoliticalIntelGenerator) GenerateDailyPoliticalIntel(_ context.Context, _ DailyPoliticalIntelRequest) (DailyPoliticalIntelResponse, error) {
	return DailyPoliticalIntelResponse{Status: StatusSkipped, Model: "noop"}, nil
}

// --- v9.9 Prediction Evolution ------------------------------------------

// PredictionEvolutionRequest is the input for the verbatim PART 9
// "обнови thesis" prompt. The worker only calls the generator when
// AI gating signals a meaningful change; for routine refreshes the
// deterministic layers carry the state forward unchanged.
type PredictionEvolutionRequest struct {
	EventSlug          string
	ConditionID        string
	PreviousPrediction string // operator-facing summary from the prior cycle
	PredictionState    string
	StateReason        string
	MarketSnapshot     string // compact "key: value" block
	AnnotationsBlock   string
	CatalystsBlock     string
	RepricingBlock     string
	FlowSummaryBlock   string
	MatchedAlertsBlock string
	WebContextBlock    string
	PublicContextOn    bool
}

// PredictionEvolutionResponse is the free-text Russian update the AI
// returns. Rendered HTML-escaped into the "PREDICTION UPDATE" Telegram
// body when delivery is gated open.
type PredictionEvolutionResponse struct {
	ThesisUpdate     string
	Status           Status
	Model            string
	PromptTokens     int
	CompletionTokens int
	EstimatedCostUSD float64
	LastError        string
}

// PredictionEvolutionGenerator is the AI seam.
type PredictionEvolutionGenerator interface {
	RefreshPredictionThesis(ctx context.Context, req PredictionEvolutionRequest) (PredictionEvolutionResponse, error)
}

// NoopPredictionEvolutionGenerator returns StatusSkipped — the
// worker treats this as "no AI update this cycle".
type NoopPredictionEvolutionGenerator struct{}

func (NoopPredictionEvolutionGenerator) RefreshPredictionThesis(_ context.Context, _ PredictionEvolutionRequest) (PredictionEvolutionResponse, error) {
	return PredictionEvolutionResponse{Status: StatusSkipped, Model: "noop"}, nil
}

// --- Prediction Creation (v10.0) ------------------------------------------
//
// PART 1 of the operational-grade prediction engine. The creation
// pipeline runs in two AI stages so we don't burn full-thesis tokens
// on every shortlisted market:
//
//  1. Ranker: takes ~15–25 SHORT candidate summaries and answers
//     "which deserve full deep-dive". One prompt per cycle.
//  2. Predictor: for the top-N selected, generates a full
//     market thesis (catalyst reasoning, repricing read, flow
//     interpretation, risk factors, practical stance).
//
// Domain types live here so the worker depends only on the analysis
// package, not on a specific AI provider.

// PredictionCandidate is one short candidate the worker hands the
// ranker. All fields are deterministic — populated by the worker
// from DB queries, never invented by AI.
type PredictionCandidate struct {
	EventSlug          string
	ConditionID        string
	Question           string
	Category           string
	Outcome            string  // outcome label (e.g. "Ken Paxton")
	LastTradePrice     float64 // current price 0–1
	OneDayPriceChange  float64 // signed % change vs 24h ago
	OneWeekPriceChange float64 // signed % change vs 1w ago
	LifecyclePct       float64 // 0–100; how much of the market lifetime has elapsed
	RecentAlerts24h    int
	StrongestSide      string // "BUY" | "SELL" | ""
	DirectionalSkew    float64
	OpenCatalysts      int
	NewAnnotations24h  int
	VolumeUSD24h       float64
	LiquidityUSD       float64
	BaselineMedianUSD  float64
}

// PredictionRankingRequest is the input to the ranker. The worker
// fills `Candidates` deterministically and asks for the top N.
type PredictionRankingRequest struct {
	AnalysisTimeUTC time.Time
	Candidates      []PredictionCandidate
	MaxSelected     int
}

// PredictionRankingPick is one row the AI returns. EventSlug must
// match a candidate the worker sent — the worker rejects picks for
// unknown slugs.
type PredictionRankingPick struct {
	EventSlug   string  `json:"event_slug"`
	ConditionID string  `json:"condition_id,omitempty"`
	Score       float64 `json:"score"`
	Reason      string  `json:"reason,omitempty"`
}

// PredictionRankingResponse is the AI ranker's verdict.
type PredictionRankingResponse struct {
	Picks            []PredictionRankingPick
	Status           Status
	Model            string
	PromptTokens     int
	CompletionTokens int
	EstimatedCostUSD float64
	LastError        string
}

// PredictionRanker is the AI seam for the first stage.
type PredictionRanker interface {
	RankCandidates(ctx context.Context, req PredictionRankingRequest) (PredictionRankingResponse, error)
}

// NoopPredictionRanker returns StatusSkipped + an empty pick set.
type NoopPredictionRanker struct{}

func (NoopPredictionRanker) RankCandidates(_ context.Context, _ PredictionRankingRequest) (PredictionRankingResponse, error) {
	return PredictionRankingResponse{Status: StatusSkipped, Model: "noop"}, nil
}

// PredictionCreationRequest is the input to the deep-dive predictor
// for ONE selected candidate. Fields mirror PredictionEvolutionRequest
// for consistency — same render helpers feed both.
type PredictionCreationRequest struct {
	EventSlug          string
	ConditionID        string
	Outcome            string
	Question           string
	Category           string
	MarketSnapshot     string
	AnnotationsBlock   string
	CatalystsBlock     string
	RepricingBlock     string
	FlowSummaryBlock   string
	MatchedAlertsBlock string
}

// PredictionCreationResponse is the structured thesis the AI
// produces. Rendered HTML-escaped into the initial prediction body
// and persisted as polymarket_market_predictions.summary.
type PredictionCreationResponse struct {
	Summary          string  // free-text Russian thesis (renders into Telegram)
	SideBias         string  // "bullish" | "bearish" | "neutral"
	Confidence       float64 // 0..1
	RiskFactors      string  // operator-facing bullet block
	Status           Status
	Model            string
	PromptTokens     int
	CompletionTokens int
	EstimatedCostUSD float64
	LastError        string
}

// PredictionCreator is the AI seam for the second stage.
type PredictionCreator interface {
	CreatePrediction(ctx context.Context, req PredictionCreationRequest) (PredictionCreationResponse, error)
}

// NoopPredictionCreator returns StatusSkipped + empty summary.
type NoopPredictionCreator struct{}

func (NoopPredictionCreator) CreatePrediction(_ context.Context, _ PredictionCreationRequest) (PredictionCreationResponse, error) {
	return PredictionCreationResponse{Status: StatusSkipped, Model: "noop"}, nil
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
