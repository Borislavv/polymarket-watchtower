package alertsender

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/anomaly"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/trade"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/repository"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/telegram"
)

// --- fakes ---------------------------------------------------------------

type fakeStore struct {
	mu              sync.Mutex
	pending         []repository.Alert
	sent            map[int64]int64 // alert id -> telegram message id
	failed          map[int64]string
	failedNextRetry map[int64]time.Time
}

func newFakeStore(rows ...repository.Alert) *fakeStore {
	return &fakeStore{
		pending:         rows,
		sent:            map[int64]int64{},
		failed:          map[int64]string{},
		failedNextRetry: map[int64]time.Time{},
	}
}

func (f *fakeStore) ClaimPending(_ context.Context, limit int32) ([]repository.Alert, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := int(limit)
	if n > len(f.pending) {
		n = len(f.pending)
	}
	out := append([]repository.Alert{}, f.pending[:n]...)
	f.pending = f.pending[n:]
	return out, nil
}

func (f *fakeStore) MarkSent(_ context.Context, id int64, msgID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent[id] = msgID
	return nil
}

func (f *fakeStore) MarkFailed(_ context.Context, id int64, msg string, nextRetryAt time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failed[id] = msg
	f.failedNextRetry[id] = nextRetryAt
	return nil
}

func (f *fakeStore) ResetStaleSending(_ context.Context, _ time.Time) error { return nil }

type fakeBot struct {
	mu    sync.Mutex
	calls []sendCall
	err   error
	delay time.Duration
	count atomic.Int32
}

type sendCall struct {
	ChatID string
	Text   string
}

func (b *fakeBot) SendHTML(_ context.Context, chatID, text string) (telegram.SendResult, error) {
	b.count.Add(1)
	if b.delay > 0 {
		time.Sleep(b.delay)
	}
	if b.err != nil {
		return telegram.SendResult{}, b.err
	}
	b.mu.Lock()
	b.calls = append(b.calls, sendCall{ChatID: chatID, Text: text})
	b.mu.Unlock()
	return telegram.SendResult{MessageID: int64(b.count.Load() + 100)}, nil
}

func nopLogger() *zerolog.Logger {
	l := zerolog.Nop()
	return &l
}

func sampleAlert(t *testing.T, id int64, sev string) repository.Alert {
	t.Helper()
	f := anomaly.Finding{
		Kind:     anomaly.KindTradeAnomaly,
		Severity: anomaly.Severity(sev),
		Reason:   anomaly.ReasonSingle,
		At:       time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC),
		Trade: &anomaly.TradeRef{
			Question: "Will X happen?", Market: "0xa", Outcome: "Yes", Side: trade.SideBuy,
			SizeShares: 100, Price: 0.5, NotionalUSD: 50, At: time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC),
		},
		Category: &anomaly.CategoryRef{Slug: "politics", Label: "Politics"},
	}
	payload, err := json.Marshal(f)
	if err != nil {
		t.Fatalf("marshal sample: %v", err)
	}
	return repository.Alert{
		ID: id, DedupKey: "single:v1:k", StrategyVersion: "v1",
		Kind: repository.AlertKindTrade, Reason: anomaly.ReasonSingle,
		Severity: sev, Payload: payload, Status: repository.AlertPending,
	}
}

// fakeEnricher implements the AIEnricher seam.
type fakeEnricher struct {
	calls int
	text  string
	err   error
}

func (f *fakeEnricher) AnalyzeAndStore(_ context.Context, _ int64, _ anomaly.Finding) (repository.AlertAnalysis, error) {
	f.calls++
	return repository.AlertAnalysis{}, f.err
}
func (f *fakeEnricher) LatestText(_ context.Context, _ int64) string { return f.text }

// fakeAttributionStore captures every Upsert call so tests can pin
// the bucketing output and verify failures do not block sends.
type fakeAttributionStore struct {
	mu     sync.Mutex
	rows   []repository.StrategyDimensions
	err    error
	called int
}

func (f *fakeAttributionStore) Upsert(_ context.Context, d repository.StrategyDimensions) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.called++
	if f.err != nil {
		return f.err
	}
	f.rows = append(f.rows, d)
	return nil
}

// --- tests ---------------------------------------------------------------

// TestAIEnricherStampsAnalystNote pins the v7 contract: the sender
// calls the enricher before render and the resulting note appears
// in the Telegram body inside an "Analyst note" block.
func TestAIEnricherStampsAnalystNote(t *testing.T) {
	st := newFakeStore(sampleAlert(t, 1, "info"))
	bot := &fakeBot{}
	w := New(Config{ChatID: "42", ClaimLimit: 16, Workers: 1, Interval: time.Hour},
		st, bot, nil, nopLogger())
	w.SetAIEnricher(&fakeEnricher{text: "Watchlist candidate. External context not checked."})

	w.Drain(context.Background())

	if bot.count.Load() != 1 {
		t.Fatalf("expected 1 send, got %d", bot.count.Load())
	}
	body := bot.calls[0].Text
	if !strings.Contains(body, "<b>Analyst note</b>") {
		t.Errorf("missing Analyst note block in:\n%s", body)
	}
	if !strings.Contains(body, "Watchlist candidate.") {
		t.Errorf("note text not stamped:\n%s", body)
	}
}

