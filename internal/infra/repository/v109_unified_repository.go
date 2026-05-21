// v109_unified_repository.go — repositories for the v10.9 unified
// intelligence engine. Five small wrappers around the sqlc-generated
// helpers; one file because the lifecycles are coupled.
package repository

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Borislavv/polymarket-watchtower/internal/infra/postgres/sqlc"
)

// MarketAICache wraps polymarket_market_ai_cache.
type MarketAICache struct {
	q *sqlc.Queries
}

func NewMarketAICache(pool *pgxpool.Pool) *MarketAICache {
	return &MarketAICache{q: sqlc.New(pool)}
}

// MarketAICacheEntry is the read shape returned by Get.
type MarketAICacheEntry struct {
	ID                  int64
	EventSlug           string
	ConditionID         string
	AISurface           string
	MarketAIKey         string
	NewsFingerprint     string
	CatalystFingerprint string
	RepricingBucket     string
	FlowBucket          string
	PriceBucket         string
	AIStatus            string
	SentinelCode        string
	DecisionJSON        []byte
	SummaryText         string
	LastAIAt            time.Time
	LastReusedAt        time.Time
	ReuseCount          int32
}

// MarketAICacheRow is the write shape consumed by Upsert.
type MarketAICacheRow struct {
	EventSlug           string
	ConditionID         string
	AISurface           string
	MarketAIKey         string
	NewsFingerprint     string
	CatalystFingerprint string
	RepricingBucket     string
	FlowBucket          string
	PriceBucket         string
	AIStatus            string
	SentinelCode        string
	DecisionJSON        []byte
	SummaryText         string
}

// Get returns the cached entry or (zero, false, nil) on miss.
func (r *MarketAICache) Get(ctx context.Context, surface, key string) (MarketAICacheEntry, bool, error) {
	row, err := r.q.GetMarketAICache(ctx, sqlc.GetMarketAICacheParams{
		AiSurface:   surface,
		MarketAiKey: key,
	})
	if err != nil {
		return MarketAICacheEntry{}, false, nil // fail-open
	}
	return MarketAICacheEntry{
		ID:                  row.ID,
		EventSlug:           row.EventSlug,
		ConditionID:         row.ConditionID,
		AISurface:           row.AiSurface,
		MarketAIKey:         row.MarketAiKey,
		NewsFingerprint:     row.NewsFingerprint,
		CatalystFingerprint: row.CatalystFingerprint,
		RepricingBucket:     row.RepricingBucket,
		FlowBucket:          row.FlowBucket,
		PriceBucket:         row.PriceBucket,
		AIStatus:            row.AiStatus,
		SentinelCode:        row.SentinelCode,
		DecisionJSON:        row.DecisionJson,
		SummaryText:         row.SummaryText,
		LastAIAt:            row.LastAiAt.Time,
		LastReusedAt:        tsTime(row.LastReusedAt),
		ReuseCount:          row.ReuseCount,
	}, true, nil
}

// Upsert writes a fresh AI result.
func (r *MarketAICache) Upsert(ctx context.Context, in MarketAICacheRow) error {
	return r.q.UpsertMarketAICache(ctx, sqlc.UpsertMarketAICacheParams{
		EventSlug:           in.EventSlug,
		ConditionID:         in.ConditionID,
		AiSurface:           in.AISurface,
		MarketAiKey:         in.MarketAIKey,
		NewsFingerprint:     in.NewsFingerprint,
		CatalystFingerprint: in.CatalystFingerprint,
		RepricingBucket:     in.RepricingBucket,
		FlowBucket:          in.FlowBucket,
		PriceBucket:         in.PriceBucket,
		AiStatus:            in.AIStatus,
		SentinelCode:        in.SentinelCode,
		DecisionJson:        in.DecisionJSON,
		SummaryText:         in.SummaryText,
	})
}

// TouchReuse stamps last_reused_at + bumps reuse_count.
func (r *MarketAICache) TouchReuse(ctx context.Context, surface, key string) error {
	return r.q.TouchMarketAICacheReuse(ctx, sqlc.TouchMarketAICacheReuseParams{
		AiSurface:   surface,
		MarketAiKey: key,
	})
}

// =========================================================================

// TelegramSemanticDedupe wraps polymarket_telegram_semantic_dedupe.
type TelegramSemanticDedupe struct {
	q *sqlc.Queries
}

func NewTelegramSemanticDedupe(pool *pgxpool.Pool) *TelegramSemanticDedupe {
	return &TelegramSemanticDedupe{q: sqlc.New(pool)}
}

// TelegramDedupeEntry is the read shape.
type TelegramDedupeEntry struct {
	ID                  int64
	Surface             string
	DedupeKey           string
	SemanticFingerprint string
	EventSlug           string
	ConditionID         string
	Wallet              string
	LastSentAt          time.Time
	SendCount           int32
	LastNotional        float64
	LastSeverity        string
	LastReason          string
}

// TelegramDedupeRow is the write shape.
type TelegramDedupeRow struct {
	Surface             string
	DedupeKey           string
	SemanticFingerprint string
	EventSlug           string
	ConditionID         string
	Wallet              string
	LastNotional        float64
	LastSeverity        string
	LastReason          string
}

