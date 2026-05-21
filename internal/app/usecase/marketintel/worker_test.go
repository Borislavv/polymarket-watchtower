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
	openai "github.com/Borislavv/polymarket-watchtower/internal/infra/ai/openai"
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
	hashes   map[string]struct{}
}

func (s *fakeStore) Insert(_ context.Context, r repository.NewMarketIntelligenceReport) (repository.MarketIntelligenceReport, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hashes == nil {
		s.hashes = make(map[string]struct{})
	}
	if _, dup := s.hashes[r.SummaryHash]; dup {
		return repository.MarketIntelligenceReport{}, false, nil
	}
	s.hashes[r.SummaryHash] = struct{}{}
	s.inserted = append(s.inserted, r)
	return repository.MarketIntelligenceReport{SummaryHash: r.SummaryHash}, true, nil
}

// fakeAnalyzer is a stateful analyzer that lets a test script a
// sequence of (result, err) tuples. The Nth call returns the Nth
// scripted response, looping the last entry forever.
type fakeAnalyzer struct {
	mu      sync.Mutex
	scripts []scriptedAIResult
	calls   int
}

type scriptedAIResult struct {
	out analysis.MarketReportAnalysis
	err error
}

func (f *fakeAnalyzer) AnalyzeMarketReport(_ context.Context, _ analysis.MarketReportRequest) (analysis.MarketReportAnalysis, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.scripts) == 0 {
		return analysis.MarketReportAnalysis{Status: analysis.StatusOK, Model: "test", ReportText: "ok"}, nil
	}
	i := f.calls
	if i >= len(f.scripts) {
		i = len(f.scripts) - 1
	}
	f.calls++
	r := f.scripts[i]
	return r.out, r.err
}

func (f *fakeAnalyzer) CallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// singleResult helper.
func okAnalyzer(text string) *fakeAnalyzer {
	return &fakeAnalyzer{scripts: []scriptedAIResult{{out: analysis.MarketReportAnalysis{
		Status: analysis.StatusOK, Model: "test", ReportText: text,
	}}}}
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

type fakeAnnotationLister struct {
	byEvent map[string][]repository.EventAnnotation
}

func (f *fakeAnnotationLister) ListRecentAnnotations(_ context.Context, slug string, limit int32) ([]repository.EventAnnotation, error) {
	rows := f.byEvent[slug]
	if int32(len(rows)) > limit {
		rows = rows[:limit]
	}
	return rows, nil
}

func nopLogger() *zerolog.Logger {
	l := zerolog.Nop()
	return &l
}

func sampleCandidates() []repository.IntelligenceCandidate {
	return []repository.IntelligenceCandidate{
		{ConditionID: "0xa", Question: "Will X win?", EventSlug: "x-event", MarketSlug: "x-market",
			Category: "Politics", CategorySlug: "politics",
			LifecyclePct: 92, LastPrice: 0.62, Trades24h: 100, Volume24hUSD: 50_000, Alerts24h: 3},
		{ConditionID: "0xb", Question: "Will Y win?", EventSlug: "y-event", MarketSlug: "y-market",
			Category: "Politics", CategorySlug: "politics",
			LifecyclePct: 96, LastPrice: 0.70, Trades24h: 80, Volume24hUSD: 30_000, Alerts24h: 1},
	}
}

func baseConfig() Config {
	return Config{
		Enabled:            true,
		MaxMarkets:         50,
		ChatID:             "42",
		Interval:           time.Hour,
		AITimeout:          60 * time.Second,
		RetryOnTimeout:     true,
		RetryBackoffMin:    10 * time.Millisecond,
		RetryBackoffMax:    20 * time.Millisecond,
		FallbackOnFailure:  true,
		SuppressOnSentinel: true,
		AnnotationsPerEvt:  3,
		VisibleMarkets:     8,
		Links: LinkConfig{
			PolymarketBase:     "https://polymarket.com",
			SourceLinksEnabled: true,
			MaxSourceLinks:     3,
			MaxLinksPerRow:     5,
		},
	}
}

// --- tests ---------------------------------------------------------------

// TestTick_FullFlow pins the canonical run: candidates → AI → render
// → persist → Telegram send. The v9.7 header is "AI analysis", not
// the legacy "Analyst summary".
func TestTick_FullFlow(t *testing.T) {
	cand := &fakeCandidates{rows: sampleCandidates()}
	st := &fakeStore{}
	an := okAnalyzer("Stable favorites dominate. External context not checked.")
	bot := &fakeBot{}
	w := New(baseConfig(), cand, st, an, bot, nopLogger())

	w.Tick(context.Background())

	if len(st.inserted) != 1 {
		t.Fatalf("expected 1 persisted report, got %d", len(st.inserted))
	}
	if len(bot.sends) != 1 {
		t.Fatalf("expected 1 Telegram send, got %d", len(bot.sends))
	}
	body := bot.sends[0].Text
	for _, want := range []string{
		"<b>MARKET INTELLIGENCE</b> · 2h",
		"<b>Type:</b> market_intel",
		"<b>Trigger:</b>",
		"<b>Strategy:</b> Market intelligence",
		"<b>AI:</b>",
		"<b>Overview</b>",
		"markets evaluated: 2",
		"<b>Markets to watch</b>",
		"Will X win?",
		"Will Y win?",
		"<b>AI analysis</b>",
		"Stable favorites dominate",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in body:\n%s", want, body)
		}
	}
}

