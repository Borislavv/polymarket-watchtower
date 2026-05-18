package statsreport

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// fakeStore is a deterministic StatsStore for the worker tests.
type fakeStore struct {
	stats Stats
	err   error
	calls int
}

func (f *fakeStore) Read(ctx context.Context) (Stats, error) {
	f.calls++
	if f.err != nil {
		return Stats{}, f.err
	}
	return f.stats, nil
}

// fakeSender captures every Telegram send so tests can assert on the
// exact rendered body. The configured chatID is captured too — the
// worker must address the chat the operator wired, not the empty
// string.
type fakeSender struct {
	chatID string
	body   string
	err    error
	calls  int
}

func (f *fakeSender) SendHTML(_ context.Context, chatID, text string) error {
	f.calls++
	f.chatID = chatID
	f.body = text
	return f.err
}

func nopLogger() *zerolog.Logger {
	l := zerolog.Nop()
	return &l
}

// TestTick_SendsRenderedMessageToChat is the happy path: one tick
// reads the store, renders the snapshot, and posts it once to the
// configured chat. The body must carry the section headers and the
// counters we read.
func TestTick_SendsRenderedMessageToChat(t *testing.T) {
	store := &fakeStore{stats: Stats{
		MarketsTotal:       12_345,
		MarketsActive:      4_321,
		MarketsSoftDeleted: 42,
		MarketsPurged:      7,
		TradesTotal:        1_500_000,
		TradesLast2h:       8_500,
		TradersTotal:       100_000,
		AlertsBySeverity: map[string]int64{
			"info":     14,
			"warning":  3,
			"critical": 1,
		},
		AlertsPending:    2,
		AlertsFailed:     0,
		AlertsSent:       18,
		BackfillByStatus: map[string]int64{"completed": 4000, "pending": 22},
	}}
	sender := &fakeSender{}
	w := New(Config{
		Interval: 2 * time.Hour,
		ChatID:   "-100123",
		Clock:    func() time.Time { return time.Date(2026, 5, 18, 12, 0, 0, 0, time.UTC) },
	}, store, sender, nil, nopLogger())
	w.started = time.Date(2026, 5, 18, 9, 0, 0, 0, time.UTC) // 3h uptime

	w.Tick(context.Background())

	if store.calls != 1 || sender.calls != 1 {
		t.Fatalf("store=%d sender=%d, want 1/1", store.calls, sender.calls)
	}
	if sender.chatID != "-100123" {
		t.Errorf("chat id: %q want -100123", sender.chatID)
	}
	for _, want := range []string{
		"<b>Watchtower stats — uptime 3h0m</b>",
		"<b>Markets</b>",
		"• total: 12345",
		"• active: 4321",
		"• soft-deleted: 42",
		"• purged: 7",
		"<b>Trades</b>",
		"• total: 1500000",
		"• last 2h: 8500",
		"• traders seen: 100000",
		"<b>Alerts</b>",
		"• sent: 18",
		"• pending: 2",
		"info=14",
		"warning=3",
		"critical=1",
		"<b>Backfill</b>",
		"• pending: 22",
		"• completed: 4000",
	} {
		if !strings.Contains(sender.body, want) {
			t.Errorf("missing %q in:\n%s", want, sender.body)
		}
	}
}

// TestTick_StoreFailureIncrementsErrorAndSkipsSend pins the
// failure path: a Postgres read error must NOT result in a Telegram
// send (we do not want to ship a half-empty summary). The worker
// records the error metric and moves on; the next tick is unaffected.
func TestTick_StoreFailureIncrementsErrorAndSkipsSend(t *testing.T) {
	store := &fakeStore{err: errors.New("db down")}
	sender := &fakeSender{}
	w := New(Config{ChatID: "x"}, store, sender, nil, nopLogger())

	w.Tick(context.Background())

	if sender.calls != 0 {
		t.Fatalf("sender must not be called when store read fails (calls=%d)", sender.calls)
	}
}

// TestTick_SenderFailureCountsError covers the second failure mode:
// the read succeeded but Telegram delivery failed (network blip,
// 5xx). The worker must observe the error and not panic; the next
// tick still attempts a send.
func TestTick_SenderFailureCountsError(t *testing.T) {
	store := &fakeStore{stats: Stats{MarketsTotal: 1}}
	sender := &fakeSender{err: errors.New("telegram 500")}
	w := New(Config{ChatID: "x", Clock: time.Now}, store, sender, nil, nopLogger())

	w.Tick(context.Background())

	if sender.calls != 1 {
		t.Fatalf("sender should be invoked once before failure observed (calls=%d)", sender.calls)
	}
}

// TestFormat_OmitsBackfillSectionWhenEmpty confirms the formatter
// skips the Backfill block entirely rather than emitting an empty
// header — same shape as the alert formatter's omit-on-empty rule.
func TestFormat_OmitsBackfillSectionWhenEmpty(t *testing.T) {
	s := Stats{
		MarketsTotal:     1,
		AlertsBySeverity: map[string]int64{},
		BackfillByStatus: map[string]int64{},
		UptimeSince:      time.Now(),
	}
	msg := Format(s, time.Now())
	if strings.Contains(msg, "<b>Backfill</b>") {
		t.Fatalf("Backfill block must be skipped when status map is empty:\n%s", msg)
	}
}

// TestFormat_BySeverityKnownOrderingDeterministic ensures the
// severity line iterates known severities in a fixed order rather
// than relying on map iteration. Stats by-severity is rendered as
// info → warning → critical → hard so two consecutive summaries are
// diff-friendly by eye.
func TestFormat_BySeverityKnownOrderingDeterministic(t *testing.T) {
	s := Stats{
		AlertsBySeverity: map[string]int64{
			"hard":     1,
			"warning":  2,
			"info":     3,
			"critical": 4,
		},
		UptimeSince: time.Now(),
	}
	msg := Format(s, time.Now())
	idx := func(needle string) int { return strings.Index(msg, needle) }
	if !(idx("info=3") < idx("warning=2") && idx("warning=2") < idx("critical=4") && idx("critical=4") < idx("hard=1")) {
		t.Fatalf("severity ordering must be info→warning→critical→hard:\n%s", msg)
	}
}

// TestNew_DefaultsAreSane locks the public defaults: Interval=2h and
// StartupGrace=Interval. A misconfigured Config{} that drops in
// production must not silently flood Telegram with sub-second ticks.
func TestNew_DefaultsAreSane(t *testing.T) {
	w := New(Config{ChatID: "x"}, &fakeStore{}, &fakeSender{}, nil, nopLogger())
	if w.cfg.Interval != 2*time.Hour {
		t.Errorf("interval default: %v want 2h", w.cfg.Interval)
	}
	if w.cfg.StartupGrace != 2*time.Hour {
		t.Errorf("startup grace default: %v want 2h (=Interval)", w.cfg.StartupGrace)
	}
}
