package outcomeai

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/analysis"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/anomaly"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/repository"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/telegram"
)

// --- fakes ---------------------------------------------------------------

type fakeAlerts struct {
	queue []repository.Alert
	calls int
}

func (f *fakeAlerts) ListResolvedForPostmortem(_ context.Context, _ int32) ([]repository.Alert, error) {
	f.calls++
	out := f.queue
	f.queue = nil
	return out, nil
}
func (f *fakeAlerts) GetByID(_ context.Context, id int64) (repository.Alert, error) {
	for _, a := range f.queue {
		if a.ID == id {
			return a, nil
		}
	}
	return repository.Alert{}, errors.New("not found")
}

type fakeStore struct {
	mu       sync.Mutex
	inserted []repository.NewAlertOutcomeAnalysis
}

func (s *fakeStore) Insert(_ context.Context, a repository.NewAlertOutcomeAnalysis) (repository.AlertOutcomeAnalysis, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inserted = append(s.inserted, a)
	return repository.AlertOutcomeAnalysis{AlertID: a.AlertID, OutcomeStatus: a.OutcomeStatus}, true, nil
}

type fakeAnalyzer struct {
	out analysis.OutcomeAnalysis
	err error
}

func (f *fakeAnalyzer) AnalyzeOutcome(_ context.Context, _ analysis.OutcomeAnalysisRequest) (analysis.OutcomeAnalysis, error) {
	return f.out, f.err
}

type fakeBot struct {
	mu          sync.Mutex
	edits       []editCall
	sends       []sendCall
	reactions   []reactionCall
	editErr     error
	sendErr     error
	reactionErr error
}

type editCall struct {
	ChatID    string
	MessageID int64
	Text      string
}
type sendCall struct {
	ChatID string
	Text   string
}
type reactionCall struct {
	ChatID    string
	MessageID int64
	Emoji     string
}

func (b *fakeBot) EditMessageText(_ context.Context, chatID string, messageID int64, text string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.edits = append(b.edits, editCall{chatID, messageID, text})
	return b.editErr
}
func (b *fakeBot) SendHTML(_ context.Context, chatID, text string) (telegram.SendResult, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sends = append(b.sends, sendCall{chatID, text})
	return telegram.SendResult{MessageID: 999}, b.sendErr
}
func (b *fakeBot) SetMessageReaction(_ context.Context, chatID string, messageID int64, emoji string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.reactions = append(b.reactions, reactionCall{chatID, messageID, emoji})
	return b.reactionErr
}

func nopLogger() *zerolog.Logger {
	l := zerolog.Nop()
	return &l
}

func sampleResolvedAlert(t *testing.T, id int64, outcome repository.OutcomeStatus, msgID int64) repository.Alert {
	t.Helper()
	f := anomaly.Finding{
		Kind:     anomaly.KindTradeAnomaly,
		Severity: anomaly.SeverityInfo,
		Reason:   anomaly.ReasonSingle,
		Trade: &anomaly.TradeRef{
			Question:    "Will it rain?",
			Outcome:     "Yes",
			NotionalUSD: 5_000,
			Price:       0.65,
		},
		ProfitIfWinUSD: 2_692,
		LifecyclePct:   95,
		Reasons:        []string{"LargeRareBet"},
	}
	payload, err := json.Marshal(f)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	a := repository.Alert{
		ID:                  id,
		Kind:                repository.AlertKindTrade,
		Severity:            "info",
		Payload:             payload,
		Status:              repository.AlertSent,
		OutcomeStatus:       outcome,
		WinningOutcomeLabel: "Yes",
		ResolvedAt:          time.Now().Add(-1 * time.Hour),
	}
	if msgID > 0 {
		a.TelegramMessageID = &msgID
	}
	return a
}

// --- tests ---------------------------------------------------------------

// TestProcessOne_EditsMessageAndReacts pins the canonical happy
// path: resolved_correct alert → AI postmortem persisted → original
// message edited with Why WON block → success reaction applied.
func TestProcessOne_EditsMessageAndReacts(t *testing.T) {
	al := &fakeAlerts{queue: []repository.Alert{sampleResolvedAlert(t, 7, repository.OutcomeCorrect, 100)}}
	st := &fakeStore{}
	an := &fakeAnalyzer{out: analysis.OutcomeAnalysis{
		Status: analysis.StatusOK, Model: "test", ReasonText: "Favorite held; debate confirmed direction.",
		WonExpected: func() *bool { v := true; return &v }(),
	}}
	bot := &fakeBot{}
	w := New(Config{Enabled: true, ChatID: "42",
		SuccessReaction: "✅", FailureReaction: "❌"},
		al, st, an, bot, nopLogger())

	w.Tick(context.Background())

	if len(st.inserted) != 1 {
		t.Fatalf("postmortem must be persisted: %v", st.inserted)
	}
	if st.inserted[0].OutcomeStatus != "resolved_correct" {
		t.Errorf("outcome_status: %q", st.inserted[0].OutcomeStatus)
	}
	if len(bot.edits) != 1 {
		t.Fatalf("expected 1 edit, got %d (edits=%+v)", len(bot.edits), bot.edits)
	}
	body := bot.edits[0].Text
	if !strings.Contains(body, "<b>Why WON</b>") {
		t.Errorf("missing Why WON block:\n%s", body)
	}
	if !strings.Contains(body, "Expected by Watchtower: yes") {
		t.Errorf("missing expectation line:\n%s", body)
	}
	if len(bot.reactions) != 1 || bot.reactions[0].Emoji != "✅" {
		t.Errorf("reaction wrong: %+v", bot.reactions)
	}
}

