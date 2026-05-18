package signalreport

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/Borislavv/polymarket-watchtower/internal/infra/repository"
)

// fakeStore is a deterministic in-memory Store implementation. Tracks
// every claim/mark and exposes the rows for assertions.
type fakeStore struct {
	mu     sync.Mutex
	rows   map[string]*fakeRow
	totals repository.SignalQualityRow
	byKind []repository.SignalQualityBreakdownRow
	bySev  []repository.SignalQualityBreakdownRow
	nextID int64
}

type fakeRow struct {
	id          int64
	periodType  string
	periodStart time.Time
	periodEnd   time.Time
	scheduledAt time.Time
	status      string
	msgID       int64
	lastError   string
}

func newFakeStore() *fakeStore {
	return &fakeStore{rows: map[string]*fakeRow{}}
}

func (f *fakeStore) SignalQualityAggregate(_ context.Context, _ time.Time, _ time.Time) (repository.SignalQualityRow, error) {
	return f.totals, nil
}

func (f *fakeStore) SignalQualityByKind(_ context.Context, _ time.Time, _ time.Time) ([]repository.SignalQualityBreakdownRow, error) {
	return f.byKind, nil
}

func (f *fakeStore) SignalQualityBySeverity(_ context.Context, _ time.Time, _ time.Time) ([]repository.SignalQualityBreakdownRow, error) {
	return f.bySev, nil
}

func (f *fakeStore) TryCreateSignalReportPending(_ context.Context, periodType, dedupKey string, periodStart, periodEnd, scheduledAt time.Time) (int64, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, exists := f.rows[dedupKey]; exists {
		return 0, false, nil // dedup hit — idempotent
	}
	f.nextID++
	id := f.nextID
	f.rows[dedupKey] = &fakeRow{
		id: id, periodType: periodType, periodStart: periodStart, periodEnd: periodEnd,
		scheduledAt: scheduledAt, status: "pending",
	}
	return id, true, nil
}

func (f *fakeStore) MarkSignalReportSent(_ context.Context, id int64, telegramMessageID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.rows {
		if r.id == id {
			r.status = "sent"
			r.msgID = telegramMessageID
			return nil
		}
	}
	return errors.New("fakeStore: row not found")
}

func (f *fakeStore) MarkSignalReportFailed(_ context.Context, id int64, lastError string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.rows {
		if r.id == id {
			r.status = "failed"
			r.lastError = lastError
			return nil
		}
	}
	return errors.New("fakeStore: row not found")
}

// fakeSender captures Telegram sends.
type fakeSender struct {
	mu      sync.Mutex
	calls   []string
	chatIDs []string
	nextMsg int64
	err     error
}

func (f *fakeSender) SendHTML(_ context.Context, chatID, text string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return 0, f.err
	}
	f.nextMsg++
	f.chatIDs = append(f.chatIDs, chatID)
	f.calls = append(f.calls, text)
	return f.nextMsg, nil
}

// gmtPlus3 anchored in IANA-style FixedZone (matches what app.go
// constructs when SIGNAL_REPORTS_TIMEZONE=Etc/GMT-3).
var workerLocation = time.FixedZone("Etc/GMT-3", 3*60*60)

func workerForTest(t *testing.T, now time.Time, store Store, sender Sender) *Worker {
	t.Helper()
	log := zerolog.Nop()
	cfg := Config{
		Enabled:      true,
		Location:     workerLocation,
		ChatID:       "-1001",
		TickInterval: time.Minute,
		SendAt:       map[PeriodType]TimeOfDay{},
		YearlyDelay:  72 * time.Hour,
		Clock:        func() time.Time { return now },
	}
	for _, p := range AllPeriods {
		cfg.SendAt[p] = TimeOfDay{Hour: 8, Minute: 0}
	}
	return New(cfg, store, sender, nil, &log)
}

