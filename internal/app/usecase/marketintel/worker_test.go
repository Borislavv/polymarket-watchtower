package marketintel

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/analysis"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/repository"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/telegram"
)

// --- fakes ---------------------------------------------------------------

type fakeCandidates struct {
	rows []repository.IntelligenceCandidate
}

func (f *fakeCandidates) ListIntelligenceCandidates(_ context.Context, _ int32) ([]repository.IntelligenceCandidate, error) {
	return f.rows, nil
}

type fakeStore struct {
	mu       sync.Mutex
	inserted []repository.NewMarketIntelligenceReport
	// periodKeys is the in-memory UNIQUE index that mirrors the
	// production polymarket_market_intelligence_reports.period_key
	// constraint: same key on a second insert → false ("dedup hit")
	// with no error, exactly the way ON CONFLICT DO NOTHING behaves.
	periodKeys map[string]struct{}
}

func (s *fakeStore) Insert(_ context.Context, r repository.NewMarketIntelligenceReport) (repository.MarketIntelligenceReport, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.periodKeys == nil {
		s.periodKeys = make(map[string]struct{})
	}
	if _, dup := s.periodKeys[r.PeriodKey]; dup {
		return repository.MarketIntelligenceReport{}, false, nil
	}
	s.periodKeys[r.PeriodKey] = struct{}{}
	s.inserted = append(s.inserted, r)
	return repository.MarketIntelligenceReport{SummaryHash: r.SummaryHash}, true, nil
}

type fakeAnalyzer struct {
	out analysis.MarketReportAnalysis
	err error
}

func (f *fakeAnalyzer) AnalyzeMarketReport(_ context.Context, _ analysis.MarketReportRequest) (analysis.MarketReportAnalysis, error) {
	return f.out, f.err
}

type fakeBot struct {
	mu    sync.Mutex
	sends []sendCall
	err   error
}

type sendCall struct {
	ChatID string
	Text   string
}

func (b *fakeBot) SendHTML(_ context.Context, chatID, text string) (telegram.SendResult, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sends = append(b.sends, sendCall{chatID, text})
	return telegram.SendResult{}, b.err
}

func nopLogger() *zerolog.Logger {
	l := zerolog.Nop()
	return &l
}

func sampleCandidates() []repository.IntelligenceCandidate {
	return []repository.IntelligenceCandidate{
		{ConditionID: "0xa", Question: "Will X win?", Category: "Politics",
			LifecyclePct: 92, LastPrice: 0.62, Trades24h: 100, Volume24hUSD: 50_000, Alerts24h: 3},
		{ConditionID: "0xb", Question: "Will Y win?", Category: "Politics",
			LifecyclePct: 96, LastPrice: 0.70, Trades24h: 80, Volume24hUSD: 30_000, Alerts24h: 1},
	}
}

// --- tests ---------------------------------------------------------------

// TestTick_FullFlow pins the canonical run: candidates → AI →
// dedup-miss → persist → Telegram send.
func TestTick_FullFlow(t *testing.T) {
	cand := &fakeCandidates{rows: sampleCandidates()}
	st := &fakeStore{}
	an := &fakeAnalyzer{out: analysis.MarketReportAnalysis{
		Status: analysis.StatusOK, Model: "test",
		ReportText: "Stable favorites dominate. External context not checked.",
	}}
	bot := &fakeBot{}
	w := New(Config{Enabled: true, MaxMarkets: 50, ChatID: "42",
		Interval: time.Hour}, cand, st, an, bot, nopLogger())

	w.Tick(context.Background())

	if len(st.inserted) != 1 {
		t.Fatalf("expected 1 persisted report, got %d", len(st.inserted))
	}
	if len(bot.sends) != 1 {
		t.Fatalf("expected 1 Telegram send, got %d", len(bot.sends))
	}
	body := bot.sends[0].Text
	for _, want := range []string{
		"<b>Market intelligence · 2h</b>",
		"<b>Overview</b>",
		"markets evaluated: 2",
		"<b>Markets to watch</b>",
		"Will X win?",
		"Will Y win?",
		"<b>Analyst summary</b>",
		"Stable favorites dominate",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in body:\n%s", want, body)
		}
	}
}

