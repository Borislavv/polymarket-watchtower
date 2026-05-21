package unifiedintel

import (
	"context"
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

type fakeAnnots struct{}

func (f *fakeAnnots) ListRecentAnnotations(_ context.Context, _ string, _ int32) ([]repository.EventAnnotation, error) {
	return nil, nil
}

type fakeCatalysts struct{}

func (f *fakeCatalysts) ListActive(_ context.Context, _ string) ([]repository.EventCatalyst, error) {
	return nil, nil
}

type fakeAnalyzer struct {
	out analysis.MarketReportAnalysis
	err error
}

func (f *fakeAnalyzer) AnalyzeMarketReport(_ context.Context, _ analysis.MarketReportRequest) (analysis.MarketReportAnalysis, error) {
	return f.out, f.err
}

type fakeStore struct {
	mu       sync.Mutex
	inserted []repository.NewMarketIntelligenceReport
	hashes   map[string]bool
}

func (f *fakeStore) Insert(_ context.Context, r repository.NewMarketIntelligenceReport) (repository.MarketIntelligenceReport, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.hashes == nil {
		f.hashes = map[string]bool{}
	}
	if f.hashes[r.SummaryHash] {
		return repository.MarketIntelligenceReport{}, false, nil
	}
	f.hashes[r.SummaryHash] = true
	f.inserted = append(f.inserted, r)
	return repository.MarketIntelligenceReport{SummaryHash: r.SummaryHash}, true, nil
}

type fakeBot struct {
	mu    sync.Mutex
	sends []string
}

func (b *fakeBot) SendHTML(_ context.Context, _ string, text string) (telegram.SendResult, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sends = append(b.sends, text)
	return telegram.SendResult{}, nil
}

func nopLogger() *zerolog.Logger { l := zerolog.Nop(); return &l }

func sampleCandidates() []repository.IntelligenceCandidate {
	return []repository.IntelligenceCandidate{
		{ConditionID: "0xa", Question: "Iran peace deal", EventSlug: "iran-peace", Category: "Geopolitics",
			LifecyclePct: 92, LastPrice: 0.21, Trades24h: 80, Volume24hUSD: 40_000, Alerts24h: 5},
		{ConditionID: "0xb", Question: "Texas runoff Paxton", EventSlug: "tx-runoff", Category: "Politics",
			LifecyclePct: 96, LastPrice: 0.62, Trades24h: 40, Volume24hUSD: 20_000, Alerts24h: 1},
	}
}

func baseCfg() Config {
	return Config{
		Enabled:         true,
		QueryInterval:   4 * time.Hour,
		MinSendInterval: 4 * time.Hour,
		MaxCandidates:   60,
		MaxSelected:     8,
		ChatID:          "42",
		PolymarketBase:  "https://polymarket.com",
	}
}

// --- tests ---------------------------------------------------------------

// PART 2/3: AiAnsweredNotFoundNoticeable → persist no-action, no Telegram.
func TestTick_SentinelNoNoticeableEdgeNeverSends(t *testing.T) {
	store := &fakeStore{}
	bot := &fakeBot{}
	w := New(baseCfg(),
		&fakeCandidates{rows: sampleCandidates()},
		&fakeAnnots{}, &fakeCatalysts{},
		&fakeAnalyzer{out: analysis.MarketReportAnalysis{
			Status: analysis.StatusOK, Model: "gpt-4.1",
			ReportText: "AiAnsweredNotFoundNoticeable",
		}},
		store, bot, nil, nopLogger())
	w.Tick(context.Background())
	if len(bot.sends) != 0 {
		t.Errorf("sentinel must NOT ship Telegram; got %d sends", len(bot.sends))
	}
	if len(store.inserted) != 1 {
		t.Fatalf("expected 1 persisted no-action row; got %d", len(store.inserted))
	}
	if !strings.HasPrefix(store.inserted[0].DeliveryStatus, "skipped_sentinel_") {
		t.Errorf("delivery_status: got %q want skipped_sentinel_*", store.inserted[0].DeliveryStatus)
	}
}

// PART 2/3: AiAnsweredAlreadyPriced → same suppression path.
func TestTick_SentinelAlreadyPricedSuppresses(t *testing.T) {
	store := &fakeStore{}
	bot := &fakeBot{}
	w := New(baseCfg(),
		&fakeCandidates{rows: sampleCandidates()},
		&fakeAnnots{}, &fakeCatalysts{},
		&fakeAnalyzer{out: analysis.MarketReportAnalysis{
			Status: analysis.StatusOK, Model: "gpt-4.1",
			ReportText: "AiAnsweredAlreadyPriced",
		}},
		store, bot, nil, nopLogger())
	w.Tick(context.Background())
	if len(bot.sends) != 0 {
		t.Errorf("already-priced must NOT ship; got %d", len(bot.sends))
	}
}

// PART 2: JSON output → ONE consolidated Telegram message.
func TestTick_JSONSelectionShipsOneMessage(t *testing.T) {
	store := &fakeStore{}
	bot := &fakeBot{}
	w := New(baseCfg(),
		&fakeCandidates{rows: sampleCandidates()},
		&fakeAnnots{}, &fakeCatalysts{},
		&fakeAnalyzer{out: analysis.MarketReportAnalysis{
			Status: analysis.StatusOK, Model: "gpt-4.1",
			ReportText: `{
  "regime": "informed_flow",
  "selected": [
    {"event_slug":"iran-peace","condition_id":"0xa","rank":1,"interest_score":0.9,"class":"informed_flow",
     "thesis":"Same wallet built 4-leg accumulation in 17 min — pre-news positioning",
     "why_now":"Wallet age 2d, p99 trade with no public catalyst",
     "expected_direction":"YES_up",
     "what_would_invalidate":"price reversion to 0.10 with no follow-on flow",
     "what_to_watch_next":"second wallet entering same side"}
  ]
}`,
		}},
		store, bot, nil, nopLogger())
	w.Tick(context.Background())
	if len(bot.sends) != 1 {
		t.Fatalf("expected 1 send; got %d", len(bot.sends))
	}
	body := bot.sends[0]
	for _, want := range []string{
		"<b>UNIFIED INTELLIGENCE</b>",
		"regime: informed_flow",
		"thesis: Same wallet built 4-leg accumulation",
		"why now: Wallet age 2d",
		"direction: YES_up",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in body:\n%s", want, body)
		}
	}
}

