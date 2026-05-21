package alerting

import (
	"strings"
	"testing"
	"time"
)

func TestRenderHeader_RegularReportFields(t *testing.T) {
	got := RenderHeader(HeaderInput{
		Type:         MessageTypeMarketIntel,
		Frequency:    2 * time.Hour,
		LastPostedAt: time.Date(2026, 5, 21, 4, 0, 0, 0, time.UTC),
		Now:          time.Date(2026, 5, 21, 6, 0, 0, 0, time.UTC),
		Strategies:   []string{"market_intel"},
		AI: AIInfo{
			Status:       AIStatusOK,
			CostUSD:      0.0123,
			PromptTokens: 4321,
			OutputTokens: 432,
		},
	})
	for _, want := range []string{
		"<b>Type:</b> market_intel",
		"<b>Trigger:</b> frequency=2h",
		"last_posted_at=2026-05-21T04:00:00Z",
		"now=2026-05-21T06:00:00Z",
		"<b>Strategy:</b> Market intelligence",
		"<b>AI:</b> status=ok",
		"cost=$0.0123",
		"tokens=4321/432",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in header:\n%s", want, got)
		}
	}
}

func TestRenderHeader_TriggeredAlertOverridesFrequency(t *testing.T) {
	got := RenderHeader(HeaderInput{
		Type:        MessageTypeTriggered,
		TriggeredBy: "single_trade_p99",
		Strategies:  []string{"whale_flow", "accumulation_recent"},
		AI:          AIInfo{Status: AIStatusOK, CostUSD: 0.0024},
	})
	if !strings.Contains(got, "<b>Trigger:</b> by=single_trade_p99") {
		t.Errorf("expected by= line; got:\n%s", got)
	}
	if strings.Contains(got, "frequency=") {
		t.Errorf("triggered alerts must NOT render frequency:\n%s", got)
	}
	if !strings.Contains(got, "Whale flow — unusually large single trade") {
		t.Errorf("strategy must render human label:\n%s", got)
	}
}

func TestRenderHeader_AIStatusSkippedRendersZeroCost(t *testing.T) {
	got := RenderHeader(HeaderInput{
		Type:       MessageTypeRegular,
		Frequency:  24 * time.Hour,
		Strategies: []string{"daily_intel"},
		AI:         AIInfo{Status: AIStatusSkipped},
	})
	if !strings.Contains(got, "status=skipped") {
		t.Errorf("expected status=skipped: %s", got)
	}
	if !strings.Contains(got, "cost=$0") {
		t.Errorf("expected explicit $0 cost on skipped AI: %s", got)
	}
}

func TestRenderHeader_NoFieldsReturnsEmpty(t *testing.T) {
	got := RenderHeader(HeaderInput{})
	if got != "" {
		t.Errorf("empty input must return empty string; got %q", got)
	}
}

func TestStrategyLabel_KnownAndUnknown(t *testing.T) {
	if got := StrategyLabel("whale_flow"); !strings.Contains(got, "Whale flow") {
		t.Errorf("known key: %q", got)
	}
	if got := StrategyLabel("Whale-Flow"); !strings.Contains(got, "Whale flow") {
		t.Errorf("normalisation failed: %q", got)
	}
	if got := StrategyLabel("totally_new_strategy_v99"); got != "totally_new_strategy_v99" {
		t.Errorf("unknown key must pass through: %q", got)
	}
	if got := StrategyLabel(""); got != "" {
		t.Errorf("empty key must return empty: %q", got)
	}
}

func TestStrategyLabels_DedupAndOrdering(t *testing.T) {
	got := StrategyLabels([]string{"whale_flow", "accumulation", "accumulation_recent", "", "whale_flow"})
	if len(got) != 2 {
		// "accumulation" and "accumulation_recent" map to the same
		// label ("Recent accumulation — same-side …").
		t.Errorf("dedupe failed; got %v", got)
	}
}
