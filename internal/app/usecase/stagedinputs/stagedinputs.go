// Package stagedinputs is the v11.8 hot-path bridge that turns
// background-worker output (market_links, risk_scores, repricing_windows,
// wallet_graph_edges, event_catalysts, recent shadow_decisions) into
// bounded, cacheable reads detect.Loop can fan out across all 9
// strategies on every alert.
//
// Architecture:
//   - Each strategy has ONE narrow reader (e.g. CatalystsReader).
//   - Every reader query is bounded by a row limit + a context
//     timeout enforced at the call site.
//   - Optional in-memory TTL cache deduplicates repeated reads
//     within the same alerting cycle.
//
// All readers fail open: a Postgres error returns (zero, false, err)
// and the caller is expected to count it as a skip-with-reason
// rather than blocking the existing alert pipeline.
package stagedinputs

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Borislavv/polymarket-watchtower/internal/infra/postgres/sqlc"
)

// Config tunes the bridge.
type Config struct {
	Enabled      bool
	CacheEnabled bool
	CacheTTL     time.Duration
	MaxRows      int
	QueryTimeout time.Duration
}

// Readers is the bundle exposed to detect.Loop.
type Readers struct {
	cfg   Config
	q     *sqlc.Queries
	cache *cache
}

// New constructs the readers bundle.
func New(pool *pgxpool.Pool, cfg Config) *Readers {
	if pool == nil {
		return nil
	}
	if cfg.MaxRows <= 0 {
		cfg.MaxRows = 200
	}
	if cfg.QueryTimeout <= 0 {
		cfg.QueryTimeout = 250 * time.Millisecond
	}
	if cfg.CacheTTL <= 0 {
		cfg.CacheTTL = 60 * time.Second
	}
	r := &Readers{
		cfg: cfg,
		q:   sqlc.New(pool),
	}
	if cfg.CacheEnabled {
		r.cache = newCache(cfg.CacheTTL)
	}
	return r
}

// Enabled reports whether the bundle is wired and active.
func (r *Readers) Enabled() bool { return r != nil && r.cfg.Enabled }

// withTimeout wraps the parent ctx in a query timeout. Always
// derive from caller's ctx so cancellation propagates.
func (r *Readers) withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, r.cfg.QueryTimeout)
}

// --- types --------------------------------------------------------

type MarketLink struct {
	SrcConditionID string
	DstConditionID string
	LinkType       string
	Direction      string
	Confidence     float64
}

type Catalyst struct {
	EventSlug    string
	CatalystKind string
	Title        string
	ExpectedAt   time.Time
	Confidence   float64
	Status       string
}

type RiskScore struct {
	ConditionID    string
	AmbiguityScore float64
	DisputeRisk    float64
}

type WalletEdge struct {
	Other           string
	Kind            string
	SimilarityScore float64
	CoEvents        int
	LastSeenAt      time.Time
}

type RepricingWindow struct {
	ID           int64
	ConditionID  string
	EventSlug    string
	TriggerKind  string
	TriggerRef   string
	OpenedAt     time.Time
	Status       string
	ObservedMove float64
	PeerMove     float64
	LagScore     float64
}

type RecentDecision struct {
	ID           int64
	StrategyName string
	Side         string
	DecisionKind string
	Level        string
	Score        float64
	Confidence   float64
	FiredAt      time.Time
}

// WalletThesisLine is one (wallet, condition, side) directional
// exposure aggregate from polymarket_wallet_thesis_lines, used by
// the v11.10 thesisaccum consumer-flip.
type WalletThesisLine struct {
	Wallet       string
	ConditionID  string
	EventSlug    string
	Side         string
	NotionalUSD  float64
	Trades       int
	LastTradedAt time.Time
}

// HolderSnapshot mirrors holderdelta.Snapshot but lives in
// stagedinputs so the reader and the detector remain decoupled. The
// v11.12-insider-prior bridge converts these into the pure detector
// input at call time.
type HolderSnapshot struct {
	SnapshotAt  time.Time
	Wallet      string
	Rank        int
	Shares      float64
	NotionalUSD float64
	PctOI       float64
	TotalOI     float64
}

// HolderSnapshotPair is the (current, previous) tuple holderdelta
// consumes. PreviousValid=false when only one snapshot exists.
type HolderSnapshotPair struct {
	Current       HolderSnapshot
	Previous      HolderSnapshot
	PreviousValid bool
}