// PART 8: MinSendInterval cooldown — second JSON cycle in <4h should
// persist but NOT send.
func TestTick_SendCooldownSuppressesTelegram(t *testing.T) {
	store := &fakeStore{}
	bot := &fakeBot{}
	cfg := baseCfg()
	cfg.MinSendInterval = 4 * time.Hour
	cfg.QueryInterval = time.Hour // query faster than we send
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	clk := func() time.Time { return now }
	cfg.Clock = clk
	w := New(cfg,
		&fakeCandidates{rows: sampleCandidates()},
		&fakeAnnots{}, &fakeCatalysts{},
		&fakeAnalyzer{out: analysis.MarketReportAnalysis{
			Status: analysis.StatusOK, Model: "gpt-4.1",
			ReportText: `{"regime":"x","selected":[{"event_slug":"a","condition_id":"b","rank":1,"interest_score":0.9,"class":"informed_flow","thesis":"x","why_now":"y","expected_direction":"YES_up"}]}`,
		}},
		store, bot, nil, nopLogger())
	w.Tick(context.Background())
	// Second tick 1h later — still inside the 4h send cooldown.
	now = now.Add(time.Hour)
	w.Tick(context.Background())
	if len(bot.sends) != 1 {
		t.Errorf("MinSendInterval must suppress 2nd send within window; got %d", len(bot.sends))
	}
	if len(store.inserted) < 1 {
		t.Errorf("both cycles should persist; got %d rows", len(store.inserted))
	}
}

// PART 2: invalid AI output → persist no-action, no Telegram, no
// "store as analysis" leakage.
func TestTick_InvalidFormatNeverShips(t *testing.T) {
	store := &fakeStore{}
	bot := &fakeBot{}
	w := New(baseCfg(),
		&fakeCandidates{rows: sampleCandidates()},
		&fakeAnnots{}, &fakeCatalysts{},
		&fakeAnalyzer{out: analysis.MarketReportAnalysis{
			Status: analysis.StatusOK, Model: "gpt-4.1",
			ReportText: "free-text prose that violates JSON-only contract",
		}},
		store, bot, nil, nopLogger())
	w.Tick(context.Background())
	if len(bot.sends) != 0 {
		t.Errorf("invalid format must NOT ship; got %d", len(bot.sends))
	}
	if len(store.inserted) != 1 || !strings.HasPrefix(store.inserted[0].DeliveryStatus, "skipped_") {
		t.Errorf("invalid format must persist as skipped_* row; got %v", store.inserted)
	}
}

