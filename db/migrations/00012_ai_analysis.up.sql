-- 00012_ai_analysis — persistence for the AI market-intelligence layer.
--
-- Three tables:
--   * polymarket_alert_analyses — one AI note per (alert, version).
--     Versioned so the refresh policy can write a new row when the
--     alert's lifecycle/severity/CLV moves materially without
--     overwriting prior reasoning.
--   * polymarket_market_intelligence_reports — the periodic 2h
--     scout/strategy summary. Deduped by content hash so a sleepy
--     window doesn't flood Telegram with identical reports.
--   * polymarket_alert_outcome_analyses — postmortem written when an
--     alert's market resolves. One row per alert. Feeds the long-
--     term lessons dataset.
--
-- All three keep token usage + estimated cost columns so an operator
-- can audit spend without an external billing dashboard.

CREATE TABLE polymarket_alert_analyses (
    id              BIGSERIAL PRIMARY KEY,
    alert_id        BIGINT      NOT NULL REFERENCES polymarket_alerts(id) ON DELETE CASCADE,
    version         INT         NOT NULL DEFAULT 1,
    -- The structured trigger for this version, so refresh-policy
    -- decisions are auditable (e.g. "lifecycle_moved=2.3" or
    -- "severity_upgraded=warning_to_critical").
    trigger_kind    TEXT        NOT NULL,
    trigger_detail  TEXT        NULL,
    -- Model output.
    model           TEXT        NOT NULL,
    prompt_chars    INT         NOT NULL DEFAULT 0,
    output_chars    INT         NOT NULL DEFAULT 0,
    prompt_tokens   INT         NOT NULL DEFAULT 0,
    completion_tokens INT       NOT NULL DEFAULT 0,
    estimated_cost_usd DOUBLE PRECISION NOT NULL DEFAULT 0,
    analysis_text   TEXT        NOT NULL,
    -- Operator-facing verdict shorthand: actionable | watchlist | avoid | n/a.
    verdict         TEXT        NULL,
    -- Status: ok | skipped (refresh policy or budget) | error.
    status          TEXT        NOT NULL DEFAULT 'ok',
    last_error      TEXT        NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE UNIQUE INDEX idx_alert_analyses_alert_version
    ON polymarket_alert_analyses (alert_id, version);

-- Latest analysis per alert — Telegram render reads this index.
CREATE INDEX idx_alert_analyses_alert_latest
    ON polymarket_alert_analyses (alert_id, version DESC);

CREATE TABLE polymarket_market_intelligence_reports (
    id                  BIGSERIAL PRIMARY KEY,
    generated_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    period_start        TIMESTAMPTZ NOT NULL,
    period_end          TIMESTAMPTZ NOT NULL,
    -- SHA-256 over the canonical report body so successive runs that
    -- produce identical content (rare but possible) can be deduped
    -- before Telegram delivery.
    summary_hash        TEXT        NOT NULL,
    report_text         TEXT        NOT NULL,
    markets_json        JSONB       NOT NULL DEFAULT '[]'::jsonb,
    -- Model + spend audit.
    model               TEXT        NOT NULL,
    prompt_tokens       INT         NOT NULL DEFAULT 0,
    completion_tokens   INT         NOT NULL DEFAULT 0,
    estimated_cost_usd  DOUBLE PRECISION NOT NULL DEFAULT 0,
    -- Telegram delivery state.
    telegram_message_id BIGINT      NULL,
    telegram_chat_id    TEXT        NULL,
    delivery_status     TEXT        NOT NULL DEFAULT 'pending',
    last_delivery_error TEXT        NULL,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE UNIQUE INDEX idx_intel_reports_summary_hash
    ON polymarket_market_intelligence_reports (summary_hash);

CREATE INDEX idx_intel_reports_generated_at
    ON polymarket_market_intelligence_reports (generated_at DESC);

CREATE TABLE polymarket_alert_outcome_analyses (
    id                  BIGSERIAL PRIMARY KEY,
    alert_id            BIGINT      NOT NULL REFERENCES polymarket_alerts(id) ON DELETE CASCADE,
    outcome_status      TEXT        NOT NULL,
    won_expected        BOOLEAN     NULL,
    ai_reason_text      TEXT        NOT NULL,
    ai_lessons_text     TEXT        NULL,
    confidence          DOUBLE PRECISION NOT NULL DEFAULT 0,
    model               TEXT        NOT NULL,
    prompt_tokens       INT         NOT NULL DEFAULT 0,
    completion_tokens   INT         NOT NULL DEFAULT 0,
    estimated_cost_usd  DOUBLE PRECISION NOT NULL DEFAULT 0,
    -- Telegram follow-up message (the "Why WON / Why LOST" body):
    -- either an edit on the original alert message, or a fresh
    -- message linked by reply_to_message_id.
    telegram_message_id BIGINT      NULL,
    telegram_chat_id    TEXT        NULL,
    delivery_status     TEXT        NOT NULL DEFAULT 'pending',
    last_delivery_error TEXT        NULL,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE UNIQUE INDEX idx_alert_outcome_analyses_alert
    ON polymarket_alert_outcome_analyses (alert_id);
