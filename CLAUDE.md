# Project decisions

Accepted decisions for polymarket-watchtower. Update when changing alerting
strategy or terminology. For usage see README.md and presets/README.md.

## Alerting strategy

Goal: surface large risky bets and suspicious wallet clusters near the end
of a market's lifetime. Spam reduction is non-negotiable.

A single-trade alert fires only when **all** are true:
1. The market's category passes `CATEGORY_WHITELIST` — i.e. is in the
   whitelist. Matching is case-insensitive substring against the category
   `slug + " " + label` ONLY; market titles, event slugs, market slugs,
   and tags are not scanned. Default whitelist: `Politics`.
2. Market lifecycle progress ≥ `LIFECYCLE_ALERT_FROM_PCT` (default 75%).
   Unknown lifecycle → **silent by default** (`ALLOW_UNKNOWN_MARKET_LIFECYCLE=false`).
3. Market absolute age ≥ `MARKET_MIN_AGE` (default 24h).
4. Per-(category, market, outcome) baseline has `Count ≥ SINGLE_MIN_BASELINE_TRADES`,
   `TotalUSD ≥ SINGLE_MIN_BASELINE_NOTIONAL_USD`, `MedianUSD > 0`, and
   `SpanActual ≥ BASELINE_MIN_READY_WINDOW`.
5. Trade clears both the absolute tier (notional AND odds) and the
   multiplier tier (notional / baseline median).
6. Final severity = `ConservativeMin(absoluteTier, multiplierTier)`. Single-
   trade severity caps at `critical`. `hard` is cluster-only.

Baseline updates run **before** the alert gates so the reservoir warms
continuously even when alerts are suppressed.

## Detection modes

`ANOMALY_MODE` is validated `oneof=single_cluster volume`. Default
`single_cluster`. The `volume` mode is the legacy aggregate-rate detector;
do not extend it.

## Baseline semantics

- `BASELINE_WINDOW` is the **maximum** lookback. NOT a minimum-age
  requirement. A 1-month-old market with `BASELINE_WINDOW=1y` uses the 1
  month of available history. `0` means "no upper bound" (only
  `BASELINE_MAX_SAMPLES` caps memory).
- Every valid trade enters the reservoir. There is **no per-trade size
  filter** — discarding small trades would weaken rarity detection (a $5
  retail trade makes a later $25k whale bet more anomalous, not less).
  Robustness comes from median + the readiness gates, not from filtering.
- Readiness gates (must all clear before the multiplier ladder is
  evaluated):
  - `SINGLE_MIN_BASELINE_TRADES` (default 20) — sample count floor.
  - `SINGLE_MIN_BASELINE_NOTIONAL_USD` (default $1,000) — aggregate USD
    floor. Catches "many micro-trades" baselines.
  - `BASELINE_MIN_READY_WINDOW` (default 24h) — observed sample span
    (newest − oldest) floor.
- Alerts display the **actual** span (`BaselineRef.Span`), never the
  configured cap.

## Lifecycle gating

- `LifecyclePct = (now - StartDate) / (EndDate - StartDate) * 100`.
- Below `LIFECYCLE_ALERT_FROM_PCT` (default 75) → no alert.
- At or above `LIFECYCLE_HOT_FROM_PCT` (default 90) → `Finding.Hot = true`,
  Telegram header carries `· HOT`.
- Markets with missing `StartDate`/`EndDate` are **silenced by default**.
  Set `ALLOW_UNKNOWN_MARKET_LIFECYCLE=true` to opt in (legacy/debugging).

## Severity rule

Three tiers, each `(MinNotionalUSD, MinOdds, MinMultiplier)`. Both ladders
must qualify at Info or above; final = `ConservativeMin` (lower wins).
Single-trade severity caps at `critical`. `hard` is the cluster detector's
output and is qualitatively different — "multiple sharks agreeing", not
"one very big bet".

Defaults:

|          | Notional ≥ | Odds ≥ | Multiplier ≥ |
|----------|------------|--------|--------------|
| Info     | $10,000    | 3      | 100×         |
| Warning  | $25,000    | 5      | 1,000×       |
| Critical | $100,000   | 8      | 10,000×      |

No promotion-override ladders (HardPromotion, HugeWhale, MegaWhale,
sub-cluster) live in the model anymore. Earlier iterations stacked them on
top of conservative-MIN and produced four overlapping ways to reach `hard`
on a single trade — too many independent knobs to reason about. Operators
who want a single $1M bet to wake them up should set the Critical
thresholds to a shape it clears.

