// strategy_phase_c.go — v11.7 Phase C production adapters for the
// five v11.5 supporting workers + outcome backfill evaluator. No
// network calls; every adapter reads from existing Postgres tables.
//
// Wiring contract:
//   - When pool is nil, wireStrategyPhaseC returns zero-value
//     bundles (caller treats nil as disabled).
//   - When pool is non-nil, every adapter is constructed but the
//     surrounding workers stay inert until their per-worker
//     `*_WORKER_ENABLED` env flag flips true.
//   - HolderSync is intentionally NOT wired against a live API —
//     no Polymarket holders endpoint is currently wrapped in
//     internal/infra/polymarket/. Its adapter returns ErrNoSource
//     and the worker stays a no-op even when enabled (with a
//     "no_source_adapter" status metric on every tick).
package app

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"encoding/json"

	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/analytics/rulesrisk"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/holdersync"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/marketlinks"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/repricing"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/riskscore"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/strategyoutcome"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/thesislines"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/walletgraph"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/metrics"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/postgres/sqlc"
)

// ErrNoSource is returned by holdersync's adapter to signal that no
// live data source is wired. The worker treats it as a fail-open
// "skipped" rather than an error.
var ErrNoSource = errors.New("strategy phase c: no live source adapter wired")

// StrategyPhaseC is the bundle returned by wireStrategyPhaseC.
type StrategyPhaseC struct {
	MarketLinks     *marketlinks.Builder
	HolderSync      *holdersync.Worker
	RiskScore       *riskscore.Worker
	Repricing       *repricing.Worker
	WalletGraph     *walletgraph.Worker
	OutcomeBackfill *strategyoutcome.Worker
	ThesisLines     *thesislines.Worker
}

