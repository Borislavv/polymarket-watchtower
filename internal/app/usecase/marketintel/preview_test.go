package marketintel

import (
	"testing"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/analysis"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/repository"
)

// TestPreviewSampleReport is a manual-eyeball test that dumps the v9.7
// rendered marketintel body for both the happy AI path and the
// AI-timeout fallback path. Run with `-v` to read the output.
func TestPreviewSampleReport(t *testing.T) {
	t.Log("\n=========== HAPPY PATH ===========\n" + samplePreview(true))
	t.Log("\n=========== FALLBACK (AI TIMEOUT) ===========\n" + samplePreview(false))
}

func samplePreview(aiOK bool) string {
	pf := func(v float64) *float64 { return &v }
	cands := []repository.IntelligenceCandidate{
		{ConditionID: "0xa", Question: "Will the next Fed meeting cut rates by 25bps?", EventSlug: "fed-cut-mar", MarketSlug: "fed-cut-25bp", Category: "Macro", CategorySlug: "macro", LifecyclePct: 98, LastPrice: 0.65, Volume24hUSD: 13_400, Alerts24h: 2},
		{ConditionID: "0xb", Question: "Will candidate X win the Iowa primary?", EventSlug: "iowa-primary-2026", MarketSlug: "iowa-x", Category: "Politics", CategorySlug: "politics", LifecyclePct: 87, LastPrice: 0.42, Volume24hUSD: 75_000, Alerts24h: 5},
	}
	req := analysis.MarketReportRequest{
		PeriodStart: time.Date(2026, 5, 21, 2, 0, 0, 0, time.UTC),
		PeriodEnd:   time.Date(2026, 5, 21, 4, 0, 0, 0, time.UTC),
		Markets: []analysis.MarketReportMarket{
			{Title: cands[0].Question, LifecyclePct: 98, Probability: 0.65, Volume24hUSD: 13_400, AlertsLast24h: 2},
			{Title: cands[1].Question, LifecyclePct: 87, Probability: 0.42, Volume24hUSD: 75_000, AlertsLast24h: 5},
		},
		WhaleFlowCandidates: 3,
		StableFavorites:     1,
		AsymmetricSetups:    2,
		DevelopingSignals:   4,
	}
	annotations := []AnnotationItem{
		{EventSlug: "iowa-primary-2026", MarketTitle: "Will candidate X win the Iowa primary?",
			Timestamp: time.Date(2026, 5, 20, 18, 30, 0, 0, time.UTC),
			Outcome:   "Yes", PriceBefore: pf(0.38), PriceAfter: pf(0.46),
			Title:       "X endorses cross-party labor coalition; polling lift in evening tracker",
			SourcesJSON: []byte(`[{"name":"AP News","url":"https://apnews.com/x-endorses"},{"name":"Reuters","url":"https://reuters.com/x-endorses"},{"name":"Polymarket","url":"https://polymarket.com/event/iowa-primary-2026"}]`),
		},
		{EventSlug: "fed-cut-mar", MarketTitle: "Will the next Fed meeting cut rates by 25bps?",
			Timestamp:   time.Date(2026, 5, 21, 1, 0, 0, 0, time.UTC),
			Title:       "Fed minutes show divided committee; dovish tilt on next move",
			SourcesJSON: []byte(`[{"name":"Bloomberg","url":"https://bloomberg.com/fed-minutes"}]`),
		},
	}
	links := LinkConfig{
		PolymarketBase:     "https://polymarket.com",
		GrafanaBase:        "https://grafana.example.com",
		GrafanaDashUID:     "watchtower-main",
		GrafanaContext:     time.Hour,
		SourceLinksEnabled: true,
		MaxSourceLinks:     3,
		MaxLinksPerRow:     5,
	}
	in := RenderInput{Request: req, Candidates: cands, Annotations: annotations, Links: links, VisibleN: 8}
	if aiOK {
		in.AIResult = analysis.MarketReportAnalysis{Status: analysis.StatusOK, Model: "gpt-4o-mini",
			ReportText: "Iowa flow concentrated on candidate X intraday. Endorsement narrative validates the move; not pure noise.\nFed cut market drifting on minutes; stable favorite range — operator-watch but not actionable today."}
	} else {
		in.Fallback = FallbackInfo{Reason: "timeout", Message: "openai timeout: context deadline exceeded"}
	}
	body, _ := Render(in)
	return body
}
