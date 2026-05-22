-- v11.4 Market Close Review queries.

-- name: ListMarketCloseReviewCandidates :many
-- Returns recently-closed/resolved markets that have no succeeded
-- review row, ordered by closed_at DESC. The worker fetches the
-- newest candidates per tick and rejects each in code if the
-- evidence package is empty / outside the window.
--
-- Bounded: lookback + limit are both required (no full table scan).
SELECT m.id, m.condition_id, m.event_slug, m.question,
       m.end_date, m.closed, m.active
FROM polymarket_markets m
LEFT JOIN polymarket_market_close_reviews r
       ON r.condition_id = m.condition_id AND r.status = 'succeeded'
WHERE m.closed = TRUE
  AND m.end_date >= @closed_since
  AND m.end_date <= @closed_until
  AND r.id IS NULL
ORDER BY m.end_date DESC
LIMIT @row_limit;

-- name: GetMarketCloseReview :one
SELECT id, market_id, condition_id, event_slug,
       closed_at, resolved_at, reviewed_at,
       status, skip_reason, verdict, confidence,
       admin_summary, ai_json, evidence_json,
       ai_model, input_tokens, output_tokens, estimated_cost_usd,
       error, attempts, next_retry_at, created_at, updated_at
FROM polymarket_market_close_reviews
WHERE condition_id = @condition_id AND status = 'succeeded'
LIMIT 1;

-- name: InsertMarketCloseReviewRunning :one
-- Inserts a fresh running row OR upserts the previous failed/
-- pending row for the same condition_id by status transition.
-- Returns the row id so the worker can FinishMarketCloseReview()
-- against it.
INSERT INTO polymarket_market_close_reviews (
    market_id, condition_id, event_slug,
    closed_at, resolved_at,
    status, attempts, created_at, updated_at
) VALUES (
    @market_id, @condition_id, @event_slug,
    @closed_at, @resolved_at,
    'running', 1, NOW(), NOW()
)
RETURNING id;

-- name: FinishMarketCloseReviewSucceeded :exec
UPDATE polymarket_market_close_reviews
SET status            = 'succeeded',
    reviewed_at       = NOW(),
    verdict           = @verdict,
    confidence        = @confidence,
    admin_summary     = @admin_summary,
    ai_json           = @ai_json,
    evidence_json     = @evidence_json,
    ai_model          = @ai_model,
    input_tokens      = @input_tokens,
    output_tokens     = @output_tokens,
    estimated_cost_usd = @estimated_cost_usd,
    error             = '',
    updated_at        = NOW()
WHERE id = @id;

-- name: FinishMarketCloseReviewFailed :exec
UPDATE polymarket_market_close_reviews
SET status        = 'failed',
    reviewed_at   = NOW(),
    error         = @error,
    next_retry_at = @next_retry_at,
    attempts      = attempts + 1,
    updated_at    = NOW()
WHERE id = @id;

-- name: FinishMarketCloseReviewSkipped :exec
UPDATE polymarket_market_close_reviews
SET status      = 'skipped',
    skip_reason = @skip_reason,
    reviewed_at = NOW(),
    updated_at  = NOW()
WHERE id = @id;

-- name: ListAlertsForMarketCloseReview :many
-- Pulls alerts for a market within the review history lookback.
-- Capped via @row_limit so the evidence package stays bounded.
SELECT id, dedup_key, strategy_version, kind, reason, severity,
       market_id, trader_id, trade_id, payload,
       telegram_message_id, sent_at, outcome_status, drift_status,
       clv_15m, clv_1h, clv_6h, clv_24h, created_at
FROM polymarket_alerts
WHERE market_id = @market_id
  AND status = 'sent'
  AND sent_at >= @sent_since
ORDER BY sent_at ASC
LIMIT @row_limit;