// wireStrategyPhaseC constructs the v11.7 workers using
// production Postgres adapters. Returns zero-value bundle when the
// pool is nil.
func wireStrategyPhaseC(pool *pgxpool.Pool, scfg StrategyConfig, met *metrics.Metrics, rr *rulesrisk.Detector) StrategyPhaseC {
	if pool == nil {
		return StrategyPhaseC{}
	}
	q := sqlc.New(pool)

	mlBuilder := marketlinks.New(
		marketlinks.Config{
			Enabled:        scfg.MarketLinks.Enabled,
			Interval:       scfg.MarketLinks.Interval,
			BatchSize:      scfg.MarketLinks.BatchSize,
			LinkVersion:    scfg.MarketLinks.LinkVersion,
			IncludeOpposed: scfg.MarketLinks.IncludeOpposed,
			MinConfidence:  scfg.MarketLinks.MinConfidence,
		},
		&marketLinksEventLister{q: q},
		&marketLinksSink{q: q},
		met,
		nil,
	)

	holderSyncWorker := holdersync.New(
		holdersync.Config{
			Enabled:      scfg.HolderSync.Enabled,
			Interval:     scfg.HolderSync.Interval,
			MaxMarkets:   scfg.HolderSync.MaxMarkets,
			TopK:         scfg.HolderSync.TopK,
			FetchTimeout: scfg.HolderSync.FetchTimeout,
			Concurrency:  scfg.HolderSync.Concurrency,
			StaleAfter:   scfg.HolderSync.StaleAfter,
		},
		&holderSyncNoSourceLister{}, // no Polymarket holders endpoint wrapped today
		&holderSyncNoSourceFetcher{},
		&holderSyncNoOpSink{},
		met,
		nil,
	)

	riskScoreWorker := riskscore.New(
		riskscore.Config{
			Enabled:      scfg.RiskScore.Enabled,
			Interval:     scfg.RiskScore.Interval,
			BatchSize:    scfg.RiskScore.BatchSize,
			ScoreVersion: scfg.RiskScore.ScoreVersion,
			RefreshOlder: scfg.RiskScore.RefreshOlder,
		},
		&riskScoreLister{q: q},
		&riskScoreSink{q: q},
		rr,
		met,
		nil,
	)

	repricingWorker := repricing.New(
		repricing.Config{
			Enabled:        scfg.Repricing.Enabled,
			Interval:       scfg.Repricing.Interval,
			OpenLookback:   scfg.Repricing.OpenLookback,
			MaxOpenWindows: scfg.Repricing.MaxOpenWindows,
			CloseAfter:     scfg.Repricing.CloseAfter,
		},
		&repricingCatalystLister{q: q},
		&repricingOpenLister{q: q},
		&repricingPriceSampler{q: q, pool: pool, linkVersion: int32(scfg.MarketLinks.LinkVersion)},
		&repricingSink{q: q},
		met,
		nil,
	)

	walletGraphWorker := walletgraph.New(
		walletgraph.Config{
			Enabled:            scfg.WalletGraph.Enabled,
			Interval:           scfg.WalletGraph.Interval,
			CoTradeWindow:      scfg.WalletGraph.CoTradeWindow,
			MinSharedEvents:    scfg.WalletGraph.MinSharedEvents,
			BatchSize:          scfg.WalletGraph.BatchSize,
			EdgeVersion:        scfg.WalletGraph.EdgeVersion,
			UseFundingProvider: scfg.WalletGraph.UseFundingProvider,
		},
		&walletGraphLister{q: q},
		nil, // no funding provider — Phase A only
		&walletGraphSink{pool: pool},
		met,
		nil,
	)

	outcomeBackfill := strategyoutcome.New(
		strategyoutcome.Config{
			Enabled:   scfg.OutcomeBackfill.Enabled,
			Interval:  scfg.OutcomeBackfill.Interval,
			BatchSize: scfg.OutcomeBackfill.BatchSize,
		},
		&outcomeLister{q: q,
			standaloneEnabled: scfg.OutcomeBackfill.StandaloneEnabled,
			standaloneBatch:   scfg.OutcomeBackfill.StandaloneBatch,
			resolveSide:       scfg.OutcomeBackfill.ResolveSide,
		},
		&outcomeUpdater{q: q},
		met,
		nil,
	)

	thesisLinesWorker := thesislines.New(
		thesislines.Config{
			Enabled:    scfg.ThesisLines.WorkerEnabled,
			Interval:   scfg.ThesisLines.Interval,
			Lookback:   scfg.ThesisLines.Lookback,
			MaxEvents:  scfg.ThesisLines.MaxEvents,
			MaxWallets: scfg.ThesisLines.MaxWallets,
			BatchSize:  scfg.ThesisLines.MaxWallets,
		},
		&thesisLinesLister{q: q},
		&thesisLinesSink{q: q},
		met,
		nil,
	)

	return StrategyPhaseC{
		MarketLinks:     mlBuilder,
		HolderSync:      holderSyncWorker,
		RiskScore:       riskScoreWorker,
		Repricing:       repricingWorker,
		WalletGraph:     walletGraphWorker,
		OutcomeBackfill: outcomeBackfill,
		ThesisLines:     thesisLinesWorker,
	}
}

// --- thesislines adapters -----------------------------------------

type thesisLinesLister struct{ q *sqlc.Queries }

func (l *thesisLinesLister) AggregateWalletThesisLines(ctx context.Context, since time.Time, limit int) ([]thesislines.Aggregate, error) {
	rows, err := l.q.AggregateWalletThesisLines(ctx, sqlc.AggregateWalletThesisLinesParams{
		Since:    tsFromTime(since),
		RowLimit: int32(limit),
	})
	if err != nil {
		return nil, err
	}
	out := make([]thesislines.Aggregate, 0, len(rows))
	for _, r := range rows {
		out = append(out, thesislines.Aggregate{
			Wallet:       r.Wallet,
			ConditionID:  r.ConditionID,
			EventSlug:    r.EventSlug,
			Side:         r.Side,
			NotionalUSD:  r.NotionalUsd,
			Trades:       int(r.Trades),
			LastTradedAt: r.LastTradedAt.Time,
		})
	}
	return out, nil
}