// BookFeatureBar mirrors bookvacuum.FeatureBar but isolates the
// reader. Pointer fields on the SQL side map to zero+valid pairs
// here so the detector never sees a NaN.
type BookFeatureBar struct {
	BarStart         time.Time
	BarSeconds       int
	BestBid          float64
	BestAsk          float64
	MidPrice         float64
	BidDepthTopN     float64
	AskDepthTopN     float64
	Spread           float64
	SpreadZ          float64
	BidDepthDeltaPct float64
	AskDepthDeltaPct float64
	MidDelta         float64
	BidDepthValid    bool
	AskDepthValid    bool
}

// --- readers ------------------------------------------------------

// MarketLinksByEvent returns same-event edges anchored on the
// canonical source condition_id. Cached per (event_slug, version).
func (r *Readers) MarketLinksByEvent(ctx context.Context, eventSlug string, linkVersion int) ([]MarketLink, error) {
	if r == nil || eventSlug == "" {
		return nil, nil
	}
	key := "ml:" + eventSlug
	if v, ok := r.cacheGet(key); ok {
		return v.([]MarketLink), nil
	}
	qctx, cancel := r.withTimeout(ctx)
	defer cancel()
	rows, err := r.q.ListMarketLinksForEvent(qctx, sqlc.ListMarketLinksForEventParams{
		EventSlug:   eventSlug,
		LinkVersion: int32(linkVersion),
		RowLimit:    int32(r.cfg.MaxRows),
	})
	if err != nil {
		return nil, err
	}
	out := make([]MarketLink, 0, len(rows))
	for _, x := range rows {
		out = append(out, MarketLink{
			SrcConditionID: x.SrcConditionID,
			DstConditionID: x.DstConditionID,
			LinkType:       x.LinkType,
			Direction:      x.Direction,
			Confidence:     x.Confidence,
		})
	}
	r.cachePut(key, out)
	return out, nil
}

// CatalystsByEvent returns active/expected catalysts for an event.
func (r *Readers) CatalystsByEvent(ctx context.Context, eventSlug string) ([]Catalyst, error) {
	if r == nil || eventSlug == "" {
		return nil, nil
	}
	key := "c:" + eventSlug
	if v, ok := r.cacheGet(key); ok {
		return v.([]Catalyst), nil
	}
	qctx, cancel := r.withTimeout(ctx)
	defer cancel()
	rows, err := r.q.ListCatalystsForEvent(qctx, sqlc.ListCatalystsForEventParams{
		EventSlug: eventSlug,
		RowLimit:  int32(r.cfg.MaxRows),
	})
	if err != nil {
		return nil, err
	}
	out := make([]Catalyst, 0, len(rows))
	for _, x := range rows {
		out = append(out, Catalyst{
			EventSlug:    x.EventSlug,
			CatalystKind: x.CatalystType,
			Title:        x.Title,
			ExpectedAt:   tsTime(x.ExpectedAt),
			Confidence:   x.Confidence,
			Status:       x.Status,
		})
	}
	r.cachePut(key, out)
	return out, nil
}

// RiskScoreForCondition returns the active risk row for a market
// or (RiskScore{}, false, nil) when none exists.
func (r *Readers) RiskScoreForCondition(ctx context.Context, conditionID string) (RiskScore, bool, error) {
	if r == nil || conditionID == "" {
		return RiskScore{}, false, nil
	}
	key := "rs:" + conditionID
	if v, ok := r.cacheGet(key); ok {
		if v == nil {
			return RiskScore{}, false, nil
		}
		return v.(RiskScore), true, nil
	}
	qctx, cancel := r.withTimeout(ctx)
	defer cancel()
	row, err := r.q.GetActiveRiskScoreForCondition(qctx, conditionID)
	if err != nil {
		if isNoRows(err) {
			r.cachePut(key, nil)
			return RiskScore{}, false, nil
		}
		return RiskScore{}, false, err
	}
	out := RiskScore{
		ConditionID:    row.ConditionID,
		AmbiguityScore: row.AmbiguityScore,
		DisputeRisk:    row.DisputeRisk,
	}
	r.cachePut(key, out)
	return out, true, nil
}

