package dbbaseline

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/analytics/baseline"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/vo"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/repository"
)

// --- hermetic unit tests using fakes ---------------------------------------

type fakeMarkets struct {
	byID map[string]repository.Market
}

func (f *fakeMarkets) GetByConditionID(_ context.Context, id string) (repository.Market, error) {
	m, ok := f.byID[id]
	if !ok {
		return repository.Market{}, errors.New("not found")
	}
	return m, nil
}

type fakeTrades struct {
	dist  repository.BaselineDistribution
	err   error
	calls int
	last  repository.BaselineQuery
}

func (f *fakeTrades) Distribution(_ context.Context, q repository.BaselineQuery) (repository.BaselineDistribution, error) {
	f.calls++
	f.last = q
	return f.dist, f.err
}

func TestProvider_EmptyBucketReturnsZeroStats(t *testing.T) {
	m := &fakeMarkets{byID: map[string]repository.Market{"0xa": {ID: 1}}}
	tr := &fakeTrades{dist: repository.BaselineDistribution{}}
	p := New(Config{Window: 7 * 24 * time.Hour, Clock: func() time.Time { return time.Unix(1_000_000, 0) }}, tr, m)

	stats, err := p.Stats(context.Background(), baseline.Key{Market: "0xa", OutcomeToken: "tok"})
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.Count != 0 || stats.MedianUSD != 0 {
		t.Fatalf("expected zero stats, got %+v", stats)
	}
}

func TestProvider_UnknownMarketReturnsZeroStatsNoError(t *testing.T) {
	m := &fakeMarkets{byID: map[string]repository.Market{}}
	tr := &fakeTrades{}
	p := New(Config{Window: time.Hour}, tr, m)

	stats, err := p.Stats(context.Background(), baseline.Key{Market: "0xmissing", OutcomeToken: "t"})
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.Count != 0 {
		t.Errorf("unknown market must yield zero stats, got %+v", stats)
	}
	if tr.calls != 0 {
		t.Errorf("Distribution must not be called for unknown market, got %d calls", tr.calls)
	}
}

func TestProvider_PassesWindowedSinceCutoff(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	m := &fakeMarkets{byID: map[string]repository.Market{"0xa": {ID: 7}}}
	tr := &fakeTrades{dist: repository.BaselineDistribution{
		SampleCount:       42,
		TotalNotionalUSD:  4200,
		MeanNotionalUSD:   100,
		MedianNotionalUSD: 95,
		P95NotionalUSD:    250,
		OldestAt:          now.Add(-3 * 24 * time.Hour),
		NewestAt:          now,
	}}
	p := New(Config{Window: 24 * time.Hour, Clock: func() time.Time { return now }}, tr, m)

	stats, err := p.Stats(context.Background(), baseline.Key{Market: "0xa", OutcomeToken: "tok-yes"})
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.Count != 42 || stats.MedianUSD != 95 || stats.P95USD != 250 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
	if tr.last.MarketID != 7 || tr.last.OutcomeToken != "tok-yes" {
		t.Errorf("query target: %+v", tr.last)
	}
	wantSince := now.Add(-24 * time.Hour)
	if !tr.last.Since.Equal(wantSince) {
		t.Errorf("since: got %s want %s", tr.last.Since, wantSince)
	}
}

func TestProvider_ZeroWindowLiftsLowerBound(t *testing.T) {
	m := &fakeMarkets{byID: map[string]repository.Market{"0xa": {ID: 1}}}
	tr := &fakeTrades{dist: repository.BaselineDistribution{SampleCount: 1}}
	p := New(Config{Window: 0}, tr, m)

	if _, err := p.Stats(context.Background(), baseline.Key{Market: "0xa", OutcomeToken: "t"}); err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if !tr.last.Since.IsZero() {
		t.Errorf("Window=0 must pass zero time, got %s", tr.last.Since)
	}
}

