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

## Detection mode

v4 cleanup made `single_cluster` the only strategy. The legacy `volume`
mode and the in-memory aggregate engine (rolling-bucket ring, gauges,
recent-window ratios) were removed; the `ANOMALY_MODE` env var is gone.

## Market lifecycle in the DB

Ended markets are **soft-deleted**, never immediately removed. The
discovery upsert query stamps `deleted_at = NOW()` when a market that
was previously active disappears from a sweep. The sanity worker
(`internal/app/usecase/sanity`) runs hourly, finds markets whose
`deleted_at` is older than `MARKET_SOFT_DELETE_RETENTION` (default 720h
= 30d), re-checks the current upstream state via the discover cache,
and either:

- **Resumes** the market (`clear deleted_at; active=true; backfill_status='pending'`)
  so the backfill worker re-fetches missing history; or
- **Purges** the market (`purged_at=NOW(); active=false`). The row is
  retained — `polymarket_trades.market_id` has no CASCADE on the trades
  side, so deleting a market would either fail FK or destroy historical
  analytics. `purged_at IS NOT NULL` excludes the market from all active
  processing (`ListActiveMarketsForBackfill`, `ListActiveMarketsForCollection`).

Hard delete is **never** performed by the worker.

## Discovery: no silent truncation

v4 cleanup removed `MAX_MARKETS`. The discovery loop processes every
market that passes the category whitelist. `DISCOVERY_SAFETY_MAX_MARKETS`
(default 0 = unlimited) is an operational emergency cap, **not** the
normal bound on coverage. Backpressure comes from rate limits, DB state,
and the per-tick concurrency knobs — not row count.

## Backfill: full history, 48 workers

`BACKFILL_WORKERS` is the single concurrency knob (default 48 — also the
per-tick claim count, since markets and goroutines are 1:1). Each market
is walked offset 0..N until the Data API returns an empty page
(`status=completed`) or the documented 3000-row offset cap is hit
(`status=partial_api_limit`). No shallow-lookback short-circuit; the goal
is the deepest DB history Polymarket allows. The 3000-row cap is
surfaced as an explicit market state so operators can spot it in
`watchtower_backfill_status_total{status=partial_api_limit}` (and act on
it if Polymarket changes the cap upstream).

## In-memory caches (v4 contract)

The only in-memory cache that survived the cleanup is
`internal/app/usecase/marketcache.Cache`: a discover-fed snapshot of the
active universe used by `detect.Loop` for hot-path category-label
lookups. Contract: cache miss returns `(zero, false)`; correctness does
not depend on the cache. The DB is the source of truth.

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
- Markets with missing `StartDate`/`EndDate` are **always silenced** — no
  env override. The previous `ALLOW_UNKNOWN_MARKET_LIFECYCLE` was removed
  in the post-v4 hardening pass; an alert without lifecycle context is
  structurally unsafe and operators must not be able to flip that off.
  Counter: `watchtower_filter_lifecycle_unknown_skipped_total`.

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

The multiplier ladder is evaluated on `max(marketMultiplier,
traderMultiplier)`. The market axis (per-bucket distribution) is the v1
signal "is this big relative to this market?"; the trader axis (wallet's
full-history distribution) is the v2 signal "is this big relative to this
wallet's normal trades?". A trade fires if it is anomalous on **either**
axis. The absolute ladder remains the spam filter.

`Finding.MultiplierAxis` records which axis contributed (`market`,
`trader`, `both`, or empty when neither was ready). Surfaced in the
Telegram "Why" section so an operator sees "x125 above wallet's typical
trade" vs "x500 above market baseline median" without reading source.

No promotion-override ladders (HardPromotion, HugeWhale, MegaWhale,
sub-cluster) live in the model anymore. Earlier iterations stacked them on
top of conservative-MIN and produced four overlapping ways to reach `hard`
on a single trade — too many independent knobs to reason about. Operators
who want a single $1M bet to wake them up should set the Critical
thresholds to a shape it clears.

## Persistence model (Strategy v4 — finalized)

Production is Postgres-only. When `POSTGRES_DSN` is set the detector
queries `dbbaseline.Provider` unconditionally, alerts are deduped via
`polymarket_alerts.dedup_key`, and accumulation reads
`polymarket_trades` directly. The previous `BASELINE_SOURCE` runtime
switch is removed — there is no configuration path that runs production
on an in-memory baseline.