// TestTick_DailyDueAt08SendsExactlyOnce pins the daily schedule:
// at exactly 08:00 GMT+3 on 2026-05-18, the daily report for
// 2026-05-17 becomes due, the dedup_key inserts, and Telegram sees
// one send. A second tick at 08:01 must NOT re-emit.
func TestTick_DailyDueAt08SendsExactlyOnce(t *testing.T) {
	now := time.Date(2026, 5, 18, 5, 0, 0, 0, time.UTC) // 08:00 GMT+3
	store := newFakeStore()
	store.totals = repository.SignalQualityRow{
		TotalAlerts: 100, SuccessCount: 40, FailureCount: 20, PendingCount: 40,
	}
	sender := &fakeSender{}
	w := workerForTest(t, now, store, sender)

	w.Tick(context.Background())
	w.Tick(context.Background()) // second tick — must be no-op

	dailyRows := 0
	for _, r := range store.rows {
		if r.periodType == string(PeriodDaily) {
			dailyRows++
			if r.status != "sent" {
				t.Errorf("daily row not marked sent: %+v", r)
			}
		}
	}
	if dailyRows != 1 {
		t.Fatalf("expected exactly 1 daily row, got %d", dailyRows)
	}
	if len(sender.calls) < 1 {
		t.Fatal("expected at least one Telegram send")
	}
	dailyCalls := 0
	for _, body := range sender.calls {
		if strings.Contains(body, "Signal quality · Daily ·") {
			dailyCalls++
		}
	}
	if dailyCalls != 1 {
		t.Errorf("expected exactly 1 daily Telegram body, got %d", dailyCalls)
	}
}

// TestTick_BeforeDueTimeDoesNotSend pins the negative path: at
// 07:59 GMT+3 the daily report is not yet due.
func TestTick_BeforeDueTimeDoesNotSend(t *testing.T) {
	now := time.Date(2026, 5, 18, 4, 59, 0, 0, time.UTC) // 07:59 GMT+3
	store := newFakeStore()
	sender := &fakeSender{}
	w := workerForTest(t, now, store, sender)

	w.Tick(context.Background())

	for _, r := range store.rows {
		if r.periodType == string(PeriodDaily) {
			t.Fatalf("daily row inserted before due time: %+v", r)
		}
	}
}

// TestTick_YearlyRespects72hDelay confirms the yearly report for
// 2026 is NOT due on 2027-01-01 08:00 (year end + 0h), but IS due on
// 2027-01-04 08:00 (year end + 72h).
func TestTick_YearlyRespects72hDelay(t *testing.T) {
	// Before delay elapses: 2027-01-01 08:00 GMT+3
	earlyNow := time.Date(2027, 1, 1, 5, 0, 0, 0, time.UTC)
	store := newFakeStore()
	sender := &fakeSender{}
	w := workerForTest(t, earlyNow, store, sender)
	w.Tick(context.Background())
	for _, r := range store.rows {
		if r.periodType == string(PeriodYearly) {
			t.Fatalf("yearly row inserted before 72h delay elapsed: %+v", r)
		}
	}

	// After delay: 2027-01-04 08:00 GMT+3
	lateNow := time.Date(2027, 1, 4, 5, 0, 0, 0, time.UTC)
	w2 := workerForTest(t, lateNow, store, sender)
	w2.Tick(context.Background())
	gotYearly := false
	for _, r := range store.rows {
		if r.periodType == string(PeriodYearly) && r.status == "sent" {
			gotYearly = true
		}
	}
	if !gotYearly {
		t.Fatal("yearly row not sent after 72h delay")
	}
}

