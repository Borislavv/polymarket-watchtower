-- 00018_annotation_intel.up.sql
--
-- Two intel-layer tables landing in v9.7:
--
--   * polymarket_event_annotation_rankings — persisted AI ranking
--     decisions for the 2h market-intelligence path. One row per
--     (period, event_slug, annotation_hash). The marketintel worker
--     uses these rows to render "Top important annotations" in the
--     Telegram body and as audit trail for which annotations the
--     model judged operator-relevant in each cycle.
--
--   * polymarket_daily_political_intel_reports — one row per
--     report_date. Carries the candidate selection, the rendered
--     AI report text, the Telegram delivery state (may be multi-
--     message — message_ids is a JSON array), and the delivery
--     status. Failures keep the row + last_delivery_error;
--     re-issue logic is handled by the worker.
--
-- Polymarket-authored / AI-authored text in these tables is DATA.
-- Renderers HTML-escape every string at the boundary.

CREATE TABLE IF NOT EXISTS polymarket_event_annotation_rankings (
    id                   BIGSERIAL    PRIMARY KEY,
    period_start         TIMESTAMPTZ  NOT NULL,
    period_end           TIMESTAMPTZ  NOT NULL,
    event_slug           TEXT         NOT NULL,
    market_slug          TEXT,
    annotation_hash      TEXT         NOT NULL,
    rank                 INTEGER      NOT NULL DEFAULT 0,
    importance           DOUBLE PRECISION NOT NULL DEFAULT 0,
    volatility_potential DOUBLE PRECISION NOT NULL DEFAULT 0,
    probability_impact   TEXT         NOT NULL DEFAULT 'unclear',
    affected_outcome     TEXT,
    title                TEXT         NOT NULL DEFAULT '',
    reason               TEXT         NOT NULL DEFAULT '',
    market_read          TEXT         NOT NULL DEFAULT 'unclear',
    created_at           TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_event_annotation_rankings UNIQUE (period_start, event_slug, annotation_hash)
);

CREATE INDEX IF NOT EXISTS idx_event_annotation_rankings_period
    ON polymarket_event_annotation_rankings (period_end DESC, rank);
CREATE INDEX IF NOT EXISTS idx_event_annotation_rankings_event
    ON polymarket_event_annotation_rankings (event_slug, period_end DESC);

CREATE TABLE IF NOT EXISTS polymarket_daily_political_intel_reports (
    id                        BIGSERIAL    PRIMARY KEY,
    report_date               DATE         NOT NULL UNIQUE,
    period_start              TIMESTAMPTZ  NOT NULL,
    period_end                TIMESTAMPTZ  NOT NULL,
    selected_markets_json     JSONB,
    selected_annotations_json JSONB,
    catalysts_json            JSONB,
    ai_report_text            TEXT         NOT NULL DEFAULT '',
    telegram_message_ids_json JSONB,
    delivery_status           TEXT         NOT NULL DEFAULT 'pending'
        CHECK (delivery_status IN ('pending', 'sent', 'failed', 'skipped', 'ai_failed')),
    last_delivery_error       TEXT         NOT NULL DEFAULT '',
    created_at                TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at                TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_daily_political_intel_reports_status
    ON polymarket_daily_political_intel_reports (delivery_status, report_date DESC);