// PART 9 test 1 — marketintel uses configured 60s timeout.
// Implementation check: the worker wraps the analyzer call in a
// context.WithTimeout(parent, w.cfg.AITimeout). We assert that an
// analyzer reading its deadline observes the configured budget.
func TestTick_UsesConfiguredAITimeout(t *testing.T) {
	configured := 1234 * time.Millisecond
	var observed time.Duration
	cand := &fakeCandidates{rows: sampleCandidates()}
	an := &fakeAnalyzer{scripts: []scriptedAIResult{{out: analysis.MarketReportAnalysis{Status: analysis.StatusOK, Model: "t", ReportText: "ok"}}}}
	// Wrap the fake analyzer to capture the deadline.
	checked := analyzerFunc(func(ctx context.Context, req analysis.MarketReportRequest) (analysis.MarketReportAnalysis, error) {
		if dl, ok := ctx.Deadline(); ok {
			observed = time.Until(dl)
		}
		return an.AnalyzeMarketReport(ctx, req)
	})
	cfg := baseConfig()
	cfg.AITimeout = configured
	w := New(cfg, cand, &fakeStore{}, checked, &fakeBot{}, nopLogger())
	w.Tick(context.Background())
	// observed should be approximately the configured value (with a
	// few ms margin for the time between WithTimeout and Deadline()).
	if observed < configured-100*time.Millisecond || observed > configured+50*time.Millisecond {
		t.Errorf("AI ctx deadline = %v; want ≈ %v", observed, configured)
	}
}

// PART 9 test 2 — OpenAI timeout retries once.
func TestTick_RetriesOnceOnTimeout(t *testing.T) {
	timeoutErr := &openai.ProviderError{Category: openai.CategoryTimeout, Retryable: true, Message: "deadline"}
	an := &fakeAnalyzer{scripts: []scriptedAIResult{
		{out: analysis.MarketReportAnalysis{Status: analysis.StatusError, Model: "t", LastError: "timeout"}, err: timeoutErr},
		{out: analysis.MarketReportAnalysis{Status: analysis.StatusOK, Model: "t", ReportText: "recovered after retry"}},
	}}
	cand := &fakeCandidates{rows: sampleCandidates()}
	bot := &fakeBot{}
	w := New(baseConfig(), cand, &fakeStore{}, an, bot, nopLogger())
	w.Tick(context.Background())
	if an.CallCount() != 2 {
		t.Errorf("expected 2 analyzer calls (initial + retry); got %d", an.CallCount())
	}
	if len(bot.sends) != 1 {
		t.Fatalf("expected 1 Telegram send after successful retry; got %d", len(bot.sends))
	}
	if !strings.Contains(bot.sends[0].Text, "recovered after retry") {
		t.Errorf("expected retry success text in body")
	}
}

