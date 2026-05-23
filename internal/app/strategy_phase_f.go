// strategy_phase_f.go — v11.10 real-source wiring for the two
// strategies that were honest stubs in v11.9:
//
//   - holdersync: replaces the NO_SOURCE stub with a real adapter
//     backed by Polymarket Data API /holders (no auth, verified
//     2026-05-23). Open Interest is derived honestly as
//     SUM(holders.amount) per token — no fake denominator.
//   - bookbars: new producer for polymarket_book_feature_bars,
//     backed by CLOB REST /book and /books (no auth, verified
//     2026-05-23). bookvacuum can now consume real top-N depth.
//
// Both wirings are feature-flag controlled. With the flags off the
// adapters are constructed but no network call ever fires.
package app

import (
	"context"
	"sort"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/bookbars"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/holdersync"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/workerbudget"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/vo"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/metrics"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/polymarket/clob"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/polymarket/dataapi"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/postgres/sqlc"
)

// StrategyPhaseF bundles the v11.10 producers wired against live
// Polymarket APIs. Nil fields = feature disabled.
type StrategyPhaseF struct {
	BookBars *bookbars.Worker
}

// wireStrategyPhaseF constructs the v11.10 producers + replaces the
// holdersync NO_SOURCE stub on the v11.9 Phase C worker (when its
// SourceMode says so). Caller passes the already-constructed dataapi
// client + a CLOB client built here.
//
// The function returns:
//   - the bookbars worker (added to the exec list);
//   - a *holdersync.Worker that REPLACES the Phase C stub when
//     HolderSync.SourceMode != "disabled" AND a dataapi client is
//     available. Caller is responsible for swapping the field on
//     StrategyPhaseC.HolderSync; this function exposes the live
//     worker but does NOT mutate phaseC behind the caller's back.
func wireStrategyPhaseF(
	pool *pgxpool.Pool,
	scfg StrategyConfig,
	met *metrics.Metrics,
	dataClient *dataapi.Client,
) (StrategyPhaseF, *holdersync.Worker) {
	if pool == nil {
		return StrategyPhaseF{}, nil
	}
	q := sqlc.New(pool)

	// CLOB client for bookbars.
	clobClient, err := clob.New(clob.Config{
		BaseURL: "https://clob.polymarket.com",
		Timeout: 5 * time.Second,
	})
	if err != nil {
		// Defensive — clob.New only fails on an unparseable URL.
		return StrategyPhaseF{}, nil
	}

	// v11.10 PART 6 — bucket-budgeted selection. The selector reads
	// from the same pool; budget is snapshotted by the worker on
	// every Tick via the closure below so an operator config change
	// (when we add hot-reload) would take effect on the next cycle.
	budgetSel := workerbudget.New(q)
	budgetSnapshot := func() workerbudget.Budget {
		return workerbudget.Budget{
			OperatorPinned:     scfg.WorkerBudget.OperatorPinned,
			RecentAlert:        scfg.WorkerBudget.RecentAlert,
			CatalystNear:       scfg.WorkerBudget.CatalystNear,
			LinkedToFired:      scfg.WorkerBudget.LinkedToFired,
			Liquid:             scfg.WorkerBudget.Liquid,
			FallbackActive:     scfg.WorkerBudget.FallbackActive,
			PinnedConditionIDs: scfg.WorkerBudget.PinnedConditionIDs,
		}
	}

	bookBarsWorker := bookbars.New(
		bookbars.Config{
			Enabled:      scfg.BookFeatureBars.Enabled,
			Interval:     scfg.BookFeatureBars.Interval,
			BarSeconds:   5,
			TopN:         scfg.BookFeatureBars.TopN,
			MaxMarkets:   scfg.BookFeatureBars.MaxMarkets,
			BatchSize:    25,
			FetchTimeout: 5 * time.Second,
		},
		&bookbarsLister{q: q, budget: budgetSnapshot, sel: budgetSel, met: met},
		&bookbarsFetcher{cl: clobClient},
		&bookbarsSink{q: q},
		met,
		nil,
	)

	// Real holdersync — only when SourceMode says so AND we have a
	// dataapi client. The constructor still allocates the worker
	// even when disabled so the caller's swap is a simple
	// non-nil-check.
	var holderWorker *holdersync.Worker
	if dataClient != nil && scfg.HolderSync.SourceMode == "dataapi" {
		holderWorker = holdersync.New(
			holdersync.Config{
				Enabled:      scfg.HolderSync.WorkerEnabled,
				Interval:     scfg.HolderSync.IntervalV2,
				MaxMarkets:   scfg.HolderSync.MaxMarketsV2,
				TopK:         scfg.HolderSync.TopKV2,
				FetchTimeout: scfg.HolderSync.PerMarketTimeout,
				Concurrency:  scfg.HolderSync.Concurrency,
				StaleAfter:   scfg.HolderSync.StaleAfter,
			},
			&holdersyncCandidatesLister{q: q, budget: budgetSnapshot, sel: budgetSel, met: met},
			&holdersyncRealFetcher{cl: dataClient, requireOI: scfg.HolderSync.RequireOpenInterest},
			&holdersyncRealSink{q: q},
			met,
			nil,
		)
	}

	return StrategyPhaseF{BookBars: bookBarsWorker}, holderWorker
}

