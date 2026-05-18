-- 00007_alert_reactions.up.sql
--
-- Telegram outcome-reaction state. After the outcomes worker classifies
-- a resolved alert (resolved_correct / resolved_wrong / unknown), a
-- reactor pass calls setMessageReaction on the original Telegram
-- message so an operator scrolling the chat history sees the alert's
-- ground truth without opening a dashboard.
--
-- Status semantics:
--   pending     — outcome not yet classified, or classified but the
--                 reactor has not run. Default value.
--   applied     — reaction successfully posted on the upstream message.
--   unsupported — Telegram returned an explicit "unsupported" error
--                 (channel reactions disabled, bot missing
--                 RIGHTS_TO_REACT, paid reactions, etc.). The reactor
--                 records the state once and never retries.
--   failed      — transient error (rate-limit, network, 5xx). The
--                 reactor may retry on the next tick.
--   disabled    — TELEGRAM_OUTCOME_REACTIONS_ENABLED=false at the time
--                 the row was eligible. Acts as a terminal "skipped"
--                 marker so flipping the flag back on later doesn't
--                 reach into the historical backlog.
--
-- last_reaction_at is the timestamp of the most recent successful
-- setMessageReaction call. NULL when no reaction has been applied.

BEGIN;

ALTER TABLE polymarket_alerts
    ADD COLUMN telegram_reaction_status TEXT NOT NULL DEFAULT 'pending',
    ADD COLUMN telegram_reaction_emoji  TEXT,
    ADD COLUMN last_reaction_at         TIMESTAMPTZ,
    ADD CONSTRAINT polymarket_alerts_reaction_status_valid
        CHECK (telegram_reaction_status IN ('pending','applied','unsupported','failed','disabled'));

-- Partial index: the reactor scans rows that are resolved + have a
-- message_id + are still in a retryable state. Keeping it partial
-- means the index stays small even as the alert table grows.
CREATE INDEX idx_alerts_reaction_pending
    ON polymarket_alerts (resolved_at, id)
    WHERE telegram_message_id IS NOT NULL
      AND outcome_status IN ('resolved_correct','resolved_wrong','unknown')
      AND telegram_reaction_status IN ('pending','failed');

COMMIT;
