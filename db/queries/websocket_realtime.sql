-- name: InsertWSEvent :exec
-- Append-only audit/correlation row. Fail-open: the WS path
-- handles a write error by logging + continuing — never blocks.
INSERT INTO polymarket_ws_events
  (received_at, exchange_timestamp, event_type, event_slug, condition_id,
   market_slug, clob_token_id, outcome, price, size, side, side_source,
   side_confidence, best_bid, best_ask, mid, tx_hash, trade_id, wallet,
   sequence, raw_json, raw_hash)
VALUES (@received_at, @exchange_timestamp, @event_type, @event_slug, @condition_id,
        @market_slug, @clob_token_id, @outcome, @price, @size, @side, @side_source,
        @side_confidence, @best_bid, @best_ask, @mid, @tx_hash, @trade_id, @wallet,
        @sequence, @raw_json, @raw_hash);

-- name: UpsertLiveMarketState :exec
-- Top-of-book / mid / last-price per condition_id. Updated from both
-- WS events and the reconciliation sweep. Idempotent — every write
-- bumps updated_at + last_ws_event_at.
INSERT INTO polymarket_live_market_state
  (condition_id, event_slug, market_slug, best_bid, best_ask, mid,
   last_price, last_trade_at, last_ws_event_at, ws_connected, updated_at)
VALUES (@condition_id, @event_slug, @market_slug, @best_bid, @best_ask, @mid,
        @last_price, @last_trade_at, @last_ws_event_at, @ws_connected, NOW())
ON CONFLICT (condition_id) DO UPDATE
   SET event_slug       = COALESCE(EXCLUDED.event_slug, polymarket_live_market_state.event_slug),
       market_slug      = COALESCE(EXCLUDED.market_slug, polymarket_live_market_state.market_slug),
       best_bid         = COALESCE(EXCLUDED.best_bid, polymarket_live_market_state.best_bid),
       best_ask         = COALESCE(EXCLUDED.best_ask, polymarket_live_market_state.best_ask),
       mid              = COALESCE(EXCLUDED.mid, polymarket_live_market_state.mid),
       last_price       = COALESCE(EXCLUDED.last_price, polymarket_live_market_state.last_price),
       last_trade_at    = COALESCE(EXCLUDED.last_trade_at, polymarket_live_market_state.last_trade_at),
       last_ws_event_at = COALESCE(EXCLUDED.last_ws_event_at, polymarket_live_market_state.last_ws_event_at),
       ws_connected     = EXCLUDED.ws_connected,
       updated_at       = NOW();

-- name: GetLiveMarketState :one
SELECT condition_id, event_slug, market_slug, best_bid, best_ask, mid,
       last_price, last_trade_at, last_ws_event_at, ws_connected, updated_at
FROM polymarket_live_market_state
WHERE condition_id = @condition_id;

-- name: SetLiveMarketWSConnected :exec
-- Bulk-flip ws_connected on/off when the client connects/disconnects.
-- Used by the realtime worker to surface "WS is alive for this market".
UPDATE polymarket_live_market_state
SET ws_connected = @ws_connected, updated_at = NOW()
WHERE condition_id = ANY(@condition_ids::text[]);

-- name: InsertGapRecovery :one
INSERT INTO polymarket_ws_gap_recoveries
  (condition_id, started_at, lookback_start, lookback_end, status, recovered_trades)
VALUES (@condition_id, NOW(), @lookback_start, @lookback_end, 'started', 0)
RETURNING id;

-- name: FinishGapRecovery :exec
UPDATE polymarket_ws_gap_recoveries
SET ended_at        = NOW(),
    status          = @status,
    recovered_trades = @recovered_trades,
    last_error      = @last_error
WHERE id = @id;

-- name: EnqueueRealtimeWork :exec
-- Idempotent: dedupe_key = condition_id + reason + minute_bucket so a
-- burst of WS events for the same market collapses to one queue row.
-- Conflicts are silently swallowed via DO NOTHING.
INSERT INTO polymarket_realtime_work_queue
  (condition_id, event_slug, reason, priority, dedupe_key, available_at)
VALUES (@condition_id, @event_slug, @reason, @priority, @dedupe_key, @available_at)
ON CONFLICT (dedupe_key) DO NOTHING;

-- name: ClaimRealtimeWorkBatch :many
-- Atomic claim of up to `limit_count` due rows. Uses SKIP LOCKED so
-- multiple drainers can run side-by-side without deadlocks.
UPDATE polymarket_realtime_work_queue
SET claimed_at = NOW(),
    attempts   = attempts + 1
WHERE id IN (
    SELECT id
    FROM polymarket_realtime_work_queue
    WHERE claimed_at IS NULL
      AND available_at <= NOW()
    ORDER BY priority ASC, available_at ASC
    FOR UPDATE SKIP LOCKED
    LIMIT @limit_count
)
RETURNING id, condition_id, event_slug, reason, priority, dedupe_key, attempts;

-- name: MarkRealtimeWorkFailed :exec
UPDATE polymarket_realtime_work_queue
SET last_error = @last_error,
    claimed_at = NULL,
    available_at = NOW() + INTERVAL '1 minute'
WHERE id = @id;

-- name: DeleteOldRealtimeWork :exec
-- Periodic cleanup so the queue doesn't grow without bound.
DELETE FROM polymarket_realtime_work_queue
WHERE created_at < @older_than;