// TestAIEnricherFailureDoesNotBlockSend pins that an enricher
// failure is logged and the alert still ships (with no Analyst-note
// block).
func TestAIEnricherFailureDoesNotBlockSend(t *testing.T) {
	st := newFakeStore(sampleAlert(t, 1, "info"))
	bot := &fakeBot{}
	w := New(Config{ChatID: "42", ClaimLimit: 16, Workers: 1, Interval: time.Hour},
		st, bot, nil, nopLogger())
	w.SetAIEnricher(&fakeEnricher{err: errors.New("upstream down")})

	w.Drain(context.Background())

	if bot.count.Load() != 1 {
		t.Fatalf("send must still fire on enricher failure: %d", bot.count.Load())
	}
	if strings.Contains(bot.calls[0].Text, "<b>Analyst note</b>") {
		t.Errorf("Analyst note block must be elided on enricher failure:\n%s", bot.calls[0].Text)
	}
}

// TestAIEnricherEmptyTextElidesBlock pins the contract for a
// successful enricher that returns empty text (skip/error status):
// the Analyst block is NOT rendered.
func TestAIEnricherEmptyTextElidesBlock(t *testing.T) {
	st := newFakeStore(sampleAlert(t, 1, "info"))
	bot := &fakeBot{}
	w := New(Config{ChatID: "42", ClaimLimit: 16, Workers: 1, Interval: time.Hour},
		st, bot, nil, nopLogger())
	w.SetAIEnricher(&fakeEnricher{text: ""})

	w.Drain(context.Background())

	if strings.Contains(bot.calls[0].Text, "<b>Analyst note</b>") {
		t.Errorf("empty note must not render the block")
	}
}

// TestAttributionWriteHappyPath pins that every claimed alert produces
// exactly one attribution row, populated with the Finding-derived
// buckets. This is the join column the "which-setups-actually-win"
// dashboards depend on.
func TestAttributionWriteHappyPath(t *testing.T) {
	st := newFakeStore(sampleAlert(t, 1, "info"))
	bot := &fakeBot{}
	attr := &fakeAttributionStore{}
	w := New(Config{ChatID: "42", ClaimLimit: 16, Workers: 1, Interval: time.Hour},
		st, bot, nil, nopLogger())
	w.SetAttributionStore(attr)

	w.Drain(context.Background())

	if attr.called != 1 {
		t.Fatalf("expected 1 attribution write, got %d", attr.called)
	}
	row := attr.rows[0]
	if row.AlertID != 1 {
		t.Errorf("alert_id mismatch: %d", row.AlertID)
	}
	if row.StrategyFamily != "whale_flow" {
		t.Errorf("family: %q", row.StrategyFamily)
	}
	if row.Category != "politics" {
		t.Errorf("category lower-cased: %q", row.Category)
	}
}

// TestAttributionFailureDoesNotBlockSend pins the safety contract:
// an attribution-store DB error must never prevent Telegram delivery.
// Attribution is research telemetry, not a hard gate.
func TestAttributionFailureDoesNotBlockSend(t *testing.T) {
	st := newFakeStore(sampleAlert(t, 1, "info"))
	bot := &fakeBot{}
	attr := &fakeAttributionStore{err: errors.New("pg down")}
	w := New(Config{ChatID: "42", ClaimLimit: 16, Workers: 1, Interval: time.Hour},
		st, bot, nil, nopLogger())
	w.SetAttributionStore(attr)

	w.Drain(context.Background())

	if bot.count.Load() != 1 {
		t.Fatalf("attribution failure must not block send: bot calls=%d", bot.count.Load())
	}
	if attr.called != 1 {
		t.Errorf("attribution write must be attempted exactly once: %d", attr.called)
	}
}

// TestAttributionCarriesAIVerdict pins the join between the AI
// verdict and the attribution row. A non-zero AlertAnalysis.Verdict
// returned by the enricher must land on the attribution row's
// ai_verdict bucket so dashboards can group "alerts the AI flagged
// as lean_yes" → resolved_correct rate.
func TestAttributionCarriesAIVerdict(t *testing.T) {
	st := newFakeStore(sampleAlert(t, 1, "info"))
	bot := &fakeBot{}
	attr := &fakeAttributionStore{}
	w := New(Config{ChatID: "42", ClaimLimit: 16, Workers: 1, Interval: time.Hour},
		st, bot, nil, nopLogger())
	w.SetAIEnricher(&fakeVerdictEnricher{verdict: "lean_yes", text: "Watchlist."})
	w.SetAttributionStore(attr)

	w.Drain(context.Background())

	if attr.called != 1 || len(attr.rows) != 1 {
		t.Fatalf("expected 1 attribution row, got called=%d rows=%d", attr.called, len(attr.rows))
	}
	if attr.rows[0].AIVerdict != "lean_yes" {
		t.Errorf("ai_verdict: %q", attr.rows[0].AIVerdict)
	}
}

