# polymarket-watchtower

A Go worker that consumes Polymarket public data, builds rolling aggregates per
market and category, and fires anomaly findings when activity spikes against
its baseline (x30 / x100 / x1000 by default). Metrics are exposed for
Prometheus and a Grafana dashboard ships in `deploy/`.

## Architecture

```
cmd/
  app/                              # worker binary
  cli/                              # reserved for future ad-hoc commands
internal/core/
  app/                              # composition root + config
  domain/
    market/, trade/, anomaly/, vo/  # entities + value objects
  usecase/
    discover/                       # Gamma market+tag refresh loop
    collect/                        # Data-API trade pull loop
    aggregate/                      # in-memory rolling-bucket engine
    detect/                         # spike rules + per-rule cooldown
  infra/
    polymarket/
      httpx/                        # shared JSON client w/ backoff + limiter
      gamma/                        # Gamma adapter
      dataapi/                      # Data-API adapter
    ratelimit/                      # x/time/rate per-host buckets
    metrics/                        # private prometheus registry
    http/                           # fasthttp /metrics + /healthz
    alerting/                       # log + webhook + telegram sinks
    log/, shutdown/                 # zerolog + signal-driven graceful stop
deploy/
  docker-compose.yml                # app + prometheus + grafana
  prometheus/prometheus.yml
  grafana/provisioning/...          # auto-loaded datasource + dashboards
  grafana/dashboards/watchtower.json
```

### Data sources

| Concern                          | API                                     | Auth |
| -------------------------------- | --------------------------------------- | ---- |
| Markets, events, categories      | `gamma-api.polymarket.com` (Gamma)      | none |
| Public trades                    | `data-api.polymarket.com` (Data API)    | none |
| Live book / price (future)       | `clob.polymarket.com` (CLOB)            | none for public methods |

Trades come from the **Data API**, not CLOB: the CLOB `/trades` endpoint
requires L2 (HMAC) credentials, while the Data API is fully public.

### Alerting

Findings fan out to multiple sinks:

- **structured log** (always on)
- **HTTP webhook** (`ALERT_WEBHOOK_URL`, JSON `Finding` payload)
- **Telegram channel** (`TELEGRAM_ENABLED=true` plus `TELEGRAM_BOT_TOKEN` and
  `TELEGRAM_CHAT_ID`) — one bot, one channel, no per-user fanout

Each finding carries severity, scope (market or category), metric, recent
value, baseline value, multiplier, window lengths, timestamp, and a
best-effort link back to the Polymarket market page.

### Anomaly model

For every market the engine maintains per-minute buckets covering the baseline
window (default 7 days). On each tick the detector folds the buckets into one
"recent" window per configured length (default `12h` and `24h`) and into one
"baseline" window (everything _before_ the longest recent window). It then
evaluates the three metrics — `trade_rate`, `notional_rate`, `avg_size` —
against the multiplier ladder (`30, 100, 1000` by default) and, when a ratio
fires, emits an `anomaly.Finding` with severity `warn`/`critical`/`fatal`.

Findings have a per-rule cooldown so a single sustained spike doesn't spam
sinks. The same rules run a second time over category roll-ups (sums across
all markets sharing a Gamma tag).

## Running

```bash
cp .env.example .env
make run                # runs the worker against the public Polymarket APIs
# or
make up                 # docker compose: app + prometheus + grafana
```

The metrics endpoint lives at `:9090/metrics`, Prometheus on `:9091`, Grafana
on `:3000` (anonymous viewer; admin/admin to edit).

## Configuration

Every knob is an env var; see `.env.example` for defaults and short
explanations. Validation runs at startup — missing/invalid env fails fast.

## Tests

```bash
make test           # all unit + httptest integration tests, no network
go test -race ./... # race detector across the same set
```

The default suite is hermetic — every adapter test uses an `httptest.Server`,
and the end-to-end pipeline test wires real `discover` / `collect` / `detect`
against fakes for Gamma, Data API, and Telegram. No internet access is
required.

External integration smoke tests for the live Polymarket Gamma API are gated
by a build tag and an env var:

```bash
POLYMARKET_INTEGRATION=1 go test -tags=integration ./internal/core/infra/polymarket/gamma/...
```

Telegram has the same opt-in pattern (`TELEGRAM_INTEGRATION=1` plus a real
token + chat id supplied through the standard env vars). Secrets are never
committed.

## Rate limits

The defaults sit at roughly 70% of Polymarket's documented per-endpoint caps
(Gamma `/markets`: 300/10s, Data API `/trades`: 200/10s). All upstream calls
go through a token bucket with exponential backoff + jitter on 429 / 5xx.
