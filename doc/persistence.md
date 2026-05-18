# Persistence

This document specifies the PostgreSQL schema, sqlc layout, repository
interfaces, and migration plan. Phase 2 has now landed the foundation
(stages 1–4 below); the live detector still reads in-memory state, with
DB-backed reads scheduled for the next changeset (stages 5–8).

## Phase 2 implementation status

| # | Stage | Status |
|---|---|---|
| 1 | Schema migrations + sqlc.yaml + Postgres compose + `POSTGRES_DSN` env | ✅ shipped |
| 2 | sqlc generation + repository wrappers + integration tests | ✅ shipped |
| 3 | CategorySync + MarketDiscovery write-through (alongside in-memory) | ✅ shipped |
| 4 | CollectWorker writes trades to DB (alongside in-memory feed) | ✅ shipped |
| 5 | DB-backed baseline (replace `baseline.Baseline` with `TradeRepo.ListBaseline`) | next session |
| 6 | BackfillWorker (full-history ingest for active whitelisted markets) | next session |
| 7 | Alert dedup wiring + AlertSenderWorker | next session |
| 8 | Delete in-memory baseline/cluster state | session after that |

Today's commit is **safe to run alongside the existing detector**: when
`POSTGRES_DSN` is empty the app stays in Phase-1 mode (in-memory only);
when set, write-through runs on a separate path and a DB-write failure
never blocks an alert.

## Why a database

The current in-memory baseline is shallow. Span values like
`17h28m of fetched history` reflect "how long the process has been up,"
not the market's actual history. Real shark-hunting needs:

1. **Restart-safe baselines.** A new process starts with the same history
   as the old one.
2. **Real historical context.** A market that's been quiet for weeks with
   $5–$20 bets should make a sudden $50k bet look obviously anomalous —
   that requires per-market history older than the process.
3. **Dedup that survives restart.** No re-firing on the trades that fell
   inside the boot lookback.
4. **Trader profiles.** "This wallet's typical bet size over the last
   90 days" is the kind of context that turns a generic large-bet alert
   into actionable signal.

## Schema

Conventions: every primary key is `BIGSERIAL`; every timestamp is
`TIMESTAMPTZ`; every multi-column unique constraint is indexed; deletes
cascade only inside the polymarket_* graph.

