package openai

import (
	"testing"

	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/analysis"
)

// TestWebContextPolicy_DisabledShortCircuits pins the kill switch:
// when the operator hasn't opted in via env, no path can sneak a
// Responses-API call.
func TestWebContextPolicy_DisabledShortCircuits(t *testing.T) {
	p := WebContextPolicy{Enabled: false, MinSeverity: "warning",
		ForHotInfo: true, ForStableFavorite: true, ForPolitics: true}
	for _, req := range []analysis.AlertAnalysisRequest{
		{Kind: "stable_favorite", Severity: "critical", Category: "Politics"},
		{Severity: "hard"},
	} {
		if p.ShouldUseWebContext(req, true) {
			t.Errorf("disabled policy must return false for %+v", req)
		}
	}
}

// TestWebContextPolicy_StableFavoriteAlwaysOn pins the spec:
// stable_favorite alerts use web context whenever the global toggle
// is on, regardless of severity.
func TestWebContextPolicy_StableFavoriteAlwaysOn(t *testing.T) {
	p := WebContextPolicy{Enabled: true, MinSeverity: "warning", ForStableFavorite: true}
	got := p.ShouldUseWebContext(analysis.AlertAnalysisRequest{
		Kind:     "stable_favorite",
		Severity: "info", // below MinSeverity
	}, false)
	if !got {
		t.Error("stable_favorite must trigger web context independently of severity")
	}
}

// TestWebContextPolicy_HotInfoUpgrade pins that HOT-lifecycle Info
// alerts qualify when ForHotInfo=true (the operator chose to spend
// AI budget on the final-stretch Info alerts).
func TestWebContextPolicy_HotInfoUpgrade(t *testing.T) {
	p := WebContextPolicy{Enabled: true, MinSeverity: "warning", ForHotInfo: true}
	if !p.ShouldUseWebContext(analysis.AlertAnalysisRequest{Severity: "info"}, true) {
		t.Error("HOT Info must trigger web context")
	}
	if p.ShouldUseWebContext(analysis.AlertAnalysisRequest{Severity: "info"}, false) {
		t.Error("non-HOT Info must NOT trigger web context")
	}
}

// TestWebContextPolicy_SeverityFloor pins the standard "Warning+
// gets web" path. The default MinSeverity is "warning" and only
// alerts at or above that rung qualify on severity alone.
func TestWebContextPolicy_SeverityFloor(t *testing.T) {
	p := WebContextPolicy{Enabled: true, MinSeverity: "warning"}
	if p.ShouldUseWebContext(analysis.AlertAnalysisRequest{Severity: "info"}, false) {
		t.Error("info below floor must NOT trigger web context")
	}
	if !p.ShouldUseWebContext(analysis.AlertAnalysisRequest{Severity: "warning"}, false) {
		t.Error("warning at floor must trigger web context")
	}
	if !p.ShouldUseWebContext(analysis.AlertAnalysisRequest{Severity: "critical"}, false) {
		t.Error("critical above floor must trigger web context")
	}
	if !p.ShouldUseWebContext(analysis.AlertAnalysisRequest{Severity: "hard"}, false) {
		t.Error("hard at top must trigger web context")
	}
}