// PART 9 test 3 — quota / rate-limit MUST NOT retry incorrectly.
func TestTick_QuotaExceededDoesNotRetry(t *testing.T) {
	quotaErr := &openai.ProviderError{Category: openai.CategoryQuotaExceeded, Retryable: false, Message: "insufficient_quota"}
	an := &fakeAnalyzer{scripts: []scriptedAIResult{
		{out: analysis.MarketReportAnalysis{Status: analysis.StatusSkipped, Model: "t", LastError: "quota_exceeded"}, err: quotaErr},
		{out: analysis.MarketReportAnalysis{Status: analysis.StatusOK, ReportText: "should not be reached"}},
	}}
	cand := &fakeCandidates{rows: sampleCandidates()}
	bot := &fakeBot{}
	w := New(baseConfig(), cand, &fakeStore{}, an, bot, nopLogger())
	w.Tick(context.Background())
	if an.CallCount() != 1 {
		t.Errorf("quota error must NOT retry; got %d analyzer calls", an.CallCount())
	}
	// Fallback should still ship (deterministic content exists).
	if len(bot.sends) != 1 {
		t.Errorf("quota failure should still ship fallback report; got %d sends", len(bot.sends))
	}
}

// PART 9 test 4 — AI timeout sends deterministic fallback when
// candidates exist.
func TestTick_AITimeoutShipsDeterministicFallback(t *testing.T) {
	timeoutErr := &openai.ProviderError{Category: openai.CategoryTimeout, Retryable: true}
	an := &fakeAnalyzer{scripts: []scriptedAIResult{
		{out: analysis.MarketReportAnalysis{Status: analysis.StatusError, Model: "t", LastError: "timeout"}, err: timeoutErr},
		// retry also times out → exhaust
		{out: analysis.MarketReportAnalysis{Status: analysis.StatusError, Model: "t", LastError: "timeout"}, err: timeoutErr},
	}}
	cand := &fakeCandidates{rows: sampleCandidates()}
	bot := &fakeBot{}
	w := New(baseConfig(), cand, &fakeStore{}, an, bot, nopLogger())
	w.Tick(context.Background())
	if len(bot.sends) != 1 {
		t.Fatalf("AI timeout must ship deterministic fallback; got %d sends", len(bot.sends))
	}
	body := bot.sends[0].Text
	if !strings.Contains(body, "AI summary unavailable") {
		t.Errorf("fallback body must contain 'AI summary unavailable':\n%s", body)
	}
	if !strings.Contains(body, "Markets to watch") {
		t.Errorf("fallback body must still render Markets to watch:\n%s", body)
	}
	if !strings.Contains(body, "Will X win?") {
		t.Errorf("fallback body must include candidate markets:\n%s", body)
	}
}

// PART 9 test 5 — empty report still skips.
func TestTick_EmptyReportStillSkips(t *testing.T) {
	cand := &fakeCandidates{rows: nil}
	bot := &fakeBot{}
	w := New(baseConfig(), cand, &fakeStore{}, okAnalyzer("x"), bot, nopLogger())
	w.Tick(context.Background())
	if len(bot.sends) != 0 {
		t.Errorf("empty candidates must not Telegram-send; got %d", len(bot.sends))
	}
}

// PART 9 test 6 — market rows render event link.
func TestTick_RowsRenderEventLink(t *testing.T) {
	cand := &fakeCandidates{rows: sampleCandidates()}
	bot := &fakeBot{}
	w := New(baseConfig(), cand, &fakeStore{}, okAnalyzer("ok"), bot, nopLogger())
	w.Tick(context.Background())
	if len(bot.sends) != 1 {
		t.Fatalf("expected 1 send")
	}
	body := bot.sends[0].Text
	if !strings.Contains(body, `<a href="https://polymarket.com/event/x-event">Polymarket event</a>`) {
		t.Errorf("expected Polymarket event link for x-event in body:\n%s", body)
	}
}

