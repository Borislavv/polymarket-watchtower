// Package annotationranking is the small adapter that powers the
// marketintel.Worker AnnotationRankingHook seam. It glues together
//
//   - eventpagecontext.Provider — to load annotations per
//     conditionID;
//   - openai.Client (analysis.AnnotationRanker) — to rank the
//     candidate annotations via the verbatim PART 1-3 prompt;
//   - AnnotationIntelRepository — to persist the ranked picks for
//     audit / dashboards;
//
// and renders the operator-facing "Top important annotations" block
// the marketintel worker appends to its Telegram body.
//
// Failure semantics: every layer fails open. Empty result returns
// "" and the marketintel report still ships without the block.
package annotationranking

import (
	"context"
	"fmt"
	"html"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/eventpagecontext"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/analysis"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/metrics"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/repository"
)

// Config tunes the hook.
type Config struct {
	MaxAnnotations int
	OutputLimit    int
	AITimeout      time.Duration
}

func (c *Config) applyDefaults() {
	if c.MaxAnnotations <= 0 {
		c.MaxAnnotations = 80
	}
	if c.OutputLimit <= 0 {
		c.OutputLimit = 10
	}
	if c.AITimeout <= 0 {
		c.AITimeout = 45 * time.Second
	}
}

// MarketResolver is the seam to MarketRepository.GetByConditionID.
type MarketResolver interface {
	GetByConditionID(ctx context.Context, conditionID string) (repository.Market, error)
}

// EventPageLoader is the seam to eventpagecontext.Provider.Load.
type EventPageLoader interface {
	Load(ctx context.Context, eventSlug string, sev eventpagecontext.Severity) eventpagecontext.Summary
}

// RankingStore is the seam to AnnotationIntelRepository.
type RankingStore interface {
	UpsertRanking(ctx context.Context, n repository.NewEventAnnotationRanking) error
}

// Hook is the marketintel-side adapter.
type Hook struct {
	cfg     Config
	markets MarketResolver
	pages   EventPageLoader
	ranker  analysis.AnnotationRanker
	store   RankingStore
	metrics *metrics.Metrics
	log     *zerolog.Logger
}

// New wires the hook.
func New(
	cfg Config,
	markets MarketResolver,
	pages EventPageLoader,
	ranker analysis.AnnotationRanker,
	store RankingStore,
	met *metrics.Metrics,
	log *zerolog.Logger,
) *Hook {
	cfg.applyDefaults()
	return &Hook{cfg: cfg, markets: markets, pages: pages, ranker: ranker, store: store, metrics: met, log: log}
}

