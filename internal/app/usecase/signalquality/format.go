// Package signalquality renders the v11.3 admin-only "Signal quality"
// telemetry report. The body is structural — counts of alerts sent
// per kind and per severity, win/loss aggregates, unresolved counts —
// it is NOT a customer-facing flow alert and NEVER routes to the
// signal chat. The Router maps SurfaceSignalQualityReport →
// TELEGRAM_ADMIN_CHAT_ID; the worker passes the typed surface so the
// routing decision can never silently drift.
//
// Failure semantics: every step degrades silently. DB read failures
// log + skip the cycle; AI is NOT involved (deterministic aggregate
// only); Telegram send failures are observed via
// watchtower_telegram_send_failed_total{surface="signal_quality_report"}.
package signalquality

import (
	"fmt"
	"html"
	"strings"
	"time"
)

// Period names the aggregation window the report covers. The
// rendered title uses the human-readable form ("Daily", "Weekly",
// etc.). Underlying aggregation is identical for every period; the
// label affects only the title.
type Period string

const (
	PeriodDaily     Period = "Daily"
	PeriodWeekly    Period = "Weekly"
	PeriodMonthly   Period = "Monthly"
	PeriodQuarterly Period = "Quarterly"
	PeriodYearly    Period = "Yearly"
)

// KindBreakdown is the per-kind row in the report.
type KindBreakdown struct {
	Kind       string
	Success    int
	Failure    int
	Unresolved int
}

// SeverityBreakdown is the per-severity row.
type SeverityBreakdown struct {
	Severity   string // info | warning | critical | hard
	Success    int
	Failure    int
	Unresolved int
}

// Snapshot is the deterministic aggregate the renderer consumes.
// Counts come from polymarket_alerts: TotalSent is rows with
// status='sent', ResolvedCorrect / ResolvedWrong sum rows whose
// outcome_status matches, Unresolved is the difference. The
// MinSampleSize threshold (default 100) is used by the formatter
// to add a "directional, not statistically stable" banner — the
// real signal-quality intelligence comes from Grafana, the
// Telegram body is just a heartbeat.
type Snapshot struct {
	Period          Period
	ReportDate      time.Time // formatted as YYYY-MM-DD
	TotalSent       int
	ResolvedCorrect int
	ResolvedWrong   int
	Unresolved      int
	Kinds           []KindBreakdown
	Severities      []SeverityBreakdown
	MinSampleSize   int

	// v11.4: bounded-scan metadata. LookbackHours and
	// MaxAlertsScanCap are the operator-configured limits; the
	// snapshot is rendered with a "<lookback>h window" caption.
	// EligibleCount is COUNT(*) over the bounded window (cheap);
	// ScannedCount is the row count that survived the LIMIT (=
	// MaxAlertsScanCap when Truncated, else = EligibleCount).
	// Truncated=true is rendered as a "scan truncated" banner.
	LookbackHours    int
	MaxAlertsScanCap int
	EligibleCount    int
	ScannedCount     int
	Truncated        bool
}

// Resolved returns the number of resolved alerts (correct + wrong).
func (s Snapshot) Resolved() int { return s.ResolvedCorrect + s.ResolvedWrong }

// Format renders a Snapshot as the v11.3 admin-channel HTML body.
// The shape is pinned by signalquality.Format_test.go — any drift
// to the section headers or bullet structure fails before merge.
//
// Layout (matches the v11.3 PART 5 fixture):
//
//	<b>Signal quality · Daily · 2026-05-20</b>
//
//	<b>Overview</b>
//	• total alerts sent: 74
//	• resolved: 0 (success 0 / failure 0)
//	• still pending: 74 (market not yet resolved)
//
//	⚠ Sample size is small; treat this as directional, not statistically stable.
//
//	<b>By alert kind</b>
//	• accumulation: 0/0 (n/a) — unresolved=40
//	• trade_anomaly: 0/0 (n/a) — unresolved=34
//
//	<b>By severity</b>
//	• info: 0/0 (n/a) — unresolved=53
//	• warning: 0/0 (n/a) — unresolved=16
//	• critical: 0/0 (n/a) — unresolved=5
//
// Every operator-visible token passes through html.EscapeString —
// the renderer never trusts raw DB strings even though the kind /
// severity values are enums today.
func Format(s Snapshot) string {
	var b strings.Builder
	fmt.Fprintf(&b, "<b>Signal quality · %s · %s</b>\n",
		html.EscapeString(string(s.Period)),
		html.EscapeString(s.ReportDate.UTC().Format("2006-01-02")),
	)

	// v11.4 lookback caption — operator sees exactly which window
	// the body covers so the "totals" can't be misread as
	// all-time.
	if s.LookbackHours > 0 {
		fmt.Fprintf(&b, "<i>window: last %dh</i>\n", s.LookbackHours)
	}

	b.WriteString("\n<b>Overview</b>\n")
	fmt.Fprintf(&b, "• total alerts sent: %d\n", s.TotalSent)
	fmt.Fprintf(&b, "• resolved: %d (success %d / failure %d)\n",
		s.Resolved(), s.ResolvedCorrect, s.ResolvedWrong)
	fmt.Fprintf(&b, "• still pending: %d (market not yet resolved)\n", s.Unresolved)

	// v11.4 scan-truncation banner. Renders only when the
	// eligible row count exceeded the operator-configured cap;
	// surfaces the cap + eligible count so the operator can
	// raise the limit if needed.
	if s.Truncated {
		fmt.Fprintf(&b, "\n⚠ Scan truncated: %d eligible rows in window, scanned newest %d (raise SIGNAL_QUALITY_MAX_ALERTS to widen).\n",
			s.EligibleCount, s.MaxAlertsScanCap)
	}

	minSample := s.MinSampleSize
	if minSample <= 0 {
		minSample = 100
	}
	if s.Resolved() < minSample {
		b.WriteString("\n⚠ Sample size is small; treat this as directional, not statistically stable.\n")
	}

	if len(s.Kinds) > 0 {
		b.WriteString("\n<b>By alert kind</b>\n")
		for _, k := range s.Kinds {
			fmt.Fprintf(&b, "• %s: %d/%d (%s) — unresolved=%d\n",
				html.EscapeString(k.Kind),
				k.Success,
				k.Success+k.Failure,
				formatRate(k.Success, k.Failure),
				k.Unresolved,
			)
		}
	}

	if len(s.Severities) > 0 {
		b.WriteString("\n<b>By severity</b>\n")
		for _, sev := range s.Severities {
			fmt.Fprintf(&b, "• %s: %d/%d (%s) — unresolved=%d\n",
				html.EscapeString(sev.Severity),
				sev.Success,
				sev.Success+sev.Failure,
				formatRate(sev.Success, sev.Failure),
				sev.Unresolved,
			)
		}
	}

	return strings.TrimRight(b.String(), "\n")
}

// formatRate returns "n/a" when the denominator is zero, otherwise a
// percentage to one decimal place. Used by both Kind and Severity
// rows.
func formatRate(success, failure int) string {
	denom := success + failure
	if denom == 0 {
		return "n/a"
	}
	pct := float64(success) / float64(denom) * 100
	return fmt.Sprintf("%.1f%%", pct)
}