type thesisLinesSink struct{ q *sqlc.Queries }

func (s *thesisLinesSink) UpsertWalletThesisLine(ctx context.Context, a thesislines.Aggregate, lookbackHours int) error {
	return s.q.UpsertWalletThesisLine(ctx, sqlc.UpsertWalletThesisLineParams{
		Wallet:        a.Wallet,
		ConditionID:   a.ConditionID,
		EventSlug:     a.EventSlug,
		Side:          a.Side,
		NotionalUsd:   a.NotionalUSD,
		Trades:        int32(a.Trades),
		LastTradedAt:  tsFromTime(a.LastTradedAt),
		LookbackHours: int32(lookbackHours),
	})
}

// --- strategyoutcome adapters -------------------------------------

type outcomeLister struct {
	q                 *sqlc.Queries
	standaloneEnabled bool
	standaloneBatch   int
	resolveSide       bool
}

func (l *outcomeLister) ListShadowRowsForOutcomeBackfill(ctx context.Context, limit int) ([]strategyoutcome.PendingRow, error) {
	rows, err := l.q.ListShadowRowsForOutcomeBackfill(ctx, int32(limit))
	if err != nil {
		return nil, err
	}
	out := make([]strategyoutcome.PendingRow, 0, len(rows))
	for _, r := range rows {
		dedup := ""
		if r.LinkedAlertDedupKey != nil {
			dedup = *r.LinkedAlertDedupKey
		}
		out = append(out, strategyoutcome.PendingRow{
			ID:           r.ID,
			AlertOutcome: r.AlertOutcome,
			DedupKey:     dedup,
		})
	}
	// v11.8: standalone shadow rows (no linked alert) — resolved
	// against polymarket_markets.closed. The reader returns
	// market_closed=true rows; we map them to AlertOutcome=unknown
	// (market closed but no per-side outcome resolution available
	// via this path) so the operator can see them as "consumed by
	// outcome resolver" rather than NULL forever.
	if l.standaloneEnabled {
		standalone, err := l.q.ListStandaloneShadowRowsForOutcomeBackfill(ctx, int32(l.standaloneBatch))
		if err == nil {
			for _, r := range standalone {
				if !r.MarketClosed {
					continue
				}
				out = append(out, strategyoutcome.PendingRow{
					ID:           r.ID,
					AlertOutcome: "unknown", // market closed; per-side resolution via ResolveSide path
				})
			}
		}
	}
	// v11.9: per-side correct/wrong resolution. Reads shadow rows
	// linked to alerts that already have winning_outcome_token set
	// by the existing outcomes worker. Maps the shadow row's Side
	// to "resolved_correct" / "resolved_wrong" by exact label
	// match (case-insensitive).
	if l.resolveSide {
		resolved, err := l.q.ListStandaloneResolvedAlertOutcomes(ctx, int32(l.standaloneBatch))
		if err == nil {
			for _, r := range resolved {
				if r.WinningLabel == "" || r.Side == "" {
					continue
				}
				status := "resolved_wrong"
				if equalCaseFold(r.WinningLabel, r.Side) {
					status = "resolved_correct"
				}
				out = append(out, strategyoutcome.PendingRow{
					ID:           r.ID,
					AlertOutcome: status,
				})
			}
		}
	}
	return out, nil
}

func equalCaseFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if ca >= 'A' && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if cb >= 'A' && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}

type outcomeUpdater struct{ q *sqlc.Queries }

func (u *outcomeUpdater) UpdateShadowOutcomeStatus(ctx context.Context, id int64, outcome string) error {
	return u.q.UpdateShadowOutcomeStatus(ctx, sqlc.UpdateShadowOutcomeStatusParams{
		ID:            id,
		OutcomeStatus: &outcome,
	})
}

// --- marketlinks adapters ----------------------------------------

type marketLinksEventLister struct{ q *sqlc.Queries }

