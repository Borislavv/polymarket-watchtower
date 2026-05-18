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
	mu      sync.Mutex
	pending []repository.Alert
	sent    map[int64]int64 // alert id -> telegram message id
	failed  map[int64]string
}

func newFakeStore(rows ...repository.Alert) *fakeStore {
	return &fakeStore{pending: rows, sent: map[int64]int64{}, failed: map[int64]string{}}
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

func (f *fakeStore) MarkFailed(_ context.Context, id int64, msg string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failed[id] = msg
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

// --- tests ---------------------------------------------------------------

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