## Cluster rule

`Cluster.Detector` accumulates fired `TradeRef`s per category in a sliding
window. Fires HARD `CategoryWatchRequired` when all hold:
- `len(entries) ≥ CLUSTER_MIN_ANOMALOUS_TRADES` (default 3)
- `uniqueWallets ≥ CLUSTER_MIN_UNIQUE_TRADERS` (default 2)
- `totalUSD ≥ CLUSTER_MIN_TOTAL_NOTIONAL_USD` (default $50k)
- Last fire ≥ `CLUSTER_COOLDOWN` ago (default 30m)

Severity always `SeverityHard`. Payload carries a `Sample []TradeRef`
(capped 5) for per-trader contributor lines. `Cluster.Count(cat)` stamps
`InCluster` + `ClusterPeerCount` on the per-trade alert.

## Category whitelist rule

- One list, `CATEGORY_WHITELIST`. ONLY whitelisted categories are processed
  — discovered, monitored, scored, alerted. There is no blacklist; the
  whitelist is the sole category-selection mechanism.
- Case-insensitive substring match against the Polymarket category
  `slug + " " + label` and **nothing else**. Market titles, event slugs,
  market slugs, and tags are deliberately NOT scanned.
- A sports-themed market (e.g. "Will France win the 2026 FIFA World Cup?")
  filed under a whitelisted non-sports category like Polymarket's
  `Hide From New` is still analysed normally. Category-identity is the
  decision; market wording is not.
- Default value: `Politics`. Add categories explicitly via
  `CATEGORY_WHITELIST=Politics,Macro,Geopolitics`.
- Empty whitelist disables the filter (every category passes). Useful for
  local exploration; not appropriate for production.
- Applied at discover (prune ids before they reach the registry) and at
  detect (defense in depth, increments
  `watchtower_filter_category_skipped_total{stage="detect"}`).

## Telegram simplification

- Single recipient: `TELEGRAM_CHAT_ID`. No `/getUpdates` polling, no
  subscriber registry, no broadcast. `TELEGRAM_UPDATES_*` env vars are
  removed.
- `Enabled=true` without `BOT_TOKEN` or `CHAT_ID` is a startup error so
  misconfiguration is visible immediately.
- ChatID accepts numeric (private/group/channel) or `@channelusername`
  forms. Numeric is sent as JSON number, `@username` as JSON string.

## Telegram alert content

HTML parse mode. Escape via `html.EscapeString` — never hand-roll.

Header: `<b>{SEV}: x{mul} · ${notional}[ · HOT] · {title}</b>`.

Sections (in order):
1. `Why` — multiplier, odds + implied probability, baseline (count + median +
   mean + p95 + **actual span**), tier composition, lifecycle pct (+ HOT
   note), single-vs-cluster context.
2. `Trade` — outcome+side, size+shares@price, trader wallet, category,
   ISO timestamp.
3. `Cluster` (HARD only) — counts, total, window, contributor list.
4. `Links` — bulleted list of real Telegram HTML anchors built by
   `renderLink(label, href)` (one helper, one escape rule). Each `<a>` only
   when URL non-empty. Order: Polymarket market → Polymarket category →
   Trader → Grafana. Never emit a plain-text label — when a URL is empty,
   the entry is omitted entirely (no bullet, no orphan section header).
   Pinned by `telegram_test.go::TestLabelsNeverAppearAsPlainText` and
   `TestGrafanaLinkClickableInWirePayload` (end-to-end JSON wire payload).

Link URLs:
- Market: `<polymarket-base>/event/<event-slug>`.
- Category: `<polymarket-base>/predictions/<category-slug>`.
- Trader: `<polymarket-base>/profile/<wallet>`.
- Grafana: deep link with `from/to=±GRAFANA_CONTEXT_WINDOW`, `var-category`,
  `var-market`, `var-severity`. Set `GRAFANA_BASE_URL` to a publicly
  reachable host or recipients can't open it from a phone.

## PostgreSQL persistence

- `POSTGRES_DSN` empty → app runs in-memory only (local/debug). No
  backfill, no cross-restart alert dedup, no sender worker. Production
  must run with a DSN set.
