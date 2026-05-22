// PART 5 / PART 8: pin the exact shape of the v11.3 Signal quality
// admin report. The fixture mirrors the operator-spec example
// verbatim — any drift to section headers or bullet structure
// breaks the test before merge.
package signalquality

import (
	"strings"
	"testing"
	"time"
)

// TestFormat_PinsExactSpecShape locks the v11.3 admin body to the
// fixture spelled out in the operator spec (PART 5 + PART 8). If a
// future renderer change moves a bullet or swaps a delimiter, this
// test fails and the operator must explicitly update the fixture.
func TestFormat_PinsExactSpecShape(t *testing.T) {
	snap := Snapshot{
		Period:          PeriodDaily,
		ReportDate:      time.Date(2026, 5, 20, 8, 0, 0, 0, time.UTC),
		TotalSent:       74,
		ResolvedCorrect: 0,
		ResolvedWrong:   0,
		Unresolved:      74,
		Kinds: []KindBreakdown{
			{Kind: "accumulation", Success: 0, Failure: 0, Unresolved: 40},
			{Kind: "trade_anomaly", Success: 0, Failure: 0, Unresolved: 34},
		},
		Severities: []SeverityBreakdown{
			{Severity: "info", Success: 0, Failure: 0, Unresolved: 53},
			{Severity: "warning", Success: 0, Failure: 0, Unresolved: 16},
			{Severity: "critical", Success: 0, Failure: 0, Unresolved: 5},
		},
	}
	got := Format(snap)

	for _, expected := range []string{
		"<b>Signal quality · Daily · 2026-05-20</b>",
		"<b>Overview</b>",
		"• total alerts sent: 74",
		"• resolved: 0 (success 0 / failure 0)",
		"• still pending: 74 (market not yet resolved)",
		"⚠ Sample size is small; treat this as directional, not statistically stable.",
		"<b>By alert kind</b>",
		"• accumulation: 0/0 (n/a) — unresolved=40",
		"• trade_anomaly: 0/0 (n/a) — unresolved=34",
		"<b>By severity</b>",
		"• info: 0/0 (n/a) — unresolved=53",
		"• warning: 0/0 (n/a) — unresolved=16",
		"• critical: 0/0 (n/a) — unresolved=5",
	} {
		if !strings.Contains(got, expected) {
			t.Errorf("rendered body missing fragment:\nwant: %q\nin body:\n%s", expected, got)
		}
	}
}

// TestFormat_HidesBannerOnLargeSample — once enough alerts have
// resolved (≥ MinSampleSize), the "directional" warning drops out
// of the body. Pinned so an operator who flips the threshold sees
// the rendered shape change immediately.
func TestFormat_HidesBannerOnLargeSample(t *testing.T) {
	snap := Snapshot{
		Period:          PeriodWeekly,
		ReportDate:      time.Date(2026, 5, 25, 0, 0, 0, 0, time.UTC),
		TotalSent:       500,
		ResolvedCorrect: 110,
		ResolvedWrong:   40,
		Unresolved:      350,
		MinSampleSize:   100,
	}
	got := Format(snap)
	if strings.Contains(got, "directional, not statistically stable") {
		t.Fatalf("banner must elide once Resolved() >= MinSampleSize; body:\n%s", got)
	}
	if !strings.Contains(got, "• resolved: 150 (success 110 / failure 40)") {
		t.Errorf("expected resolved aggregate, got body:\n%s", got)
	}
}

// TestFormat_EscapesEvilKindLabels — kind/severity are enums today
// but the renderer must HTML-escape them anyway. Future kinds added
// from operator-authored config must not be able to inject markup.
func TestFormat_EscapesEvilKindLabels(t *testing.T) {
	snap := Snapshot{
		Period:     PeriodMonthly,
		ReportDate: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		TotalSent:  1,
		Unresolved: 1,
		Kinds: []KindBreakdown{
			{Kind: "<script>alert(1)</script>", Unresolved: 1},
		},
	}
	got := Format(snap)
	if strings.Contains(got, "<script>") {
		t.Fatalf("kind label MUST be HTML-escaped; body:\n%s", got)
	}
}

// TestFormat_HeaderTitlePerPeriod — title carries the period label
// literally so the operator can grep across days.
func TestFormat_HeaderTitlePerPeriod(t *testing.T) {
	for _, p := range []Period{PeriodDaily, PeriodWeekly, PeriodMonthly, PeriodQuarterly, PeriodYearly} {
		snap := Snapshot{
			Period:     p,
			ReportDate: time.Date(2026, 7, 4, 0, 0, 0, 0, time.UTC),
		}
		got := Format(snap)
		if !strings.HasPrefix(got, "<b>Signal quality · "+string(p)+" · 2026-07-04</b>") {
			t.Errorf("period=%s: title missing or wrong; body=%q", p, got[:min(120, len(got))])
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// TestFormat_RendersLookbackWindowCaption pins the v11.4 caption.
// Operators read it to verify the rendered body is bounded by the
// configured window (not "all-time").
func TestFormat_RendersLookbackWindowCaption(t *testing.T) {
	snap := Snapshot{
		Period:        PeriodDaily,
		ReportDate:    time.Date(2026, 5, 20, 0, 0, 0, 0, time.UTC),
		LookbackHours: 24,
	}
	got := Format(snap)
	if !strings.Contains(got, "<i>window: last 24h</i>") {
		t.Errorf("missing lookback caption; got:\n%s", got)
	}
}

// TestFormat_RendersTruncatedBanner verifies the v11.4 "Scan
// truncated" banner fires when the snapshot says Truncated=true.
// The banner surfaces the eligible count + the cap so the operator
// can raise SIGNAL_QUALITY_MAX_ALERTS without trial-and-error.
func TestFormat_RendersTruncatedBanner(t *testing.T) {
	snap := Snapshot{
		Period:           PeriodWeekly,
		ReportDate:       time.Date(2026, 5, 22, 0, 0, 0, 0, time.UTC),
		TotalSent:        5000,
		LookbackHours:    168,
		EligibleCount:    12000,
		MaxAlertsScanCap: 5000,
		Truncated:        true,
	}
	got := Format(snap)
	if !strings.Contains(got, "Scan truncated") {
		t.Errorf("missing truncated banner; got:\n%s", got)
	}
	if !strings.Contains(got, "12000 eligible") {
		t.Errorf("missing eligible count; got:\n%s", got)
	}
	if !strings.Contains(got, "scanned newest 5000") {
		t.Errorf("missing scan-cap mention; got:\n%s", got)
	}
}

// TestFormat_NoTruncationBannerWhenWithinCap — the banner is
// elided when the bounded scan didn't hit the cap.
func TestFormat_NoTruncationBannerWhenWithinCap(t *testing.T) {
	snap := Snapshot{
		Period:           PeriodDaily,
		ReportDate:       time.Date(2026, 5, 22, 0, 0, 0, 0, time.UTC),
		TotalSent:        300,
		LookbackHours:    24,
		EligibleCount:    300,
		MaxAlertsScanCap: 5000,
		Truncated:        false,
	}
	got := Format(snap)
	if strings.Contains(got, "Scan truncated") {
		t.Errorf("banner must elide when within cap; got:\n%s", got)
	}
}