When `POSTGRES_DSN` is empty the binary boots a dev-only mode with the
embedded `baseline.Baseline` reservoir, no dedup, no backfill, no
accumulation. Boot logs are loud about this. Production must run with a
DSN set; CI integration tests opt in via `POSTGRES_TEST_DSN`.

## Strategy v2 additions

### Trader-history multiplier

- `TraderRepository.Distribution(traderID, since)` returns the wallet's
  full-history distribution (count, median, p95, mean, total, span). Same
  shape as `BaselineDistribution` so the scorer flows trader stats through
  the existing `baseline.Stats` type and the existing readiness gates.
- `traderbaseline.Provider` is the Postgres adapter; mirrors
  `dbbaseline.Provider`. Wallet→id is resolved with a `sync.Map` cache.
- `TRADER_BASELINE_WINDOW` (default `2160h` = 90 days) is the MAX lookback
  over the wallet's stored history; 0 lifts the cap.
- `TRADER_MIN_HISTORY_TRADES` (default 5) gates the trader axis. Below this
  count the trader axis is silently disabled and v1 market-only scoring
  applies — sensible because a 2-trade median is too noisy. Set 0 to
  disable the trader axis entirely.
- Each axis has its own readiness; when only one is ready the trade can
  still fire on the ready axis alone.

### MM/arbitrage suppression

- `mmfilter.Filter.Decide(wallet, market, outcome)` examines the wallet's
  two-sided BUY+SELL activity on the same `(market, outcome)` over
  `MM_LOOKBACK`. Suppresses the single-trade alert when both:
  1. `count(BUY) ≥ MM_MIN_TRADES_PER_SIDE` AND `count(SELL) ≥ MM_MIN_TRADES_PER_SIDE`
  2. `|buy − sell| / max(buy, sell) ≤ MM_NEUTRALITY_TOL`
- Cluster alerts are deliberately not filtered — multiple wallets converging
  is meaningful even if some are MMs.
- Fails open on DB errors (a hiccup must not swallow a real alert).
- `MM_FILTER_ENABLED=false` disables suppression entirely.
- Metrics: `watchtower_filter_alert_mm_suppressed_total{category}`.
- Log line at info level with the buy/sell breakdown for every
  suppression so a reviewer can audit.

### Strategy versioning decision (v4 cleanup)

Strategy identity is **code-owned**. It lives at
`anomaly.StrategyIdentity = "informed-flow-v4"` and is woven into every
alert dedup key. The previous `STRATEGY_VERSION` env var was removed —
an operator must not be able to flip the dedup namespace at runtime; a
stray value would silently re-alert on trades already deduped.

When to bump the constant:

