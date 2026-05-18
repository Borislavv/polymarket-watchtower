package signalreport

import (
	"testing"
	"time"
)

// gmtPlus3 is the reporting zone the watchtower runs in. Etc/GMT-3 in
// the IANA database is UTC+3 (the sign is inverted by historical
// convention — see https://en.wikipedia.org/wiki/Tz_database#Area).
var gmtPlus3 = time.FixedZone("Etc/GMT-3", 3*60*60)

// TestCompletedWindow_Daily exhaustively pins the daily window for a
// representative `now` — given any moment on 2026-05-18 in UTC+3, the
// "previous completed day" is 2026-05-17 (start 2026-05-16T21:00Z,
// end 2026-05-17T21:00Z).
func TestCompletedWindow_Daily(t *testing.T) {
	// 2026-05-18 08:00 GMT+3 == 2026-05-18 05:00 UTC
	now := time.Date(2026, 5, 18, 5, 0, 0, 0, time.UTC)
	w, err := CompletedWindow(now, PeriodDaily, gmtPlus3)
	if err != nil {
		t.Fatalf("CompletedWindow: %v", err)
	}
	wantStart := time.Date(2026, 5, 16, 21, 0, 0, 0, time.UTC) // 2026-05-17 00:00 GMT+3
	wantEnd := time.Date(2026, 5, 17, 21, 0, 0, 0, time.UTC)   // 2026-05-18 00:00 GMT+3
	if !w.Start.Equal(wantStart) {
		t.Errorf("Start: got %s want %s", w.Start, wantStart)
	}
	if !w.End.Equal(wantEnd) {
		t.Errorf("End: got %s want %s", w.End, wantEnd)
	}
	if w.Span() != 24*time.Hour {
		t.Errorf("Span: got %s want 24h", w.Span())
	}
}

// TestCompletedWindow_Weekly_MondayAnchor pins the ISO-8601 Monday
// anchor. "Now" is Monday 2026-05-18 (GMT+3); the previous full week
// is 2026-05-11..2026-05-17 (inclusive, displayed as [2026-05-11,
// 2026-05-18) in clock terms).
func TestCompletedWindow_Weekly_MondayAnchor(t *testing.T) {
	// 2026-05-18 is a Monday.
	now := time.Date(2026, 5, 18, 5, 0, 0, 0, time.UTC)
	w, err := CompletedWindow(now, PeriodWeekly, gmtPlus3)
	if err != nil {
		t.Fatalf("CompletedWindow: %v", err)
	}
	wantStart := time.Date(2026, 5, 10, 21, 0, 0, 0, time.UTC) // 2026-05-11 00:00 GMT+3
	wantEnd := time.Date(2026, 5, 17, 21, 0, 0, 0, time.UTC)   // 2026-05-18 00:00 GMT+3
	if !w.Start.Equal(wantStart) {
		t.Errorf("Start: got %s want %s", w.Start, wantStart)
	}
	if !w.End.Equal(wantEnd) {
		t.Errorf("End: got %s want %s", w.End, wantEnd)
	}
}

// TestCompletedWindow_Weekly_WednesdayBacksToPrevMonday confirms a
// mid-week `now` still anchors back to the previous Monday, not the
// current week's Monday.
func TestCompletedWindow_Weekly_WednesdayBacksToPrevMonday(t *testing.T) {
	// 2026-05-20 is a Wednesday.
	now := time.Date(2026, 5, 20, 5, 0, 0, 0, time.UTC)
	w, _ := CompletedWindow(now, PeriodWeekly, gmtPlus3)
	wantStart := time.Date(2026, 5, 10, 21, 0, 0, 0, time.UTC)
	wantEnd := time.Date(2026, 5, 17, 21, 0, 0, 0, time.UTC)
	if !w.Start.Equal(wantStart) || !w.End.Equal(wantEnd) {
		t.Errorf("Window: got [%s, %s); want [%s, %s)", w.Start, w.End, wantStart, wantEnd)
	}
}

// TestCompletedWindow_Monthly_PrevMonth confirms a moment in May
// returns the April window.
func TestCompletedWindow_Monthly_PrevMonth(t *testing.T) {
	now := time.Date(2026, 5, 18, 5, 0, 0, 0, time.UTC)
	w, _ := CompletedWindow(now, PeriodMonthly, gmtPlus3)
	wantStart := time.Date(2026, 3, 31, 21, 0, 0, 0, time.UTC) // 2026-04-01 00:00 GMT+3
	wantEnd := time.Date(2026, 4, 30, 21, 0, 0, 0, time.UTC)   // 2026-05-01 00:00 GMT+3
	if !w.Start.Equal(wantStart) || !w.End.Equal(wantEnd) {
		t.Errorf("Window: got [%s, %s); want [%s, %s)", w.Start, w.End, wantStart, wantEnd)
	}
}