// fakeVerdictEnricher is a separate fake because fakeEnricher returns
// an empty AlertAnalysis. This one mimics a real enricher that
// returns Verdict on its OK path.
type fakeVerdictEnricher struct {
	verdict string
	text    string
}

func (f *fakeVerdictEnricher) AnalyzeAndStore(_ context.Context, _ int64, _ anomaly.Finding) (repository.AlertAnalysis, error) {
	return repository.AlertAnalysis{Verdict: f.verdict}, nil
}
func (f *fakeVerdictEnricher) LatestText(_ context.Context, _ int64) string { return f.text }

func TestSendsPendingAlertAndMarksSent(t *testing.T) {
	st := newFakeStore(sampleAlert(t, 1, "info"))
	bot := &fakeBot{}
	w := New(Config{ChatID: "42", ClaimLimit: 16, Workers: 1, Interval: time.Hour},
		st, bot, nil, nopLogger())

	w.Drain(context.Background())

	if got := bot.count.Load(); got != 1 {
		t.Fatalf("send calls: %d want 1", got)
	}
	if _, ok := st.sent[1]; !ok {
		t.Errorf("alert 1 not marked sent: %+v", st.sent)
	}
	if len(st.failed) != 0 {
		t.Errorf("unexpected failed: %+v", st.failed)
	}
	if !strings.Contains(bot.calls[0].Text, "INFO") {
		t.Errorf("text missing severity header: %q", bot.calls[0].Text)
	}
}

func TestNoSendWhenChatIDEmpty(t *testing.T) {
	st := newFakeStore(sampleAlert(t, 1, "info"))
	bot := &fakeBot{}
	w := New(Config{ChatID: "", ClaimLimit: 16, Workers: 1, Interval: time.Hour},
		st, bot, nil, nopLogger())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := w.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if bot.count.Load() != 0 {
		t.Errorf("no chat id must idle, got %d sends", bot.count.Load())
	}
}

func TestSendFailureMarksFailedAndDoesNotMarkSent(t *testing.T) {
	st := newFakeStore(sampleAlert(t, 1, "warning"))
	bot := &fakeBot{err: errors.New("400 bad")}
	w := New(Config{ChatID: "42", ClaimLimit: 16, Workers: 1, Interval: time.Hour},
		st, bot, nil, nopLogger())

	w.Drain(context.Background())

	if _, ok := st.sent[1]; ok {
		t.Errorf("must not mark sent on send failure")
	}
	if msg, ok := st.failed[1]; !ok || !strings.Contains(msg, "400 bad") {
		t.Errorf("expected failure recorded with upstream message, got %v", st.failed)
	}
}

func TestCancelledContextLeavesRowPending(t *testing.T) {
	st := newFakeStore(sampleAlert(t, 1, "info"))
	bot := &fakeBot{err: context.Canceled}
	w := New(Config{ChatID: "42", ClaimLimit: 16, Workers: 1, Interval: time.Hour},
		st, bot, nil, nopLogger())

	w.Drain(context.Background())

	if _, ok := st.failed[1]; ok {
		t.Errorf("cancellation must not mark failed: %+v", st.failed)
	}
	if _, ok := st.sent[1]; ok {
		t.Errorf("cancellation must not mark sent")
	}
}

func TestSendsEachClaimedRowExactlyOnce(t *testing.T) {
	rows := []repository.Alert{}
	for i := 1; i <= 5; i++ {
		rows = append(rows, sampleAlert(t, int64(i), "info"))
	}
	st := newFakeStore(rows...)
	bot := &fakeBot{}
	w := New(Config{ChatID: "42", ClaimLimit: 16, Workers: 1, Interval: time.Hour},
		st, bot, nil, nopLogger())

	w.Drain(context.Background())

	if got := bot.count.Load(); got != 5 {
		t.Fatalf("sends: %d want 5", got)
	}
	if len(st.sent) != 5 {
		t.Fatalf("marked sent: %d want 5", len(st.sent))
	}
}

func TestMalformedPayloadFailsTheRowOnly(t *testing.T) {
	bad := repository.Alert{ID: 1, Severity: "info", Payload: []byte("{not-json")}
	st := newFakeStore(bad, sampleAlert(t, 2, "info"))
	bot := &fakeBot{}
	w := New(Config{ChatID: "42", ClaimLimit: 16, Workers: 1, Interval: time.Hour},
		st, bot, nil, nopLogger())

	w.Drain(context.Background())

	if _, ok := st.failed[1]; !ok {
		t.Errorf("malformed payload must mark the row failed")
	}
	if _, ok := st.sent[2]; !ok {
		t.Errorf("the next row must still send: %+v", st.sent)
	}
}
