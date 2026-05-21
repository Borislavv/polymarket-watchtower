-- name: GetEventNewsFingerprint :one
SELECT event_slug, news_fingerprint, annotation_count, latest_annotation_at,
       first_seen_at, last_seen_at, changed_at, last_seen_unchanged_at,
       last_ai_called_at, last_semantic_fingerprint, last_semantic_at,
       last_semantic_code, repeated_count
FROM polymarket_event_news_fingerprints
WHERE event_slug = @event_slug;

-- name: UpsertEventNewsFingerprint :exec
-- Idempotent. Flips changed_at only when the fingerprint string
-- actually moved. last_seen_at refreshes every call.
INSERT INTO polymarket_event_news_fingerprints (
    event_slug, news_fingerprint, annotation_count, latest_annotation_at,
    first_seen_at, last_seen_at, changed_at, last_seen_unchanged_at
) VALUES (
    @event_slug, @news_fingerprint, @annotation_count, @latest_annotation_at,
    NOW(), NOW(), NOW(), NULL
)
ON CONFLICT (event_slug) DO UPDATE SET
    news_fingerprint = EXCLUDED.news_fingerprint,
    annotation_count = EXCLUDED.annotation_count,
    latest_annotation_at = EXCLUDED.latest_annotation_at,
    last_seen_at = NOW(),
    changed_at = CASE
        WHEN polymarket_event_news_fingerprints.news_fingerprint = EXCLUDED.news_fingerprint
        THEN polymarket_event_news_fingerprints.changed_at
        ELSE NOW()
    END,
    last_seen_unchanged_at = CASE
        WHEN polymarket_event_news_fingerprints.news_fingerprint = EXCLUDED.news_fingerprint
        THEN NOW()
        ELSE polymarket_event_news_fingerprints.last_seen_unchanged_at
    END;

-- name: TouchEventNewsAICalled :exec
UPDATE polymarket_event_news_fingerprints
SET last_ai_called_at = NOW()
WHERE event_slug = @event_slug;

-- name: UpsertSemanticFingerprint :exec
-- Records the most recent semantic-fingerprint + code we shipped for
-- this event so the cooldown check can suppress repeats.
UPDATE polymarket_event_news_fingerprints
SET last_semantic_fingerprint = @semantic_fingerprint,
    last_semantic_at = NOW(),
    last_semantic_code = @semantic_code,
    repeated_count = CASE
        WHEN polymarket_event_news_fingerprints.last_semantic_fingerprint = @semantic_fingerprint
        THEN polymarket_event_news_fingerprints.repeated_count + 1
        ELSE 0
    END
WHERE event_slug = @event_slug;