// --- bookbars adapters --------------------------------------------
//
// v11.10 PART 6: the lister now consults the workerbudget selector
// FIRST. When a non-zero budget is configured the bucketed selection
// is authoritative — the legacy unbucketed query is only used as a
// fallback when (a) the budget is all-zero, or (b) the bucketed query
// errored and we want to keep the worker producing rows rather than
// stalling. Per-bucket counts are emitted as Prometheus labels via
// the shared StrategyWorkerItems counter (op="bucket:<name>").

type bookbarsLister struct {
	q      *sqlc.Queries
	budget func() workerbudget.Budget
	sel    *workerbudget.Selector
	met    *metrics.Metrics
}

func (l *bookbarsLister) ListBookbarsCandidates(ctx context.Context, limit int) ([]bookbars.Candidate, error) {
	if l.budget != nil && l.sel != nil {
		b := l.budget()
		if !b.AllZero() {
			res, err := l.sel.Select(ctx, b)
			if err == nil {
				recordBucketMetrics(l.met, "bookbars", res)
				out := make([]bookbars.Candidate, 0, len(res.Rows))
				for _, r := range res.Rows {
					out = append(out, bookbars.Candidate{ConditionID: r.ConditionID, Token: r.TokenID})
				}
				return out, nil
			}
			// fall through to legacy on error — never stall.
		}
	}
	rows, err := l.q.ListBookbarsCandidates(ctx, int32(limit))
	if err != nil {
		return nil, err
	}
	out := make([]bookbars.Candidate, 0, len(rows))
	for _, r := range rows {
		out = append(out, bookbars.Candidate{ConditionID: r.ConditionID, Token: r.TokenID})
	}
	return out, nil
}

// recordBucketMetrics emits one counter increment per (worker, bucket)
// pair so an operator can graph the bucket mix per worker.
func recordBucketMetrics(met *metrics.Metrics, worker string, res workerbudget.Result) {
	if met == nil || met.StrategyWorkerItems == nil {
		return
	}
	for b, n := range res.PerBucket {
		if n <= 0 {
			continue
		}
		met.StrategyWorkerItems.WithLabelValues(worker, "bucket:"+b.Name()).Add(float64(n))
	}
}

type bookbarsFetcher struct{ cl *clob.Client }

func (f *bookbarsFetcher) GetBook(ctx context.Context, tokenID string) (clob.Book, error) {
	return f.cl.GetBook(ctx, tokenID)
}
func (f *bookbarsFetcher) GetBooks(ctx context.Context, tokenIDs []string) ([]clob.Book, error) {
	return f.cl.GetBooks(ctx, tokenIDs)
}

type bookbarsSink struct{ q *sqlc.Queries }

func (s *bookbarsSink) UpsertBar(ctx context.Context, b bookbars.Bar) error {
	bid := b.BestBid
	ask := b.BestAsk
	mid := b.MidPrice
	spread := b.Spread
	imb := b.DepthImbal
	return s.q.UpsertBookFeatureBar(ctx, sqlc.UpsertBookFeatureBarParams{
		ConditionID:      b.ConditionID,
		OutcomeToken:     b.OutcomeToken,
		BarSeconds:       int32(b.BarSeconds),
		BarStart:         tsFromTime(b.BarStart),
		BestBid:          &bid,
		BestAsk:          &ask,
		MidPrice:         &mid,
		BidDepthTopN:     &b.BidDepthTopN,
		AskDepthTopN:     &b.AskDepthTopN,
		Spread:           &spread,
		SpreadZ:          nil,
		BidDepthDeltaPct: nil,
		AskDepthDeltaPct: nil,
		MidDelta:         &imb,
	})
}