// v10.9 PART 3 — market AI cache. Second cycle with same candidate
// set MUST NOT invoke the analyzer; cache hit short-circuits.
type fakeAICache struct {
	mu      sync.Mutex
	cached  map[string]repository.MarketAICacheEntry
	upserts int
	hits    int
}

func (c *fakeAICache) Get(_ context.Context, surface, key string) (repository.MarketAICacheEntry, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.cached[surface+"|"+key]
	if ok {
		c.hits++
	}
	return e, ok, nil
}
func (c *fakeAICache) Upsert(_ context.Context, in repository.MarketAICacheRow) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cached == nil {
		c.cached = map[string]repository.MarketAICacheEntry{}
	}
	c.upserts++
	c.cached[in.AISurface+"|"+in.MarketAIKey] = repository.MarketAICacheEntry{
		AISurface:    in.AISurface,
		MarketAIKey:  in.MarketAIKey,
		AIStatus:     in.AIStatus,
		SentinelCode: in.SentinelCode,
		DecisionJSON: in.DecisionJSON,
		SummaryText:  in.SummaryText,
	}
	return nil
}
func (c *fakeAICache) TouchReuse(_ context.Context, surface, key string) error { return nil }

func TestTick_MarketAICacheReuseSkipsAI(t *testing.T) {
	store := &fakeStore{}
	bot := &fakeBot{}
	cache := &fakeAICache{}
	cands := &fakeCandidates{rows: sampleCandidates()}

	calls := 0
	analyzer := &countingAnalyzer{n: &calls, res: analysis.MarketReportAnalysis{
		Status: analysis.StatusOK, Model: "gpt-4.1",
		ReportText: `{"regime":"informed_flow","selected":[{"event_slug":"iran-peace","condition_id":"0xa","rank":1,"interest_score":0.9,"class":"informed_flow","why_now":"y","expected_direction":"YES_up"}]}`,
	}}
	w := New(baseCfg(), cands, &fakeAnnots{}, &fakeCatalysts{}, analyzer, store, bot, nil, nopLogger())
	w.SetMarketAICache(cache)

	w.Tick(context.Background())
	if calls != 1 {
		t.Fatalf("first tick must call AI once; got %d", calls)
	}
	// Same candidates → cache hit → no AI on second cycle.
	w.Tick(context.Background())
	if calls != 1 {
		t.Errorf("second tick must hit cache; got %d analyzer calls", calls)
	}
	if cache.hits == 0 {
		t.Errorf("expected cache hit on second tick")
	}
}

// countingAnalyzer is a deterministic AI that increments a counter
// each time AnalyzeMarketReport is called.
type countingAnalyzer struct {
	n   *int
	res analysis.MarketReportAnalysis
}

func (c *countingAnalyzer) AnalyzeMarketReport(_ context.Context, _ analysis.MarketReportRequest) (analysis.MarketReportAnalysis, error) {
	*c.n++
	return c.res, nil
}

// PART 2: empty `selected` from the parser collapses to a sentinel
// internally, suppressed identically to AiAnsweredNotFoundNoticeable.
func TestTick_EmptySelectedTreatedAsSentinel(t *testing.T) {
	bot := &fakeBot{}
	w := New(baseCfg(),
		&fakeCandidates{rows: sampleCandidates()},
		&fakeAnnots{}, &fakeCatalysts{},
		&fakeAnalyzer{out: analysis.MarketReportAnalysis{
			Status: analysis.StatusOK, Model: "gpt-4.1",
			ReportText: `{"regime":"x","selected":[]}`,
		}},
		&fakeStore{}, bot, nil, nopLogger())
	w.Tick(context.Background())
	if len(bot.sends) != 0 {
		t.Errorf("empty selected must collapse to sentinel — no send; got %d", len(bot.sends))
	}
}