// TestTick_DedupSkipsSendWithinSamePeriod pins the v8 dedup
// contract: two ticks landing in the same bucketed period collapse
// to a single Telegram send via ON CONFLICT (period_key) DO NOTHING.
// This is the incident-driven fix — the previous summary_hash
// primitive let "duplicate intelligence reports within one minute"
// slip through because the body embedded the absolute `now`
// timestamp.
func TestTick_DedupSkipsSendWithinSamePeriod(t *testing.T) {
	cand := &fakeCandidates{rows: sampleCandidates()}
	st := &fakeStore{}
	an := &fakeAnalyzer{out: analysis.MarketReportAnalysis{
		Status: analysis.StatusOK, ReportText: "summary",
	}}
	bot := &fakeBot{}
	// Freeze the clock inside the same 2h bucket for both ticks.
	frozen := time.Date(2026, 5, 19, 10, 0, 30, 0, time.UTC)
	w := New(Config{
		Enabled: true, MaxMarkets: 50, ChatID: "42",
		Interval: 2 * time.Hour,
		Clock:    func() time.Time { return frozen },
	}, cand, st, an, bot, nopLogger())

	w.Tick(context.Background())
	w.Tick(context.Background())

	if len(bot.sends) != 1 {
		t.Errorf("two ticks within one period must produce one send; got %d", len(bot.sends))
	}
	if len(st.inserted) != 1 {
		t.Errorf("expected 1 persisted row, got %d", len(st.inserted))
	}
}

// TestTick_DifferentPeriodsBothSend pins the inverse: two ticks in
// different windows MUST both produce a Telegram send.
func TestTick_DifferentPeriodsBothSend(t *testing.T) {
	cand := &fakeCandidates{rows: sampleCandidates()}
	st := &fakeStore{}
	an := &fakeAnalyzer{out: analysis.MarketReportAnalysis{Status: analysis.StatusOK, ReportText: "x"}}
	bot := &fakeBot{}

	clk := time.Date(2026, 5, 19, 10, 0, 0, 0, time.UTC)
	w := New(Config{
		Enabled: true, MaxMarkets: 50, ChatID: "42",
		Interval: 2 * time.Hour,
		Clock:    func() time.Time { return clk },
	}, cand, st, an, bot, nopLogger())

	w.Tick(context.Background())
	// Advance into the NEXT 2h bucket.
	clk = clk.Add(3 * time.Hour)
	w.Tick(context.Background())

	if len(bot.sends) != 2 {
		t.Errorf("two distinct periods must both send: %d", len(bot.sends))
	}
}

// TestBucketedPeriod pins the deterministic bucket math used as the
// period_key. A tick at 10:00:30 with a 2h interval must produce
// (10:00, 12:00) — not (08:00:30, 10:00:30).
func TestBucketedPeriod(t *testing.T) {
	cases := []struct {
		now      time.Time
		interval time.Duration
		wantEnd  time.Time
	}{
		{time.Date(2026, 5, 19, 10, 0, 30, 0, time.UTC), 2 * time.Hour, time.Date(2026, 5, 19, 10, 0, 0, 0, time.UTC)},
		{time.Date(2026, 5, 19, 11, 59, 59, 0, time.UTC), 2 * time.Hour, time.Date(2026, 5, 19, 10, 0, 0, 0, time.UTC)},
		{time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC), 2 * time.Hour, time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)},
	}
	for _, c := range cases {
		end, start := bucketedPeriod(c.now, c.interval)
		if !end.Equal(c.wantEnd) {
			t.Errorf("now=%v end=%v want=%v", c.now, end, c.wantEnd)
		}
		if !start.Equal(c.wantEnd.Add(-c.interval)) {
			t.Errorf("now=%v start=%v want=%v", c.now, start, c.wantEnd.Add(-c.interval))
		}
	}
}

// TestTick_AnalyzerErrorStillProducesReport pins that an analyzer
// failure does NOT block the report. The body explains the AI
// summary is unavailable and the candidate list still ships.
func TestTick_AnalyzerErrorStillProducesReport(t *testing.T) {
	cand := &fakeCandidates{rows: sampleCandidates()}
	st := &fakeStore{}
	an := &fakeAnalyzer{err: errors.New("upstream down")}
	bot := &fakeBot{}
	w := New(Config{Enabled: true, MaxMarkets: 50, ChatID: "42"},
		cand, st, an, bot, nopLogger())

	w.Tick(context.Background())

	if len(bot.sends) != 1 {
		t.Fatalf("must still send report on analyzer error: %d", len(bot.sends))
	}
	if !strings.Contains(bot.sends[0].Text, "AI summary unavailable") {
		t.Errorf("body must explain AI failure:\n%s", bot.sends[0].Text)
	}
}