// TestTick_RestartDoesNotDuplicate pins idempotency: a "restart"
// (fresh Worker against the same store at a slightly later time)
// must not re-send a report that the previous worker already
// claimed.
func TestTick_RestartDoesNotDuplicate(t *testing.T) {
	store := newFakeStore()
	sender := &fakeSender{}

	// First worker: ticks at 08:00.
	now1 := time.Date(2026, 5, 18, 5, 0, 0, 0, time.UTC)
	workerForTest(t, now1, store, sender).Tick(context.Background())

	// "Restart" — fresh worker an hour later. Re-evaluates the same
	// completed window; dedup_key collision blocks re-emission.
	now2 := now1.Add(time.Hour)
	workerForTest(t, now2, store, sender).Tick(context.Background())

	// Exactly one daily Telegram body across both ticks.
	count := 0
	for _, body := range sender.calls {
		if strings.Contains(body, "Signal quality · Daily ·") {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 daily send across restart, got %d", count)
	}
}

// TestTick_SmallSampleAddsCaveat pins the report body shape: when the
// resolved count is below the threshold (default 30) the renderer
// surfaces the "treat as directional" caveat.
func TestTick_SmallSampleAddsCaveat(t *testing.T) {
	now := time.Date(2026, 5, 18, 5, 0, 0, 0, time.UTC)
	store := newFakeStore()
	store.totals = repository.SignalQualityRow{
		TotalAlerts: 10, SuccessCount: 3, FailureCount: 4, PendingCount: 3,
	}
	sender := &fakeSender{}
	w := workerForTest(t, now, store, sender)
	w.Tick(context.Background())

	if len(sender.calls) == 0 {
		t.Fatal("no Telegram send")
	}
	body := sender.calls[0]
	if !strings.Contains(body, "Sample size is small") {
		t.Errorf("small-sample caveat missing:\n%s", body)
	}
	if !strings.Contains(body, "success rate: <b>42.9%</b>") {
		t.Errorf("success-rate line missing/wrong:\n%s", body)
	}
}

// TestTick_BreakdownByKindRendersTable pins the by-kind section.
func TestTick_BreakdownByKindRendersTable(t *testing.T) {
	now := time.Date(2026, 5, 18, 5, 0, 0, 0, time.UTC)
	store := newFakeStore()
	store.totals = repository.SignalQualityRow{
		TotalAlerts: 100, SuccessCount: 40, FailureCount: 20,
	}
	store.byKind = []repository.SignalQualityBreakdownRow{
		{Label: "trade_anomaly", Total: 60, Success: 25, Failure: 15, Unresolved: 20},
		{Label: "accumulation", Total: 40, Success: 15, Failure: 5, Unresolved: 20},
	}
	store.bySev = []repository.SignalQualityBreakdownRow{
		{Label: "info", Total: 80, Success: 30, Failure: 18, Unresolved: 32},
		{Label: "warning", Total: 20, Success: 10, Failure: 2, Unresolved: 8},
	}
	sender := &fakeSender{}
	w := workerForTest(t, now, store, sender)
	w.Tick(context.Background())

	if len(sender.calls) == 0 {
		t.Fatal("no Telegram send")
	}
	body := sender.calls[0]
	for _, want := range []string{
		"<b>By alert kind</b>",
		"<code>trade_anomaly</code>: 25/40 (62.5%)",
		"<code>accumulation</code>: 15/20 (75.0%)",
		"<b>By severity</b>",
		"<code>info</code>: 30/48 (62.5%)",
		"<code>warning</code>: 10/12 (83.3%)",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in body:\n%s", want, body)
		}
	}
}

// TestTick_DisabledIsNoop confirms Enabled=false is a strict no-op.
func TestTick_DisabledIsNoop(t *testing.T) {
	now := time.Date(2026, 5, 18, 5, 0, 0, 0, time.UTC)
	store := newFakeStore()
	sender := &fakeSender{}
	log := zerolog.Nop()
	w := New(Config{Enabled: false, Clock: func() time.Time { return now }}, store, sender, nil, &log)
	// Run() should return immediately when disabled.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := w.Run(ctx); err != nil {
		t.Errorf("Run on disabled config: %v", err)
	}
	if len(store.rows) != 0 || len(sender.calls) != 0 {
		t.Errorf("disabled worker must do nothing: rows=%d calls=%d", len(store.rows), len(sender.calls))
	}
}
