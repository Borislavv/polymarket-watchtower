package signalreport

import (
	"fmt"
	"html"
	"strings"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/infra/repository"
)

// FormatTelegram renders a Report into the HTML body the Telegram
// bot accepts. The output is intentionally compact: an operator
// reading on a phone should see the headline numbers without
// scrolling.
//
// Vocabulary discipline:
//   - "directionally correct after resolution" (never "guessed right")
//   - "informed-flow candidate" (never "insider")
//   - "signal quality" (never "edge", "alpha", "guaranteed profit")
func FormatTelegram(r Report) string {
	var b strings.Builder
	writeReportHeader(&b, r)
	writeReportTotals(&b, r)
	if r.SmallSample {
		b.WriteString("\n⚠ <i>Sample size is small; treat this as directional, not statistically stable.</i>\n")
	}
	writeReportCLV(&b, r)
	writeReportBreakdown(&b, "By alert kind", r.ByKind)
	writeReportBreakdown(&b, "By severity", r.BySeverity)
	return b.String()
}

func writeReportHeader(b *strings.Builder, r Report) {
	// Period label is human-friendly: "Daily · 2026-05-17" /
	// "Weekly · 2026-05-11 → 2026-05-17" / "Monthly · 2026-04" / etc.
	label := periodLabel(r)
	fmt.Fprintf(b, "<b>Signal quality · %s · %s</b>\n",
		titleCase(string(r.PeriodType)), html.EscapeString(label))
}

func writeReportTotals(b *strings.Builder, r Report) {
	t := r.Totals
	resolved := t.SuccessCount + t.FailureCount
	b.WriteString("\n<b>Overview</b>\n")
	fmt.Fprintf(b, "• total alerts sent: <b>%d</b>\n", t.TotalAlerts)
	fmt.Fprintf(b, "• resolved: <b>%d</b> (success <b>%d</b> / failure <b>%d</b>)\n", resolved, t.SuccessCount, t.FailureCount)
	if resolved > 0 {
		fmt.Fprintf(b, "• success rate: <b>%.1f%%</b>\n", 100*SuccessRate(t))
	}
	if t.AmbiguousCount > 0 {
		fmt.Fprintf(b, "• ambiguous: %d <i>(resolution unclear at threshold)</i>\n", t.AmbiguousCount)
	}
	if t.UnavailableCount > 0 {
		fmt.Fprintf(b, "• unavailable: %d <i>(market not in upstream snapshot)</i>\n", t.UnavailableCount)
	}
	if t.PendingCount > 0 {
		fmt.Fprintf(b, "• still pending: %d <i>(market not yet resolved)</i>\n", t.PendingCount)
	}
}

func writeReportCLV(b *strings.Builder, r Report) {
	t := r.Totals
	if t.CLV24hSampleCount == 0 {
		return
	}
	b.WriteString("\n<b>CLV-lite (24h post-trade drift)</b>\n")
	fmt.Fprintf(b, "• samples: %d\n", t.CLV24hSampleCount)
	fmt.Fprintf(b, "• avg favourable drift: <b>%+.2f%%</b>\n", t.AvgCLV24h*100)
	fmt.Fprintf(b, "• positive-drift ratio: <b>%.1f%%</b> (%d / %d)\n",
		100*PositiveCLVRatio(t), t.PositiveCLV24hCount, t.CLV24hSampleCount)
}

// writeReportBreakdown renders a "By alert kind" or "By severity"
// section as a list of `label: success/total (rate%) — unresolved=X`
// lines. Skipped when the breakdown is empty.
func writeReportBreakdown(b *strings.Builder, title string, rows []repository.SignalQualityBreakdownRow) {
	if len(rows) == 0 {
		return
	}
	fmt.Fprintf(b, "\n<b>%s</b>\n", html.EscapeString(title))
	for _, r := range rows {
		resolved := r.Success + r.Failure
		rateStr := "n/a"
		if resolved > 0 {
			rateStr = fmt.Sprintf("%.1f%%", 100*float64(r.Success)/float64(resolved))
		}
		fmt.Fprintf(b, "• <code>%s</code>: %d/%d (%s) — unresolved=%d\n",
			html.EscapeString(r.Label), r.Success, resolved, rateStr, r.Unresolved)
	}
}

// periodLabel produces the human-readable label rendered in the
// report header. Anchored on the local-calendar year/month/day of the
// reporting window (Window timestamps are already UTC but the choice
// of label is independent of the operator's timezone).
func periodLabel(r Report) string {
	// All windows are right-exclusive — End is the start of the next
	// period — so subtract 1 second when labelling the boundary.
	endInclusive := r.Window.End.Add(-time.Second)
	switch r.PeriodType {
	case PeriodDaily:
		// Daily ends at 00:00 the NEXT day in local time; the label is
		// the day BEFORE the End boundary.
		return r.Window.Start.Format("2006-01-02")
	case PeriodWeekly:
		return r.Window.Start.Format("2006-01-02") + " → " + endInclusive.Format("2006-01-02")
	case PeriodMonthly:
		return r.Window.Start.Format("2006-01")
	case PeriodQuarterly:
		quarter := (int(r.Window.Start.Month())-1)/3 + 1
		return fmt.Sprintf("%dQ%d", r.Window.Start.Year(), quarter)
	case PeriodYearly:
		return r.Window.Start.Format("2006")
	}
	return r.Window.Start.Format("2006-01-02")
}

func titleCase(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