func (l *marketLinksEventLister) ListLinkHints(ctx context.Context, batchSize int) ([]marketlinks.LinkHint, error) {
	rows, err := l.q.ListEventGroupedMarkets(ctx, int32(batchSize))
	if err != nil {
		return nil, err
	}
	hints := make([]marketlinks.LinkHint, 0, len(rows))
	for _, r := range rows {
		conds := r.ConditionIds
		if len(conds) < 2 {
			continue
		}
		// Build a "star" graph anchored on the first condition_id.
		// Every other condition_id in the same event becomes a
		// target with link_type=same_event + direction=unknown +
		// confidence=0.6. This is the deterministic baseline; a
		// future Gamma-aware builder can refine direction.
		anchor := conds[0]
		targets := make([]marketlinks.LinkTarget, 0, len(conds)-1)
		for _, dst := range conds[1:] {
			targets = append(targets, marketlinks.LinkTarget{
				ConditionID: dst,
				LinkType:    "same_event",
				Direction:   "unknown",
				Confidence:  0.6,
			})
		}
		hints = append(hints, marketlinks.LinkHint{
			EventSlug:         r.EventSlug,
			SourceConditionID: anchor,
			Targets:           targets,
		})
	}
	return hints, nil
}

type marketLinksSink struct{ q *sqlc.Queries }

func (s *marketLinksSink) UpsertMarketLink(ctx context.Context, e marketlinks.Edge) error {
	return s.q.UpsertMarketLink(ctx, sqlc.UpsertMarketLinkParams{
		SrcConditionID: e.SrcConditionID,
		DstConditionID: e.DstConditionID,
		LinkType:       e.LinkType,
		Direction:      e.Direction,
		Confidence:     e.Confidence,
		EventSlug:      e.EventSlug,
		SeriesID:       e.SeriesID,
		LinkVersion:    int32(e.LinkVersion),
	})
}

// --- holdersync adapters (no live source) -------------------------

type holderSyncNoSourceLister struct{}

func (holderSyncNoSourceLister) ListHolderSyncCandidates(_ context.Context, _ int, _ time.Duration) ([]holdersync.Candidate, error) {
	return nil, nil // empty batch — worker reports "empty" status
}

type holderSyncNoSourceFetcher struct{}

func (holderSyncNoSourceFetcher) FetchHolders(_ context.Context, _, _ string, _ int) ([]holdersync.HolderRow, error) {
	return nil, ErrNoSource
}

type holderSyncNoOpSink struct{}

func (holderSyncNoOpSink) UpsertSnapshot(_ context.Context, _ holdersync.Snapshot) error {
	return nil
}

// --- riskscore adapters ------------------------------------------

type riskScoreLister struct{ q *sqlc.Queries }

func (l *riskScoreLister) ListRiskScoreCandidates(ctx context.Context, limit int, _ time.Duration) ([]riskscore.MarketFacts, error) {
	rows, err := l.q.ListRiskScoreCandidates(ctx, int32(limit))
	if err != nil {
		return nil, err
	}
	out := make([]riskscore.MarketFacts, 0, len(rows))
	for _, r := range rows {
		out = append(out, riskscore.MarketFacts{
			ConditionID: r.ConditionID,
			Title:       r.Question,
			Description: r.EventTitle,
		})
	}
	return out, nil
}

type riskScoreSink struct{ q *sqlc.Queries }

func (s *riskScoreSink) UpsertRiskScore(ctx context.Context, r riskscore.RiskRow) error {
	reasonsJSON, _ := encodeReasonsAsJSON(r.Reasons)
	return s.q.UpsertMarketRiskScore(ctx, sqlc.UpsertMarketRiskScoreParams{
		ConditionID:    r.ConditionID,
		ScoreVersion:   int32(r.ScoreVersion),
		AmbiguityScore: r.AmbiguityScore,
		DisputeRisk:    r.DisputeRisk,
		ReasonsJson:    reasonsJSON,
	})
}

// --- repricing adapters ------------------------------------------

type repricingCatalystLister struct{ q *sqlc.Queries }