// Get returns the dedupe row for `(surface, dedupe_key)` or
// (zero, false, nil) on miss.
func (r *TelegramSemanticDedupe) Get(ctx context.Context, surface, key string) (TelegramDedupeEntry, bool, error) {
	row, err := r.q.GetTelegramSemanticDedupe(ctx, sqlc.GetTelegramSemanticDedupeParams{
		Surface:   surface,
		DedupeKey: key,
	})
	if err != nil {
		return TelegramDedupeEntry{}, false, nil
	}
	out := TelegramDedupeEntry{
		ID:                  row.ID,
		Surface:             row.Surface,
		DedupeKey:           row.DedupeKey,
		SemanticFingerprint: row.SemanticFingerprint,
		EventSlug:           row.EventSlug,
		ConditionID:         row.ConditionID,
		Wallet:              row.Wallet,
		LastSentAt:          row.LastSentAt.Time,
		SendCount:           row.SendCount,
		LastSeverity:        row.LastSeverity,
		LastReason:          row.LastReason,
	}
	if row.LastNotional != nil {
		out.LastNotional = *row.LastNotional
	}
	return out, true, nil
}

// Upsert records a new send. Called AFTER the caller decides to ship.
func (r *TelegramSemanticDedupe) Upsert(ctx context.Context, in TelegramDedupeRow) error {
	var lastNotional *float64
	if in.LastNotional > 0 {
		v := in.LastNotional
		lastNotional = &v
	}
	return r.q.UpsertTelegramSemanticDedupe(ctx, sqlc.UpsertTelegramSemanticDedupeParams{
		Surface:             in.Surface,
		DedupeKey:           in.DedupeKey,
		SemanticFingerprint: in.SemanticFingerprint,
		EventSlug:           in.EventSlug,
		ConditionID:         in.ConditionID,
		Wallet:              in.Wallet,
		LastNotional:        lastNotional,
		LastSeverity:        in.LastSeverity,
		LastReason:          in.LastReason,
	})
}

// =========================================================================

// UnifiedIntelRuns wraps polymarket_unified_intel_runs +
// polymarket_unified_intel_decisions + polymarket_repricing_theses.
// One repo because the three tables share a run_id lifecycle.
type UnifiedIntelRuns struct {
	q *sqlc.Queries
}

func NewUnifiedIntelRuns(pool *pgxpool.Pool) *UnifiedIntelRuns {
	return &UnifiedIntelRuns{q: sqlc.New(pool)}
}

// NewUnifiedIntelRun is the input for Insert.
type NewUnifiedIntelRun struct {
	TriggerReason    string
	InputFingerprint string
	NewsChangedCount int32
	CandidatesCount  int32
	SelectedCount    int32
	AICalled         bool
	AIStatus         string
	SentinelCode     string
	AICostUSD        float64
	TelegramSent     bool
}

// FinishUnifiedIntelRunInput closes the run row.
type FinishUnifiedIntelRunInput struct {
	ID            int64
	Status        string
	AICalled      bool
	AIStatus      string
	SentinelCode  string
	AICostUSD     float64
	TelegramSent  bool
	SelectedCount int32
}

func (r *UnifiedIntelRuns) Insert(ctx context.Context, in NewUnifiedIntelRun) (int64, error) {
	return r.q.InsertUnifiedIntelRun(ctx, sqlc.InsertUnifiedIntelRunParams{
		TriggerReason:    in.TriggerReason,
		InputFingerprint: in.InputFingerprint,
		NewsChangedCount: in.NewsChangedCount,
		CandidatesCount:  in.CandidatesCount,
		SelectedCount:    in.SelectedCount,
		AiCalled:         in.AICalled,
		AiStatus:         in.AIStatus,
		SentinelCode:     in.SentinelCode,
		AiCostUsd:        in.AICostUSD,
		TelegramSent:     in.TelegramSent,
	})
}

func (r *UnifiedIntelRuns) Finish(ctx context.Context, in FinishUnifiedIntelRunInput) error {
	return r.q.FinishUnifiedIntelRun(ctx, sqlc.FinishUnifiedIntelRunParams{
		ID:            in.ID,
		Status:        in.Status,
		AiCalled:      in.AICalled,
		AiStatus:      in.AIStatus,
		SentinelCode:  in.SentinelCode,
		AiCostUsd:     in.AICostUSD,
		TelegramSent:  in.TelegramSent,
		SelectedCount: in.SelectedCount,
	})
}

// NewUnifiedIntelDecision is one selected-market row.
type NewUnifiedIntelDecision struct {
	RunID                    int64
	EventSlug                string
	ConditionID              string
	Decision                 string
	Regime                   string
	Class                    string
	InterestScore            float64
	Confidence               float64
	CurrentPrice             *float64
	ExpectedDirection        string
	ExpectedPriceMin         *float64
	ExpectedPriceMax         *float64
	ExpectedWindow           string
	WhyMarketMisprices       string
	WhatMarketWillUnderstand string
	TriggerCondition         string
	InvalidatesIf            string
	TradeStance              string
	TelegramWorthy           bool
}