// WalletEdgesForWallet returns the cohort edges for a wallet.
func (r *Readers) WalletEdgesForWallet(ctx context.Context, wallet string, edgeVersion int) ([]WalletEdge, error) {
	if r == nil || wallet == "" {
		return nil, nil
	}
	key := "we:" + wallet
	if v, ok := r.cacheGet(key); ok {
		return v.([]WalletEdge), nil
	}
	qctx, cancel := r.withTimeout(ctx)
	defer cancel()
	rows, err := r.q.ListWalletEdgesForWallet(qctx, sqlc.ListWalletEdgesForWalletParams{
		Wallet:      wallet,
		EdgeVersion: int32(edgeVersion),
		RowLimit:    int32(r.cfg.MaxRows),
	})
	if err != nil {
		return nil, err
	}
	out := make([]WalletEdge, 0, len(rows))
	for _, x := range rows {
		other := x.WalletA
		if other == wallet {
			other = x.WalletB
		}
		out = append(out, WalletEdge{
			Other:           other,
			Kind:            x.EdgeKind,
			SimilarityScore: x.SimilarityScore,
			CoEvents:        int(x.CoEventsCount),
			LastSeenAt:      x.LastSeenAt.Time,
		})
	}
	r.cachePut(key, out)
	return out, nil
}

// ClosedRepricingWindowsForCondition returns closed (lag/no-lag) windows.
func (r *Readers) ClosedRepricingWindowsForCondition(ctx context.Context, conditionID string, since time.Time) ([]RepricingWindow, error) {
	if r == nil || conditionID == "" {
		return nil, nil
	}
	key := "rw:" + conditionID
	if v, ok := r.cacheGet(key); ok {
		return v.([]RepricingWindow), nil
	}
	qctx, cancel := r.withTimeout(ctx)
	defer cancel()
	rows, err := r.q.ListClosedRepricingWindowsForCondition(qctx, sqlc.ListClosedRepricingWindowsForConditionParams{
		ConditionID: conditionID,
		Since:       tsFromTime(since),
		RowLimit:    int32(r.cfg.MaxRows),
	})
	if err != nil {
		return nil, err
	}
	out := make([]RepricingWindow, 0, len(rows))
	for _, x := range rows {
		w := RepricingWindow{
			ID:          x.ID,
			ConditionID: x.ConditionID,
			EventSlug:   x.EventSlug,
			TriggerKind: x.TriggerKind,
			TriggerRef:  x.TriggerRef,
			OpenedAt:    x.OpenedAt.Time,
			Status:      x.Status,
		}
		if x.ObservedMove != nil {
			w.ObservedMove = *x.ObservedMove
		}
		if x.PeerMove != nil {
			w.PeerMove = *x.PeerMove
		}
		if x.LagScore != nil {
			w.LagScore = *x.LagScore
		}
		out = append(out, w)
	}
	r.cachePut(key, out)
	return out, nil
}

// WalletThesisLinesForEvent returns per-wallet directional exposure
// across an event's linked markets, materialised by the v11.9
// thesislines worker. Hot-path-safe: bounded by row limit + a
// per-query timeout.
func (r *Readers) WalletThesisLinesForEvent(ctx context.Context, eventSlug, wallet string, lookbackHours int) ([]WalletThesisLine, error) {
	if r == nil || eventSlug == "" || wallet == "" {
		return nil, nil
	}
	key := "wtl:" + eventSlug + "|" + wallet
	if v, ok := r.cacheGet(key); ok {
		return v.([]WalletThesisLine), nil
	}
	qctx, cancel := r.withTimeout(ctx)
	defer cancel()
	rows, err := r.q.ListWalletThesisLinesForEvent(qctx, sqlc.ListWalletThesisLinesForEventParams{
		EventSlug:     eventSlug,
		Wallet:        wallet,
		LookbackHours: int32(lookbackHours),
		RowLimit:      int32(r.cfg.MaxRows),
	})
	if err != nil {
		return nil, err
	}
	out := make([]WalletThesisLine, 0, len(rows))
	for _, x := range rows {
		out = append(out, WalletThesisLine{
			Wallet:       x.Wallet,
			ConditionID:  x.ConditionID,
			EventSlug:    x.EventSlug,
			Side:         x.Side,
			NotionalUSD:  x.NotionalUsd,
			Trades:       int(x.Trades),
			LastTradedAt: tsTime(x.LastTradedAt),
		})
	}
	r.cachePut(key, out)
	return out, nil
}