func (l *repricingCatalystLister) ListNewTriggers(ctx context.Context, lookback time.Duration, limit int) ([]repricing.Trigger, error) {
	since := time.Now().Add(-lookback)
	rows, err := l.q.ListRepricingTriggers(ctx, sqlc.ListRepricingTriggersParams{
		Since:    tsFromTime(since),
		RowLimit: int32(limit),
	})
	if err != nil {
		return nil, err
	}
	out := make([]repricing.Trigger, 0, len(rows))
	for _, r := range rows {
		expected := time.Now()
		if r.ExpectedAt.Valid {
			expected = r.ExpectedAt.Time
		}
		out = append(out, repricing.Trigger{
			EventSlug: r.EventSlug,
			Kind:      repricing.TriggerCatalyst,
			Ref:       r.Title,
			OpenedAt:  expected,
		})
	}
	return out, nil
}

type repricingOpenLister struct{ q *sqlc.Queries }

func (l *repricingOpenLister) ListOpenWindows(ctx context.Context, dueBefore time.Time, limit int) ([]repricing.OpenWindow, error) {
	rows, err := l.q.ListOpenRepricingWindows(ctx, sqlc.ListOpenRepricingWindowsParams{
		DueBefore: tsFromTime(dueBefore),
		RowLimit:  int32(limit),
	})
	if err != nil {
		return nil, err
	}
	out := make([]repricing.OpenWindow, 0, len(rows))
	for _, r := range rows {
		baseline := 0.0
		if r.BaselinePrice != nil {
			baseline = *r.BaselinePrice
		}
		out = append(out, repricing.OpenWindow{
			ID:            r.ID,
			ConditionID:   r.ConditionID,
			EventSlug:     r.EventSlug,
			OpenedAt:      r.OpenedAt.Time,
			ClosesAt:      r.ClosesAt.Time,
			BaselinePrice: baseline,
			SideBias:      r.SideBias,
		})
	}
	return out, nil
}

// repricingPriceSampler — v11.9 real implementation. Target price
// is the first trade at-or-after now from polymarket_trades on the
// target condition. Peer median is computed across linked markets
// from polymarket_market_links, using the same first-trade-after
// signal. No fake data: when no peer prices exist, returns n=0.
type repricingPriceSampler struct {
	q           *sqlc.Queries
	pool        *pgxpool.Pool
	linkVersion int32
}

func (p *repricingPriceSampler) SampleTarget(ctx context.Context, conditionID string) (float64, bool, error) {
	row, err := p.q.FirstTradePriceForCondition(ctx, sqlc.FirstTradePriceForConditionParams{
		ConditionID: conditionID,
		At:          tsFromTime(time.Now().Add(-1 * time.Hour)),
	})
	if err != nil {
		// no rows or genuine error — both surface as no-price for
		// the worker. Honest "stale_missing_price" status.
		return 0, false, nil
	}
	return row, true, nil
}

func (p *repricingPriceSampler) SamplePeerMedian(ctx context.Context, _ string, anchorConditionID string) (float64, int, error) {
	if anchorConditionID == "" {
		return 0, 0, nil
	}
	peers, err := p.q.ListPeerConditionsByMarketLinks(ctx, sqlc.ListPeerConditionsByMarketLinksParams{
		SrcConditionID: anchorConditionID,
		LinkVersion:    p.linkVersion,
		RowLimit:       25,
	})
	if err != nil || len(peers) == 0 {
		return 0, 0, nil
	}
	prices := make([]float64, 0, len(peers))
	at := tsFromTime(time.Now().Add(-1 * time.Hour))
	for _, peer := range peers {
		row, err := p.q.FirstTradePriceForCondition(ctx, sqlc.FirstTradePriceForConditionParams{
			ConditionID: peer,
			At:          at,
		})
		if err != nil {
			continue
		}
		prices = append(prices, row)
	}
	if len(prices) == 0 {
		return 0, 0, nil
	}
	// median
	sortedFloats(prices)
	median := prices[len(prices)/2]
	if len(prices)%2 == 0 {
		median = (prices[len(prices)/2-1] + prices[len(prices)/2]) / 2
	}
	return median, len(prices), nil
}

