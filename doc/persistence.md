# Persistence

PostgreSQL is the source of truth for everything the watchtower's
decision path consumes: categories, markets, outcomes, traders, trades,
and alerts. This document describes the schema, the sqlc-generated
interface, and the queue patterns the workers rely on.

## What runs against the DB

| Component                | Reads from                                  | Writes to                          |
|--------------------------|---------------------------------------------|------------------------------------|
| `discover.Loop`          | —                                           | `polymarket_categories`, `polymarket_markets`, `polymarket_market_categories`, `polymarket_market_outcomes` |
| `collect.Loop`           | `polymarket_trades` (cursor)                | `polymarket_trades`, `polymarket_traders` |
| `backfill.Worker`        | `polymarket_markets` (claim)                | `polymarket_markets` (status), `polymarket_trades`, `polymarket_traders` |
| `detect.Loop`            | `polymarket_trades` (baseline distribution) | `polymarket_alerts` (TryCreatePending) |
| `alertsender.Worker`     | `polymarket_alerts` (claim, by status)      | `polymarket_alerts` (status, send_attempts) |

When `POSTGRES_DSN` is empty the app drops into an in-memory shape
intended for local exploration only — no backfill, no cross-restart
dedup, no sender worker.

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

The concrete types live in `internal/infra/repository` and wrap the
sqlc-generated `internal/infra/postgres/sqlc` package. Nothing above this
layer imports sqlc — every conversion between `pgtype.Timestamptz` and
`time.Time` happens here. The shipped surface:

```
CategoryRepository
    UpsertSeen      MarkSeenInactive    ApplyWhitelist     ListEnabled

MarketRepository
    UpsertSeen      MarkSeenInactive    UpsertOutcome
    BeginBackfill   CompleteBackfill    FailBackfill       ResetStaleRunning
    ListActiveForBackfill               ListActiveForCollection
    GetByConditionID

TraderRepository
    UpsertSeen      GetByWallet         Stats(traderID, since)

TradeRepository
    UpsertBatch                      // dedup_key UNIQUE; same trade twice = one row
    Distribution(BaselineQuery)      // count + total + mean + median + p95 + span (1 roundtrip)
    SummarizeBaseline(BaselineQuery) // compact roll-up (no median/p95)
    ListBaseline(BaselineQuery)      // raw samples (used by tests)
    ExistingDedupKeys(marketID, []string)
    LatestTradedAt(marketID)         // collector cursor
    OldestTradedAt(marketID)

AlertRepository
    TryCreatePending(NewAlert)       // ON CONFLICT DO NOTHING
    ClaimPending(limit)              // UPDATE … FOR UPDATE SKIP LOCKED → 'sending'
    MarkSent(id, telegramMessageID)  // 'sending' → 'sent'
    MarkFailed(id, errMsg)           // 'sending' → 'pending' (bump send_attempts)
    ResetStaleSending(cutoff)        // crash recovery
    Exists(dedupKey)
    LatestClusterForMarket(marketID, strategyVersion)
```

Repositories are pure data access. Severity decisions, API orchestration,
worker scheduling — none of it lives here.

## Alert queue mechanics

Two reasons the alert table is shaped as a queue rather than a fire-and-
forget log:

1. **Cross-restart dedup.** The UNIQUE `dedup_key` index is the single
   source of truth for "have we already alerted on this?". A detector
   restart re-observing the same trade can re-create no new row.
2. **Single-delivery-per-row.** Multiple sender processes must be able
   to run safely. The naive `SELECT … FOR UPDATE SKIP LOCKED` over an
   autocommit connection releases the lock the moment the SELECT
   returns — both processes see the same batch.

The shipped queue avoids this with a transient `sending` status (added
in `00002_alerts_sending_state.up.sql`):

```
   pending ─claim─▶ sending ─MarkSent────▶ sent
                        │
                        └─MarkFailed────▶ pending (with bumped send_attempts)
```

`ClaimPending` is:

```sql
UPDATE polymarket_alerts SET status = 'sending', updated_at = NOW()
WHERE id IN (
    SELECT id FROM polymarket_alerts
    WHERE status = 'pending'
    ORDER BY created_at
    LIMIT $1 FOR UPDATE SKIP LOCKED
)
RETURNING *;
```

A concurrent sender races on the inner SELECT, takes a disjoint batch,
and the UPDATE commits atomically. Crashed senders are recovered by
`ResetStaleSendingAlerts`, which the worker calls on every tick.

## sqlc layout

```
db/
  migrations/
    00001_init.up.sql        / 00001_init.down.sql        -- base schema
    00002_alerts_sending_state.up.sql / .down.sql         -- transient sending status
  queries/
    categories.sql           -- one query per repo method
    markets.sql
    traders.sql
    trades.sql
    alerts.sql
  sqlc.yaml                  -- engine: postgresql; pgx/v5
internal/
  infra/
    postgres/sqlc/           -- generated by `sqlc generate`
    repository/              -- hand-written wrappers; this is the boundary
```

`sqlc generate` is run locally before commit. The generated package
emits pointers for nullable types so the wrappers can use simple
`*time.Time` / `*string` semantics above the boundary.

## Migration tool

`golang-migrate/migrate` with the embedded source driver. The migrator
is invoked from `internal/infra/postgres.Migrate` when `POSTGRES_AUTO_MIGRATE=true`
(the default) and via `go run ./cmd/cli migrate -dsn …` for ad-hoc runs.
The binary embeds `db/migrations/` so no external file copy is required.

## Operational notes

- **Backups:** WAL-archiving recommended for any deployment longer than
  a demo. Trade volume on whitelisted categories is small enough that
  nightly logical dumps suffice for most setups.
- **Indexes:** every query in `db/queries/*.sql` has a matching index in
  `00001_init.up.sql`. Adding a hot query that scans the trade table
  without one will visibly slow the detector.
- **Retention:** there is no automatic deletion. The per-bucket multiplier
  ladder gets no benefit from data older than a year (median is already
  robust); operators who want to cap storage can do so safely with a
  monthly partition or a periodic DELETE.
- **Crash recovery:** the backfill worker resets stale `running` markets
  on every tick (`BACKFILL_STALE_AFTER`, default 15 m); the alert sender
  resets stale `sending` rows on every tick (`ALERT_SENDER_STALE_AFTER`,
  default 5 m). Both are idempotent — they cost one UPDATE with no rows
  matched in the steady state.