// RecentDecisionsForCondition returns shadow rows that already
// landed on the same market in the conflict window.
func (r *Readers) RecentDecisionsForCondition(ctx context.Context, conditionID string, since time.Time) ([]RecentDecision, error) {
	if r == nil || conditionID == "" {
		return nil, nil
	}
	key := "rd:" + conditionID
	if v, ok := r.cacheGet(key); ok {
		return v.([]RecentDecision), nil
	}
	qctx, cancel := r.withTimeout(ctx)
	defer cancel()
	rows, err := r.q.ListRecentShadowDecisionsForCondition(qctx, sqlc.ListRecentShadowDecisionsForConditionParams{
		ConditionID: conditionID,
		Since:       tsFromTime(since),
		RowLimit:    int32(r.cfg.MaxRows),
	})
	if err != nil {
		return nil, err
	}
	out := make([]RecentDecision, 0, len(rows))
	for _, x := range rows {
		out = append(out, RecentDecision{
			ID:           x.ID,
			StrategyName: x.StrategyName,
			Side:         x.Side,
			DecisionKind: x.DecisionKind,
			Level:        x.DecisionLevel,
			Score:        x.Score,
			Confidence:   x.Confidence,
			FiredAt:      x.FiredAt.Time,
		})
	}
	r.cachePut(key, out)
	return out, nil
}

// HolderSnapshotPairForWallet returns the (current, previous)
// snapshot pair for a single (condition_id, outcome_token, wallet).
// Returns (zero, false, nil) when the table has no rows for the
// wallet (the typical case before holdersync materialises one);
// returns (current-only, true-but-PreviousValid=false, nil) when a
// single row exists. Cached per key for the staged TTL.
//
// v11.12-insider-prior load-bearing path for holderdelta.
func (r *Readers) HolderSnapshotPairForWallet(ctx context.Context, conditionID, outcomeToken, wallet string) (HolderSnapshotPair, bool, error) {
	if r == nil || conditionID == "" || wallet == "" {
		return HolderSnapshotPair{}, false, nil
	}
	key := "hsp:" + conditionID + "|" + outcomeToken + "|" + wallet
	if v, ok := r.cacheGet(key); ok {
		if v == nil {
			return HolderSnapshotPair{}, false, nil
		}
		p := v.(HolderSnapshotPair)
		return p, true, nil
	}
	qctx, cancel := r.withTimeout(ctx)
	defer cancel()
	rows, err := r.q.GetCurrentAndPreviousHolderSnapshot(qctx, sqlc.GetCurrentAndPreviousHolderSnapshotParams{
		ConditionID:  conditionID,
		OutcomeToken: outcomeToken,
		Wallet:       wallet,
	})
	if err != nil {
		return HolderSnapshotPair{}, false, err
	}
	if len(rows) == 0 {
		r.cachePut(key, nil)
		return HolderSnapshotPair{}, false, nil
	}
	pair := HolderSnapshotPair{
		Current: HolderSnapshot{
			SnapshotAt:  tsTime(rows[0].SnapshotAt),
			Wallet:      rows[0].Wallet,
			Rank:        int(rows[0].Rank),
			Shares:      rows[0].Shares,
			NotionalUSD: rows[0].NotionalUsd,
			PctOI:       rows[0].PctOi,
			TotalOI:     rows[0].TotalOi,
		},
	}
	if len(rows) >= 2 {
		pair.Previous = HolderSnapshot{
			SnapshotAt:  tsTime(rows[1].SnapshotAt),
			Wallet:      rows[1].Wallet,
			Rank:        int(rows[1].Rank),
			Shares:      rows[1].Shares,
			NotionalUSD: rows[1].NotionalUsd,
			PctOI:       rows[1].PctOi,
			TotalOI:     rows[1].TotalOi,
		}
		pair.PreviousValid = true
	}
	r.cachePut(key, pair)
	return pair, true, nil
}

