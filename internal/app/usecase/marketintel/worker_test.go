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

type fakeCandidates struct{ rows []repository.IntelligenceCandidate }

func (f *fakeCandidates) ListIntelligenceCandidates(_ context.Context, _ int32) ([]repository.IntelligenceCandidate, error) {
	return f.rows, nil
}

type fakeStore struct {
	mu        sync.Mutex
	inserted  []repository.NewMarketIntelligenceReport
	dedupHash string // pretend a row with this hash already exists
}

func (s *fakeStore) Insert(_ context.Context, r repository.NewMarketIntelligenceReport) (repository.MarketIntelligenceReport, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dedupHash != "" && r.SummaryHash == s.dedupHash {
		return repository.MarketIntelligenceReport{}, false, nil
	}
	s.inserted = append(s.inserted, r)
	return repository.MarketIntelligenceReport{SummaryHash: r.SummaryHash}, true, nil
}

type fakeAnalyzer struct{ out analysis.MarketReportAnalysis; err error }

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

// TestTick_DedupSkipsSend pins the SHA-256 dedup contract: a tick
// whose content matches a prior summary_hash does NOT trigger a
// Telegram send.
func TestTick_DedupSkipsSend(t *testing.T) {
	cand := &fakeCandidates{rows: sampleCandidates()}
	st := &fakeStore{}
	an := &fakeAnalyzer{out: analysis.MarketReportAnalysis{
		Status: analysis.StatusOK, ReportText: "summary",
	}}
	bot := &fakeBot{}
	w := New(Config{Enabled: true, MaxMarkets: 50, ChatID: "42"},
		cand, st, an, bot, nopLogger())

	// First tick → fresh insert + send.
	w.Tick(context.Background())
	if len(bot.sends) != 1 {
		t.Fatalf("first tick should send: %d", len(bot.sends))
	}
	// Configure the store to refuse the same hash next time.
	st.dedupHash = st.inserted[0].SummaryHash

	// Second tick → dedup hit, NO send.
	w.Tick(context.Background())
	if len(bot.sends) != 1 {
		t.Errorf("second tick must not send on dedup hit; got %d total", len(bot.sends))
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

// TestTick_EmptyCandidatesIsNoop pins the quiet-day path.
func TestTick_EmptyCandidatesIsNoop(t *testing.T) {
	cand := &fakeCandidates{rows: nil}
	st := &fakeStore{}
	an := &fakeAnalyzer{}
	bot := &fakeBot{}
	w := New(Config{Enabled: true, MaxMarkets: 50, ChatID: "42"},
		cand, st, an, bot, nopLogger())

	w.Tick(context.Background())

	if len(st.inserted) != 0 || len(bot.sends) != 0 {
		t.Errorf("empty candidates must produce no work: stored=%d sent=%d",
			len(st.inserted), len(bot.sends))
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
