-- v11.0 Hourly News Intelligence queries (PART 6).

-- =========================================================================
-- News intel runs
-- =========================================================================

-- name: InsertNewsIntelRun :one
INSERT INTO polymarket_news_intel_runs (
    started_at, status, lookback_start, lookback_end,
    news_items_count, selected_count, ai_called, ai_status,
    sentinel_code, ai_cost_usd, input_fingerprint, output_fingerprint,
    telegram_sent
) VALUES (
    NOW(), 'started', @lookback_start, @lookback_end,
    @news_items_count, @selected_count, @ai_called, @ai_status,
    @sentinel_code, @ai_cost_usd, @input_fingerprint, @output_fingerprint,
    @telegram_sent
)
RETURNING id;

-- name: FinishNewsIntelRun :exec
UPDATE polymarket_news_intel_runs
SET finished_at        = NOW(),
    status             = @status,
    news_items_count   = @news_items_count,
    selected_count     = @selected_count,
    ai_called          = @ai_called,
    ai_status          = @ai_status,
    sentinel_code      = @sentinel_code,
    ai_cost_usd        = @ai_cost_usd,
    output_fingerprint = @output_fingerprint,
    telegram_sent      = @telegram_sent,
    last_error         = @last_error
WHERE id = @id;

-- name: ListRecentNewsIntelRuns :many
SELECT id, started_at, finished_at, status, lookback_start, lookback_end,
       news_items_count, selected_count, ai_called, ai_status,
       sentinel_code, ai_cost_usd, input_fingerprint, output_fingerprint,
       telegram_sent, last_error, created_at
FROM polymarket_news_intel_runs
ORDER BY started_at DESC
LIMIT @row_limit;

-- =========================================================================
-- News intel decisions
-- =========================================================================

-- name: InsertNewsIntelDecision :exec
INSERT INTO polymarket_news_intel_decisions (
    run_id, news_item_hash, event_slug, condition_id, market_title,
    rank, decision, confidence, impact_direction,
    expected_price_impact_min, expected_price_impact_max, expected_window,
    why_it_matters, what_market_may_miss, trigger_condition, invalidates_if,
    trade_stance, telegram_worthy, affected_markets_json
) VALUES (
    @run_id, @news_item_hash, @event_slug, @condition_id, @market_title,
    @rank, @decision, @confidence, @impact_direction,
    @expected_price_impact_min, @expected_price_impact_max, @expected_window,
    @why_it_matters, @what_market_may_miss, @trigger_condition, @invalidates_if,
    @trade_stance, @telegram_worthy, @affected_markets_json
);

-- name: ListNewsIntelDecisionsByRun :many
SELECT id, run_id, news_item_hash, event_slug, condition_id, market_title,
       rank, decision, confidence, impact_direction,
       expected_price_impact_min, expected_price_impact_max, expected_window,
       why_it_matters, what_market_may_miss, trigger_condition, invalidates_if,
       trade_stance, telegram_worthy, affected_markets_json, created_at
FROM polymarket_news_intel_decisions
WHERE run_id = @run_id
ORDER BY rank ASC, id ASC;

-- =========================================================================
-- News intel processed items (dedupe ledger)
-- =========================================================================

-- name: GetNewsIntelProcessedItem :one
SELECT item_hash, event_slug, title, first_seen_at, last_seen_at,
       processed_at, last_run_id, created_at
FROM polymarket_news_intel_processed_items
WHERE item_hash = @item_hash;

-- name: UpsertNewsIntelProcessedItem :exec
INSERT INTO polymarket_news_intel_processed_items (
    item_hash, event_slug, title, first_seen_at, last_seen_at, processed_at, last_run_id
) VALUES (
    @item_hash, @event_slug, @title, NOW(), NOW(), NOW(), @last_run_id
)
ON CONFLICT (item_hash) DO UPDATE SET
    last_seen_at = NOW(),
    processed_at = NOW(),
    last_run_id  = EXCLUDED.last_run_id;

-- name: TouchNewsIntelProcessedItem :exec
UPDATE polymarket_news_intel_processed_items
SET last_seen_at = NOW()
WHERE item_hash = @item_hash;

-- name: ListNewsIntelProcessedHashes :many
SELECT item_hash
FROM polymarket_news_intel_processed_items
WHERE item_hash = ANY(@item_hashes::TEXT[]);

-- =========================================================================
-- Cross-event annotation feed
-- =========================================================================

-- name: ListAnnotationsSince :many
-- Pulls all annotations newer than a threshold across every event. The
-- v11.0 news intel worker uses this to enumerate the candidate pool in
-- a single roundtrip rather than per-event_slug.
SELECT
    id, event_slug, item_hash, timestamp, unix_time, time_range,
    title, summary, outcome,
    price_before, price_after, price_change,
    source, sources_json, tweets_json, raw_json,
    first_seen_at, last_seen_at
FROM polymarket_event_annotations
WHERE first_seen_at >= @since
   OR last_seen_at  >= @since
ORDER BY first_seen_at DESC, id DESC
LIMIT @row_limit;