// RecentBookFeatureBars returns the most-recent bars for a token,
// freshest-first. The caller treats index 0 as the "Recent" bar
// for bookvacuum and computes a rolling baseline from index 1..N.
// Returns at most r.cfg.MaxRows rows. Empty result is NOT an error
// — production tokens often have no producer wired yet.
//
// `since` bounds the lookback to a known horizon (typically 5–15
// minutes for vacuum freshness gates). The caller is responsible
// for staleness gating via the latest bar's BarStart.
func (r *Readers) RecentBookFeatureBars(ctx context.Context, conditionID, outcomeToken string, since time.Time, rowLimit int) ([]BookFeatureBar, error) {
	if r == nil || conditionID == "" || outcomeToken == "" {
		return nil, nil
	}
	if rowLimit <= 0 || rowLimit > r.cfg.MaxRows {
		rowLimit = r.cfg.MaxRows
	}
	key := "bfb:" + conditionID + "|" + outcomeToken
	if v, ok := r.cacheGet(key); ok {
		return v.([]BookFeatureBar), nil
	}
	qctx, cancel := r.withTimeout(ctx)
	defer cancel()
	rows, err := r.q.ListRecentBookFeatureBars(qctx, sqlc.ListRecentBookFeatureBarsParams{
		ConditionID:  conditionID,
		OutcomeToken: outcomeToken,
		Since:        tsFromTime(since),
		RowLimit:     int32(rowLimit),
	})
	if err != nil {
		return nil, err
	}
	out := make([]BookFeatureBar, 0, len(rows))
	for _, x := range rows {
		bar := BookFeatureBar{
			BarStart:   tsTime(x.BarStart),
			BarSeconds: int(x.BarSeconds),
		}
		if x.BestBid != nil {
			bar.BestBid = *x.BestBid
		}
		if x.BestAsk != nil {
			bar.BestAsk = *x.BestAsk
		}
		if x.MidPrice != nil {
			bar.MidPrice = *x.MidPrice
		}
		if x.BidDepthTopN != nil {
			bar.BidDepthTopN = *x.BidDepthTopN
			bar.BidDepthValid = true
		}
		if x.AskDepthTopN != nil {
			bar.AskDepthTopN = *x.AskDepthTopN
			bar.AskDepthValid = true
		}
		if x.Spread != nil {
			bar.Spread = *x.Spread
		}
		if x.SpreadZ != nil {
			bar.SpreadZ = *x.SpreadZ
		}
		if x.BidDepthDeltaPct != nil {
			bar.BidDepthDeltaPct = *x.BidDepthDeltaPct
		}
		if x.AskDepthDeltaPct != nil {
			bar.AskDepthDeltaPct = *x.AskDepthDeltaPct
		}
		if x.MidDelta != nil {
			bar.MidDelta = *x.MidDelta
		}
		out = append(out, bar)
	}
	r.cachePut(key, out)
	return out, nil
}

// --- cache --------------------------------------------------------

type cache struct {
	mu   sync.RWMutex
	ttl  time.Duration
	rows map[string]cacheEntry
}

type cacheEntry struct {
	value     any
	expiresAt time.Time
}

func newCache(ttl time.Duration) *cache {
	return &cache{ttl: ttl, rows: map[string]cacheEntry{}}
}

func (r *Readers) cacheGet(key string) (any, bool) {
	if r.cache == nil {
		return nil, false
	}
	r.cache.mu.RLock()
	defer r.cache.mu.RUnlock()
	e, ok := r.cache.rows[key]
	if !ok {
		return nil, false
	}
	if time.Now().After(e.expiresAt) {
		return nil, false
	}
	return e.value, true
}

func (r *Readers) cachePut(key string, value any) {
	if r.cache == nil {
		return
	}
	r.cache.mu.Lock()
	defer r.cache.mu.Unlock()
	r.cache.rows[key] = cacheEntry{value: value, expiresAt: time.Now().Add(r.cache.ttl)}
}

// --- small helpers ------------------------------------------------

// tsTime extracts time.Time from pgtype.Timestamptz, returning the
// zero time when invalid.
func tsTime(ts pgtype.Timestamptz) time.Time {
	if !ts.Valid {
		return time.Time{}
	}
	return ts.Time
}

// tsFromTime is the inverse helper, mirroring the pattern used by
// internal/infra/repository.
func tsFromTime(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t, Valid: !t.IsZero()}
}

func isNoRows(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "no rows in result set")
}