// TestTick_EmptyCandidatesSkipsTelegram pins the v8 contract:
// an empty periodic report is suppressed before Telegram delivery
// AND nothing is persisted (so the next tick in a new period that
// DOES have candidates can still run cleanly). Real Info/Warning/
// Critical alerts are not affected — those ship through the
// alertsender worker on a different path.
func TestTick_EmptyCandidatesSkipsTelegram(t *testing.T) {
	cand := &fakeCandidates{rows: nil}
	st := &fakeStore{}
	an := &fakeAnalyzer{}
	bot := &fakeBot{}
	w := New(Config{Enabled: true, MaxMarkets: 50, ChatID: "42"},
		cand, st, an, bot, nopLogger())

	w.Tick(context.Background())

	if len(bot.sends) != 0 {
		t.Errorf("empty periodic report must NOT Telegram-send: %d", len(bot.sends))
	}
	if len(st.inserted) != 0 {
		t.Errorf("empty periodic report must NOT persist a row: %d", len(st.inserted))
	}
}

// TestFilterAndDedupCandidates pins the candidate-quality rules:
// drop near-degenerate prices (≤0.02 / ≥0.98) and collapse
// per-(condition_id) duplicates the SQL fan-out may produce.
func TestFilterAndDedupCandidates(t *testing.T) {
	rows := []repository.IntelligenceCandidate{
		{ConditionID: "0xa", Question: "A", LastPrice: 0.65},     // keep
		{ConditionID: "0xb", Question: "B", LastPrice: 0.01},     // drop (near-zero)
		{ConditionID: "0xc", Question: "C", LastPrice: 0.995},    // drop (near-one)
		{ConditionID: "0xa", Question: "A-dup", LastPrice: 0.65}, // drop (dup)
		{ConditionID: "0xd", Question: "D", LastPrice: 0.55},     // keep
		{ConditionID: "0xe", Question: "E", LastPrice: 0},        // keep (no price observed yet — let downstream decide)
	}
	got := filterAndDedupCandidates(rows)
	wantIDs := []string{"0xa", "0xd", "0xe"}
	if len(got) != len(wantIDs) {
		t.Fatalf("got %d rows want %d: %+v", len(got), len(wantIDs), got)
	}
	for i, want := range wantIDs {
		if got[i].ConditionID != want {
			t.Errorf("row %d: id=%q want %q", i, got[i].ConditionID, want)
		}
	}
}

// TestTick_AllCandidatesFilteredSkipsSend pins that when every
// upstream candidate fails the quality filter, the report is
// treated as empty — no persistence, no Telegram send.
func TestTick_AllCandidatesFilteredSkipsSend(t *testing.T) {
	cand := &fakeCandidates{rows: []repository.IntelligenceCandidate{
		{ConditionID: "0xa", Question: "A", LastPrice: 0.005}, // dropped
		{ConditionID: "0xb", Question: "B", LastPrice: 0.999}, // dropped
	}}
	st := &fakeStore{}
	an := &fakeAnalyzer{}
	bot := &fakeBot{}
	w := New(Config{Enabled: true, MaxMarkets: 50, ChatID: "42"},
		cand, st, an, bot, nopLogger())

	w.Tick(context.Background())

	if len(bot.sends) != 0 {
		t.Errorf("filtered-empty candidates must not send: %d", len(bot.sends))
	}
	if len(st.inserted) != 0 {
		t.Errorf("filtered-empty candidates must not persist: %d", len(st.inserted))
	}
}

// TestRequestBuilderRemainingReturn pins the math: 1-p/p as percent.
func TestRequestBuilderRemainingReturn(t *testing.T) {
	rows := []repository.IntelligenceCandidate{
		{Question: "T", LastPrice: 0.50},
		{Question: "U", LastPrice: 0.80},
		{Question: "V", LastPrice: 0.0}, // degenerate
		{Question: "W", LastPrice: 1.0}, // degenerate
	}
	req := buildRequest(rows, time.Now(), time.Hour)
	got := []float64{
		req.Markets[0].RemainingReturnPct,
		req.Markets[1].RemainingReturnPct,
		req.Markets[2].RemainingReturnPct,
		req.Markets[3].RemainingReturnPct,
	}
	wants := []float64{100, 25, 0, 0}
	for i, w := range wants {
		if delta := got[i] - w; delta > 0.5 || delta < -0.5 {
			t.Errorf("row %d: got %.2f want %.2f", i, got[i], w)
		}
	}
}