- new detector wired in / removed → BUMP
- dedup-key format change → BUMP
- scorer formula change → BUMP
- tier threshold tuning → NO bump (operator knob; per-trade keys carry
  the trade's own identity, so dedup stays sound)

A bump is a code-reviewed commit; the field stays an env var inside the
detector Config (`StrategyVersion string`) so tests can override.

## Strategy v4 additions

### Same-trader accumulation line

A new signal class: one wallet repeatedly building exposure on a single
`(market, outcome, side)` inside `ACCUMULATION_WINDOW`. Severity anchored
on the existing Info/Warning/Critical thresholds via two size paths:

- **meaningful**: `medianTrade ≥ FRACTION × T.MinNotionalUSD` AND
  `lineTotal ≥ TotalMultiplier × T.MinNotionalUSD`
- **many-smalls**: `lineTotal ≥ ManySmallsMultiplier × T.MinNotionalUSD`
  (catches 200 × $200 = $40k vs Info $5k)

Plus all tiers require `trades ≥ tier_min_trades(T)`, `avg_odds ≥
T.MinOdds`, and `lineTotal/marketMedian ≥ T.MinMultiplier`. Hard is
reserved for very large lines in HOT lifecycle.

Dedup key:
`accumulation:<sv>:<wallet>:<mid>:<token>:<side>:<window_bucket>`.

Package: `internal/app/usecase/analytics/accumulation` — `Detector.Decide`
is pure (no I/O); the repository read happens in `detect.Loop`.
Query: `repository.AccumulationLineSummary` (sqlc) — backed by
`idx_trades_trader_market_outcome_side_time`.

MM filter applies. Cluster path is unaffected.

### Quiet-market wake-up (v4)

Context detector — never fires alerts on its own. Runs AFTER a single-
trade or accumulation alert has qualified and stamps
`Finding.QuietMarket` + appends `QUIET_MARKET_WAKEUP` to
`Finding.Reasons` when:

- baseline tradesPerDay ≤ `MaxTradesPerDay` AND notionalPerDay ≤
  `MaxNotionalPerDayUSD`
- now − LastTradedAt ≥ `MinIdleDuration` (zero LastTradedAt passes by
  default — strongest quiet signal)
- event notional ≥ `MinCurrentNotionalUSD`
- event notional / marketMedian ≥ `MinMultiplier` (optional)

Package: `internal/app/usecase/analytics/quietmarket`. Pure (no I/O);
detect.Loop fetches `LastTradedAtBefore` from `repository.TradeRepository`
(new sqlc query backed by `idx_trades_market_outcome_time`).

Cluster alerts are not tagged — a multi-wallet cluster is intrinsically
not quiet.

## Failed-alert retry policy

The `alertsender` worker claims both `status='pending'` rows AND
retryable `status='failed'` rows whose `next_retry_at <= now()`.
Behavior:

- Transient errors (network blips, Telegram 5xx, timeout) bump
  `send_attempts` and schedule `next_retry_at = now + backoff` where
  `backoff = min(initial * 2^attempts, max) ± jitter`.
- Permanent errors (`render:`-prefixed payload bugs, Telegram HTML
  parse rejection, "chat not found", "bot was kicked",
  "message is too long") are stamped `next_retry_at=NULL` — the row
  stays `failed` forever and an operator must intervene. Retrying them
  would just burn quota.
- After `ALERT_RETRY_MAX_ATTEMPTS` attempts the row is exhausted (same
  permanent-failed outcome).

Config: `ALERT_RETRY_ENABLED`, `ALERT_RETRY_MAX_ATTEMPTS=5`,
`ALERT_RETRY_INITIAL_BACKOFF=30s`, `ALERT_RETRY_MAX_BACKOFF=30m`,
`ALERT_RETRY_JITTER_FRACTION=0.2`.

## Post-alert outcome tracking

The `outcomes` worker periodically scans sent alerts whose markets may
be resolved (`closed=true` OR `end_date <= now()`), queries Gamma for
the market's `outcomePrices`, and stamps a verdict on each alert:

- `pending` — alert was sent but market hasn't resolved yet.
- `resolved_correct` — alert direction matches the winning outcome
  (BUY winning-side, or SELL losing-side).
- `resolved_wrong` — alert direction is opposite the winning outcome.
- `unknown` — market closed but `outcomePrices` are inconclusive (no
  token cleared `OUTCOMES_WINNING_PRICE_THRESHOLD`).
- `unavailable` — market not in Gamma (archived, expired) or the
  Finding payload lacks the outcome label.

Signal-quality measurement only — never re-emits alerts, never reverses
the dedup namespace. Verdicts live on the alert row
(`outcome_status`, `winning_outcome_token`, `winning_outcome_label`,
`resolved_at`).

Config: `OUTCOMES_ENABLED`, `OUTCOMES_INTERVAL=15m`,
`OUTCOMES_CLAIM_LIMIT=64`, `OUTCOMES_WINNING_PRICE_THRESHOLD=0.99`.

## CLV-lite post-trade drift enrichment

The `drift` worker periodically scans sent alerts whose reference
windows have elapsed and persists the *signed favourable* fractional
price drift for each window:

- `clv_15m`, `clv_1h`, `clv_6h`, `clv_24h` — `DOUBLE PRECISION NULL`
- Positive = favourable for the alert direction (BUY: laterPrice >
  tradePrice; SELL: laterPrice < tradePrice).
- Each value is the fractional move relative to the trade price.

Reference prices come from `polymarket_trades` (the first trade on the
same `(market, outcome)` at or after `T+window`). The worker NEVER
uses future data for the firing decision — it only operates on alerts
whose `sent_at + window` has elapsed. `drift_status` flips to
`available` once any window produced a number; `unavailable` once the
24h window has elapsed without a reference price.

Outcome token: the drift worker reads `Finding.Accumulation.OutcomeToken`
when present. Single-trade Findings without accumulation context are
stamped `unavailable` (the `TradeRef` carries the outcome LABEL only,
not the CLOB token id).

Config: `DRIFT_ENABLED`, `DRIFT_INTERVAL=5m`, `DRIFT_CLAIM_LIMIT=64`.

## Grafana dashboard

`deploy/grafana/dashboards/watchtower-main.json` is provisioned at
container startup (UID `watchtower-main`). Panels cover: alerts by
severity, single-trade anomaly axis, Telegram delivery, upstream API
latency / errors, trade ingest rate, queue depth, Postgres growth, MM
suppression, lifecycle-unknown skipped. Alert deep-links from Telegram
reach this UID via `GRAFANA_DASH_UID`.

### POSSIBLE_MARKET_MAKER reason code

Lives in `mmfilter.ReasonPossibleMarketMaker`. Emitted on the MM-
suppression metric label (`watchtower_filter_alert_mm_suppressed_total{category, reason}`)
and the structured log line whenever the MM filter suppresses a single-
trade or accumulation alert. No Telegram path — suppression is
intentionally silent to recipients.

### Compatibility

The v2 additions are gated on Postgres being wired. In memory/debug mode
the detector falls back cleanly to v1 behaviour (market-only scoring, no
MM suppression). Tests do not need either axis configured.

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
   `renderLink(label, href)` (one helper, one escape rule). Every URL is
   first passed through `sanitizeLinkURL` — empty / unparseable / non-
   http(s) / localhost / loopback / link-local hosts return empty and
   the entry is elided entirely (no bullet, no orphan section header,
   no dead-text label). Order: Polymarket market → Polymarket category
   → Trader → Grafana. Pinned by `telegram_test.go::TestLabelsNeverAppearAsPlainText`,
   `TestGrafanaLinkClickableInWirePayload`, `TestSanitizeLinkURLRejectsNonReachable`,
   and `TestLinksElideLocalhostGrafana`.
5. `Data` — trailing machine-readable block carrying `market_id`,
   `outcome_token` (accumulation findings only), and `dedup`
   (`polymarket_alerts.dedup_key`). Each value is HTML-escaped and
   wrapped in `<code>`. The whole block is skipped when all three are
   empty. The dedup_key is the primary join key between Telegram
   messages, logs, and the DB.

Link URLs:
- Market: `<polymarket-base>/event/<event-slug>`.
- Category: `<polymarket-base>/predictions/<category-slug>`.
- Trader: `<polymarket-base>/profile/<wallet>`.
- Grafana: deep link with `from/to=±GRAFANA_CONTEXT_WINDOW`, `var-category`,
  `var-market`, `var-severity`. `GRAFANA_BASE_URL` defaults to empty
  (rather than the docker-compose `http://localhost:3000`) so a
  misconfigured deployment fails loud — leaving the docker-compose
  default in production would silently produce dead-text bullets that
  the sanitizer now strips. Set it to a host recipients can reach from
  a phone, or leave blank to disable.

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

## Periodic Telegram stats summary

- `internal/app/usecase/statsreport.Worker` posts a single aggregate
  message to the same Telegram chat on a configurable cadence
  (`TELEGRAM_STATS_INTERVAL`, default 2h). Disabled by default; turn
  on in production with `TELEGRAM_STATS_ENABLED=true`.
- Body sections: `Markets` (total / active / soft-deleted / purged),
  `Trades` (total / last-2h / unique traders), `Alerts` (sent /
  pending / failed + per-severity breakdown), `Backfill` (rows per
  terminal status). Section omitted when its counters are all zero
  (same omit-on-empty rule as the alert formatter).
- `Sender` is an interface over `*telegram.Bot.SendHTML`; the worker
  has no other Telegram coupling.
- The first send is delayed by `TELEGRAM_STATS_STARTUP_GRACE` (default
  = Interval) so a freshly-started process doesn't immediately ship
  "0 trades, 0 alerts" before any work has happened.
- Metrics: `watchtower_stats_summaries_sent_total` and
  `watchtower_stats_summary_errors_total`.

## Ingestion / processing metrics

The persist + sanity + backfill paths emit counters so an operator can
graph `rate(...)` to see whether ingest is keeping up with the
discover/collect cadence:

- `watchtower_persist_markets_upserted_total`,
  `watchtower_persist_market_outcomes_upserted_total`,
  `watchtower_persist_markets_soft_deleted_total`
- `watchtower_persist_trades_upserted_total`,
  `watchtower_persist_trades_duplicates_skipped_total`,
  `watchtower_persist_traders_upserted_total`
- `watchtower_sanity_markets_purged_total`,
  `watchtower_sanity_markets_resumed_total`
- `watchtower_backfill_pages_fetched_total`,
  `watchtower_backfill_runs_total{status}`

Dashboard panels (`deploy/grafana/dashboards/watchtower-main.json`,
ids 50-57) render these as rates + 24h stat blocks.

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

Apply via `set -a && source presets/<name>.env` or `env_file:` in compose.
Preset behaviour pinned by tests in `internal/app/preset_test.go`.