// PART 9 test 7 — market rows render market link.
func TestTick_RowsRenderMarketLink(t *testing.T) {
	cand := &fakeCandidates{rows: sampleCandidates()}
	bot := &fakeBot{}
	w := New(baseConfig(), cand, &fakeStore{}, okAnalyzer("ok"), bot, nopLogger())
	w.Tick(context.Background())
	body := bot.sends[0].Text
	if !strings.Contains(body, `<a href="https://polymarket.com/markets/x-market">Market</a>`) {
		t.Errorf("expected Market link for x-market in body:\n%s", body)
	}
}

// PART 9 test 8 — annotation rows render source links.
func TestTick_AnnotationsRenderSourceLinks(t *testing.T) {
	cand := &fakeCandidates{rows: sampleCandidates()}
	lister := &fakeAnnotationLister{byEvent: map[string][]repository.EventAnnotation{
		"x-event": {{
			Title:       "Big rumor",
			Outcome:     "Yes",
			Timestamp:   time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC),
			SourcesJSON: []byte(`[{"name":"AP","url":"https://apnews.com/story"},{"name":"Reuters","url":"https://reuters.com/story"}]`),
		}},
	}}
	bot := &fakeBot{}
	w := New(baseConfig(), cand, &fakeStore{}, okAnalyzer("ok"), bot, nopLogger())
	w.SetAnnotationLister(lister)
	w.Tick(context.Background())
	if len(bot.sends) != 1 {
		t.Fatalf("expected 1 send")
	}
	body := bot.sends[0].Text
	if !strings.Contains(body, `<a href="https://apnews.com/story">AP</a>`) {
		t.Errorf("expected AP source link in body:\n%s", body)
	}
	if !strings.Contains(body, `<a href="https://reuters.com/story">Reuters</a>`) {
		t.Errorf("expected Reuters source link in body:\n%s", body)
	}
}

// PART 9 test 9 — unsafe source links skipped.
func TestTick_UnsafeSourceLinksSkipped(t *testing.T) {
	cand := &fakeCandidates{rows: sampleCandidates()}
	lister := &fakeAnnotationLister{byEvent: map[string][]repository.EventAnnotation{
		"x-event": {{
			Title:       "Bad URLs",
			Timestamp:   time.Now().UTC(),
			SourcesJSON: []byte(`[{"name":"localhost","url":"http://localhost:8080/x"},{"name":"javascript","url":"javascript:alert(1)"},{"name":"loopback","url":"http://127.0.0.1/x"},{"name":"AP","url":"https://apnews.com/x"}]`),
		}},
	}}
	bot := &fakeBot{}
	w := New(baseConfig(), cand, &fakeStore{}, okAnalyzer("ok"), bot, nopLogger())
	w.SetAnnotationLister(lister)
	w.Tick(context.Background())
	body := bot.sends[0].Text
	if strings.Contains(body, "localhost") || strings.Contains(body, "127.0.0.1") || strings.Contains(body, "javascript:") {
		t.Errorf("unsafe URLs must be elided:\n%s", body)
	}
	if !strings.Contains(body, `<a href="https://apnews.com/x">AP</a>`) {
		t.Errorf("safe URL must still render:\n%s", body)
	}
}

// PART 9 test 10 — no orphan "links:" line.
func TestRenderNoOrphanLinksLine(t *testing.T) {
	in := RenderInput{
		Request: analysis.MarketReportRequest{
			PeriodStart: time.Now().Add(-time.Hour), PeriodEnd: time.Now(),
			Markets: []analysis.MarketReportMarket{{Title: "Alpha", LifecyclePct: 80, Probability: 0.5}},
		},
		AIResult:   analysis.MarketReportAnalysis{Status: analysis.StatusOK, ReportText: "ok"},
		Candidates: []repository.IntelligenceCandidate{{Question: "Alpha"}},
		// No PolymarketBase, no GrafanaBase → every URL elides.
		Links:    LinkConfig{},
		VisibleN: 8,
	}
	body, _ := Render(in)
	if strings.Contains(body, "links:") {
		t.Errorf("no link config → no orphan 'links:' line should render:\n%s", body)
	}
}

