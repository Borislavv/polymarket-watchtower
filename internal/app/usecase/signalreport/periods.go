// Package signalreport owns the scheduled signal-quality reports
// (daily / weekly / monthly / quarterly / yearly) sent to Telegram.
//
// The package is split deliberately:
//
//   - periods.go    : pure clock math. Zero I/O. Given a now() and a
//     PeriodType, returns the most recent COMPLETED
//     window plus the absolute timestamp at which the
//     report for that window becomes due. Easy to
//     exhaustively test against a frozen clock.
//   - aggregator.go : repository read + result projection. Talks to
//     polymarket_alerts via *AlertRepository; returns
//     a Report value.
//   - render.go     : pure Telegram HTML body. Operates on Report;
//     no clock, no I/O.
//   - worker.go     : the wiring. Hosts the ticker, talks to the SQL
//     scheduler table, calls the bot.
//
// Vocabulary discipline: the package surfaces "signal quality",
// "directional correctness", "resolved alert success rate". It never
// claims insider trading, guaranteed profit, or proof of intent. The
// product value here is honest measurement, not narrative.
package signalreport

import (
	"fmt"
	"time"
)

// PeriodType names the cadence. The values map 1:1 to the
// polymarket_signal_reports.period_type CHECK constraint.
type PeriodType string

const (
	PeriodDaily     PeriodType = "daily"
	PeriodWeekly    PeriodType = "weekly"
	PeriodMonthly   PeriodType = "monthly"
	PeriodQuarterly PeriodType = "quarterly"
	PeriodYearly    PeriodType = "yearly"
)

// AllPeriods is the canonical iteration order — short → long.
var AllPeriods = [...]PeriodType{
	PeriodDaily, PeriodWeekly, PeriodMonthly, PeriodQuarterly, PeriodYearly,
}

// Window is one closed-open time range [Start, End). The report for a
// window covers every alert with sent_at in [Start, End).
type Window struct {
	Start time.Time
	End   time.Time
}

// Span returns End − Start.
func (w Window) Span() time.Duration { return w.End.Sub(w.Start) }

// CompletedWindow returns the most recent CLOSED window of the given
// period_type relative to the supplied `now`, anchored in the supplied
// `loc` (the reporting timezone). The window is the previous full
// period: for daily that's yesterday, for weekly it's the previous
// ISO week (Monday-anchored), monthly is the previous calendar month,
// quarterly is the previous calendar quarter, yearly is the previous
// calendar year.
//
// Both Start and End are returned in UTC so they can be persisted
// without ambiguity; the loc parameter only controls the calendar
// boundaries (a "day" in Etc/GMT-3 starts at 21:00 UTC the previous
// civil day).
func CompletedWindow(now time.Time, kind PeriodType, loc *time.Location) (Window, error) {
	if loc == nil {
		loc = time.UTC
	}
	local := now.In(loc)
	switch kind {
	case PeriodDaily:
		todayStart := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
		return Window{
			Start: todayStart.AddDate(0, 0, -1).UTC(),
			End:   todayStart.UTC(),
		}, nil
	case PeriodWeekly:
		// ISO-8601: weeks start on Monday. Walk back to Monday 00:00,
		// then subtract 7 days to land on the start of the previous
		// completed week.
		offset := int(local.Weekday()-time.Monday+7) % 7
		thisWeekStart := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc).AddDate(0, 0, -offset)
		return Window{
			Start: thisWeekStart.AddDate(0, 0, -7).UTC(),
			End:   thisWeekStart.UTC(),
		}, nil
	case PeriodMonthly:
		thisMonthStart := time.Date(local.Year(), local.Month(), 1, 0, 0, 0, 0, loc)
		return Window{
			Start: thisMonthStart.AddDate(0, -1, 0).UTC(),
			End:   thisMonthStart.UTC(),
		}, nil
	case PeriodQuarterly:
		quarterMonth := time.Month(((int(local.Month())-1)/3)*3 + 1) // 1, 4, 7, 10
		thisQuarterStart := time.Date(local.Year(), quarterMonth, 1, 0, 0, 0, 0, loc)
		return Window{
			Start: thisQuarterStart.AddDate(0, -3, 0).UTC(),
			End:   thisQuarterStart.UTC(),
		}, nil
	case PeriodYearly:
		thisYearStart := time.Date(local.Year(), 1, 1, 0, 0, 0, 0, loc)
		return Window{
			Start: thisYearStart.AddDate(-1, 0, 0).UTC(),
			End:   thisYearStart.UTC(),
		}, nil
	}
	return Window{}, fmt.Errorf("signalreport: unknown period_type %q", kind)
}

// DueAt returns the absolute time (UTC) at which the report for `window`
// becomes due, given a HH:MM-of-day in `loc` and an optional grace
// delay (used only by the yearly report — 72h after year-end so late
// outcome settlement has a chance to land).
//
// The due time is computed as: take window.End, switch to `loc`,
// replace HH:MM with the configured deadline, add `delay`. The result
// converts back to UTC because every persisted timestamp in this
// package is UTC.
func DueAt(window Window, sendAt TimeOfDay, delay time.Duration, loc *time.Location) time.Time {
	if loc == nil {
		loc = time.UTC
	}
	endLocal := window.End.In(loc)
	due := time.Date(endLocal.Year(), endLocal.Month(), endLocal.Day(),
		sendAt.Hour, sendAt.Minute, 0, 0, loc)
	return due.Add(delay).UTC()
}

// TimeOfDay is the parsed form of the SIGNAL_REPORTS_*_AT env vars.
type TimeOfDay struct {
	Hour   int
	Minute int
}

// ParseTimeOfDay accepts "HH:MM" with leading zeros optional. Returns
// an error on anything else — silently defaulting would be worse than
// failing the boot.
func ParseTimeOfDay(s string) (TimeOfDay, error) {
	var hh, mm int
	n, err := fmt.Sscanf(s, "%d:%d", &hh, &mm)
	if err != nil || n != 2 {
		return TimeOfDay{}, fmt.Errorf("signalreport: invalid time-of-day %q (want HH:MM)", s)
	}
	if hh < 0 || hh > 23 || mm < 0 || mm > 59 {
		return TimeOfDay{}, fmt.Errorf("signalreport: time-of-day out of range: %02d:%02d", hh, mm)
	}
	return TimeOfDay{Hour: hh, Minute: mm}, nil
}

// DedupKey is the canonical idempotency key for one period emission.
// The polymarket_signal_reports.dedup_key UNIQUE constraint enforces
// that two workers racing on the same period collapse to one row.
func DedupKey(kind PeriodType, window Window) string {
	return fmt.Sprintf("signal-report:%s:%s:%s",
		kind,
		window.Start.UTC().Format(time.RFC3339),
		window.End.UTC().Format(time.RFC3339))
}
