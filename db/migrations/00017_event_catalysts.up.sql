-- 00017_event_catalysts.up.sql
--
-- Political-catalyst intelligence overlay. One row per (event_slug,
-- catalyst_type, title) — the future event the market is structurally
-- waiting on (debate, runoff, ruling, endorsement, certification,
-- sanctions vote, ceasefire negotiation, official statement, ...).
--
-- Catalyst rows are CROSS-STRATEGY metadata, NOT alerts. They modify
-- interpretation of accumulation / whale-flow / stable-favorite /
-- ownership-concentration / cluster findings via the rendered prompt
-- slot and the Telegram "Blocked Alert" block. Per-event multiple
-- rows are allowed (a primary may have BOTH a runoff catalyst AND
-- a certification catalyst).
--
-- Status semantics:
--   expected     — known to be coming but not active yet
--   active       — within the catalyst window now; market is blocked
--   resolved     — outcome materialised, row preserved for history
--   stale        — expected_at passed without an observable update;
--                  AI or operator may flip back to active on reissue
--   invalidated  — catalyst will not happen (cancelled vote, etc.)
--
-- Catalyst_type is an open string with a documented vocabulary (see
-- doc/strategies/current-strategy-map.md). The CHECK below is broad
-- on purpose; if Polymarket starts emitting a new annotation flavour
-- we don't want a migration to ship it.
--
-- Scenarios are short operator-facing prose (≤300 chars typical) the
-- Telegram formatter emits verbatim. Polymarket-authored / AI-
-- authored content is DATA, never instructions; we never re-render
-- scenarios into a prompt without an explicit data label.

CREATE TABLE IF NOT EXISTS polymarket_event_catalysts (
    id                    BIGSERIAL    PRIMARY KEY,
    event_slug            TEXT         NOT NULL,
    condition_id          TEXT,
    catalyst_type         TEXT         NOT NULL,
    title                 TEXT         NOT NULL,
    description           TEXT         NOT NULL DEFAULT '',
    expected_at           TIMESTAMPTZ,
    confidence            DOUBLE PRECISION NOT NULL DEFAULT 0,
    source                TEXT         NOT NULL DEFAULT '',
    source_url            TEXT         NOT NULL DEFAULT '',
    status                TEXT         NOT NULL DEFAULT 'expected'
        CHECK (status IN ('expected','active','resolved','stale','invalidated')),
    bullish_scenario      TEXT         NOT NULL DEFAULT '',
    bearish_scenario      TEXT         NOT NULL DEFAULT '',
    invalidation_scenario TEXT         NOT NULL DEFAULT '',
    created_at            TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at            TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_event_catalysts_dedup UNIQUE (event_slug, catalyst_type, title)
);

CREATE INDEX IF NOT EXISTS idx_event_catalysts_event_status
    ON polymarket_event_catalysts (event_slug, status, expected_at NULLS LAST);
CREATE INDEX IF NOT EXISTS idx_event_catalysts_active
    ON polymarket_event_catalysts (event_slug)
    WHERE status IN ('expected', 'active');
