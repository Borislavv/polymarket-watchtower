package alerting

import (
	"strings"
	"testing"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/marketprediction"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/repricing"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/repository"
)

func ptr(v float64) *float64 { return &v }

func TestRenderNewsPrediction_FullBodySections(t *testing.T) {
	drift := 0.05
	out := RenderNewsPrediction(NewsPredictionInputs{
		Market: NewsPredictionMarket{
			Title: "Will Paxton win the runoff?",
			Price: 0.62, OneDayPriceChange: &drift,
			LifecyclePct: 92.0, Volume24hUSD: 95_000,
		},
		Prediction: repository.MarketPrediction{
			CurrentState: "watching",
			StateReason:  "no decisive signal",
		},
		Decision: marketprediction.Decision{
			NewState: "blocked", PreviousState: "watching",
			Reason: "active catalyst blocks repricing",
		},
		Blocked: &BlockedAlertView{
			BlockedUntil:    "2026-06-15T12:00:00Z",
			Reason:          "TX runoff resolution pending",
			BullishScenario: "decisive Paxton win",
		},
		Repricing: &repricing.Signal{
			RepricingStatus: repricing.StatusUnderreacting,
			FlowTiming:      repricing.FlowTimingPreEvent,
			PriceBefore:     ptr(0.50),
			PriceAfter:      ptr(0.62),
			CurrentPrice:    ptr(0.55),
			Explanation:     "price still 0.07 below price_after",
		},
		AIText: "<honest> opinion text\nwith newline",
		MatchedAlerts: []marketprediction.MatchedAlert{
			{AlertID: 1, Severity: "critical", Kind: "accumulation",
				Score: 0.82, DirectionAlignment: "aligned"},
		},
		LatestAnnotations: []NewsPredictionAnnotation{
			{Date: "2026-05-09", Title: "Paxton poll up", Outcome: "Ken Paxton"},
		},
	})
	for _, want := range []string{
		"<b>NEWS &amp; PREDICTION: blocked · Will Paxton win the runoff?</b>",
		"<b>Market</b>",
		"price: 0.6200",
		"24h: +0.050",
		"<b>Prediction state</b>",
		"<b>Blocked / Catalyst</b>",
		"blocked until: 2026-06-15T12:00:00Z",
		"due to: TX runoff resolution pending",
		"<b>Repricing intelligence</b>",
		"status: underreacting",
		"flow timing: pre_event_positioning",
		"<b>AI prediction</b>",
		// AI text must be HTML-escaped.
		"&lt;honest&gt; opinion text",
		"<b>Matched Watchtower alerts</b>",
		"critical · accumulation · score=0.82 · aligned",
		"<b>Latest Polymarket events</b>",
		"1. 2026-05-09 · Paxton poll up · outcome=Ken Paxton",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestRenderNewsPrediction_OmitsEmptySections(t *testing.T) {
	out := RenderNewsPrediction(NewsPredictionInputs{
		Market: NewsPredictionMarket{Title: "X", Price: 0.5},
	})
	for _, mustNot := range []string{
		"<b>Blocked / Catalyst</b>",
		"<b>Repricing intelligence</b>",
		"<b>AI prediction</b>",
		"<b>Matched Watchtower alerts</b>",
		"<b>Latest Polymarket events</b>",
	} {
		if strings.Contains(out, mustNot) {
			t.Errorf("empty input must not render %q:\n%s", mustNot, out)
		}
	}
	if !strings.Contains(out, "NEWS &amp; PREDICTION") {
		t.Errorf("header must always render: %s", out)
	}
}

func TestRenderNewsPrediction_DefaultsStateToWatching(t *testing.T) {
	out := RenderNewsPrediction(NewsPredictionInputs{
		Market: NewsPredictionMarket{Title: "X", Price: 0.5},
	})
	if !strings.Contains(out, "watching") {
		t.Errorf("default state must be watching: %s", out)
	}
}

// Stable iteration: building MatchedAlertsForRender from a 3-row
// slice must preserve insertion order on tie + sort by score desc.
func TestMatchedAlertsForRender_SortsByScoreDesc(t *testing.T) {
	in := []marketprediction.MatchedAlert{
		{AlertID: 1, Score: 0.4, AlertAt: time.Now()},
		{AlertID: 2, Score: 0.9, AlertAt: time.Now()},
		{AlertID: 3, Score: 0.7, AlertAt: time.Now()},
	}
	out := MatchedAlertsForRender(in)
	if out[0].AlertID != 2 || out[1].AlertID != 3 || out[2].AlertID != 1 {
		t.Errorf("sort order wrong: %+v", out)
	}
}