// RankAndRender satisfies marketintel.AnnotationRankingHook. Collects
// annotations from the candidate markets' events, calls the ranker,
// persists the picks, and returns an HTML-formatted block. Empty
// result on any failure path.
func (h *Hook) RankAndRender(ctx context.Context, candidates []repository.IntelligenceCandidate, periodStart, periodEnd time.Time, limit int) string {
	if h == nil || h.ranker == nil || len(candidates) == 0 {
		return ""
	}
	if limit <= 0 {
		limit = h.cfg.OutputLimit
	}
	rankingMarkets := make([]analysis.RankingMarket, 0, len(candidates))
	rankingAnnotations := make([]analysis.RankingAnnotation, 0, h.cfg.MaxAnnotations)
	seenSlug := map[string]struct{}{}
	hashToAnnotation := map[string]repository.EventAnnotation{}
	for _, c := range candidates {
		if len(rankingAnnotations) >= h.cfg.MaxAnnotations {
			break
		}
		m, err := h.markets.GetByConditionID(ctx, c.ConditionID)
		if err != nil || m.EventSlug == "" {
			continue
		}
		if _, dup := seenSlug[m.EventSlug]; dup {
			continue
		}
		seenSlug[m.EventSlug] = struct{}{}
		summary := h.pages.Load(ctx, m.EventSlug, eventpagecontext.SeverityInfo)
		if len(summary.Annotations) == 0 {
			continue
		}
		drift := summary.Markets // marker — narrative loader populates 0+ rows
		_ = drift
		rankingMarkets = append(rankingMarkets, analysis.RankingMarket{
			EventSlug:    m.EventSlug,
			MarketSlug:   m.Slug,
			ConditionID:  c.ConditionID,
			Question:     c.Question,
			LastPrice:    c.LastPrice,
			Volume24hUSD: c.Volume24hUSD,
		})
		for _, a := range summary.Annotations {
			if len(rankingAnnotations) >= h.cfg.MaxAnnotations {
				break
			}
			rankingAnnotations = append(rankingAnnotations, analysis.RankingAnnotation{
				EventSlug:      m.EventSlug,
				MarketSlug:     m.Slug,
				AnnotationHash: a.ItemHash,
				Timestamp:      a.Timestamp,
				Title:          a.Title,
				Summary:        a.Summary,
				Outcome:        a.Outcome,
				PriceBefore:    a.PriceBefore,
				PriceAfter:     a.PriceAfter,
				PriceChange:    a.PriceChange,
			})
			hashToAnnotation[a.ItemHash] = a
		}
	}
	if len(rankingAnnotations) == 0 {
		return ""
	}

	aiCtx, cancel := context.WithTimeout(ctx, h.cfg.AITimeout)
	defer cancel()
	res, err := h.ranker.RankAnnotations(aiCtx, analysis.AnnotationRankingRequest{
		PeriodStart: periodStart,
		PeriodEnd:   periodEnd,
		OutputLimit: limit,
		Markets:     rankingMarkets,
		Annotations: rankingAnnotations,
	})
	if err != nil {
		h.observeRanking("failed")
		if h.log != nil {
			h.log.Warn().Err(err).Msg("annotation ranking: AI call failed")
		}
		return ""
	}
	if res.Status != analysis.StatusOK {
		h.observeRanking(string(res.Status))
		return ""
	}
	h.observeRanking("ok")
	h.observeSelected(len(res.Selected))

	// Persist the picks for audit. Failures don't block rendering.
	for _, s := range res.Selected {
		// Best-effort title→hash match: use the annotation whose
		// title equals the selected title. The verbatim prompt's
		// schema doesn't carry the source hash through, so a title
		// match is the cleanest reverse lookup.
		hash := ""
		for h2, a := range hashToAnnotation {
			if strings.EqualFold(strings.TrimSpace(a.Title), strings.TrimSpace(s.Title)) {
				hash = h2
				break
			}
		}
		ms := ""
		if s.MarketSlug != nil {
			ms = *s.MarketSlug
		}
		ao := ""
		if s.AffectedOutcome != nil {
			ao = *s.AffectedOutcome
		}
		_ = h.store.UpsertRanking(ctx, repository.NewEventAnnotationRanking{
			PeriodStart:         periodStart,
			PeriodEnd:           periodEnd,
			EventSlug:           s.EventSlug,
			MarketSlug:          ms,
			AnnotationHash:      hash,
			Rank:                int32(s.Rank),
			Importance:          s.Importance,
			VolatilityPotential: s.VolatilityPotential,
			ProbabilityImpact:   s.ProbabilityImpact,
			AffectedOutcome:     ao,
			Title:               s.Title,
			Reason:              s.Reason,
			MarketRead:          s.MarketRead,
		})
	}

	return RenderTopAnnotationsBlock(res.Selected)
}

// RenderTopAnnotationsBlock emits the operator-facing HTML block.
// Exposed for tests.
func RenderTopAnnotationsBlock(rows []analysis.SelectedAnnotation) string {
	if len(rows) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("<b>Top important annotations</b>\n")
	for i, s := range rows {
		fmt.Fprintf(&b, "%d. <b>%s</b>", i+1, html.EscapeString(strings.TrimSpace(s.Title)))
		if s.ProbabilityImpact != "" && s.ProbabilityImpact != "unclear" {
			fmt.Fprintf(&b, " · impact=%s", html.EscapeString(s.ProbabilityImpact))
		}
		if s.MarketRead != "" && s.MarketRead != "unclear" {
			fmt.Fprintf(&b, " · read=%s", html.EscapeString(s.MarketRead))
		}
		if s.AffectedOutcome != nil && *s.AffectedOutcome != "" {
			fmt.Fprintf(&b, " · outcome=%s", html.EscapeString(*s.AffectedOutcome))
		}
		b.WriteString("\n")
		if r := strings.TrimSpace(s.Reason); r != "" {
			if len(r) > 240 {
				r = r[:239] + "…"
			}
			fmt.Fprintf(&b, "  %s\n", html.EscapeString(r))
		}
	}
	return b.String()
}

func (h *Hook) observeRanking(status string) {
	if h.metrics == nil || h.metrics.MarketIntelAnnotationRankingRequests == nil {
		return
	}
	h.metrics.MarketIntelAnnotationRankingRequests.WithLabelValues(status).Inc()
}

func (h *Hook) observeSelected(n int) {
	if h.metrics == nil || h.metrics.MarketIntelAnnotationsSelected == nil {
		return
	}
	h.metrics.MarketIntelAnnotationsSelected.Add(float64(n))
}
