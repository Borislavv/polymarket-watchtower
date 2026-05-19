-- 00010_trade_detection — DB-backed detection state on polymarket_trades.
--
-- Why: before this migration the detection path was source-dependent.
-- `collect` called detect.Observe synchronously; `backfill` did not.
-- A trade persisted by backfill (or any future ingest source) never
-- reached the scorer, so the "no Info alerts overnight" failure mode
-- recurred whenever backfill outpaced collect.
--
-- After this migration every persisted trade lands with
-- detection_status = NULL ("pending") and is drained by a dedicated
-- worker that runs detect.Loop.Observe and stamps a terminal state.

ALTER TABLE polymarket_trades
    ADD COLUMN detected_at            TIMESTAMPTZ NULL,
    ADD COLUMN detection_status       TEXT        NULL,
    ADD COLUMN detection_skip_reason  TEXT        NULL,
    ADD COLUMN detection_attempts     INT         NOT NULL DEFAULT 0,
    ADD COLUMN last_detection_error   TEXT        NULL;

ALTER TABLE polymarket_trades
    ADD CONSTRAINT polymarket_trades_detection_status_valid CHECK (
        detection_status IS NULL
        OR detection_status IN ('analyzed', 'skipped', 'failed')
    );

-- Partial index for the worker's claim query. Lists undetected rows
-- newest-first so live trades (most likely within LIVE_ALERT_MAX_LAG)
-- get processed before historical backfill backlog.
CREATE INDEX IF NOT EXISTS idx_trades_detection_pending
    ON polymarket_trades (traded_at DESC, id DESC)
    WHERE detected_at IS NULL;

-- Stamp existing rows as analyzed so the worker does not flood every
-- already-ingested row through the scorer on first boot. We do this in
-- the down direction too — operators rolling back can re-instate the
-- pending pile via UPDATE.
UPDATE polymarket_trades
SET detected_at = ingested_at,
    detection_status = 'analyzed'
WHERE detected_at IS NULL;