// PART 9 test 11 — SafeSplitForTelegram used (worker delivers >1
// chunk when the body exceeds the 4000-char cap).
func TestTick_LongBodyUsesSafeSplit(t *testing.T) {
	// Build a candidate set whose rendered body alone is huge:
	// 200 candidates with long titles.
	rows := make([]repository.IntelligenceCandidate, 200)
	for i := range rows {
		rows[i] = repository.IntelligenceCandidate{
			ConditionID:  string(rune('a'+i%26)) + string(rune('0'+i/26)),
			Question:     strings.Repeat("Q", 80),
			EventSlug:    "e",
			MarketSlug:   "m",
			Category:     "Politics",
			CategorySlug: "politics",
			LifecyclePct: 90, LastPrice: 0.55, Volume24hUSD: 1000, Alerts24h: 1,
		}
	}
	// Note: dedup will collapse repeated ConditionIDs. Make unique ids.
	for i := range rows {
		rows[i].ConditionID = "cond-" + strings.Repeat("x", 1) + string(rune('A'+i%26)) + string(rune('A'+i/26))
	}
	cand := &fakeCandidates{rows: rows}
	bot := &fakeBot{}
	cfg := baseConfig()
	cfg.VisibleMarkets = 200 // force all 200 rows to render
	w := New(cfg, cand, &fakeStore{}, okAnalyzer(strings.Repeat("Long AI text. ", 200)), bot, nopLogger())
	w.Tick(context.Background())
	if len(bot.sends) < 2 {
		t.Errorf("expected SafeSplit to chunk the body into ≥2 sends; got %d", len(bot.sends))
	}
}

// PART 9 test 12 — duplicate market rows removed.
func TestFilterAndDedupCandidates(t *testing.T) {
	rows := []repository.IntelligenceCandidate{
		{ConditionID: "0xa", Question: "A", LastPrice: 0.65},
		{ConditionID: "0xb", Question: "B", LastPrice: 0.01},     // dropped near-zero
		{ConditionID: "0xc", Question: "C", LastPrice: 0.995},    // dropped near-one
		{ConditionID: "0xa", Question: "A-dup", LastPrice: 0.65}, // dropped dup
		{ConditionID: "0xd", Question: "D", LastPrice: 0.55},
	}
	got := filterAndDedupCandidates(rows)
	wantIDs := []string{"0xa", "0xd"}
	if len(got) != len(wantIDs) {
		t.Fatalf("got %d rows want %d", len(got), len(wantIDs))
	}
	for i, want := range wantIDs {
		if got[i].ConditionID != want {
			t.Errorf("row %d: id=%q want %q", i, got[i].ConditionID, want)
		}
	}
}

// --- existing baseline tests (kept for regression coverage) -------------

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

