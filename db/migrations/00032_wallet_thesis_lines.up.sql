-- v11.9 PART 5 — wallet thesis lines.
--
-- One row per (wallet, condition_id, side) aggregated over the
-- configured lookback. The thesisaccum detector reads bounded rows
-- via event_slug join with polymarket_market_links to compute
-- cross-market breadth + consistency without hot-path aggregates.
CREATE TABLE IF NOT EXISTS polymarket_wallet_thesis_lines (
    id              BIGSERIAL PRIMARY KEY,
    wallet          TEXT             NOT NULL,
    condition_id    TEXT             NOT NULL,
    event_slug      TEXT             NOT NULL DEFAULT '',
    side            TEXT             NOT NULL,
    notional_usd    DOUBLE PRECISION NOT NULL DEFAULT 0,
    trades          INTEGER          NOT NULL DEFAULT 0,
    last_traded_at  TIMESTAMPTZ      NOT NULL,
    lookback_hours  INTEGER          NOT NULL,
    refreshed_at    TIMESTAMPTZ      NOT NULL DEFAULT NOW(),
    created_at      TIMESTAMPTZ      NOT NULL DEFAULT NOW(),
    UNIQUE (wallet, condition_id, side, lookback_hours)
);

CREATE INDEX IF NOT EXISTS idx_wallet_thesis_lines_event
    ON polymarket_wallet_thesis_lines (event_slug, wallet, side);
CREATE INDEX IF NOT EXISTS idx_wallet_thesis_lines_wallet
    ON polymarket_wallet_thesis_lines (wallet, last_traded_at DESC);
