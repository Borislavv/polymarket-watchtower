package eventflow

import (
	"strings"
	"testing"
	"time"
)

func TestRender_EmptySummaryProducesNoDataSentence(t *testing.T) {
	out := EventFlowSummary{
		EventSlug: "tx",
		Lookback:  24 * time.Hour,
	}.RenderPromptBlock()
	if !strings.Contains(out, "No meaningful stored flow") {
		t.Errorf("empty summary must say no-data: %s", out)
	}
	if !strings.Contains(out, "Do not infer weak flow from missing data") {
		t.Errorf("must include directive: %s", out)
	}
}

func TestRender_PopulatedSummaryEmitsAllFields(t *testing.T) {
	s := EventFlowSummary{
		EventSlug:    "tx",
		Lookback:     24 * time.Hour,
		RecentAlerts: 7,
		InfoAlerts:   3, WarningAlerts: 2, CriticalAlerts: 1, HardAlerts: 1,
		AlertKinds:                map[string]int{"accumulation": 3, "trade_anomaly": 4},
		StrongestOutcome:          "Ken Paxton",
		StrongestSide:             "BUY",
		StrongestConditionID:      "0xpax",
		SameSideNotionalUSD:       45_000,
		OppositeSideNotionalUSD:   12_000,
		NetDirectionalNotionalUSD: 33_000,
		DirectionalImbalance:      0.58,
		LargestTradeUSD:           50_000,
		LargestTradeOutcome:       "Ken Paxton",
		LargestTradeSide:          "BUY",
		LargestTradeWallet:        "0xabcdef1234567890",
		LargestTradeAt:            time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC),
		AccumulationAlerts:        3,
		WhaleFlowAlerts:           4,
		AccumulationNote:          "3 accumulation alerts in last 24h",
		WhaleNote:                 "4 single-trade whale-flow alerts",
	}
	out := s.RenderPromptBlock()
	for _, want := range []string{
		"window: last 24h",
		"alerts: total=7",
		"strongest side: BUY on Ken Paxton",
		"same-side notional: $45000",
		"opposite-side notional: $12000",
		"directional imbalance: +0.58",
		"largest trade: $50000",
		"accumulation: 3 accumulation alerts",
		"whale flow: 4 single-trade",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestSummary_EmptyDetectsZero(t *testing.T) {
	if !(EventFlowSummary{}).Empty() {
		t.Errorf("zero summary must be empty")
	}
	if (EventFlowSummary{RecentAlerts: 1}).Empty() {
		t.Errorf("non-zero alerts must not be empty")
	}
}
