# polymarket-watchtower

A Go worker that watches the public Polymarket trade feed and surfaces
**individual abnormal bets** — the kind that suggest some players may know
something. It does this in two layers:

1. **Per-trade anomaly detector.** Every public trade is compared against the
   recent baseline of trade USD notionals for its own
   `(category, market, outcome)` bucket. Two independent ladders fire,
   higher-severity wins:
   - **Multiplier ladder** — `notional / baseline-median` ≥ `{30, 100, 1000}×`
     → `info / warning / critical`. Skipped when the bucket has fewer than
     `MIN_BASELINE_TRADES` samples (guards against the "first trade looks like
     ∞×" false-positive class).
   - **Absolute USD ladder** — applied regardless of baseline:
     `≥ {$3k, $10k, $100k}` → `info / warning / critical`. This catches whales
     on cold markets.
2. **Category cluster (HARD) alert — `CategoryWatchRequired`.** When in a
   sliding window (default `1h`) a single category sees ≥ N anomalous trades
   from ≥ M unique wallets totalling ≥ X USD, the watchtower fires a HARD
   alert — this is the "many sharks circling one category" signal we care most
   about.

The legacy aggregate-rate signals (`trade_rate`, `notional_rate`,
`avg_size`) are kept as **supporting** Grafana panels but **never** drive
alerts on their own — they were the wrong unit of detection.

## Pipeline

```
Gamma /events,/markets,/tags          Data API /trades?market=…
        │                                       │
        ▼                                       ▼
   discover  ─▶ persist.Sink ─▶ Postgres ◀───┬─ collect (per market)
   (categories, markets,        ▲           │      ├─ persist BEFORE observe
    outcomes, mark-inactive)    │           │      ▼
                                │           │   detect.Observe(market, trade)
                                │           │      ├─ dbbaseline.Provider      ──▶ polymarket_trades
                                │           │      ├─ score.Score                  (PERCENTILE_CONT 1 roundtrip)
                                │           │      ├─ cluster.Observe             (in-process window)
                                │           │      └─ AlertRepository.TryCreatePending
                                │           │             └─▶ polymarket_alerts   (UNIQUE dedup_key)
                                │           │
                                │           └─ backfill.Worker  (pending → completed | partial_api_limit)
                                │              fills polymarket_trades up to Data API offset cap 3000
                                │
   alertsender.Worker  ◀───────  polymarket_alerts (status = pending)
       │     atomic claim: UPDATE … IN (SELECT … FOR UPDATE SKIP LOCKED) RETURNING *
       ▼
   internal/infra/telegram.Bot.SendHTML  ──▶  Telegram chat
       │
       ▼
   MarkSent / MarkFailed  (status → 'sent' / back to 'pending' with bumped attempts)
```

Postgres is the source of truth for every decision. The detector reads the
baseline from `polymarket_trades`; the BackfillWorker fills missing history
within the upstream Data API's offset-3000 cap; alerts are inserted with a
UNIQUE dedup_key so concurrent detection and restarts cannot double-send;
the sender worker drains the queue through an atomic claim flow.

Without `POSTGRES_DSN` the app runs in-memory only — for local exploration
on a single process. Production must run with the DB-backed flow above.

## Layout

```
cmd/app/                            # worker binary
cmd/cli/                            # reserved for ad-hoc commands
internal/
  app/                              # composition root + config
  domain/
    model/
      market/, trade/, anomaly/     # entities
    vo/                             # value objects (ids)
  app/usecase/
    discover/                       # Gamma market+tag refresh loop
    collect/                        # Data-API trade pull loop, feeds detect.Observe per trade
    aggregate/                      # supporting rolling-bucket engine (gauges)
    analytics/
      baseline/                     # per-(cat,market,outcome) trade-notional reservoir + stats
      score/                        # pure trade-anomaly scorer (multiplier + absolute ladders)
      cluster/                      # per-category sliding-window cluster detector (HARD alerts)
    detect/                         # orchestrates baseline/score/cluster + emits findings
  infra/
    polymarket/{httpx,gamma,dataapi}# upstream adapters
    ratelimit/                      # x/time/rate token buckets
    metrics/                        # private Prometheus registry
    http/                           # /metrics + /healthz
    alerting/                       # log + webhook + telegram sinks (Kind-aware formatter)
    log/, shutdown/                 # zerolog + signal-driven graceful stop
deploy/
  docker-compose.yml                # app + prometheus + grafana
  prometheus/prometheus.yml
  grafana/provisioning/...          # auto-loaded datasource + dashboard
  grafana/dashboards/watchtower.json
```

## What an alert looks like

**Single-trade anomaly (Telegram):**

```
[CRIT] Polymarket CRITICAL — single bet anomaly
rule: multiplier+absolute_tier
Will Trump win Florida?
outcome: `Yes`  side: `BUY`
size: $250,000  (500000.00 @ 0.5000)
wallet: 0xabc1234567890def1234567890abcdef12345678
category: Politics
baseline: median $42  mean $58  p95 $410  N=812  window=168h
multiplier: x5952
absolute tier crossed: $100,000
at: 2026-05-17T14:23:11Z
[open market](https://polymarket.com/event/trump-florida-2028)  •  [open in Grafana](http://localhost:3000/d/watchtower-main/?...&var-category=Politics&var-market=trump-florida-2028&from=…&to=…)
```

**Category-cluster HARD alert (`CategoryWatchRequired`):**

```
[HARD] Polymarket — CATEGORY WATCH REQUIRED
category: Politics
7 anomalous trades from 5 unique wallets totalling $312,500 in the last 1h

recent contributors:
  • $100,000 on Will Trump win Florida? — 0xabc1…5678 Yes
  • $80,000  on Will Trump win Florida? — 0xfeed…78ab Yes
  • …

at: 2026-05-17T14:24:02Z
[open market](https://polymarket.com/event/trump-florida-2028)  •  [open in Grafana](…&var-category=Politics&from=…&to=…)
```

The Grafana link is a deep-link to the dashboard with `var-category`,
`var-market`, and `from`/`to` set to `±GRAFANA_CONTEXT_WINDOW` around the
trade — one click and you're looking at the right time window.

## Metrics

Per-trade and per-category metrics drive the new dashboard. Per-market gauges
are kept for supporting panels (bounded by `MAX_MARKETS`).

| Metric | Labels | Kind | Purpose |
|---|---|---|---|
| `watchtower_trade_size_usd` | — | histogram | Every ingested trade's USD notional |
| `watchtower_trade_anomaly_multiplier` | — | histogram | Observed `notional/baseline-median` on fire |
| `watchtower_trade_anomalies_total` | `severity, category, reason` | counter | Single-trade anomalies emitted |
| `watchtower_category_anomalous_trades_total` | `category, severity` | counter | Anomalous trades per category |
| `watchtower_category_anomalous_notional_usd_total` | `category, severity` | counter | Anomalous USD per category |
| `watchtower_category_hard_alerts_total` | `category` | counter | HARD `CategoryWatchRequired` alerts |
| `watchtower_baseline_buckets` | — | gauge | Live `(cat, market, outcome)` baseline buckets |
| `watchtower_telegram_alerts_sent_total` | `severity` | counter | Telegram successes |
| `watchtower_telegram_alert_errors_total` | `severity` | counter | Telegram failures |
| `watchtower_collect_trades_total` | `market` | counter | Trades ingested per market |
| `watchtower_collect_notional_usd_total` | `market` | counter | Notional USD ingested per market |
| `watchtower_window_*` | `market, window` | gauge | Supporting per-market rate gauges (not alerted) |
| `watchtower_upstream_*` | `api, endpoint, status` | counter / histogram | Upstream traffic |

Wallet/trade IDs/tx hashes deliberately do **not** appear as label values — they
go in logs and alert payloads. Polymarket has too many of them to safely use as
Prometheus dimensions.

## Running

```bash
cp .env.example .env
make run        # local worker, real Polymarket APIs (in-memory only)
# or
make up         # docker compose: app + prometheus + grafana + postgres
```

Local URLs after `make up`:

- App health: <http://localhost:9090/healthz>
- App metrics: <http://localhost:9090/metrics>
- Prometheus: <http://localhost:9091>
- Grafana: <http://localhost:3000> (anonymous viewer; `admin/admin` to edit)
  - Dashboard: **Polymarket Watchtower** (`/d/watchtower-main`)
- Postgres: `postgres://watchtower:watchtower@localhost:5433/watchtower`
  (host port 5433 to avoid clashing with a local 5432)

### PostgreSQL persistence (production shape)

When `POSTGRES_DSN` is set the watchtower runs its production graph:
write-through of every discovery sweep and collected trade; a continuous
BackfillWorker filling historical trades; the detector reads its baseline
from the DB; every fired alert is `INSERT ... ON CONFLICT DO NOTHING`
into `polymarket_alerts`; an AlertSender worker drains the queue and
delivers via Telegram. Restart-safe and concurrent-process-safe by
construction.

Bring just Postgres up, apply migrations, run the repository and DB-
detector integration tests. Run them serially across packages (`-p 1`):
every live integration suite TRUNCATEs the shared database on setup, so
parallel packages collide.

```bash
make pg-up          # start the postgres service
make migrate        # apply embedded SQL migrations
make pg-test        # run repository integration tests
POSTGRES_TEST_DSN="postgres://watchtower:watchtower@localhost:5433/watchtower?sslmode=disable" \
  go test -p 1 -count=1 \
    ./internal/infra/repository/... \
    ./internal/app/usecase/analytics/dbbaseline/... \
    ./internal/app/usecase/detect/...
make sqlc           # regenerate db code (requires sqlc binary on PATH)
```

The full stack (compose `make up`) auto-applies migrations on app boot.

## Tests

```bash
make test            # all unit + httptest pipeline tests; no network
go test -race ./...  # race detector across the same set
```

Hermetic by default — every adapter test uses an `httptest.Server`, and the
end-to-end pipeline test wires real `discover` / `collect` / `detect` against
fakes for Gamma, Data API, and Telegram.

Live integration tests are opt-in:

```bash
POLYMARKET_INTEGRATION=1 go test -tags=integration ./internal/infra/polymarket/...
```

These hit `gamma-api.polymarket.com` and `data-api.polymarket.com` with
conservative limits.

## Configuration cheat-sheet

Every knob is an env var; see `.env.example`. The headline knobs:

| Env | Default | Meaning |
|---|---|---|
| `SINGLE_TRADE_MULTIPLIERS` | `30,100,1000` | Per-trade multiplier ladder (× baseline median) |
| `SINGLE_TRADE_ABSOLUTE_USD` | `3000,10000,100000` | Per-trade absolute USD ladder |
| `MIN_BASELINE_TRADES` | `20` | Multiplier ladder skipped below this sample count |
| `BASELINE_WINDOW` | `168h` | Lookback for the per-bucket reservoir |
| `HARD_ALERT_WINDOW` | `1h` | Cluster window for `CategoryWatchRequired` |
| `HARD_ALERT_MIN_ANOMALOUS_TRADES` | `5` | Anomalous trades required in window |
| `HARD_ALERT_MIN_UNIQUE_TRADERS` | `3` | Distinct wallets required |
| `HARD_ALERT_MIN_TOTAL_NOTIONAL_USD` | `25000` | Cluster USD floor |
| `HARD_ALERT_COOLDOWN` | `1h` | Per-category cooldown |
| `GRAFANA_BASE_URL` / `GRAFANA_DASH_UID` | `http://localhost:3000` / `watchtower-main` | Deep-link target |
| `POLYMARKET_PUBLIC_BASE_URL` | `https://polymarket.com` | Used in alert "open market" links |

## Polymarket API notes (verified live, 2026-05-17)

- Data API `/trades?market=<conditionId>` returns trades **DESC by timestamp
  within the market**. There is no server-side `since` parameter; we filter
  client-side and stop paging when a page contains trades older than the
  cutoff.
- `filterType` is `CASH | TOKENS` (a min-value filter on the trade notional).
  The earlier `filterType=TIMESTAMP` we used was rejected with HTTP 400 —
  that's why the previous version of the analyzer never ingested anything.
- Upstream caps `offset ≤ 3000` ("max historical activity offset of 3000
  exceeded"); the client refuses to send a higher offset.
- Wallet is exposed as `proxyWallet` on each trade row. We propagate it into
  alert payloads so a human can pivot from "5 anomalous trades on Politics"
  to "who's behind them".
- Outcome label (`Yes` / `No` / `Trump` / …) comes from Gamma `/markets`
  via the `outcomes` array, mapped to the trade's `asset` by index in
  `clobTokenIds`.

## Rate limits

The defaults sit at roughly 70% of Polymarket's documented per-endpoint caps
(Gamma `/markets`: 300/10s, Data API `/trades`: 200/10s). All upstream calls
go through a token bucket with exponential backoff + jitter on 429 / 5xx.