- DSN set → pool opens at startup, `db/migrations` apply (unless
  `POSTGRES_AUTO_MIGRATE=false`), and the production graph wires up:
  - `persist.Sink` writes categories, markets, market_categories,
    market_outcomes, traders, trades through every discover/collect tick.
  - `backfill.Worker` fills historical trades for markets in whitelisted
    categories, driving each market through
    `pending → running → completed | partial_api_limit | failed`.
  - `dbbaseline.Provider` serves the detector's baseline reads from
    `polymarket_trades` (BaselineDistribution computes
    count/total/mean/median/p95/span server-side in one roundtrip).
  - Every fired alert is `TryCreatePending`-ed against
    `polymarket_alerts` with a UNIQUE dedup_key. Conflicts are skipped
    silently, so concurrent detectors / restarts cannot double-emit.
  - `alertsender.Worker` drains pending alerts via an atomic queue
    pattern (`UPDATE … IN (SELECT … FOR UPDATE SKIP LOCKED)` flipping to
    a transient `sending` status), renders via the alerting formatter,
    and posts through `internal/infra/telegram.Bot`.
- `BASELINE_SOURCE=postgres` (default) selects the DB read path.
  `BASELINE_SOURCE=memory` is local/debug only.
- `STRATEGY_VERSION` (default `v1`) is woven into every dedup_key so
  retunes can re-alert on previously seen trades.
- Schema lives in `db/migrations/` and is the source for both the
  runtime migrator (`internal/infra/postgres/migrate.go` via the
  `db.Migrations` embed) and sqlc generation (`sqlc.yaml`).
- Repositories (`internal/infra/repository/`) wrap sqlc-generated code
  in `internal/infra/postgres/sqlc/`. Nothing above the repo layer
  imports sqlc — pgtype.* never leaks into domain code.

## Telegram delivery

- HTTP transport lives in `internal/infra/telegram` (`Bot.SendHTML`).
  Single endpoint, single chat id, no `/getUpdates`, no subscriber
  registry.
- Message rendering (`FormatTelegramMessage`, `renderLink`) lives in
  `internal/infra/alerting`. It does not import any HTTP code.
- In production (Postgres wired), the only path that calls Bot.SendHTML
  is `alertsender.Worker`. `detect.Loop` writes to `polymarket_alerts`
  and stops there — the sender drains the queue.
- Without Postgres (local/debug), a synchronous `alerting.TelegramSink`
  is added to the realtime fanout so a developer can still see alerts
  on Telegram without standing up a database.

## Standard library for infrastructure primitives

URL building via `net/url`. JSON via `encoding/json`. HTTP via `net/http`
(or `internal/infra/polymarket/httpx` for upstreams — adds rate-limiting,
backoff, typed `APIError`). Time via `time.Duration`. No hand-rolled URL/
JSON/HTTP/retry primitives.

## Testing expectations

Hermetic by default: `go test ./...` must not touch the public internet.
Live integration tests opt-in behind `//go:build integration` and
`POLYMARKET_INTEGRATION=1`.

Required coverage for any alerting/baseline/lifecycle change:
- Baseline behavior across (window > market age, window < market age,
  window=0).
- Lifecycle pct at 50% / 75% / 90% boundaries.
- `MARKET_MIN_AGE` and `BASELINE_MIN_READY_WINDOW` gates.
- `ALLOW_UNKNOWN_MARKET_LIFECYCLE` — fail-closed default.
- Tier composition (`ConservativeMin` of absolute and multiplier ladders).
- Cluster floors (trades / wallets / total / cooldown).
- Telegram link rendering with each URL present and missing.
- Grafana URL includes `var-severity`.
- Category whitelist: non-whitelisted category blocked (e.g. Sports when
  whitelist is `Politics`); sports-themed market under a whitelisted
  category (e.g. inside `Hide From New` when that's whitelisted) still
  alerts; sports keyword in market metadata only is irrelevant.
- Every preset loads and matches its documented strictness.

## Presets

Three opinionated overlays under `presets/` (see `presets/README.md`):
- `conservative.env` — pager-grade; lifecycle ≥85%, Info multiplier ≥500×,
  Info notional ≥$50k.
- `balanced.env` — defaults; lifecycle ≥75%, Info multiplier ≥100×, Info
  notional ≥$10k.
- `aggressive.env` — local exploration only; lifecycle ≥60%, Info
  multiplier ≥30×, Info notional ≥$2.5k. `ALLOW_UNKNOWN_MARKET_LIFECYCLE=true`.

Apply via `set -a && source presets/<name>.env` or `env_file:` in compose.
Preset behaviour pinned by tests in `internal/app/preset_test.go`.
