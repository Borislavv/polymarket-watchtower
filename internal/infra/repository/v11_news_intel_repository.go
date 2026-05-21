// v11_news_intel_repository.go — v11.0 Hourly News Intelligence
// persistence (PART 6). Wraps polymarket_news_intel_{runs,decisions,
// processed_items}. Nothing above this layer imports sqlc.
package repository

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Borislavv/polymarket-watchtower/internal/infra/postgres/sqlc"
)

// NewsIntelRunInsert is the at-start payload the worker hands the
// repository when opening a new hourly cycle. NewsItemsCount /
// SelectedCount are best-effort estimates at this point — the row is
// rewritten by Finish once the cycle resolves.
type NewsIntelRunInsert struct {
	LookbackStart     time.Time
	LookbackEnd       time.Time
	NewsItemsCount    int
	SelectedCount     int
	AICalled          bool
	AIStatus          string
	SentinelCode      string
	AICostUSD         float64
	InputFingerprint  string
	OutputFingerprint string
	TelegramSent      bool
}

// NewsIntelRunFinish is the at-end payload the worker hands the
// repository to close the row.
type NewsIntelRunFinish struct {
	ID                int64
	Status            string // ok | skipped | failed
	NewsItemsCount    int
	SelectedCount     int
	AICalled          bool
	AIStatus          string
	SentinelCode      string
	AICostUSD         float64
	OutputFingerprint string
	TelegramSent      bool
	LastError         string
}

// NewsIntelDecision is one row in polymarket_news_intel_decisions —
// the per-(news_item, market) AI output the worker persists from the
// returned JSON.
type NewsIntelDecision struct {
	RunID                  int64
	NewsItemHash           string
	EventSlug              string
	ConditionID            string
	MarketTitle            string
	Rank                   int
	Decision               string
	Confidence             float64
	ImpactDirection        string
	ExpectedPriceImpactMin *float64
	ExpectedPriceImpactMax *float64
	ExpectedWindow         string
	WhyItMatters           string
	WhatMarketMayMiss      string
	TriggerCondition       string
	InvalidatesIf          string
	TradeStance            string
	TelegramWorthy         bool
	AffectedMarkets        any // JSON-serialisable list of (event_slug, condition_id, market_title)
}

// NewsIntelProcessedItem is the dedupe-ledger row shape.
type NewsIntelProcessedItem struct {
	ItemHash     string
	EventSlug    string
	Title        string
	FirstSeenAt  time.Time
	LastSeenAt   time.Time
	ProcessedAt  time.Time
	LastRunID    int64 // 0 when NULL
}

// NewsIntelRepository wraps the three v11.0 tables.
type NewsIntelRepository struct {
	q *sqlc.Queries
}

func NewNewsIntelRepository(pool *pgxpool.Pool) *NewsIntelRepository {
	return &NewsIntelRepository{q: sqlc.New(pool)}
}

// InsertRun opens a cycle row and returns its id. Status is hard-coded
// to 'started' by the underlying query.
func (r *NewsIntelRepository) InsertRun(ctx context.Context, in NewsIntelRunInsert) (int64, error) {
	return r.q.InsertNewsIntelRun(ctx, sqlc.InsertNewsIntelRunParams{
		LookbackStart:     tsFromTime(in.LookbackStart),
		LookbackEnd:       tsFromTime(in.LookbackEnd),
		NewsItemsCount:    int32(in.NewsItemsCount),
		SelectedCount:     int32(in.SelectedCount),
		AiCalled:          in.AICalled,
		AiStatus:          in.AIStatus,
		SentinelCode:      in.SentinelCode,
		AiCostUsd:         in.AICostUSD,
		InputFingerprint:  in.InputFingerprint,
		OutputFingerprint: in.OutputFingerprint,
		TelegramSent:      in.TelegramSent,
	})
}

// FinishRun closes a cycle row with its terminal status + counters.
func (r *NewsIntelRepository) FinishRun(ctx context.Context, in NewsIntelRunFinish) error {
	return r.q.FinishNewsIntelRun(ctx, sqlc.FinishNewsIntelRunParams{
		ID:                in.ID,
		Status:            in.Status,
		NewsItemsCount:    int32(in.NewsItemsCount),
		SelectedCount:     int32(in.SelectedCount),
		AiCalled:          in.AICalled,
		AiStatus:          in.AIStatus,
		SentinelCode:      in.SentinelCode,
		AiCostUsd:         in.AICostUSD,
		OutputFingerprint: in.OutputFingerprint,
		TelegramSent:      in.TelegramSent,
		LastError:         in.LastError,
	})
}