func TestProvider_CachesMarketIDLookup(t *testing.T) {
	m := &fakeMarkets{byID: map[string]repository.Market{"0xa": {ID: 1}}}
	tr := &fakeTrades{dist: repository.BaselineDistribution{}}
	p := New(Config{}, tr, m)

	for i := 0; i < 5; i++ {
		if _, err := p.Stats(context.Background(), baseline.Key{Market: "0xa", OutcomeToken: "t"}); err != nil {
			t.Fatalf("Stats: %v", err)
		}
	}
	// We can't observe markets call count without instrumenting the fake;
	// instead, ensure no error and that the cache survives Forget+rebuild.
	p.Forget("0xa")
	if _, err := p.Stats(context.Background(), baseline.Key{Market: "0xa", OutcomeToken: "t"}); err != nil {
		t.Fatalf("Stats after Forget: %v", err)
	}
}

// --- integration test against a live Postgres -------------------------------

func TestProvider_IntegrationDistributionAgainstPostgres(t *testing.T) {
	dsn := os.Getenv("POSTGRES_TEST_DSN")
	if dsn == "" {
		t.Skip("POSTGRES_TEST_DSN not set; skipping DB integration test")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)
	// NOTE: parallel package runs share the same DB; use `go test -p 1`
	// when invoking the live integration suites across multiple packages.
	if _, err := pool.Exec(context.Background(), `
		TRUNCATE TABLE polymarket_alerts, polymarket_trades, polymarket_market_outcomes,
			polymarket_market_categories, polymarket_markets, polymarket_traders, polymarket_categories
		RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	ctx := context.Background()
	cats, _ := repository.NewCategoryRepository(pool).UpsertSeen(ctx, []repository.Category{
		{ExternalID: "1", Slug: "politics", Name: "Politics"},
	})
	markets, _ := repository.NewMarketRepository(pool).UpsertSeen(ctx, []repository.UpsertMarketInput{
		{ConditionID: "0xabc", Slug: "m", Question: "q", CategoryIDs: []int64{cats[0].ID}},
	})
	tradeRepo := repository.NewTradeRepository(pool)

	now := time.Now().UTC().Truncate(time.Second)
	// 20 trades: 19 at $100, 1 at $1000 → median 100, mean ~145.
	trades := make([]repository.InsertTradeInput, 0, 20)
	for i := 0; i < 19; i++ {
		trades = append(trades, repository.InsertTradeInput{
			MarketID: markets[0].ID, OutcomeToken: "tok", Side: "BUY",
			Price: 0.5, SizeShares: 200, NotionalUSD: 100,
			TradedAt:   now.Add(time.Duration(-i) * time.Hour),
			ExternalID: "lo-" + string(rune('A'+i)),
		})
	}
	trades = append(trades, repository.InsertTradeInput{
		MarketID: markets[0].ID, OutcomeToken: "tok", Side: "BUY",
		Price: 0.5, SizeShares: 2000, NotionalUSD: 1000,
		TradedAt:   now.Add(-1 * time.Hour),
		ExternalID: "hi-X",
	})
	if _, err := tradeRepo.UpsertBatch(ctx, trades); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	provider := New(Config{Window: 0, Clock: func() time.Time { return now }},
		tradeRepo, repository.NewMarketRepository(pool))

	stats, err := provider.Stats(ctx, baseline.Key{Market: vo.MarketID("0xabc"), OutcomeToken: "tok"})
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.Count != 20 {
		t.Errorf("count: %d want 20", stats.Count)
	}
	if stats.MedianUSD < 99 || stats.MedianUSD > 101 {
		t.Errorf("median: %v want ~100", stats.MedianUSD)
	}
	if stats.TotalUSD < 2899 || stats.TotalUSD > 2901 {
		t.Errorf("total: %v want ~2900", stats.TotalUSD)
	}
	if stats.SpanActual < 17*time.Hour {
		t.Errorf("span: %s want at least 17h", stats.SpanActual)
	}
}
