-- v10.9 unified intel queries.

-- =========================================================================
-- Market AI cache (PART 3)
-- =========================================================================

-- name: GetMarketAICache :one
SELECT id, event_slug, condition_id, ai_surface, market_ai_key,
       news_fingerprint, catalyst_fingerprint, repricing_bucket,
       flow_bucket, price_bucket,
       ai_status, sentinel_code, decision_json, summary_text,
       last_ai_at, last_reused_at, reuse_count, created_at, updated_at
FROM polymarket_market_ai_cache
WHERE ai_surface = @ai_surface AND market_ai_key = @market_ai_key;

-- name: UpsertMarketAICache :exec
INSERT INTO polymarket_market_ai_cache (
    event_slug, condition_id, ai_surface, market_ai_key,
    news_fingerprint, catalyst_fingerprint, repricing_bucket,
    flow_bucket, price_bucket,
    ai_status, sentinel_code, decision_json, summary_text,
    last_ai_at, updated_at
) VALUES (
    @event_slug, @condition_id, @ai_surface, @market_ai_key,
    @news_fingerprint, @catalyst_fingerprint, @repricing_bucket,
    @flow_bucket, @price_bucket,
    @ai_status, @sentinel_code, @decision_json, @summary_text,
    NOW(), NOW()
)
ON CONFLICT (ai_surface, market_ai_key) DO UPDATE SET
    news_fingerprint     = EXCLUDED.news_fingerprint,
    catalyst_fingerprint = EXCLUDED.catalyst_fingerprint,
    repricing_bucket     = EXCLUDED.repricing_bucket,
    flow_bucket          = EXCLUDED.flow_bucket,
    price_bucket         = EXCLUDED.price_bucket,
    ai_status            = EXCLUDED.ai_status,
    sentinel_code        = EXCLUDED.sentinel_code,
    decision_json        = EXCLUDED.decision_json,
    summary_text         = EXCLUDED.summary_text,
    last_ai_at           = NOW(),
    updated_at           = NOW();

-- name: TouchMarketAICacheReuse :exec
UPDATE polymarket_market_ai_cache
SET last_reused_at = NOW(),
    reuse_count    = reuse_count + 1,
    updated_at     = NOW()
WHERE ai_surface = @ai_surface AND market_ai_key = @market_ai_key;

-- =========================================================================
-- Telegram semantic dedupe (PART 4)
-- =========================================================================

-- name: GetTelegramSemanticDedupe :one
SELECT id, surface, dedupe_key, semantic_fingerprint, event_slug,
       condition_id, wallet, last_sent_at, send_count,
       last_notional, last_severity, last_reason, created_at, updated_at
FROM polymarket_telegram_semantic_dedupe
WHERE surface = @surface AND dedupe_key = @dedupe_key;

-- name: UpsertTelegramSemanticDedupe :exec
INSERT INTO polymarket_telegram_semantic_dedupe (
    surface, dedupe_key, semantic_fingerprint, event_slug,
    condition_id, wallet, last_sent_at, send_count,
    last_notional, last_severity, last_reason, updated_at
) VALUES (
    @surface, @dedupe_key, @semantic_fingerprint, @event_slug,
    @condition_id, @wallet, NOW(), 1,
    @last_notional, @last_severity, @last_reason, NOW()
)
ON CONFLICT (surface, dedupe_key) DO UPDATE SET
    semantic_fingerprint = EXCLUDED.semantic_fingerprint,
    last_sent_at         = NOW(),
    send_count           = polymarket_telegram_semantic_dedupe.send_count + 1,
    last_notional        = EXCLUDED.last_notional,
    last_severity        = EXCLUDED.last_severity,
    last_reason          = EXCLUDED.last_reason,
    updated_at           = NOW();

-- =========================================================================
-- Unified intel runs + decisions (PART 13)
-- =========================================================================

-- name: InsertUnifiedIntelRun :one
INSERT INTO polymarket_unified_intel_runs (
    started_at, status, trigger_reason, input_fingerprint,
    news_changed_count, candidates_count, selected_count,
    ai_called, ai_status, sentinel_code, ai_cost_usd, telegram_sent
) VALUES (
    NOW(), 'started', @trigger_reason, @input_fingerprint,
    @news_changed_count, @candidates_count, @selected_count,
    @ai_called, @ai_status, @sentinel_code, @ai_cost_usd, @telegram_sent
)
RETURNING id;

-- name: FinishUnifiedIntelRun :exec
UPDATE polymarket_unified_intel_runs
SET finished_at   = NOW(),
    status        = @status,
    ai_called     = @ai_called,
    ai_status     = @ai_status,
    sentinel_code = @sentinel_code,
    ai_cost_usd   = @ai_cost_usd,
    telegram_sent = @telegram_sent,
    selected_count = @selected_count
WHERE id = @id;

-- name: InsertUnifiedIntelDecision :exec
INSERT INTO polymarket_unified_intel_decisions (
    run_id, event_slug, condition_id, decision, regime, class,
    interest_score, confidence, current_price,
    expected_direction, expected_price_min, expected_price_max,
    expected_window, why_market_misprices, what_market_will_understand,
    trigger_condition, invalidates_if, trade_stance, telegram_worthy
) VALUES (
    @run_id, @event_slug, @condition_id, @decision, @regime, @class,
    @interest_score, @confidence, @current_price,
    @expected_direction, @expected_price_min, @expected_price_max,
    @expected_window, @why_market_misprices, @what_market_will_understand,
    @trigger_condition, @invalidates_if, @trade_stance, @telegram_worthy
);

-- =========================================================================
-- Repricing theses (PART 9)
-- =========================================================================

-- name: InsertRepricingThesis :exec
INSERT INTO polymarket_repricing_theses (
    run_id, event_slug, condition_id, current_price, expected_direction,
    expected_price_min, expected_price_max, expected_window,
    trigger_condition, confidence, reason, invalidates_if, source
) VALUES (
    @run_id, @event_slug, @condition_id, @current_price, @expected_direction,
    @expected_price_min, @expected_price_max, @expected_window,
    @trigger_condition, @confidence, @reason, @invalidates_if, @source
);

-- =========================================================================
-- Market price snapshots (PART 8)
-- =========================================================================

-- name: InsertMarketPriceSnapshot :exec
INSERT INTO polymarket_market_price_snapshots (
    condition_id, event_slug, market_slug, price, best_bid, best_ask, mid, source, sampled_at
) VALUES (
    @condition_id, @event_slug, @market_slug, @price, @best_bid, @best_ask, @mid, @source, NOW()
);

-- name: PriceSnapshotAtOrBefore :one
SELECT price, best_bid, best_ask, mid, sampled_at
FROM polymarket_market_price_snapshots
WHERE condition_id = @condition_id AND sampled_at <= @upper
ORDER BY sampled_at DESC LIMIT 1;
