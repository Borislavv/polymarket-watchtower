-- name: UpsertEventCatalyst :exec
-- Idempotent insert keyed on (event_slug, catalyst_type, title).
-- Mutable fields refresh on conflict; created_at stays frozen.
INSERT INTO polymarket_event_catalysts (
    event_slug, condition_id, catalyst_type, title, description,
    expected_at, confidence, source, source_url, status,
    bullish_scenario, bearish_scenario, invalidation_scenario
) VALUES (
    @event_slug, @condition_id, @catalyst_type, @title, @description,
    @expected_at, @confidence, @source, @source_url, @status,
    @bullish_scenario, @bearish_scenario, @invalidation_scenario
)
ON CONFLICT (event_slug, catalyst_type, title) DO UPDATE SET
    condition_id          = COALESCE(NULLIF(EXCLUDED.condition_id, ''), polymarket_event_catalysts.condition_id),
    description           = COALESCE(NULLIF(EXCLUDED.description, ''), polymarket_event_catalysts.description),
    expected_at           = COALESCE(EXCLUDED.expected_at, polymarket_event_catalysts.expected_at),
    confidence            = GREATEST(EXCLUDED.confidence, polymarket_event_catalysts.confidence),
    source                = COALESCE(NULLIF(EXCLUDED.source, ''), polymarket_event_catalysts.source),
    source_url            = COALESCE(NULLIF(EXCLUDED.source_url, ''), polymarket_event_catalysts.source_url),
    status                = EXCLUDED.status,
    bullish_scenario      = COALESCE(NULLIF(EXCLUDED.bullish_scenario, ''), polymarket_event_catalysts.bullish_scenario),
    bearish_scenario      = COALESCE(NULLIF(EXCLUDED.bearish_scenario, ''), polymarket_event_catalysts.bearish_scenario),
    invalidation_scenario = COALESCE(NULLIF(EXCLUDED.invalidation_scenario, ''), polymarket_event_catalysts.invalidation_scenario),
    updated_at            = NOW();

-- name: ListActiveEventCatalysts :many
-- Returns the catalysts the operator + AI should reason about: any
-- row in (expected, active) status. Newest-first by expected_at
-- (NULL-last) so the renderer prefers near-term events.
SELECT
    id, event_slug, condition_id, catalyst_type, title, description,
    expected_at, confidence, source, source_url, status,
    bullish_scenario, bearish_scenario, invalidation_scenario,
    created_at, updated_at
FROM polymarket_event_catalysts
WHERE event_slug = @event_slug
  AND status IN ('expected', 'active')
ORDER BY
    CASE status WHEN 'active' THEN 0 ELSE 1 END,
    expected_at NULLS LAST,
    id;

-- name: ListEventCatalysts :many
-- Returns ALL catalysts for an event (including resolved + stale +
-- invalidated). Used by the postmortem AI path so the model can
-- reason about whether a previously expected catalyst actually
-- materialised.
SELECT
    id, event_slug, condition_id, catalyst_type, title, description,
    expected_at, confidence, source, source_url, status,
    bullish_scenario, bearish_scenario, invalidation_scenario,
    created_at, updated_at
FROM polymarket_event_catalysts
WHERE event_slug = @event_slug
ORDER BY
    CASE status WHEN 'active' THEN 0 WHEN 'expected' THEN 1 ELSE 2 END,
    expected_at NULLS LAST,
    id;

-- name: SetEventCatalystStatus :exec
UPDATE polymarket_event_catalysts
SET status = @status, updated_at = NOW()
WHERE id = @id;
