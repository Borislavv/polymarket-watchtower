# Architecture

The watchtower runs as a single Go process that hosts a small set of
long-running loops and workers, all wired by `internal/app`. PostgreSQL is
the source of truth for everything decisions are made from: categories,
markets, outcomes, traders, trades, and alerts.

## Wired graph

```
                       ┌─────────────────────────────┐
                       │  gamma /tags + /markets     │
                       │  data-api /trades           │
                       └──────────────┬──────────────┘
                                      │
   ┌──────────────────────────────────┼──────────────────────────────────┐
   │                                  │                                  │
┌──┴─────────────┐               ┌────┴─────┐                  ┌─────────┴──────────┐
│ discover.Loop  │ registry      │ collect  │ persist BEFORE   │ BackfillWorker     │
│  (whitelist)   ├──────────────▶│ (60 s)   │ observe          │  fills history     │
│                │ in-process    │          │                  │  per market        │
│  persist.Sink: │               │ persist  │                  │  newest → oldest   │
│  categories +  │               │ trades + │                  │  bounded by API    │
│  markets +     │               │ traders  │                  │  offset cap 3000   │
│  outcomes +    │               │ via Sink │                  └────────────────────┘
│  inactivate    │               │          │
└────────────────┘               └────┬─────┘
                                      │ per-trade Observe
                                      ▼
                            ┌──────────────────────┐
                            │ detect.Loop          │
                            │  • dbbaseline.Provider│
                            │    (PG-backed stats) │
                            │  • cluster window    │
                            │  • TryCreatePending  │
                            │    (single + cluster │
                            │     dedup keys)      │
                            └────────────┬─────────┘
                                         │ realtime fanout
                                         ▼
                            ┌──────────────────────────┐
                            │ Fanout (log + webhook)   │
                            │ ── no Telegram here ──   │
                            └──────────────────────────┘
                                         │
                                         │ Telegram is isolated:
                                         ▼
                            ┌──────────────────────────┐
                            │ polymarket_alerts        │
                            │   status = pending       │
                            └────────────┬─────────────┘
                                         │ atomic claim
                                         │ UPDATE … FOR UPDATE SKIP LOCKED
                                         ▼
                            ┌──────────────────────────┐
                            │ alertsender.Worker       │
                            │   • render via alerting  │
                            │     .FormatTelegramMsg   │
                            │   • Bot.SendHTML(…)      │
                            │   • MarkSent / MarkFailed│
                            └────────────┬─────────────┘
                                         ▼
                            ┌──────────────────────────┐
                            │ internal/infra/telegram  │
                            │   Bot API HTTP transport │
                            └──────────────────────────┘
```

## Loops and workers

- **`discover.Loop`** (`internal/app/usecase/discover`): every
  `DISCOVER_INTERVAL`, pulls `/tags` + `/events` + `/markets` from Gamma,
  applies `CATEGORY_WHITELIST` (slug+label only), updates the in-process
  registry, and hands the result to `persist.Sink.PersistDiscovery` which
  upserts categories, markets, market↔category links, and outcomes, then
  marks markets that disappeared as inactive within the whitelisted scope.
- **`collect.Loop`** (`internal/app/usecase/collect`): every
  `COLLECT_INTERVAL`, fans out per market, pulls new trades since the
  cursor, **persists the batch FIRST** (so the DB-baseline read sees
  them), then calls the per-trade detector. The cursor is sourced from
  `polymarket_trades.MAX(traded_at)` via `persist.Sink.LatestTradedAt`
  when Postgres is wired.
- **`backfill.Worker`** (`internal/app/usecase/backfill`): every
  `BACKFILL_INTERVAL`, resets any market wedged in `running` for longer
  than `BACKFILL_STALE_AFTER`, claims the next `BACKFILL_WORKERS`
  candidates (`pending` or `partial_api_limit`), and pages `/trades`
  newest→oldest until either the upstream returns a short page
  (`completed`) or the offset-3000 cap is hit (`partial_api_limit`). Each
  page is persisted before advancing so progress is durable.
- **`detect.Loop`** (`internal/app/usecase/detect`): per trade, queries
  `dbbaseline.Provider` for the per-(market, outcome) baseline statistics
  (count, total, median, mean, p95, observed span), enforces the
  lifecycle + readiness gates, scores against the absolute and multiplier
  tier ladders, takes the conservative-MIN, and inserts an alert row via
  `AlertRepository.TryCreatePending`. On a fresh insert (the alert was
  not a duplicate), the realtime emitter (log + webhook) is notified.
- **`alertsender.Worker`** (`internal/app/usecase/alertsender`): every
  `ALERT_SENDER_INTERVAL`, resets stale `sending` rows back to `pending`,
  atomically claims a batch of pending alerts (UPDATE … IN (SELECT …
  FOR UPDATE SKIP LOCKED) RETURNING *), renders each via
  `alerting.FormatTelegramMessage`, and posts via
  `internal/infra/telegram.Bot.SendHTML`. Successful sends are marked
  `sent`; failures bump `send_attempts` and return to `pending` for
  retry.

## Persistence model (what the DB owns)

| Table                           | Rows                                                                        |
|---------------------------------|-----------------------------------------------------------------------------|
| `polymarket_categories`         | One per Gamma tag. `enabled` is the local whitelist; `active` is upstream.  |
| `polymarket_markets`            | One per condition id; carries `backfill_status` lifecycle.                  |
| `polymarket_market_categories`  | Many-to-many link.                                                          |
| `polymarket_market_outcomes`    | Per (market, token) human label (Yes/No/…); upserted by `persist.Sink`.     |
| `polymarket_traders`            | One per wallet, lazy-upserted from trades.                                  |
| `polymarket_trades`             | Single source of truth for every alerting decision; idempotent via dedup_key. |
| `polymarket_alerts`             | The Telegram queue; UNIQUE dedup_key prevents double-send.                  |

See [doc/persistence.md](persistence.md) for column-level detail.

## Dedup keys

```
single:<strategy_version>:<trade_dedup_key>
cluster:<strategy_version>:<category_id>:<window_start_unix>
```

- The trade portion of `<trade_dedup_key>` is computed exactly the same
  way as the trade row's own `dedup_key` (prefer external_id; fall back to
  composite SHA-256). So the single-trade alert is idempotent across
  processes and restarts.
- For the cluster alert, `window_start_unix` is `floor(now /
  CLUSTER_COOLDOWN)`. Two cluster fires landing in the same bucket dedup;
  the next bucket gets a fresh key.

## Memory mode (local/debug)

When `POSTGRES_DSN` is empty the app drops into an in-memory shape:
- `discover` and `collect` skip persistence.
- `detect.Loop` uses the in-process `baseline.Baseline` reservoir.
- Alerts go straight to the realtime fanout, which re-includes a
  synchronous `TelegramSink` for developer convenience.
- No backfill, no cross-restart alert dedup, no alert sender worker.

This shape exists for local exploration only — production must run with
Postgres and the DB-backed flow above.