func TestRequestBuilderRemainingReturn(t *testing.T) {
	rows := []repository.IntelligenceCandidate{
		{Question: "T", LastPrice: 0.50},
		{Question: "U", LastPrice: 0.80},
		{Question: "V", LastPrice: 0.0},
		{Question: "W", LastPrice: 1.0},
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

func TestTick_PersistsAnalysisTextNotRenderedBody(t *testing.T) {
	cand := &fakeCandidates{rows: sampleCandidates()}
	st := &fakeStore{}
	aiText := "Stable favorites dominate. Watchlist KY-04. Next: primary results."
	bot := &fakeBot{}
	w := New(baseConfig(), cand, st, okAnalyzer(aiText), bot, nopLogger())
	w.Tick(context.Background())
	if len(st.inserted) != 1 {
		t.Fatalf("expected 1 persisted row, got %d", len(st.inserted))
	}
	stored := st.inserted[0].ReportText
	if stored != aiText {
		t.Errorf("report_text must equal AI analysis text verbatim.\n got: %q\nwant: %q", stored, aiText)
	}
	for _, banned := range []string{"<b>Market intelligence", "<b>Overview</b>", "<b>Markets to watch</b>", "<b>AI analysis</b>"} {
		if strings.Contains(stored, banned) {
			t.Errorf("report_text MUST NOT contain rendered Telegram boilerplate %q:\n%s", banned, stored)
		}
	}
	if len(bot.sends) != 1 {
		t.Fatalf("expected 1 Telegram send, got %d", len(bot.sends))
	}
}

// TestTick_AnalyzerErrorShipsFallback_v97 is the inverse of the legacy
// v8 "skip on AI failure" test — v9.7 explicitly SHIPS a deterministic
// fallback when candidates exist. The legacy behaviour is reachable
// via MARKET_INTEL_FALLBACK_ON_AI_FAILURE=false but is not the default.
func TestTick_AnalyzerErrorShipsFallback_v97(t *testing.T) {
	cand := &fakeCandidates{rows: sampleCandidates()}
	st := &fakeStore{}
	an := &fakeAnalyzer{scripts: []scriptedAIResult{{err: errors.New("upstream down")}}}
	bot := &fakeBot{}
	cfg := baseConfig()
	cfg.RetryOnTimeout = false // not a timeout, so this knob is irrelevant
	w := New(cfg, cand, st, an, bot, nopLogger())
	w.Tick(context.Background())
	if len(bot.sends) != 1 {
		t.Errorf("v9.7: AI failure must SHIP a deterministic fallback (not skip); got %d sends", len(bot.sends))
	}
	if !strings.Contains(bot.sends[0].Text, "AI summary unavailable") {
		t.Errorf("fallback body must carry 'AI summary unavailable':\n%s", bot.sends[0].Text)
	}
}

// TestTick_FallbackDisabledRevertsToSkip pins the kill-switch.
func TestTick_FallbackDisabledRevertsToSkip(t *testing.T) {
	cand := &fakeCandidates{rows: sampleCandidates()}
	st := &fakeStore{}
	an := &fakeAnalyzer{scripts: []scriptedAIResult{{err: errors.New("down")}}}
	bot := &fakeBot{}
	cfg := baseConfig()
	cfg.FallbackOnFailure = false
	w := New(cfg, cand, st, an, bot, nopLogger())
	w.Tick(context.Background())
	if len(bot.sends) != 0 {
		t.Errorf("FallbackOnFailure=false must skip on AI failure; got %d sends", len(bot.sends))
	}
}

// PART 17 test 13 — AI_NO_NOTICEABLE_EDGE suppresses Telegram.
func TestTick_SentinelNoEdgeSuppressesTelegram(t *testing.T) {
	cand := &fakeCandidates{rows: sampleCandidates()}
	st := &fakeStore{}
	an := okAnalyzer("AI_NO_NOTICEABLE_EDGE")
	bot := &fakeBot{}
	w := New(baseConfig(), cand, st, an, bot, nopLogger())
	w.Tick(context.Background())
	if len(bot.sends) != 0 {
		t.Errorf("AI_NO_NOTICEABLE_EDGE must suppress Telegram; got %d sends", len(bot.sends))
	}
	if len(st.inserted) != 1 {
		t.Errorf("sentinel result must STILL persist for audit; got %d rows", len(st.inserted))
	}
	if st.inserted[0].DeliveryStatus != "skipped_sentinel" {
		t.Errorf("persisted row delivery_status: got %q want skipped_sentinel", st.inserted[0].DeliveryStatus)
	}
	if st.inserted[0].ReportText != "" {
		t.Errorf("sentinel must NOT persist as analysis text; got %q", st.inserted[0].ReportText)
	}
}

// PART 17 test 14 — AI_ALREADY_PRICED suppresses Telegram.
func TestTick_SentinelAlreadyPricedSuppressesTelegram(t *testing.T) {
	cand := &fakeCandidates{rows: sampleCandidates()}
	st := &fakeStore{}
	an := okAnalyzer("AI_ALREADY_PRICED")
	bot := &fakeBot{}
	w := New(baseConfig(), cand, st, an, bot, nopLogger())
	w.Tick(context.Background())
	if len(bot.sends) != 0 {
		t.Errorf("AI_ALREADY_PRICED must suppress Telegram; got %d sends", len(bot.sends))
	}
}

// PART 17 test 16 — AI_CONTEXT_STALE does not produce a "no fresh news"
// claim in the Telegram body.
func TestTick_SentinelContextStaleDoesNotShipNoNewsClaim(t *testing.T) {
	cand := &fakeCandidates{rows: sampleCandidates()}
	st := &fakeStore{}
	an := okAnalyzer("AI_CONTEXT_STALE")
	bot := &fakeBot{}
	w := New(baseConfig(), cand, st, an, bot, nopLogger())
	w.Tick(context.Background())
	if len(bot.sends) != 0 {
		t.Errorf("stale context must NOT ship a 'no fresh news' Telegram; got %d sends", len(bot.sends))
	}
}

// PART 17 test 17 — no-edge result persisted but not sent.
func TestTick_SentinelPersistedButNotSent(t *testing.T) {
	cand := &fakeCandidates{rows: sampleCandidates()}
	st := &fakeStore{}
	an := okAnalyzer("AI_NO_NOTICEABLE_EDGE")
	bot := &fakeBot{}
	w := New(baseConfig(), cand, st, an, bot, nopLogger())
	w.Tick(context.Background())
	if len(st.inserted) != 1 {
		t.Fatalf("expected persisted sentinel row; got %d", len(st.inserted))
	}
	if len(bot.sends) != 0 {
		t.Errorf("expected zero sends; got %d", len(bot.sends))
	}
}

// PART 9: news-fingerprint gating — unchanged annotation set skips AI.
type fakeFingerprintStore struct {
	mu      sync.Mutex
	current repository.NewsFingerprint
	upserts int
	gets    int
}

func (f *fakeFingerprintStore) Get(_ context.Context, _ string) (repository.NewsFingerprint, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets++
	if f.current.Fingerprint == "" {
		return repository.NewsFingerprint{}, false, nil
	}
	return f.current, true, nil
}
func (f *fakeFingerprintStore) Upsert(_ context.Context, in repository.NewsFingerprint) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.upserts++
	f.current = in
	return nil
}
func (f *fakeFingerprintStore) TouchAICalled(_ context.Context, _ string) error { return nil }

// PART 9 — wired news-fingerprint gating: when annotations unchanged
// between two cycles, the second cycle MUST NOT invoke the analyzer.
func TestTick_NewsGateSkipsAIWhenUnchanged(t *testing.T) {
	cand := &fakeCandidates{rows: sampleCandidates()}
	st := &fakeStore{}
	an := &fakeAnalyzer{scripts: []scriptedAIResult{{out: analysis.MarketReportAnalysis{
		Status: analysis.StatusOK, Model: "t", ReportText: "ok",
	}}}}
	bot := &fakeBot{}
	fp := &fakeFingerprintStore{}
	// Lister returns the same annotation set every call.
	lister := &fakeAnnotationLister{byEvent: map[string][]repository.EventAnnotation{
		"x-event": {{
			Title:     "stable annotation",
			Outcome:   "Yes",
			Timestamp: time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC),
		}},
	}}
	w := New(baseConfig(), cand, st, an, bot, nopLogger())
	w.SetAnnotationLister(lister)
	w.SetNewsFingerprintStore(fp)

	// First tick: no prior fingerprint → AI runs, fingerprint stored.
	w.Tick(context.Background())
	if an.CallCount() != 1 {
		t.Errorf("first tick must invoke AI; got %d", an.CallCount())
	}
	if fp.upserts == 0 {
		t.Error("first tick must persist fingerprint")
	}
	// Second tick: same annotations → gate must skip AI.
	w.Tick(context.Background())
	if an.CallCount() != 1 {
		t.Errorf("second tick with unchanged news must skip AI; got %d AI calls total", an.CallCount())
	}
}

// --- helpers -------------------------------------------------------------

type analyzerFunc func(ctx context.Context, req analysis.MarketReportRequest) (analysis.MarketReportAnalysis, error)

func (f analyzerFunc) AnalyzeMarketReport(ctx context.Context, req analysis.MarketReportRequest) (analysis.MarketReportAnalysis, error) {
	return f(ctx, req)
}
