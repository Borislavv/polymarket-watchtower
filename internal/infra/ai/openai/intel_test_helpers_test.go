package openai

import (
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/analysis"
)

// stubRankingRequest is a tiny request used by tests in this package.
func stubRankingRequest() analysis.AnnotationRankingRequest {
	priceBefore := 0.54
	priceAfter := 0.61
	priceChange := 0.07
	return analysis.AnnotationRankingRequest{
		PeriodStart: time.Date(2026, 5, 20, 10, 0, 0, 0, time.UTC),
		PeriodEnd:   time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC),
		OutputLimit: 10,
		Markets: []analysis.RankingMarket{
			{EventSlug: "texas", MarketSlug: "paxton", ConditionID: "0xpax",
				Question: "Will Ken Paxton win?", LastPrice: 0.62, Volume24hUSD: 95000},
		},
		Annotations: []analysis.RankingAnnotation{
			{EventSlug: "texas", MarketSlug: "paxton", AnnotationHash: "h1",
				Timestamp: time.Date(2026, 5, 9, 14, 0, 0, 0, time.UTC),
				Title:     "Final pre-runoff poll shows Paxton leading", Outcome: "Ken Paxton",
				PriceBefore: &priceBefore, PriceAfter: &priceAfter, PriceChange: &priceChange},
		},
		FlowSummary: analysis.RankingFlowSummary{
			RecentAlertsCount: 4, StrongestSide: "BUY",
			SameSideNotional24h: 25000, AccumulationNote: "5 trades, $40k",
		},
	}
}

func stubDailyIntelRequest() analysis.DailyPoliticalIntelRequest {
	return analysis.DailyPoliticalIntelRequest{
		ReportDate:  time.Date(2026, 5, 20, 0, 0, 0, 0, time.UTC),
		PeriodStart: time.Date(2026, 5, 19, 0, 0, 0, 0, time.UTC),
		PeriodEnd:   time.Date(2026, 5, 20, 0, 0, 0, 0, time.UTC),
		Markets: []analysis.DailyIntelMarket{
			{EventSlug: "texas", MarketSlug: "paxton", ConditionID: "0xpax",
				Question: "Will Ken Paxton win?", Category: "Politics", LifecyclePct: 92.0,
				LastPrice: 0.62, Volume24hUSD: 95000, AlertsLast24h: 4,
				Annotations: []analysis.RankingAnnotation{
					{EventSlug: "texas", Title: "Paxton poll up", Outcome: "Ken Paxton",
						Timestamp: time.Date(2026, 5, 9, 14, 0, 0, 0, time.UTC)},
				}},
		},
		KnownCatalysts: []analysis.DailyIntelCatalyst{
			{EventSlug: "texas", CatalystType: "runoff", Title: "TX runoff",
				Status: "expected", Confidence: 0.78,
				ExpectedAt: time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)},
		},
	}
}