// TestCompletedWindow_Quarterly tests every quarter boundary so a
// silent off-by-one regression on the (month-1)/3*3+1 math is loud.
func TestCompletedWindow_Quarterly(t *testing.T) {
	cases := []struct {
		now                time.Time
		wantStart, wantEnd time.Time
	}{
		// February → previous quarter is Q4 of previous year.
		{
			now:       time.Date(2026, 2, 15, 5, 0, 0, 0, time.UTC),
			wantStart: time.Date(2025, 9, 30, 21, 0, 0, 0, time.UTC),  // 2025-10-01 00:00 GMT+3
			wantEnd:   time.Date(2025, 12, 31, 21, 0, 0, 0, time.UTC), // 2026-01-01 00:00 GMT+3
		},
		// May → previous quarter is Q1.
		{
			now:       time.Date(2026, 5, 18, 5, 0, 0, 0, time.UTC),
			wantStart: time.Date(2025, 12, 31, 21, 0, 0, 0, time.UTC),
			wantEnd:   time.Date(2026, 3, 31, 21, 0, 0, 0, time.UTC),
		},
		// August → previous quarter is Q2.
		{
			now:       time.Date(2026, 8, 10, 5, 0, 0, 0, time.UTC),
			wantStart: time.Date(2026, 3, 31, 21, 0, 0, 0, time.UTC),
			wantEnd:   time.Date(2026, 6, 30, 21, 0, 0, 0, time.UTC),
		},
		// November → previous quarter is Q3.
		{
			now:       time.Date(2026, 11, 5, 5, 0, 0, 0, time.UTC),
			wantStart: time.Date(2026, 6, 30, 21, 0, 0, 0, time.UTC),
			wantEnd:   time.Date(2026, 9, 30, 21, 0, 0, 0, time.UTC),
		},
	}
	for _, tc := range cases {
		t.Run(tc.now.Month().String(), func(t *testing.T) {
			w, _ := CompletedWindow(tc.now, PeriodQuarterly, gmtPlus3)
			if !w.Start.Equal(tc.wantStart) {
				t.Errorf("Start: got %s want %s", w.Start, tc.wantStart)
			}
			if !w.End.Equal(tc.wantEnd) {
				t.Errorf("End: got %s want %s", w.End, tc.wantEnd)
			}
		})
	}
}

// TestCompletedWindow_Yearly pins the year boundary including the
// 72h-delay yearly report use case.
func TestCompletedWindow_Yearly(t *testing.T) {
	now := time.Date(2027, 1, 4, 5, 0, 0, 0, time.UTC) // 2027-01-04 08:00 GMT+3
	w, _ := CompletedWindow(now, PeriodYearly, gmtPlus3)
	wantStart := time.Date(2025, 12, 31, 21, 0, 0, 0, time.UTC) // 2026-01-01 00:00 GMT+3
	wantEnd := time.Date(2026, 12, 31, 21, 0, 0, 0, time.UTC)   // 2027-01-01 00:00 GMT+3
	if !w.Start.Equal(wantStart) || !w.End.Equal(wantEnd) {
		t.Errorf("Window: got [%s, %s); want [%s, %s)", w.Start, w.End, wantStart, wantEnd)
	}
}

// TestDueAt_DailyAt8AM confirms the daily report becomes due at 08:00
// GMT+3 on the day after the window ends.
func TestDueAt_DailyAt8AM(t *testing.T) {
	w := Window{
		Start: time.Date(2026, 5, 16, 21, 0, 0, 0, time.UTC),
		End:   time.Date(2026, 5, 17, 21, 0, 0, 0, time.UTC),
	}
	due := DueAt(w, TimeOfDay{Hour: 8, Minute: 0}, 0, gmtPlus3)
	// 2026-05-18 08:00 GMT+3 == 2026-05-18 05:00 UTC
	want := time.Date(2026, 5, 18, 5, 0, 0, 0, time.UTC)
	if !due.Equal(want) {
		t.Errorf("DueAt: got %s want %s", due, want)
	}
}

// TestDueAt_YearlyDelayed72h pins the yearly delay: a yearly report
// for 2026 lands on 2027-01-04 08:00 GMT+3 (3 days after year end).
func TestDueAt_YearlyDelayed72h(t *testing.T) {
	w := Window{
		Start: time.Date(2025, 12, 31, 21, 0, 0, 0, time.UTC),
		End:   time.Date(2026, 12, 31, 21, 0, 0, 0, time.UTC), // 2027-01-01 00:00 GMT+3
	}
	due := DueAt(w, TimeOfDay{Hour: 8, Minute: 0}, 72*time.Hour, gmtPlus3)
	// 2027-01-01 08:00 GMT+3 + 72h == 2027-01-04 08:00 GMT+3 == 2027-01-04 05:00 UTC
	want := time.Date(2027, 1, 4, 5, 0, 0, 0, time.UTC)
	if !due.Equal(want) {
		t.Errorf("DueAt: got %s want %s", due, want)
	}
}

// TestDedupKey_StableAcrossLocations confirms the dedup key only
// depends on the UTC window, not on the reporting timezone. Two
// different operators in different zones produce identical keys for
// the same period.
func TestDedupKey_StableAcrossLocations(t *testing.T) {
	w := Window{
		Start: time.Date(2026, 5, 16, 21, 0, 0, 0, time.UTC),
		End:   time.Date(2026, 5, 17, 21, 0, 0, 0, time.UTC),
	}
	const want = "signal-report:daily:2026-05-16T21:00:00Z:2026-05-17T21:00:00Z"
	if got := DedupKey(PeriodDaily, w); got != want {
		t.Errorf("DedupKey: got %q want %q", got, want)
	}
}

// TestParseTimeOfDay validates parsing + rejections.
func TestParseTimeOfDay(t *testing.T) {
	cases := []struct {
		s         string
		wantHour  int
		wantMin   int
		wantError bool
	}{
		{"08:00", 8, 0, false},
		{"23:59", 23, 59, false},
		{"00:00", 0, 0, false},
		{"24:00", 0, 0, true},
		{"08:60", 0, 0, true},
		{"8", 0, 0, true},
		{"", 0, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.s, func(t *testing.T) {
			got, err := ParseTimeOfDay(tc.s)
			if (err != nil) != tc.wantError {
				t.Fatalf("err: %v wantError=%v", err, tc.wantError)
			}
			if !tc.wantError && (got.Hour != tc.wantHour || got.Minute != tc.wantMin) {
				t.Errorf("got %+v want %d:%d", got, tc.wantHour, tc.wantMin)
			}
		})
	}
}
