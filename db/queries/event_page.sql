-- name: InsertEventPageSnapshot :one
-- Inserts a snapshot row. raw_json is capped by the writer before
-- this call lands so the column stays bounded.
INSERT INTO polymarket_event_page_snapshots (
    event_slug, build_id, fetched_at, raw_hash, raw_json
) VALUES (
    @event_slug, @build_id, @fetched_at, @raw_hash, @raw_json
)
RETURNING id;

-- name: InsertEventPageMarket :exec
INSERT INTO polymarket_event_page_markets (
    snapshot_id, event_slug, market_id, condition_id, market_slug,
    question, group_item_title, outcomes_json, outcome_prices_json,
    volume, volume_24h, liquidity, active, closed, end_date,
    one_hour_price_change, one_day_price_change, one_week_price_change,
    last_trade_price, best_bid, best_ask, clob_token_ids_json, raw_json
) VALUES (
    @snapshot_id, @event_slug, @market_id, @condition_id, @market_slug,
    @question, @group_item_title, @outcomes_json, @outcome_prices_json,
    @volume, @volume_24h, @liquidity, @active, @closed, @end_date,
    @one_hour_price_change, @one_day_price_change, @one_week_price_change,
    @last_trade_price, @best_bid, @best_ask, @clob_token_ids_json, @raw_json
);

-- name: UpsertEventAnnotation :exec
-- Idempotent insert keyed on (event_slug, item_hash). On conflict we
-- bump last_seen_at + refresh mutable fields (Polymarket sometimes
-- edits a summary in place) but keep first_seen_at frozen.
INSERT INTO polymarket_event_annotations (
    event_slug, item_hash, timestamp, unix_time, time_range,
    title, summary, outcome,
    price_before, price_after, price_change,
    source, sources_json, tweets_json, raw_json,
    first_seen_at, last_seen_at
) VALUES (
    @event_slug, @item_hash, @timestamp, @unix_time, @time_range,
    @title, @summary, @outcome,
    @price_before, @price_after, @price_change,
    @source, @sources_json, @tweets_json, @raw_json,
    NOW(), NOW()
)
ON CONFLICT (event_slug, item_hash) DO UPDATE SET
    timestamp     = COALESCE(EXCLUDED.timestamp, polymarket_event_annotations.timestamp),
    unix_time     = GREATEST(EXCLUDED.unix_time, polymarket_event_annotations.unix_time),
    time_range    = COALESCE(EXCLUDED.time_range, polymarket_event_annotations.time_range),
    title         = EXCLUDED.title,
    summary       = COALESCE(EXCLUDED.summary, polymarket_event_annotations.summary),
    outcome       = COALESCE(EXCLUDED.outcome, polymarket_event_annotations.outcome),
    price_before  = COALESCE(EXCLUDED.price_before, polymarket_event_annotations.price_before),
    price_after   = COALESCE(EXCLUDED.price_after, polymarket_event_annotations.price_after),
    price_change  = COALESCE(EXCLUDED.price_change, polymarket_event_annotations.price_change),
    source        = COALESCE(EXCLUDED.source, polymarket_event_annotations.source),
    sources_json  = COALESCE(EXCLUDED.sources_json, polymarket_event_annotations.sources_json),
    tweets_json   = COALESCE(EXCLUDED.tweets_json, polymarket_event_annotations.tweets_json),
    raw_json      = COALESCE(EXCLUDED.raw_json, polymarket_event_annotations.raw_json),
    last_seen_at  = NOW();

-- name: ListRecentEventAnnotations :many
SELECT
    id, event_slug, item_hash, timestamp, unix_time, time_range,
    title, summary, outcome,
    price_before, price_after, price_change,
    source, sources_json, tweets_json, raw_json,
    first_seen_at, last_seen_at
FROM polymarket_event_annotations
WHERE event_slug = @event_slug
ORDER BY timestamp DESC NULLS LAST, id DESC
LIMIT @limit_count;

-- name: ListLatestEventPageMarkets :many
-- Returns one row per (event_slug, market_id) — the newest snapshot
-- wins via the DISTINCT ON ordering. Used by the renderer + lag
-- detector so we always read the freshest event-wide pricing.
SELECT DISTINCT ON (market_id)
    id, snapshot_id, event_slug, market_id, condition_id, market_slug,
    question, group_item_title, outcomes_json, outcome_prices_json,
    volume, volume_24h, liquidity, active, closed, end_date,
    one_hour_price_change, one_day_price_change, one_week_price_change,
    last_trade_price, best_bid, best_ask, clob_token_ids_json, raw_json,
    created_at
FROM polymarket_event_page_markets
WHERE event_slug = @event_slug
ORDER BY market_id, created_at DESC;

-- name: GetEventPageFetchState :one
SELECT
    event_slug, last_fetched_at, last_success_at, last_error,
    last_build_id, last_annotations, updated_at
FROM polymarket_event_page_fetches
WHERE event_slug = @event_slug;

-- name: UpsertEventPageFetchState :exec
INSERT INTO polymarket_event_page_fetches (
    event_slug, last_fetched_at, last_success_at, last_error,
    last_build_id, last_annotations, updated_at
) VALUES (
    @event_slug, @last_fetched_at, @last_success_at, @last_error,
    @last_build_id, @last_annotations, NOW()
)
ON CONFLICT (event_slug) DO UPDATE SET
    last_fetched_at  = EXCLUDED.last_fetched_at,
    last_success_at  = COALESCE(EXCLUDED.last_success_at, polymarket_event_page_fetches.last_success_at),
    last_error       = EXCLUDED.last_error,
    last_build_id    = COALESCE(NULLIF(EXCLUDED.last_build_id, ''), polymarket_event_page_fetches.last_build_id),
    last_annotations = EXCLUDED.last_annotations,
    updated_at       = NOW();

-- name: UpsertEventSlugAlias :exec
-- v10.5 canonical-slug alias persistence. Idempotent.
INSERT INTO polymarket_event_slug_aliases (
    original_slug, canonical_slug, source, first_seen_at, last_seen_at
) VALUES (
    @original_slug, @canonical_slug, @source, NOW(), NOW()
)
ON CONFLICT (original_slug) DO UPDATE SET
    canonical_slug = EXCLUDED.canonical_slug,
    source         = EXCLUDED.source,
    last_seen_at   = NOW();

-- name: GetEventSlugAlias :one
SELECT canonical_slug FROM polymarket_event_slug_aliases
WHERE original_slug = @original_slug;