// --- holdersync v11.10 real adapters ------------------------------

type holdersyncCandidatesLister struct {
	q      *sqlc.Queries
	budget func() workerbudget.Budget
	sel    *workerbudget.Selector
	met    *metrics.Metrics
}

func (l *holdersyncCandidatesLister) ListHolderSyncCandidates(ctx context.Context, limit int, _ time.Duration) ([]holdersync.Candidate, error) {
	if l.budget != nil && l.sel != nil {
		b := l.budget()
		if !b.AllZero() {
			res, err := l.sel.Select(ctx, b)
			if err == nil {
				recordBucketMetrics(l.met, "holdersync", res)
				out := make([]holdersync.Candidate, 0, len(res.Rows))
				for _, r := range res.Rows {
					out = append(out, holdersync.Candidate{
						ConditionID:  r.ConditionID,
						OutcomeToken: r.TokenID,
					})
				}
				return out, nil
			}
		}
	}
	// Reuse the bookbars candidate query — it returns (condition_id,
	// token_id) pairs for active markets, which is exactly what
	// holdersync needs (one snapshot per token).
	rows, err := l.q.ListBookbarsCandidates(ctx, int32(limit))
	if err != nil {
		return nil, err
	}
	out := make([]holdersync.Candidate, 0, len(rows))
	for _, r := range rows {
		out = append(out, holdersync.Candidate{
			ConditionID:  r.ConditionID,
			OutcomeToken: r.TokenID,
		})
	}
	return out, nil
}

type holdersyncRealFetcher struct {
	cl        *dataapi.Client
	requireOI bool
}

func (f *holdersyncRealFetcher) FetchHolders(ctx context.Context, conditionID, outcomeToken string, topK int) ([]holdersync.HolderRow, error) {
	groups, err := f.cl.ListHolders(ctx, dataapi.ListHoldersOpts{
		Market: vo.MarketID(conditionID),
		Limit:  topK,
	})
	if err != nil {
		return nil, err
	}
	// Find the requested token group.
	var match *dataapi.HoldersByToken
	for i := range groups {
		if groups[i].Token == outcomeToken {
			match = &groups[i]
			break
		}
	}
	if match == nil {
		return nil, nil // honest empty — no fake row
	}
	if f.requireOI && match.OpenInterest <= 0 {
		return nil, nil
	}
	// Map to holdersync.HolderRow with rank + pct_oi derived
	// honestly from SUM(amount).
	rows := make([]holdersync.HolderRow, 0, len(match.Holders))
	sort.SliceStable(match.Holders, func(i, j int) bool {
		return match.Holders[i].Amount > match.Holders[j].Amount
	})
	for i, h := range match.Holders {
		row := holdersync.HolderRow{
			Wallet: h.Wallet,
			Rank:   i + 1,
			Shares: h.Amount,
		}
		if match.OpenInterest > 0 {
			row.PctOI = h.Amount / match.OpenInterest
			row.TotalOI = match.OpenInterest
		}
		rows = append(rows, row)
	}
	return rows, nil
}

type holdersyncRealSink struct{ q *sqlc.Queries }

func (s *holdersyncRealSink) UpsertSnapshot(ctx context.Context, snap holdersync.Snapshot) error {
	for _, r := range snap.Rows {
		if err := s.q.UpsertHolderSnapshot(ctx, sqlc.UpsertHolderSnapshotParams{
			ConditionID:  snap.ConditionID,
			OutcomeToken: snap.OutcomeToken,
			SnapshotAt:   tsFromTime(snap.SnapshotAt),
			Wallet:       r.Wallet,
			Rank:         int32(r.Rank),
			Shares:       r.Shares,
			NotionalUsd:  r.NotionalUSD,
			PctOi:        r.PctOI,
			TotalOi:      r.TotalOI,
			RawJson:      nil,
		}); err != nil {
			return err
		}
	}
	return nil
}