```sql
-- Categories (Polymarket "tags")
CREATE TABLE polymarket_categories (
  id           BIGSERIAL PRIMARY KEY,
  external_id  TEXT NOT NULL UNIQUE,        -- gamma tag id (numeric, kept as text)
  slug         TEXT NOT NULL,
  name         TEXT NOT NULL,
  enabled      BOOLEAN NOT NULL DEFAULT FALSE,  -- in CATEGORY_WHITELIST
  active       BOOLEAN NOT NULL DEFAULT TRUE,   -- still returned by gamma /tags
  created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_categories_enabled ON polymarket_categories(enabled) WHERE enabled;

-- Markets (per condition_id from Polymarket)
CREATE TABLE polymarket_markets (
  id                          BIGSERIAL PRIMARY KEY,
  condition_id                TEXT NOT NULL UNIQUE,
  slug                        TEXT NOT NULL,
  question                    TEXT NOT NULL,
  event_slug                  TEXT,
  event_title                 TEXT,
  start_date                  TIMESTAMPTZ,
  end_date                    TIMESTAMPTZ,
  active                      BOOLEAN NOT NULL DEFAULT TRUE,
  closed                      BOOLEAN NOT NULL DEFAULT FALSE,
  last_seen_at                TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  backfill_status             TEXT NOT NULL DEFAULT 'pending',
    -- pending | in_progress | completed | partial_api_limit | failed | skipped
  backfill_oldest_fetched_at  TIMESTAMPTZ,
  backfill_newest_fetched_at  TIMESTAMPTZ,
  backfill_attempts           INT NOT NULL DEFAULT 0,
  backfill_last_error         TEXT,
  backfill_completed_at       TIMESTAMPTZ,
  created_at                  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at                  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_markets_active_backfill ON polymarket_markets(active, backfill_status)
  WHERE active;
CREATE INDEX idx_markets_end_date ON polymarket_markets(end_date) WHERE active;

-- Market ↔ category (many-to-many; a market can belong to multiple)
CREATE TABLE polymarket_market_categories (
  market_id   BIGINT NOT NULL REFERENCES polymarket_markets(id) ON DELETE CASCADE,
  category_id BIGINT NOT NULL REFERENCES polymarket_categories(id) ON DELETE CASCADE,
  PRIMARY KEY (market_id, category_id)
);

-- Market outcomes (Yes/No tokens per market)
CREATE TABLE polymarket_market_outcomes (
  id        BIGSERIAL PRIMARY KEY,
  market_id BIGINT NOT NULL REFERENCES polymarket_markets(id) ON DELETE CASCADE,
  token_id  TEXT NOT NULL,
  label     TEXT NOT NULL,
  UNIQUE (market_id, token_id)
);

-- Traders (one row per wallet)
CREATE TABLE polymarket_traders (
  id             BIGSERIAL PRIMARY KEY,
  wallet_address TEXT NOT NULL UNIQUE,
  first_seen_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  last_seen_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Trades (one row per public trade event)
CREATE TABLE polymarket_trades (
  id            BIGSERIAL PRIMARY KEY,
  market_id     BIGINT NOT NULL REFERENCES polymarket_markets(id),
  trader_id     BIGINT REFERENCES polymarket_traders(id),  -- nullable in early ingest
  outcome_token TEXT NOT NULL,
  side          TEXT NOT NULL CHECK (side IN ('BUY','SELL')),
  price         DOUBLE PRECISION NOT NULL CHECK (price > 0 AND price < 1),
  size_shares   DOUBLE PRECISION NOT NULL,
  notional_usd  DOUBLE PRECISION NOT NULL,
  traded_at     TIMESTAMPTZ NOT NULL,
  external_id   TEXT,        -- the `id` field from /trades (content hash on Polymarket)
  tx_hash       TEXT,
  ingested_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  dedup_key     TEXT NOT NULL UNIQUE
    -- prefer external_id; fall back to sha256(market_id|outcome|wallet|traded_at|price|size|side)
);
CREATE INDEX idx_trades_market_outcome_time
  ON polymarket_trades(market_id, outcome_token, traded_at DESC);
CREATE INDEX idx_trades_market_time
  ON polymarket_trades(market_id, traded_at DESC);
CREATE INDEX idx_trades_trader_time
  ON polymarket_trades(trader_id, traded_at DESC)
  WHERE trader_id IS NOT NULL;

-- Alerts (dedup table + send state)
CREATE TABLE polymarket_alerts (
  id                  BIGSERIAL PRIMARY KEY,
  dedup_key           TEXT NOT NULL UNIQUE,
  kind                TEXT NOT NULL,      -- trade_anomaly | category_watch
  reason              TEXT NOT NULL,      -- LargeRareBet | WhaleClusterDetected | …
  severity            TEXT NOT NULL,      -- info | warning | critical | hard
  market_id           BIGINT REFERENCES polymarket_markets(id),
  trader_id           BIGINT REFERENCES polymarket_traders(id),
  trade_id            BIGINT REFERENCES polymarket_trades(id),
  payload             JSONB NOT NULL,
  status              TEXT NOT NULL DEFAULT 'pending', -- pending | sent | failed
  telegram_message_id BIGINT,
  send_attempts       INT NOT NULL DEFAULT 0,
  last_send_error     TEXT,
  sent_at             TIMESTAMPTZ,
  created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_alerts_status_created
  ON polymarket_alerts(status, created_at)
  WHERE status = 'pending';
```

## Dedup key formats

`STRATEGY_VERSION` is an env var (default `v1`). Bumping it invalidates the
existing dedup state and lets a retuned strategy re-alert on previously
seen trades.

- **Trade row:** prefer `external_id` from the Data API; fall back to
  `sha256(market_id|outcome|wallet|traded_at|price|size|side)`. The
  `dedup_key` column persists whichever is computed at insert time.
- **Single-trade alert:** `single:<STRATEGY_VERSION>:<trade.dedup_key>`.
- **Cluster alert:**
  `cluster:<STRATEGY_VERSION>:<market_id>:<outcome_token>:<window_start_unix_minute>`.

## Repository interfaces