// InsertDecision persists one selected (news_item, market) decision.
// AffectedMarkets is JSON-marshalled at the boundary; nil → NULL.
func (r *NewsIntelRepository) InsertDecision(ctx context.Context, d NewsIntelDecision) error {
	var raw []byte
	if d.AffectedMarkets != nil {
		b, err := json.Marshal(d.AffectedMarkets)
		if err != nil {
			return err
		}
		raw = b
	}
	return r.q.InsertNewsIntelDecision(ctx, sqlc.InsertNewsIntelDecisionParams{
		RunID:                  d.RunID,
		NewsItemHash:           d.NewsItemHash,
		EventSlug:              d.EventSlug,
		ConditionID:            d.ConditionID,
		MarketTitle:            d.MarketTitle,
		Rank:                   int32(d.Rank),
		Decision:               d.Decision,
		Confidence:             d.Confidence,
		ImpactDirection:        d.ImpactDirection,
		ExpectedPriceImpactMin: d.ExpectedPriceImpactMin,
		ExpectedPriceImpactMax: d.ExpectedPriceImpactMax,
		ExpectedWindow:         d.ExpectedWindow,
		WhyItMatters:           d.WhyItMatters,
		WhatMarketMayMiss:      d.WhatMarketMayMiss,
		TriggerCondition:       d.TriggerCondition,
		InvalidatesIf:          d.InvalidatesIf,
		TradeStance:            d.TradeStance,
		TelegramWorthy:         d.TelegramWorthy,
		AffectedMarketsJson:    raw,
	})
}

// FilterUnprocessed returns the subset of item_hashes that are NOT
// already present in polymarket_news_intel_processed_items. Empty
// input → empty output. Errors are propagated; a worker failure here
// must not silently re-process every item.
func (r *NewsIntelRepository) FilterUnprocessed(ctx context.Context, itemHashes []string) ([]string, error) {
	if len(itemHashes) == 0 {
		return nil, nil
	}
	present, err := r.q.ListNewsIntelProcessedHashes(ctx, itemHashes)
	if err != nil {
		return nil, err
	}
	if len(present) == 0 {
		out := make([]string, len(itemHashes))
		copy(out, itemHashes)
		return out, nil
	}
	presentSet := make(map[string]struct{}, len(present))
	for _, h := range present {
		presentSet[h] = struct{}{}
	}
	out := make([]string, 0, len(itemHashes))
	for _, h := range itemHashes {
		if _, ok := presentSet[h]; ok {
			continue
		}
		out = append(out, h)
	}
	return out, nil
}

// GetProcessedItem returns a processed-item row. (zero, false, nil) on
// missing, (row, true, nil) on hit, (zero, false, err) on real errors.
func (r *NewsIntelRepository) GetProcessedItem(ctx context.Context, itemHash string) (NewsIntelProcessedItem, bool, error) {
	row, err := r.q.GetNewsIntelProcessedItem(ctx, itemHash)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return NewsIntelProcessedItem{}, false, nil
		}
		return NewsIntelProcessedItem{}, false, err
	}
	out := NewsIntelProcessedItem{
		ItemHash:    row.ItemHash,
		EventSlug:   row.EventSlug,
		Title:       row.Title,
		FirstSeenAt: row.FirstSeenAt.Time,
		LastSeenAt:  row.LastSeenAt.Time,
		ProcessedAt: row.ProcessedAt.Time,
	}
	if row.LastRunID != nil {
		out.LastRunID = *row.LastRunID
	}
	return out, true, nil
}

// MarkProcessed stamps the item as consumed by the given run. runID=0
// is persisted as NULL — useful for tests / dry-runs.
func (r *NewsIntelRepository) MarkProcessed(ctx context.Context, itemHash, eventSlug, title string, runID int64) error {
	var rid *int64
	if runID > 0 {
		v := runID
		rid = &v
	}
	return r.q.UpsertNewsIntelProcessedItem(ctx, sqlc.UpsertNewsIntelProcessedItemParams{
		ItemHash:  itemHash,
		EventSlug: eventSlug,
		Title:     title,
		LastRunID: rid,
	})
}

// TouchProcessed bumps last_seen_at without changing anything else.
// Used when an item re-appears in the candidate pool but was already
// consumed in a prior cycle.
func (r *NewsIntelRepository) TouchProcessed(ctx context.Context, itemHash string) error {
	return r.q.TouchNewsIntelProcessedItem(ctx, itemHash)
}