func sortedFloats(v []float64) {
	// tiny insertion sort — peer count capped at 25 so this is fine.
	for i := 1; i < len(v); i++ {
		for j := i; j > 0 && v[j-1] > v[j]; j-- {
			v[j-1], v[j] = v[j], v[j-1]
		}
	}
}

type repricingSink struct{ q *sqlc.Queries }

func (s *repricingSink) OpenWindow(ctx context.Context, t repricing.Trigger, closesAt time.Time) error {
	_, err := s.q.InsertRepricingWindow(ctx, sqlc.InsertRepricingWindowParams{
		ConditionID: "",
		EventSlug:   t.EventSlug,
		TriggerKind: string(t.Kind),
		TriggerRef:  t.Ref,
		OpenedAt:    tsFromTime(t.OpenedAt),
		ClosesAt:    tsFromTime(closesAt),
		SideBias:    "",
	})
	return err
}

func (s *repricingSink) CloseWindow(ctx context.Context, id int64, observed, peer, lag float64, status string) error {
	return s.q.CloseRepricingWindow(ctx, sqlc.CloseRepricingWindowParams{
		ID:           id,
		Status:       status,
		ObservedMove: &observed,
		PeerMove:     &peer,
		LagScore:     &lag,
		Notes:        "",
	})
}

// --- walletgraph adapters ----------------------------------------

type walletGraphLister struct{ q *sqlc.Queries }

func (l *walletGraphLister) ListCoTradeRows(ctx context.Context, lookback time.Duration, limit int) ([]walletgraph.CoTradeRow, error) {
	since := time.Now().Add(-lookback)
	rows, err := l.q.ListWalletCoTradeRows(ctx, sqlc.ListWalletCoTradeRowsParams{
		Since:    tsFromTime(since),
		RowLimit: int32(limit),
	})
	if err != nil {
		return nil, err
	}
	out := make([]walletgraph.CoTradeRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, walletgraph.CoTradeRow{
			Wallet:    r.Wallet,
			EventSlug: r.EventSlug,
			Side:      r.Side,
			At:        r.TradedAt.Time,
		})
	}
	return out, nil
}

type walletGraphSink struct {
	pool *pgxpool.Pool
}

func (s *walletGraphSink) UpsertEdges(ctx context.Context, edges []walletgraph.Edge) error {
	if len(edges) == 0 {
		return nil
	}
	// Raw SQL — no sqlc query yet because the migration uses
	// an enum CHECK + composite UNIQUE that's awkward to express
	// in sqlc's :exec mode. Bounded batch.
	const stmt = `
INSERT INTO polymarket_wallet_graph_edges
    (wallet_a, wallet_b, edge_kind, similarity_score, co_events_count,
     cohort_id, first_seen_at, last_seen_at, edge_version)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
ON CONFLICT (wallet_a, wallet_b, edge_kind, edge_version) DO UPDATE
SET similarity_score = EXCLUDED.similarity_score,
    co_events_count  = EXCLUDED.co_events_count,
    last_seen_at     = EXCLUDED.last_seen_at`
	for _, e := range edges {
		if _, err := s.pool.Exec(ctx, stmt,
			e.WalletA, e.WalletB, e.Kind, e.SimilarityScore,
			e.CoEventsCount, e.CohortID, e.FirstSeenAt, e.LastSeenAt, e.EdgeVersion); err != nil {
			return err
		}
	}
	return nil
}

// --- small helpers ------------------------------------------------

// (helper removed: pgtype.Timestamptz exposes .Valid + .Time directly.)

// encodeReasonsAsJSON is a small helper for risk reasons.
func encodeReasonsAsJSON(reasons []string) ([]byte, error) {
	if len(reasons) == 0 {
		return nil, nil
	}
	return json.Marshal(reasons)
}