```go
package repo

type MarketRepo interface {
    UpsertSeen(ctx context.Context, markets []Market) (inserted, updated int, err error)
    MarkInactiveMissing(ctx context.Context, seenConditionIDs []string) (int, error)
    ListActiveForBackfill(ctx context.Context, limit int) ([]Market, error)
    UpdateBackfillState(ctx context.Context, marketID int64, st BackfillState) error
}

type CategoryRepo interface {
    UpsertSeen(ctx context.Context, cats []Category) (int, error)
    SetEnabled(ctx context.Context, slugOrName []string) error  // applies CATEGORY_WHITELIST
    ListEnabled(ctx context.Context) ([]Category, error)
}

type TradeRepo interface {
    UpsertBatch(ctx context.Context, trades []Trade) (inserted int, err error)
    ListBaseline(ctx context.Context, q BaselineQuery) (BaselineSamples, error)
    ListClusterWindow(ctx context.Context, marketID int64, outcome string, window time.Duration) ([]Trade, error)
    LatestTradedAt(ctx context.Context, marketID int64) (time.Time, bool, error)
}

type TraderRepo interface {
    UpsertSeen(ctx context.Context, wallets []string) error
    Stats(ctx context.Context, traderID int64, since time.Time) (TraderStats, error)
}

type AlertRepo interface {
    // ON CONFLICT (dedup_key) DO NOTHING returning the row that exists.
    // created=true when this call inserted; false when it already existed.
    TryCreatePending(ctx context.Context, a NewAlert) (alert Alert, created bool, err error)
    ListPending(ctx context.Context, limit int) ([]Alert, error)
    MarkSent(ctx context.Context, id int64, telegramMessageID int64) error
    MarkFailed(ctx context.Context, id int64, errMsg string) error
}
```

Repositories are pure data access — no severity decisions, no API calls.
Detector and worker code orchestrates.

## sqlc layout

```
db/
  migrations/
    00001_init.up.sql         -- schema above
    00001_init.down.sql
  queries/
    categories.sql            -- one query per repo method
    markets.sql
    market_outcomes.sql
    traders.sql
    trades.sql
    alerts.sql
  sqlc.yaml                   -- engine: postgresql, queries dir, gen dir
internal/
  repo/                       -- generated by sqlc + hand-written wrappers
    categories.sql.go
    markets.sql.go
    ...
    repo.go                   -- the Go interfaces above; sqlc impl satisfies them
```

`sqlc generate` is run locally before commit. CI verifies the generated
code is up to date via `sqlc diff`.

## Migration tool

`golang-migrate/migrate` with the file driver, run from `cmd/migrate`. On
`docker compose up`, the app container blocks on a one-shot
`migrate-runner` service that applies pending migrations before
`watchtower` starts.

## Phase-2 implementation stages (in order)

Each stage is a self-contained PR.

| # | Stage | What | Risk |
|---|---|---|---|
| 1 | Schema | migrations + `sqlc.yaml` + queries.sql + compose Postgres service + `POSTGRES_DSN` env. App does not yet connect. | low |
| 2 | Generated code + repo interfaces | run sqlc; commit generated code; add `internal/repo` interfaces + sqlc impls; unit tests against testcontainers. App still does not use them. | low |
| 3 | Category + Market sync to DB | new `CategorySyncWorker` + `MarketDiscoveryWorker` writing to DB. The existing in-memory registry continues to feed detect during transition. | medium |
| 4 | Trade persistence | `CollectWorker` writes trades to DB in addition to feeding the in-memory baseline. Dedup proven. | medium |
| 5 | DB-backed baseline | swap `baseline.Baseline` for `TradeRepo.ListBaseline`. **High-risk:** every numeric test must pass with the new source. | **high** |
| 6 | Backfill worker | continuous backfill for markets with `status IN ('pending','partial_api_limit')`. Idempotent on retry. | medium |
| 7 | Alert dedup table | detect emits via `AlertRepo.TryCreatePending`; new `AlertSenderWorker` drains pending → Telegram. Cluster cooldown moves to DB. | medium |
| 8 | Delete in-memory state | drop `aggregate.MarketRegistry`, `baseline.Baseline`, `cluster.Detector` once the DB path is proven. | low |

## Operational notes

- **Backups:** WAL-archiving recommended for any deployment longer than
  a demo. Trade volume on whitelisted categories should be modest enough
  that nightly logical dumps suffice.
- **Indexes:** every query in `queries/*.sql` is reviewed for an index
  match before it ships. The CI lint will fail PRs that add a query
  without a matching index.
- **Retention:** there is no automatic deletion. A 1-year retention is the
  practical ceiling for the per-bucket baseline (the multiplier ladder
  doesn't benefit from older data and median is already robust).