func (r *UnifiedIntelRuns) InsertDecision(ctx context.Context, in NewUnifiedIntelDecision) error {
	return r.q.InsertUnifiedIntelDecision(ctx, sqlc.InsertUnifiedIntelDecisionParams{
		RunID:                    in.RunID,
		EventSlug:                in.EventSlug,
		ConditionID:              in.ConditionID,
		Decision:                 in.Decision,
		Regime:                   in.Regime,
		Class:                    in.Class,
		InterestScore:            in.InterestScore,
		Confidence:               in.Confidence,
		CurrentPrice:             in.CurrentPrice,
		ExpectedDirection:        in.ExpectedDirection,
		ExpectedPriceMin:         in.ExpectedPriceMin,
		ExpectedPriceMax:         in.ExpectedPriceMax,
		ExpectedWindow:           in.ExpectedWindow,
		WhyMarketMisprices:       in.WhyMarketMisprices,
		WhatMarketWillUnderstand: in.WhatMarketWillUnderstand,
		TriggerCondition:         in.TriggerCondition,
		InvalidatesIf:            in.InvalidatesIf,
		TradeStance:              in.TradeStance,
		TelegramWorthy:           in.TelegramWorthy,
	})
}

// NewRepricingThesisV109 — the v10.9 deterministic repricing thesis.
type NewRepricingThesisV109 struct {
	RunID             *int64
	EventSlug         string
	ConditionID       string
	CurrentPrice      float64
	ExpectedDirection string
	ExpectedPriceMin  *float64
	ExpectedPriceMax  *float64
	ExpectedWindow    string
	TriggerCondition  string
	Confidence        float64
	Reason            string
	InvalidatesIf     string
	Source            string
}

func (r *UnifiedIntelRuns) InsertRepricingThesis(ctx context.Context, in NewRepricingThesisV109) error {
	return r.q.InsertRepricingThesis(ctx, sqlc.InsertRepricingThesisParams{
		RunID:             in.RunID,
		EventSlug:         in.EventSlug,
		ConditionID:       in.ConditionID,
		CurrentPrice:      in.CurrentPrice,
		ExpectedDirection: in.ExpectedDirection,
		ExpectedPriceMin:  in.ExpectedPriceMin,
		ExpectedPriceMax:  in.ExpectedPriceMax,
		ExpectedWindow:    in.ExpectedWindow,
		TriggerCondition:  in.TriggerCondition,
		Confidence:        in.Confidence,
		Reason:            in.Reason,
		InvalidatesIf:     in.InvalidatesIf,
		Source:            in.Source,
	})
}

// =========================================================================

// MarketPriceSnapshots wraps polymarket_market_price_snapshots.
type MarketPriceSnapshots struct {
	q *sqlc.Queries
}

func NewMarketPriceSnapshots(pool *pgxpool.Pool) *MarketPriceSnapshots {
	return &MarketPriceSnapshots{q: sqlc.New(pool)}
}

// NewMarketPriceSnapshot is the write shape.
type NewMarketPriceSnapshot struct {
	ConditionID string
	EventSlug   string
	MarketSlug  string
	Price       *float64
	BestBid     *float64
	BestAsk     *float64
	Mid         *float64
	Source      string
}

func (r *MarketPriceSnapshots) Insert(ctx context.Context, in NewMarketPriceSnapshot) error {
	return r.q.InsertMarketPriceSnapshot(ctx, sqlc.InsertMarketPriceSnapshotParams{
		ConditionID: in.ConditionID,
		EventSlug:   in.EventSlug,
		MarketSlug:  in.MarketSlug,
		Price:       in.Price,
		BestBid:     in.BestBid,
		BestAsk:     in.BestAsk,
		Mid:         in.Mid,
		Source:      in.Source,
	})
}

// PriceAtOrBefore returns the most recent snapshot at-or-before
// `upper`. (zero, false, nil) on miss.
type PriceSnapshotProbe struct {
	Price     float64
	BestBid   float64
	BestAsk   float64
	Mid       float64
	SampledAt time.Time
}

func (r *MarketPriceSnapshots) PriceAtOrBefore(ctx context.Context, conditionID string, upper time.Time) (PriceSnapshotProbe, bool, error) {
	row, err := r.q.PriceSnapshotAtOrBefore(ctx, sqlc.PriceSnapshotAtOrBeforeParams{
		ConditionID: conditionID,
		Upper:       tsFromTime(upper),
	})
	if err != nil {
		return PriceSnapshotProbe{}, false, nil
	}
	out := PriceSnapshotProbe{SampledAt: row.SampledAt.Time}
	if row.Price != nil {
		out.Price = *row.Price
	}
	if row.BestBid != nil {
		out.BestBid = *row.BestBid
	}
	if row.BestAsk != nil {
		out.BestAsk = *row.BestAsk
	}
	if row.Mid != nil {
		out.Mid = *row.Mid
	}
	return out, true, nil
}