// TestProcessOne_WrongOutcomeReactsFailure pins the resolved_wrong
// path: Why LOST block + failure reaction.
func TestProcessOne_WrongOutcomeReactsFailure(t *testing.T) {
	al := &fakeAlerts{queue: []repository.Alert{sampleResolvedAlert(t, 8, repository.OutcomeWrong, 200)}}
	st := &fakeStore{}
	an := &fakeAnalyzer{out: analysis.OutcomeAnalysis{
		Status: analysis.StatusOK, Model: "test",
		ReasonText: "Polling shifted late; favorite collapsed.",
	}}
	bot := &fakeBot{}
	w := New(Config{Enabled: true, ChatID: "42",
		SuccessReaction: "✅", FailureReaction: "❌"},
		al, st, an, bot, nopLogger())

	w.Tick(context.Background())

	if !strings.Contains(bot.edits[0].Text, "<b>Why LOST</b>") {
		t.Errorf("missing Why LOST block:\n%s", bot.edits[0].Text)
	}
	if bot.reactions[0].Emoji != "❌" {
		t.Errorf("reaction: got %q want ❌", bot.reactions[0].Emoji)
	}
}

// TestProcessOne_EditFailureFallsBackToFollowup pins the contract:
// when EditMessageText returns ErrEditUnsupported (e.g. message too
// old), the worker sends a fresh follow-up instead.
func TestProcessOne_EditFailureFallsBackToFollowup(t *testing.T) {
	al := &fakeAlerts{queue: []repository.Alert{sampleResolvedAlert(t, 9, repository.OutcomeCorrect, 300)}}
	st := &fakeStore{}
	an := &fakeAnalyzer{out: analysis.OutcomeAnalysis{Status: analysis.StatusOK, ReasonText: "stable"}}
	bot := &fakeBot{editErr: telegram.ErrEditUnsupported}
	w := New(Config{Enabled: true, ChatID: "42",
		SuccessReaction: "✅", FailureReaction: "❌"},
		al, st, an, bot, nopLogger())

	w.Tick(context.Background())

	if len(bot.edits) != 1 {
		t.Fatalf("edit attempt count: %d", len(bot.edits))
	}
	if len(bot.sends) != 1 {
		t.Fatalf("follow-up send count: %d", len(bot.sends))
	}
	if !strings.Contains(bot.sends[0].Text, "Alert resolution follow-up") {
		t.Errorf("follow-up body shape wrong:\n%s", bot.sends[0].Text)
	}
}

// TestProcessOne_NoMessageIDSendsFollowup pins the legacy-alert
// path: an alert without a stored message_id still produces a
// follow-up so the operator sees the resolution.
func TestProcessOne_NoMessageIDSendsFollowup(t *testing.T) {
	al := &fakeAlerts{queue: []repository.Alert{sampleResolvedAlert(t, 10, repository.OutcomeCorrect, 0)}}
	st := &fakeStore{}
	an := &fakeAnalyzer{out: analysis.OutcomeAnalysis{Status: analysis.StatusOK, ReasonText: "x"}}
	bot := &fakeBot{}
	w := New(Config{Enabled: true, ChatID: "42"}, al, st, an, bot, nopLogger())

	w.Tick(context.Background())

	if len(bot.edits) != 0 {
		t.Errorf("must not edit when no message id: %+v", bot.edits)
	}
	if len(bot.sends) != 1 {
		t.Errorf("expected follow-up: %+v", bot.sends)
	}
}

// TestProcessOne_AnalyzerErrorStillPersistsAndDelivers pins that
// when the analyzer errors out, we still persist a row + still
// edit/react. Operator sees "no AI postmortem available" in body.
func TestProcessOne_AnalyzerErrorStillPersistsAndDelivers(t *testing.T) {
	al := &fakeAlerts{queue: []repository.Alert{sampleResolvedAlert(t, 11, repository.OutcomeCorrect, 500)}}
	st := &fakeStore{}
	an := &fakeAnalyzer{err: errors.New("upstream down")}
	bot := &fakeBot{}
	w := New(Config{Enabled: true, ChatID: "42", SuccessReaction: "✅"},
		al, st, an, bot, nopLogger())

	w.Tick(context.Background())

	if len(st.inserted) != 1 {
		t.Fatalf("must persist a row even on analyzer error")
	}
	if !strings.Contains(bot.edits[0].Text, "No AI postmortem available") {
		t.Errorf("body must explain missing postmortem:\n%s", bot.edits[0].Text)
	}
	if len(bot.reactions) != 1 {
		t.Errorf("reaction still expected: %+v", bot.reactions)
	}
}

// TestDisabledWorkerIsNoop pins the master switch.
func TestDisabledWorkerIsNoop(t *testing.T) {
	al := &fakeAlerts{queue: []repository.Alert{sampleResolvedAlert(t, 12, repository.OutcomeCorrect, 600)}}
	st := &fakeStore{}
	an := &fakeAnalyzer{out: analysis.OutcomeAnalysis{Status: analysis.StatusOK, ReasonText: "x"}}
	bot := &fakeBot{}
	w := New(Config{Enabled: false, ChatID: "42"}, al, st, an, bot, nopLogger())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w.Run(ctx) // returns immediately

	if al.calls != 0 || len(st.inserted) != 0 || len(bot.edits) != 0 {
		t.Errorf("disabled worker did work: alerts=%d stored=%d edits=%d",
			al.calls, len(st.inserted), len(bot.edits))
	}
}
