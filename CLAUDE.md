# Project decisions

Accepted decisions for polymarket-watchtower. Update when changing alerting
strategy or terminology. For usage see README.md and presets/README.md.

**Knowledge base (start here for new contributors):**
- `doc/project/overview.md` — what Watchtower is / is not, philosophy.
- `doc/project/architecture-map.md` — package + DB + flow map.
- `doc/project/runtime-flow.md` — startup → ingest → detect → alert → outcome.
- `doc/project/operator-guide.md` — SQL health checks, metrics, runbook.
- `doc/project/tuning-methodology.md` — how to build `.env.prod` from DB evidence.
- `doc/strategies/current-strategy-map.md` — per-strategy gates + interaction matrix.
- `doc/observability/ai-metrics.md` — AI metric inventory + log events.
- `doc/observability/dashboards.md` — Grafana panel reference.

**AI handoff bundle (for ChatGPT / fresh Claude sessions):**
- `doc/ai/chatgpt-handoff.md` — dense AI-to-AI; paste this first.
- `doc/ai/project-philosophy.md` — WHY the project exists.
- `doc/ai/strategy-philosophy.md` — which signals matter and why.
- `doc/ai/runtime-mental-model.md` — runtime behaviour at a glance.
- `doc/ai/current-state.md` — what is true today (refresh after big changes).
- `doc/ai/context-recovery.md` — bootstrapping a fresh AI session.
- Regenerate with `./scripts/generate-chatgpt-context.sh` →
  outputs `tmp/chatgpt-context-{compact,full}.md`. The compact
  bundle (~34KB) is the load-bearing paste for a new session;
  the full bundle (~115KB) adds gate-level detail.

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
`anomaly.StrategyIdentity = "informed-flow-v6"` and is woven into every
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

## Strategies A, B, E — lifetime accumulation, new-wallet context, ownership concentration

Strategy A widens the accumulation detector to evaluate BOTH a recent
window (24h default) AND a lifetime window (full stored history). The
math is identical; per-window dedup keeps them from spamming:

  - `accumulation:<sv>:recent:<wallet>:<mid>:<token>:<side>:<bucket>`
    — cooldown-bucket dedup (existing behaviour)
  - `accumulation:<sv>:lifetime:<wallet>:<mid>:<token>:<side>:<severity>`
    — exactly one alert per severity tier per line. Severity upgrades
    emit one new alert per tier.

Strategy B is a CONTEXT BOOSTER only. After a single-trade or
accumulation alert qualifies, the detector reads
`polymarket_traders.first_seen_at` and the wallet's trade count, and
attaches `NEW_WALLET_LARGE_BET` / `NEW_WALLET_ACCUMULATION` /
`LOW_TRADER_HISTORY` reason codes when EITHER `age < NEW_WALLET_MAX_AGE`
OR `historyTrades ≤ NEW_WALLET_MAX_HISTORY_TRADES`. Never standalone,
never promotes severity.

Strategy E is a DISTINCT ALERT KIND `ownership_concentration` fired
alongside the accumulation path (not standalone, not a separate
worker). APPROXIMATION — no holders endpoint is wired upstream; the
percentage is `(wallet_net_BUY_shares / market_total_BUY_shares) × 100`
from polymarket_trades. Per-tier dedup; severity upgrades emit one
new alert at each tier.

Deferred: Strategy D persisted watchlist + worker. Available
per-wallet outcome data is `polymarket_alerts.outcome_status` only —
biased toward already-alerted wallets. A persisted watchlist driven
by alerts-on-alerts would mislead. Build when per-trade resolved-PnL
data lands.

Full doc: `doc/strategies/lifetime-and-context.md`.

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

## Polymarket event-page narrative context (v9.5)

- Source: `https://polymarket.com/_next/data/<buildId>/en/event/<slug>.json`
  — the Next.js hydrated payload backing the Polymarket UI event page.
  Carries event metadata, per-event market pricing, similar markets,
  and most importantly the `["annotations","event",<slug>]` query
  (the curated chart annotations the UI renders around the event
  chart). This is an INTERNAL endpoint; the buildId rotates on every
  Vercel deploy.
- `internal/infra/polymarket/eventpage.BuildIDResolver` scrapes the
  buildId from `/event/<slug>` HTML (`__NEXT_DATA__.buildId` first,
  `/_next/static/<id>/` regex fallback, prefers `build-*`), caches it
  in memory for `POLYMARKET_EVENT_PAGE_BUILD_ID_TTL` (default 30m),
  and refreshes transparently once on JSON 404 (stale buildId).
  Singleflight per resolver instance prevents stampedes.
- `internal/infra/polymarket/eventpage.Client.FetchEventPage` resolves
  the buildId, fetches the JSON, and parses dehydrated queries:
  event slug, annotations, similar markets, series, tags,
  derivative-data. Unknown queryKeys land in `RawQueryKeys` for
  telemetry but never fail parse.
- `internal/app/usecase/eventpagecontext.Provider` is the usecase
  facade. It depends on a `SlugResolver` (production: closure over
  `marketsRepo.GetByConditionID`), the eventpage client, and an
  `EventPageRepository`. Severity-aware refresh TTL: Info=10m,
  Warning/Critical/Hard/HOT=5m, singleflight per event_slug. Persists
  `polymarket_event_page_snapshots`, `polymarket_event_page_markets`,
  and `polymarket_event_annotations` (UNIQUE on event_slug+item_hash;
  hash = SHA-256 over event_slug|unix_time|outcome|title).
- AI integration: every alert prompt (`buildAlertPrompt`), every 2h
  intelligence prompt (`buildMarketReportPrompt`), and every
  postmortem (`buildOutcomePrompt`) receives a "Polymarket event
  page context:" slot. When empty the slot renders "unavailable. Do
  not invent market news; reduce confidence."
- Polymarket-authored fields (event title, annotation summaries,
  source names) are passed through as DATA. The model MUST NOT
  treat them as instructions. The renderer caps raw JSON to 1 MB at
  the writer boundary.
- Failure NEVER blocks alert delivery. Fetch errors record a row in
  `polymarket_event_page_fetches.last_error` and increment
  `watchtower_event_page_fetch_total{status="failed"}`.
- Metrics: `watchtower_event_page_fetch_total{status}`,
  `watchtower_event_page_build_id_changes_total`,
  `watchtower_event_page_annotations_total`,
  `watchtower_event_page_context_used_total{target_kind}`,
  `watchtower_event_page_fetch_latency_seconds`,
  `watchtower_event_page_alerts_total{status}` (PART 8 scaffold),
  `watchtower_event_page_lag_candidates_total` (PART 9 scaffold).
- Deferred: a periodic Event Page Review worker (annotation alerts)
  and a related-market lag detector. Types + config types live in
  `internal/app/usecase/eventpagecontext/review.go` but no
  `Worker.Run` is wired — operators see the surface without
  committing to behaviour that hasn't been validated against live
  data.

Context-layer separation (do not confuse):
- Market Activity Context = data-api `/trades` microstructure.
- Event Page Context = Polymarket event metadata + chart annotations
  (the layer above).
- Web News Context = external web_search via the OpenAI Responses API.
- Breaking Feed = NOT used as primary narrative source.

## Political-Catalyst Intelligence overlay (v9.5)

Catalysts are CROSS-STRATEGY metadata, NOT a standalone strategy.
They modify interpretation of whale-flow / accumulation / ownership-
concentration / stable-favorite / cluster / low-baseline findings via
two surfaces:

1. **Telegram "Blocked Alert" block.** Rendered ABOVE the AI analysis
   when the alertsender finds an active catalyst for the event slug.
   The block names the catalyst, expected timing, and the bullish /
   bearish / invalidation scenarios. The operator sees structural-
   uncertainty context BEFORE the AI body.
2. **AI prompt "Future catalysts:" slot.** Stamped onto
   `req.CatalystContext` by `aianalysis.Service.SetCatalystLoader`
   before every alert call. The verbatim PART 6 prompt asks the
   model to reason about pre/post-catalyst flow timing, blocked-
   market state, repricing risk, and edge persistence using these
   scenarios.

Storage: `polymarket_event_catalysts` (migration 00017). Rows are
keyed on `(event_slug, catalyst_type, title)` and carry status
(expected | active | resolved | stale | invalidated) plus the three
operator-facing scenarios. Rows are operator-seeded today; AI-driven
extraction is a follow-up.

Strategy-interaction matrix (load-bearing):
- accumulation BEFORE catalyst → stronger signal;
- accumulation AFTER repricing → weaker signal;
- stable-favorite waiting for runoff → blocked, edge limited;
- cluster forming after annotation spike → possible reactive chasing;
- ownership concentration before catalyst → meaningful positioning;
- low-baseline displacement around catalyst → high-information event.

Safety: Polymarket-authored annotations + AI-authored scenarios are
DATA. Telegram renderer HTML-escapes every catalyst field
(pinned by `TestBlockedAlertEscapesHTML`). The AI prompt treats the
"Future catalysts:" block as evidence; the verbatim PART 6 prompt
forbids inventing catalysts. The system has no prompt-injection
surface: no Polymarket / AI string is ever interpreted as
instructions.

Failure contract: every catalyst lookup degrades silently. A missing
slug, an empty `polymarket_event_catalysts` row set, or a DB error
all yield an empty Blocked block + a "no catalyst recorded" prompt
slot. The alert path NEVER blocks on the overlay.

## Political-Catalyst importer (v9.6) — automatic extraction

The catalyst layer is no longer operator-seeded. The
`internal/app/usecase/eventcatalyst/importer.Worker` runs every
`EVENT_CATALYST_IMPORTER_INTERVAL` (default 5m) and:

1. Pulls candidate markets via `MarketIntelligenceRepository.ListIntelligenceCandidates`
   and filters by `EVENT_CATALYST_IMPORTER_CATEGORY_WHITELIST`
   (default `Politics,Geopolitics,Elections`, case-insensitive
   substring on `category`). Resolves each conditionID to its
   `event_slug` via `MarketRepository.GetByConditionID`. Dedupes,
   caps at `EVENT_CATALYST_IMPORTER_BATCH_SIZE`.
2. Refreshes the Polymarket event-page payload for each unique
   slug via the existing `eventpagecontext.Provider` — annotations
   + markets + fetch-state land in DB through the same path the
   per-alert loader uses.
3. Builds a `CatalystExtractionRequest` carrying event metadata,
   per-market pricing, the newest `EVENT_CATALYST_IMPORTER_MAX_ANNOTATIONS`
   annotations (capped + sorted newest-first), a compact flow
   summary, and the existing catalyst rows.
4. Calls `openai.Client.ExtractCatalysts` — a dedicated
   `/chat/completions` call with `response_format=json_object` and
   the EXACT verbatim prompt from PART 4 of the v9.6 spec
   (`catalyst_extraction_prompt.go::catalystExtractionPrompt`). The
   model returns strict JSON matching the documented schema. Output
   is validated: markdown-wrapped output rejected, enum membership
   enforced, confidence clamped to [0,1], `expected_at` normalised
   to UTC RFC3339, rows with empty title or pre-2000/post-2100
   dates dropped.
5. Upserts every accepted catalyst (≥ `EVENT_CATALYST_IMPORTER_MIN_CONFIDENCE`,
   default 0.55) into `polymarket_event_catalysts` via the existing
   `(event_slug, catalyst_type, title)` UNIQUE conflict path.
   Mutable fields refresh on conflict; rows are NEVER deleted by
   the importer.
6. Marks stale: any existing `(expected, active)` row whose title
   wasn't re-emitted this cycle, whose `updated_at` is older than
   `EVENT_CATALYST_IMPORTER_STALE_AFTER` (default 7d), and whose
   `expected_at` is in the past or NULL → status flips to `stale`.
   Resolved/invalidated rows are never touched.

The importer runs concurrently with `EVENT_CATALYST_IMPORTER_CONCURRENCY`
workers per cycle (default 4). One event's failure (fetch, AI,
parse, upsert) never affects siblings.

Failure semantics: every layer fails open. Event-page fetch failure
keeps the cycle going; AI failure logs + `ai_failed` metric;
parse failure logs + `invalid_json` request_log; DB write failure
logs and continues. The alert pipeline is completely decoupled —
Telegram delivery never waits on, depends on, or is influenced by
the importer's status.

Disable: `EVENT_CATALYST_IMPORTER_ENABLED=false` (the importer
goroutine never starts; the catalyst loader still reads any rows
operators may have seeded manually).

Smoke / dry-run: `go run ./cmd/cli import-catalysts --event <slug>
--ai-key $OPENAI_API_KEY` fetches one event, runs extraction, and
prints the JSON output without DB writes.

Metrics (registered in `internal/infra/metrics/metrics.go`):
- `watchtower_event_catalyst_importer_runs_total{status}`
- `watchtower_event_catalyst_importer_events_selected_total`
- `watchtower_event_catalyst_importer_events_processed_total{status}`
- `watchtower_event_catalyst_ai_requests_total{status}`
- `watchtower_event_catalyst_upserted_total{status,type}`
- `watchtower_event_catalyst_import_latency_seconds`
- `watchtower_event_catalyst_blocked_alerts_total`

## Annotation rendering · ranking · daily intel (v9.7)

Three independent surfaces consume the persisted event-page
annotations:

1. **Per-alert annotations block** — `alertsender.Worker` calls
   `AlertAnnotationStamper.StampRecentAnnotations` (production:
   `eventpagecontext.Provider`) which loads up to 3 newest
   same-event annotations and attaches them to
   `Finding.RecentAnnotations`. The Telegram formatter
   (`writeRecentAnnotationsBlock`) emits a "Recent annotations"
   block BELOW the AI analysis with full HTML escape. Block elides
   when the slice is empty.

2. **2h market-intelligence ranking** — when the AI key is wired,
   `marketintel.Worker` calls `AnnotationRankingHook.RankAndRender`
   after the intel report succeeds. The hook (`annotationranking.Hook`)
   pulls event-page annotations for the candidate markets, calls
   `openai.Client.RankAnnotations` (verbatim PART 1-3 ranking
   prompt + JSON mode), persists picks to
   `polymarket_event_annotation_rankings`, and appends a
   "Top important annotations" block (top 10) to the Telegram body.

3. **Daily political intelligence report** —
   `internal/app/usecase/dailypoliticalintel.Worker` ticks every
   minute and fires once per day at `DAILY_POLITICAL_INTEL_TIME`
   in `DAILY_POLITICAL_INTEL_TIMEZONE` (Europe/Tallinn default).
   Selects up to 100 candidate markets, hydrates each with 4
   newest annotations, collects active catalysts, calls
   `openai.Client.GenerateDailyPoliticalIntel` (verbatim PART 5
   prompt; free-text Russian), persists the row to
   `polymarket_daily_political_intel_reports` (UNIQUE on
   `report_date`), splits the body on `\n\n` boundaries when
   above the Telegram message cap (3500 default), and sends. The
   row carries `delivery_status` (pending / sent / failed /
   skipped / ai_failed) + `telegram_message_ids_json` for the
   multi-message case + `last_delivery_error`.

Failure semantics across all three surfaces: every layer is
fail-open. Per-alert annotation stamping failure → block elides.
2h ranking failure → empty appendix; intel report still ships.
Daily AI failure → row stamped ai_failed; nothing sent; next
schedule retries. Telegram failure on the daily worker → row
stamped failed with `last_delivery_error`.

Safety reaffirmation (v9.7): Polymarket-authored annotations and
AI-authored ranking reasons / report text are DATA. Every renderer
HTML-escapes at the boundary; the ranking JSON parser validates
enum membership; bad enums collapse to `unclear`. No prompt
injection surface — Polymarket strings are never re-fed to the
model as instructions.

Disable knobs:
- `DAILY_POLITICAL_INTEL_ENABLED=false` — daily worker disabled.
- `DAILY_POLITICAL_INTEL_AI_ENABLED=false` — daily worker still
  runs but emits a `skipped` row instead of calling the AI.
- `DAILY_POLITICAL_INTEL_SEND_TELEGRAM=false` — daily report
  persisted without Telegram delivery (dry-run).
- Removing the AI key disables both the ranking hook and the
  daily AI generator without disabling the surrounding wiring.

v9.7 metrics (registered in `internal/infra/metrics/metrics.go`):
- `watchtower_alert_annotation_blocks_total{status}` (rendered / empty)
- `watchtower_market_intel_annotation_ranking_requests_total{status}`
- `watchtower_market_intel_annotations_selected_total`
- `watchtower_daily_political_intel_reports_total{status}`
- `watchtower_daily_political_intel_markets_selected_total`
- `watchtower_daily_political_intel_annotations_total`
- `watchtower_daily_political_intel_ai_latency_seconds`

## Intelligence Hardening (v9.8)

Three deterministic intel layers eliminate the "AI guesses without
data" failure mode that v9.7 still had:

1. **Event flow summary** (`internal/app/usecase/eventflow.EventFlowRepository`).
   One DB roundtrip per event aggregates polymarket_alerts +
   polymarket_trades over `EVENT_FLOW_SUMMARY_LOOKBACK` (default
   24h). Produces severity / kind counts, strongest side, same-side
   vs opposite-side notional + directional imbalance, largest trade,
   top-N alert + trade lists, and per-kind operator notes. Renderer
   emits the "Recent Watchtower flow:" prompt block; empty input
   renders an explicit "No meaningful stored flow" sentence with a
   "do not infer weak flow from missing data" directive so the AI
   never confuses silence with weak signal.
   Wired into `dailypoliticalintel.Worker.SetFlowLoader`; the same
   loader will also be passed to the catalyst importer's prompt
   path. NEVER blocks alerts; failure falls through silently.

2. **Repricing intelligence** (`internal/app/usecase/repricing.Provider`).
   Deterministic per-annotation features: annotation
   priceBefore/priceAfter vs current event-page price, pre/post-
   annotation flow USD via `SumConditionTradesInWindow`, flow timing
   classifier (pre_event_positioning / post_event_chasing / mixed /
   no_flow / unknown), repricing status classifier (underreacting /
   overreacting / already_priced / still_repricing / reversed /
   unclear) per `REPRICING_*` thresholds. Persisted in
   `polymarket_repricing_signals` (UNIQUE on event+condition+
   annotation_hash). Renderer emits the "Repricing intelligence:"
   prompt slot. AI consumes the signal as evidence; the layer
   itself is pure math — NO AI calls.

3. **Market prediction state machine + match scoring**
   (`internal/app/usecase/marketprediction`). Pure `Decide(...)`
   transitions between the 11 documented states (new / watching /
   blocked / active_catalyst / confirmed_by_flow / contradicted_by_flow
   / repricing / already_priced / stale / resolved / invalidated).
   Priority: resolution > invalidation > blocked > confirmed >
   contradicted > repricing/already_priced > stale > watching.
   `Applier` persists the prediction upsert + the transition audit
   row in `polymarket_market_prediction_states`. Pure `Score(...)`
   computes a deterministic alert↔prediction match score in [0,1]
   plus direction_alignment (aligned / contradict / neutral) +
   match_reason — a transparent weighted sum the operator can audit.
   Telegram renderer (`RenderTelegramBlock`) emits the operator-
   facing "Prediction state" block.

A News & Prediction Telegram renderer
(`internal/infra/alerting.RenderNewsPrediction`) composes Market /
Prediction state / Blocked / Repricing / AI / Matched alerts /
Latest annotations sections from the same data. Every Polymarket /
AI / operator string is HTML-escaped at the boundary; sections elide
when empty.

Tables landed in migration 00019:
- `polymarket_repricing_signals` — deterministic per-annotation
  features. UNIQUE (event_slug, condition_id, annotation_hash).
- `polymarket_market_predictions` — evolving (event, condition)
  state snapshot. UNIQUE (event_slug, condition_id).
- `polymarket_market_prediction_states` — append-only transition
  audit log.

v9.8 metrics:
- `watchtower_event_flow_summary_load_total{status}` (ok / empty / alerts_failed)
- `watchtower_event_flow_summary_empty_total` — counter
- `watchtower_repricing_signals_total{status,flow_timing}`
- `watchtower_market_prediction_state_transitions_total{from,to}`
- `watchtower_market_prediction_matches_total{alignment}`
- `watchtower_prediction_context_blocks_total{block,status}`

Safety reaffirmation: every Polymarket-authored field
(annotations, market questions, wallet addresses) is DATA.
Repricing scenarios and prediction state reasons are likewise DATA.
Renderers HTML-escape; the AI consumes the prompt block as
evidence, never as instructions.

## Prediction Evolution worker (v9.9)

The operational heartbeat of the prediction system. Without this
worker, predictions were "born and forgotten" — created once and
never revisited. v9.9 makes a prediction a **living market thesis**.

Worker (`internal/app/usecase/marketprediction/evolution.Worker`)
runs on a `MARKET_PREDICTION_EVOLUTION_INTERVAL` ticker (default
15m, immediate first tick). Each tick:

1. **Select.** `ListPredictionsForEvolution(maxAge, limit)` returns
   active predictions ordered by state priority — blocked /
   active_catalyst / confirmed_by_flow / contradicted_by_flow first,
   then repricing / already_priced, then watching / new, then stale.
   `NULLS FIRST` on `last_evolved_at` so never-evolved rows lead the
   queue. Resolved/invalidated excluded via partial index.
2. **Refresh deterministic intel per prediction.** Re-fetch the
   event page (eventpagecontext), top recent catalysts
   (`EventCatalystRepository.GetTopByConfidenceForEvent`), event
   flow summary (eventflow), repricing signal for the newest
   annotation (`repricing.Provider.ComputeForAnnotation`), and
   re-score matched alerts against the prediction
   (`marketprediction.Score` over `polymarket_alerts` recent window).
3. **Decide + apply.** `marketprediction.Decide(...)` returns the
   new state; `marketprediction.Applier.Apply(...)` upserts the
   prediction row + the transition audit row.
4. **Decay.** When the prediction is idle (flow empty, no state
   change, state not in {blocked, active_catalyst, resolved,
   invalidated}) and confidence > floor, apply
   `decayPerCycle = DecayPerDay × cyclesPerDay` via
   `ApplyPredictionDecay` — `GREATEST(confidence - delta, floor)`.
5. **AI gating.** Refresh thesis ONLY when one of:
   `dec.Changed` ‖ repricing status in {underreacting, overreacting,
   reversed} ‖ ≥1 strong recent alert (severity ≥
   `AIStrongAlertSeverity`) ‖ no prior `predicted_summary` ‖
   `now - last_ai_at ≥ AIStaleAfter`. Otherwise skipped with one
   of `state_unchanged` / `repricing_quiet` / `no_strong_alerts` /
   `cooldown_active` / `budget_exhausted` and a metric label so an
   operator can see *why* AI was skipped.
6. **Telegram.** Only on `dec.Changed` AND per-prediction cooldown
   elapsed (`TelegramCooldown`, default 4h, in-memory map —
   restart-resets are safe; worst case one extra post per flap).
   Body via `evolution.RenderEvolutionUpdate` (HTML-escaped header
   + Prediction state / Repricing / Catalyst / Flow / AI sections).
7. **Touch.** `TouchPredictionEvolution` always runs at the end so
   the row drops to the back of the queue even when nothing changed.

Concurrency: bounded fan-out via `sem` channel
(`MARKET_PREDICTION_EVOLUTION_CONCURRENCY`, default 2). Per-
prediction `recover()` ensures one panic never stops the batch.
AI budget shared across goroutines via mutex-protected counter
(`MARKET_PREDICTION_EVOLUTION_AI_BUDGET_PER_TICK`, default 8).

**Fail-open.** Worker NEVER blocks normal alerting. If the worker
panics, deadlocks, or never runs, the alertsender + detector loops
are completely independent — `detect.Loop → polymarket_alerts →
alertsender.Worker → Telegram` has zero dependency on prediction
state.

**Dry-run.** `TickOne(ctx, pred, dryRun=true)` runs every
deterministic layer end-to-end against the live DB but short-
circuits all writes (Apply / Touch / Decay) and Telegram. Used
by `cli evolve-predictions --dry-run`.

Schema (migration 00020):
- `polymarket_market_predictions.last_evolved_at TIMESTAMPTZ NULL`
- Partial index `idx_predictions_evolution_queue` ON
  `(last_evolved_at NULLS FIRST, state)` WHERE
  `state NOT IN ('resolved','invalidated')`.

The EXACT verbatim Russian Prediction Evolution prompt (PART 9 of
the operator spec) lives in
`internal/infra/ai/openai/prediction_evolution_prompt.go` —
starts "Ты — senior analyst на political/geopolitical prediction-
market desk." Nine placeholders ({{PREVIOUS_PREDICTION}},
{{PREDICTION_STATE}}, {{MARKET_DATA}}, {{ANNOTATIONS}},
{{CATALYSTS}}, {{REPRICING}}, {{FLOW_SUMMARY}},
{{MATCHED_ALERTS}}, {{WEB_CONTEXT}}) substituted with fallbacks
("(no prior thesis on file)", etc.) — pinned by
`TestBuildPredictionEvolutionUserMessage_*`.

v9.9 metrics:
- `watchtower_prediction_evolution_runs_total{status}` (ok / failed / skipped)
- `watchtower_prediction_evolution_selected_total` — counter
- `watchtower_prediction_evolution_processed_total{status}`
- `watchtower_prediction_evolution_state_changes_total{from,to}`
- `watchtower_prediction_evolution_ai_requests_total{status}` (refreshed / failed)
- `watchtower_prediction_evolution_ai_skipped_total{reason}`
- `watchtower_prediction_evolution_telegram_total{status}` (sent / skipped / failed)
- `watchtower_prediction_evolution_latency_seconds` (histogram)
- `watchtower_prediction_evolution_decay_total{state}`

CLI: `go run ./cmd/cli evolve-predictions --dsn=$POSTGRES_DSN
--once --limit 10 --dry-run`. Prints a per-prediction table (id /
event_slug / old_state -> new_state / AI yes-no-skp / repricing /
strongest_side / matched_alerts / decay / telegram).

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

## Strategy Learning Loop (v11.5)

Nine new detectors land as a **shadow-first surveillance layer**. None
of them can fire a Telegram alert today: every one writes to
`polymarket_strategy_shadow_decisions` and the bus
(`internal/app/usecase/strategybus`) rewrites `shadow_only=true` until
both `STRATEGY_LEARNING_LOOP_PROMOTION_ALLOWED=true` AND the
per-strategy `*_SHADOW_ONLY=false`. Promotion is operator-driven.

Detector classification (matches the ТЗ — do NOT treat as nine equal
Telegram surfaces):

- **Primary standalone**: `thesisaccum`, `holderdelta` (a.k.a.
  ownership_v2), `repricinglag`, `cheaptail`.
- **Boosters / upgraders**: `catalystwindow`, `bookvacuum`,
  `walletcohort`.
- **Safety / arbitration**: `conflictresolve`, `rulesrisk`.

Detector contract (pinned by tests under
`internal/app/usecase/analytics/<name>/detector_test.go`):

- `New(Config) *Detector` — pure constructor; no I/O.
- `Decide(Input) Verdict` — pure; no SQL, HTTP, Telegram, OpenAI.
- No global state. Unit tests run hermetically.

The bus is the single sink + flag enforcer. Detectors / workers call
`bus.ShouldEvaluate(name)` before running `Decide()` and
`bus.Record(ctx, Decision)` afterward. The bus forces
`Decision.ShadowOnly=true` whenever promotion is not allowed for the
strategy. It NEVER calls Telegram, OpenAI, or the alertsender path.

Supporting workers (also default-disabled):

- `marketlinks.Builder` — builds market ↔ market thesis graph;
  upserts `polymarket_market_links`.
- `holdersync.Worker` — periodic holder snapshots →
  `polymarket_holder_snapshots`.
- `riskscore.Worker` — pure `rulesrisk.Detector.Decide` over market
  facts; persists `polymarket_market_risk_scores`.
- `repricing.Worker` — opens/closes `polymarket_repricing_windows`
  rows around annotations + catalysts.
- `walletgraph.Worker` — co-trade behavioural edges →
  `polymarket_wallet_graph_edges`. Phase B (shared funding) is
  flag-gated behind `WALLETGRAPH_USE_FUNDING_PROVIDER`.

Every worker is fail-open: per-item failures log + metric
(`strategy_worker_items{op=errored}`), the batch continues, and the
existing alert pipeline is fully insulated.

Value-tracking columns on `polymarket_strategy_shadow_decisions`
(`clv_15m`, `clv_1h`, `clv_6h`, `clv_24h`, `outcome_status`) are
filled async by the existing drift + outcomes workers via the
`linked_alert_dedup_key` join key. A dedicated shadow evaluator for
unlinked rows is Phase B.

Promotion criteria (encoded in this doc; enforced by an operator
review job, not in code today):

- **Standalone**: N ≥ 50 firings, median `signed_move_6h` uplift ≥
  +1.5c vs the matched control bucket
  (`category|lifecycle|odds|notional|event_kind`), `reversal_15m` ≤
  control, alerts/day ≤ budget.
- **Booster**: parent uplift ≥ +1.0c OR PAL uplift ≥ +4pp.
- **Safety**: ≥ 15% reduction in later-reversed alerts without
  destroying parent drift.

Per-strategy / per-worker env knobs (defaults — all OFF):

`THESIS_ACCUM_*`, `OWNERSHIP_V2_*`, `CATALYST_WINDOW_*`,
`BOOK_VACUUM_*`, `REPRICING_LAG_*`, `WALLET_COHORT_*`,
`CONFLICT_RESOLVE_*`, `RULES_RISK_*`, `CHEAPTAIL_*`,
`MARKETLINKS_*`, `OWNERSHIP_SYNC_*`, `RISKSCORE_*`,
`REPRICING_WORKER_*`, `WALLETGRAPH_*`. Every detector exposes its
own `*_ENABLED` + `*_SHADOW_ONLY` knob; the global kill-switch is
`STRATEGY_LEARNING_LOOP_PROMOTION_ALLOWED=false`.

Metrics: `watchtower_strategy_shadow_decisions_total{strategy,
decision_kind, level}`, `watchtower_strategy_shadow_score_bucket`,
`watchtower_strategy_shadow_confidence_bucket`,
`watchtower_strategy_value_signed_move_cents{strategy, window}`,
`watchtower_strategy_reversal_total{strategy, window}`,
`watchtower_strategy_promotion_eligible{strategy}` (gauge),
`watchtower_strategy_shadow_write_errors_total{strategy}`,
`watchtower_strategy_worker_runs_total{worker, status}`,
`watchtower_strategy_worker_items_total{worker, op}`,
`watchtower_strategy_worker_latency_seconds{worker}`. Wallet /
condition_id / event_slug are deliberately NOT labels.

Verify-local (`scripts/verify-local.sh`): authoritative go version
read from `go.mod`; gates on go vet + go test; lint is advisory
because of pre-existing legacy debt — but the script explicitly
fails if any v11.5 path appears in the lint report.

## Strategy Learning Loop Phase B (v11.6)

Phase B closes the v11.5 gaps: shadow rows are now written from the
live detect.Loop path; the shadow value evaluator backfills CLV
columns; a promotion review worker re-evaluates eligibility on a
schedule and gates the bus.

**detect.Loop hook (`internal/app/usecase/detect/strategy_shadow.go`):**
Every newly-persisted alert (single-trade, accumulation, ownership)
triggers a single call to `Loop.recordStrategyShadow`. The bridge
runs the pure `rulesrisk.Detector` against the market's question
text and writes ONE tag-kind shadow row keyed to the alert dedup
key. Other detectors that need staged worker output (thesisaccum,
holderdelta, walletcohort, …) are counted as `eval_skipped{reason=
no_detect_loop_input}` here; they remain owned by their respective
workers. The hook is fully gated behind `cfg.StrategyShadowBus !=
nil` so a Postgres-less boot path is unaffected.

**Shadow value evaluator (`internal/app/usecase/strategyvalue`):**
Bounded batch worker. Reads pending shadow rows whose CLV columns
are still NULL (sqlc query `ListPendingValueRows`), samples first-
trade prices at fired_at + {15m, 1h, 6h, 24h} via
`PriceWindowStats`, and writes signed-cents moves through the
nullable-friendly `UpdateShadowDecisionValues` query. Idempotent:
the SQL uses `SET clv_15m = COALESCE(clv_15m, sqlc.narg('clv_15m'))`
so a re-run can never overwrite an already-computed value. Reversal
heuristic (`reversal_15m`) is derived per row when both 15m and 1h
land in the same tick. Missing price → `missing_price_total` metric,
row left NULL.

**Promotion review (`internal/app/usecase/strategypromotion`):**
Periodic worker computes per-(strategy, version) aggregates from
`polymarket_strategy_shadow_decisions` (sample size, median
signed_move_6h, reversal_15m ratio, alerts/day), writes one row to
`polymarket_strategy_promotion_reviews` (migration 00031), and
caches the latest eligibility in-memory. The bus consults
`PromotionGate.Allow(strategy)` BEFORE letting a non-shadow row
through. ТЗ guarantee pinned by
`TestRecord_PromotionGateDeniesForcesShadow`: even with
`STRATEGY_LEARNING_LOOP_PROMOTION_ALLOWED=true` AND
`<STRATEGY>_SHADOW_ONLY=false`, the gate's denial forces shadow.
`STRATEGY_PROMOTION_BYPASS_EXPLICIT=true` is a kill-switch — when
set, `Allow` returns false for everything regardless of state.

**App wiring (`internal/app/strategy_phase_b.go` +
`internal/app/app.go`):** when Postgres is wired, `wireStrategyPhaseB`
constructs the bus + RulesRisk detector + value evaluator + promotion
worker; the bus is injected into `detect.Config.StrategyShadowBus` and
the two new workers are appended to the graceful-shutdown exec list.
Workers are inert when their per-strategy flags stay disabled.

**Production integration test:** `internal/app/strategy_phase_b_integration_test.go`
is gated on `POSTGRES_TEST_DSN`. Connects to the live pool, writes
one probe row through the bus, runs the value evaluator + promotion
worker once, and asserts the row is present + shadow_only=true.
Cleans up afterward. Skipped when DSN is unset.

## Strategy Learning Loop Phase C (v11.7)

Phase C closes the v11.6 adapter gap and proves real shadow data
against the live Postgres pool.

**Production adapters (`internal/app/strategy_phase_c.go`):**

- `marketlinks.Builder` — sqlc `ListEventGroupedMarkets` builds a
  same-event star graph (anchor → siblings), persists through
  `UpsertMarketLink`. Real run: **833 rows** from 827 multi-market
  events.
- `riskscore.Worker` — sqlc `ListRiskScoreCandidates` →
  `rulesrisk.Detector.Decide` → `UpsertMarketRiskScore`. Real
  run: **50 rows** with ambiguity scoring on real market questions.
- `walletgraph.Worker` — sqlc `ListWalletCoTradeRows` against
  polymarket_trades, computes Phase A co-trade similarity in pure
  Go, persists via direct pool exec (composite-UNIQUE keeps
  sqlc-narg awkward). Real run: **58 edges** from 1.6M recent trades.
- `repricing.Worker` — sqlc `ListRepricingTriggers` against
  polymarket_event_catalysts opens windows; close-phase no-ops
  pending Phase D price sampler. Real run: **22 windows** opened
  from 39 catalysts.
- `holdersync.Worker` — adapter is intentionally a NO-LIVE-SOURCE
  stub (no Polymarket holders endpoint wrapped in
  `internal/infra/polymarket/`). The worker still ticks but
  `ListHolderSyncCandidates` returns empty + `FetchHolders`
  returns `ErrNoSource`. Documented Phase D gap.

**Outcome backfill (`internal/app/usecase/strategyoutcome`):** new
worker that joins `polymarket_strategy_shadow_decisions` to
`polymarket_alerts` via `linked_alert_dedup_key` and propagates the
terminal `outcome_status`. Idempotent — only writes to rows whose
outcome_status is still NULL. Standalone shadow rows that never
linked to an alert remain NULL.

**Promotion thresholds → env-driven:**
`STRATEGY_PROMOTION_MIN_SAMPLE=50`,
`STRATEGY_PROMOTION_MIN_SIGNED_MOVE_6H_CENTS=1.5`,
`STRATEGY_PROMOTION_MAX_REVERSAL_15M_RATIO=0.5`,
`STRATEGY_PROMOTION_MAX_ALERTS_PER_DAY=40`,
`STRATEGY_PROMOTION_BYPASS_EXPLICIT=false`,
`STRATEGY_PROMOTION_REVIEW_INTERVAL=1h`,
`STRATEGY_PROMOTION_REVIEW_LOOKBACK=336h`. Defaults exactly match
the v11.6 hardcoded values; test
`TestStrategyConfig_PromotionDefaultsMatchV11_6` pins them.

**Real-run evidence:** `internal/app/strategy_phase_c_integration_test.go`
exercises every Phase C worker against the live pool and asserts
non-zero rows in marketlinks + risk_scores. Run with
`POSTGRES_TEST_DSN=... go test -tags integration -run TestPhaseC_RealRunOnLivePool`.

**SQL audit hooks (operator):**
```
-- worker output counts
SELECT 'market_links', COUNT(*) FROM polymarket_market_links UNION ALL
SELECT 'risk_scores',  COUNT(*) FROM polymarket_market_risk_scores WHERE is_active UNION ALL
SELECT 'wallet_edges', COUNT(*) FROM polymarket_wallet_graph_edges UNION ALL
SELECT 'repricing',    COUNT(*) FROM polymarket_repricing_windows UNION ALL
SELECT 'shadow',       COUNT(*) FROM polymarket_strategy_shadow_decisions;

-- per-strategy shadow rate + CLV uplift
SELECT strategy_name,
       COUNT(*) AS rows,
       COUNT(*) FILTER (WHERE clv_15m IS NOT NULL) AS evaluated_15m,
       AVG(clv_6h) AS avg_6h_cents
FROM polymarket_strategy_shadow_decisions
GROUP BY strategy_name
ORDER BY rows DESC;

-- skip reasons (Prometheus)
sum by (strategy, reason) (rate(watchtower_strategy_eval_skipped_total[5m]))
```

## Strategy Learning Loop Phase D (v11.8)

Phase D closes the hot-path fanout gap: `internal/app/usecase/stagedinputs`
exposes 6 bounded Postgres readers (market_links, catalysts,
risk_scores, wallet_edges, repricing_windows, recent_decisions) with
a TTL cache, and `internal/app/usecase/detect/strategy_shadow.go`
fans every newly-persisted alert across all 9 strategies. Each
strategy either writes a real shadow row through `strategybus.Bus`
or emits a precise `eval_skipped{reason=...}` metric for missing
staged data.

**Strategies on the hot path:**

| Strategy | Hot-path read | Verdict mapping |
|---|---|---|
| rulesrisk | none (pure on market title) | KindTag |
| catalystwindow | `CatalystsByEvent` | KindBoost |
| walletcohort | `WalletEdgesForWallet` | KindBoost |
| conflictresolve | `RecentDecisionsForCondition` | KindDegrade/Suppress/Tag |
| cheaptail | `CatalystsByEvent` + `RiskScoreForCondition` | KindStandalone |
| repricinglag | `ClosedRepricingWindowsForCondition` | KindStandalone |
| thesisaccum | `MarketLinksByEvent` | KindTag (structural only — wallet aggregate is Phase E) |
| holderdelta | _no holder_snapshots producer_ | skip `no_holder_snapshots_available` |
| bookvacuum | _no book_feature_bars producer_ | skip `no_book_feature_bars_producer` |

**Real production proof:** `internal/app/strategy_phase_d_integration_test.go`
replays 25 most-recent real alerts through `bus.Record` + staged
readers and verifies non-zero shadow rows across multiple
strategies. Last live run wrote **32 rows across 3 strategies**
(rulesrisk: 14, thesisaccum: 17, catalystwindow: 1) from real
production data.

**Promotion review excludes probe rows.** `AggregatePromotionSamples`
now filters `strategy_version NOT ILIKE '%integration%' / '%probe%'
/ '%test%'` AND requires `clv_6h IS NOT NULL`, so promotion can
never promote a strategy that only saw synthetic data.

**Standalone outcome resolver (v11.8 PART 7).** The outcome lister
also reads `ListStandaloneShadowRowsForOutcomeBackfill` — shadow
rows without `linked_alert_dedup_key` whose market is `closed=TRUE`
get `outcome_status='unknown'` (market closed but per-side resolution
is Phase E). Stops the "forever NULL" state.

**Operator runbook:**

```sql
-- per-strategy shadow row counts in the last 24h
SELECT strategy_name, COUNT(*), AVG(clv_6h) AS avg_clv_6h_cents
FROM polymarket_strategy_shadow_decisions
WHERE fired_at >= NOW() - INTERVAL '24 hours'
GROUP BY strategy_name ORDER BY 2 DESC;

-- skip reasons over the last hour
sum by (strategy, reason) (rate(watchtower_strategy_eval_skipped_total[1h]))

-- promotion eligibility per strategy
SELECT strategy_name, sample_size, median_signed_move_6h,
       reversal_15m_ratio, alerts_per_day, eligible
FROM polymarket_strategy_promotion_reviews
WHERE reviewed_at = (SELECT MAX(reviewed_at) FROM polymarket_strategy_promotion_reviews)
ORDER BY eligible DESC, strategy_name;
```

**To promote a strategy to live:**
1. Wait for ≥50 shadow firings AND `eligible=true` in the latest review.
2. Set `STRATEGY_LEARNING_LOOP_PROMOTION_ALLOWED=true`.
3. Set `<STRATEGY>_SHADOW_ONLY=false` for the specific strategy.
4. Verify `STRATEGY_PROMOTION_BYPASS_EXPLICIT=false` (the default).

Bus enforces all three gates simultaneously. Operator-flag-alone is
insufficient.

## Strategy Learning Loop v11.9 (gap closure / honest limitations)

v11.9 closed multiple v11.8 stubs and made the remaining limitations
explicit.

**Repricing close phase — real (PART 4).**
`repricingPriceSampler.SampleTarget` reads the first trade price on
the target condition via the sqlc `FirstTradePriceForCondition`
query. `SamplePeerMedian` resolves peers via
`ListPeerConditionsByMarketLinks` and computes a median across
per-peer first-trade prices. The worker now emits
`stale_missing_price` / `stale_missing_peers` instead of the v11.8
universal `closed_blocked` stub. Migration 00033 widens the status
CHECK to cover both stale codes.

**Thesis lines background matrix (PART 5).**
New table `polymarket_wallet_thesis_lines` (migration 00032). The
`thesislines.Worker` periodically aggregates per-(wallet, condition,
side) directional exposure via sqlc `AggregateWalletThesisLines` and
upserts into the table. The hot-path reader (`ListWalletThesisLinesForEvent`)
is bounded by `THESIS_HOTPATH_MAX_LINKED_MARKETS` + a 250ms timeout.
thesisaccum can now compute real breadth/consistency once the worker
populates the table — replacing the v11.8 structural-tag-only path.

**Outcome resolver per-side correct/wrong (PART 6).**
`outcomeLister.ListShadowRowsForOutcomeBackfill` now also iterates
`ListStandaloneResolvedAlertOutcomes` and maps shadow rows to
`resolved_correct`/`resolved_wrong` by exact case-insensitive match
of `Side` against the alert's `winning_outcome_label`. Closed
markets without a winning label still fall back to `unknown`.

**Holdersync — no live source (HONEST limitation).**
`internal/infra/polymarket/dataapi/client.go` does NOT wrap a
Polymarket holders/positions/OI endpoint. `HOLDERSYNC_SOURCE_MODE`
defaults to `disabled` and the adapter returns `ErrNoSource`. No
fake pct_oi or holder rows. holderdelta remains explicitly silent
with `eval_skipped{reason=no_holder_snapshots_available}`. Adding
a verified endpoint is **Phase F**.

**Book feature bars — no producer (HONEST limitation).**
The Polymarket CLOB WS `book` event has `Bids/Asks` depth on the
wire (`internal/infra/polymarket/ws/types.go::wireBook`) but the
decoded `Event` struct only surfaces `BestBid/BestAsk/Mid`. Wiring
depth through Event + aggregating into `polymarket_book_feature_bars`
is **Phase F**. bookvacuum remains silent with
`eval_skipped{reason=no_book_feature_bars_producer}`.

**v11.9 env audit invariant (PART X).**
Three pinned tests in `internal/app/env_audit_test.go`:
- `TestEnvFiles_StrategyKeysSynchronized` — `.env` ≡ `.env.example`
  key sets.
- `TestEnvFiles_StrategyV11KeysAllPresent` — every v11.5–v11.9
  required strategy key is in both files.
- `TestEnvFiles_DangerousDefaultsBlocked` — promotion / Telegram
  noise surfaces default `false` everywhere.

`scripts/audit-env.sh` is the operator-facing diff tool; it prints
keys missing on either side AND warns on `.env` keys that don't bind
to a config struct (legacy stale keys).

**Phase F (residual — verified-external-source gated):**

1. Polymarket holders / positions / OI HTTP client (verified
   endpoint shape needed before adapter can be wired).
2. WS orderbook depth → `polymarket_book_feature_bars` aggregator.
3. thesisaccum hot-path consumes `polymarket_wallet_thesis_lines`
   (currently the staged reader has structure but the detector path
   continues to emit Tag-only — once the lines table is populated by
   the v11.9 worker, the existing thesisaccum hooks can switch from
   structural Tag to real KindStandalone fires).

## Strategy Learning Loop v11.10 (production-grade hardening)

v11.10 PART 1 — **WS selector COALESCE bug fix.** The hot-mode
subscription SQL used `unnest()` inside a `COALESCE()` expression to
align JSONB `clob_token_ids` with `outcomes` by index. Postgres 14+
rejects this with SQLSTATE 0A000 ("set-returning functions are not
allowed in COALESCE"). The fix uses two `LATERAL ... WITH ORDINALITY`
joins on `jsonb_array_elements_text` — token ↔ outcome are paired by
ordinal position. Pinned by
`TestBuildSelectorSQL_NoSRFInsideCoalesce`,
`TestBuildSelectorSQL_UsesLateralWithOrdinality`, and
`TestBuildSelectorSQL_AlwaysLimitsByOne` in
`internal/app/usecase/realtime/selector_test.go`.

The companion safety guard in `Worker.persistEvent`: `raw_json` is a
JSONB column, so any byte slice that is NOT valid JSON (heartbeat /
pong / truncated frames) is dropped to nil at the persist boundary
via `json.Valid` rather than fed to Postgres. Otherwise the driver
returns SQLSTATE 22P02 per event. Pinned by
`TestHandle_RawCaptureDropsInvalidJSON`.

v11.10 PART 6 — **Worker priority-bucket budgeting.** Strategy-
supporting workers (holdersync, bookbars) no longer scan every active
market. They consult `internal/app/usecase/workerbudget.Selector`,
which issues ONE Postgres roundtrip (`ListBucketedMarketTokens`) that
returns deduped `(condition_id, token_id)` rows annotated with their
highest-priority bucket:

| Bucket | Source |
|---|---|
| 1 — operator_pinned | `WORKER_OPERATOR_PINNED_CONDITION_IDS` |
| 2 — recent_alert | `polymarket_alerts.created_at > NOW() - 24h` |
| 3 — catalyst_near | `polymarket_event_catalysts.status IN ('active','expected')` AND `expected_at ≤ NOW()+72h` |
| 4 — linked_to_fired | `polymarket_market_links` neighbour of any recent alert |
| 5 — liquid | top by `polymarket_event_page_markets.liquidity` over recent 24h snapshot |
| 6 — fallback_active | safety net — `polymarket_markets ORDER BY last_seen_at DESC` |

Each bucket has its own per-cycle cap so a fat bucket cannot starve
operator pins. Dedup keeps MIN(bucket) per `condition_id`. Per-bucket
selection counts are emitted as
`watchtower_strategy_worker_items_total{worker, op="bucket:<name>"}`
so an operator can see the bucket mix per worker on the dashboard.
When all caps are 0 AND no pins are set, workers fall back to the
legacy unbucketed lister (backward-compatible).

Config keys (defaults match the v11.10 ТЗ):
`WORKER_BUDGET_OPERATOR_PINNED_MARKETS=20`,
`WORKER_BUDGET_RECENT_ALERT_MARKETS=30`,
`WORKER_BUDGET_CATALYST_NEAR_MARKETS=40`,
`WORKER_BUDGET_LINKED_TO_FIRED_MARKETS=30`,
`WORKER_BUDGET_LIQUID_MARKETS=50`,
`WORKER_BUDGET_FALLBACK_ACTIVE_MARKETS=20`,
`WORKER_OPERATOR_PINNED_CONDITION_IDS=`. All pinned in
`internal/app/env_audit_test.go::TestEnvFiles_StrategyV11KeysAllPresent`.

v11.10 PART 7 — **Bucketed promotion review.**
`polymarket_strategy_promotion_reviews.bucket_diagnostics JSONB`
(migration 00034) carries per-bucket sub-aggregates so a strategy
cannot be declared healthy purely on a weak whole-strategy median.
Two dimensions today: `decision_level` (info / warning / critical /
hard) and `linkage` (linked / standalone). The worker now consumes
`AggregatePromotionSamplesByDecisionLevel` +
`AggregatePromotionSamplesByLinkage`, computes a `BucketReview` per
key, and persists the assembly into the JSONB column. The eligibility
veto: a strategy with `Eligible=true` on the whole-strategy aggregate
is downgraded to `Eligible=false` (reason `no_eligible_non_trivial_bucket`)
unless at least ONE bucket with `SampleSize ≥ MinSampleSize` is also
eligible — exactly the failure mode PART 7 closes. Pinned by
`TestEvaluateBuckets_PerBucketEligibilityFollowsSameRules`,
`TestTick_BucketVetoBlocksHealthyAggregate`, and
`TestTick_BucketDiagnosticsPersisted`.

v11.10 PART 4 — **Strategy fanout replay report.** New CLI command
`go run ./cmd/cli replay-strategy-shadow --dsn=… [--since 24h] [--json]`.
Read-only aggregate over `polymarket_strategy_shadow_decisions` +
latest promotion review (with bucket diagnostics) +
staged-input coverage of recent alerts (active catalysts,
market_links, closed/stale repricing windows, risk scores,
walletgraph edges, fresh bookbars, fresh holder snapshots). Lets the
operator see per-strategy eval / fired / shadow_only / promoted /
linked / standalone / with_clv counts WITHOUT touching Telegram, AI,
or the live alert pipeline. The report is the canonical "did the
fanout actually fire?" diagnostic.

The command is intentionally NOT a write path — synthesising shadow
rows during a smoke would violate the v11.10 "never fake data" rule.
Fresh shadow rows only land when a real alert fires on the production
binary.

## WebSocket selector v11.12 (insider-prior; market-limited)

v11.12 rebuilt `internal/app/usecase/realtime/selector.go` around two
non-negotiable contracts:

- **prediction-free.** The selector no longer references
  `polymarket_market_predictions`, `current_state`, or any prediction-
  state input. Prediction tables are stale (v11.2 stopped writing) and
  were silently degrading WS coverage by ranking dead rows into the hot
  set. The legacy modes `predictions` and `all_active_limited` are
  removed; `hot` and `alerts` are the only valid `WS_SUBSCRIPTION_MODE`
  values. Pinned by
  `selector_test.go::TestBuildSelectorSQL_NoPredictionReferences`.

- **market-limited, not token-limited.** The selector caps at
  `WS_MAX_MARKETS` ($1) and every selected market expands to ALL its
  `clob_token_ids`. The WS client subscribes to every distinct token
  with no per-market cap, no first-N slicing, no `WS_MAX_TOKENS`
  truncation. The previous behaviour silently dropped May/Jul/Dec
  multi-leg outcome tokens when the binary cap landed between
  outcomes — the explicit failure mode this rebuild closes. The legacy
  `WS_MAX_TOKENS` key is in `staleEnvKeys{}` and boot-fails loudly if
  set. The replacement `WS_MAX_TOKENS_HARD_CAP` (default 50000) is a
  circuit-breaker only — when exceeded, `ws.Client.Run` returns
  `ws.ErrTokenHardCapExceeded` rather than slicing. Pinned by
  `selector_test.go::TestBuildSelectorSQL_LimitIsMarketsNotTokens`,
  `subscribe_resolver_test.go::TestResolveSubscribeTokens_*`.

New hot bucket scheme (priority 1 = highest):

| Priority | Bucket | Source |
|---|---|---|
| 1 | `operator_pinned` | `WORKER_OPERATOR_PINNED_CONDITION_IDS` (text[] passed as $2) |
| 2 | `recent_alert` | `polymarket_alerts.created_at > NOW() - 24h` |
| 3 | `active_or_expected_catalyst` | `polymarket_event_catalysts.status IN ('active','expected')` |
| 4 | `repricing_signal` | `polymarket_repricing_signals.created_at > NOW() - 24h` with status / flow_timing filter |
| 5 | `event_annotation_recent` | `polymarket_event_annotations.timestamp > NOW() - WS_ANNOTATION_LOOKBACK` (default 168h = 7d) |
| 6 | `high_trade_market` | opt-in (`WS_INCLUDE_HIGH_TRADE_MARKETS=true`) |

**Annotation lookback widened from 12h to 7d.** Live audit of
`polymarket_event_annotations` (875 rows, source=`linkup`, title +
summary + price_change) showed the linkup feed emits ~1 entry per day
per event. The previous 12h window caught **1** event vs **4** events
at 7d on identical live data. The widening is configured via
`WS_ANNOTATION_LOOKBACK=168h` (default).

CLI: `go run ./cmd/cli ws-selector-debug --dsn=… --limit-markets=… [--show-tokens] [--include-high-trade] [--operator-pinned=cid1,cid2] [--json]`
reports `selected markets`, `selected tokens`, `max tokens per market`,
`markets with >2 tokens`, `total token expansion`,
`dropped tokens` (must be 0), `prediction buckets present`
(must be `no`), and per-market full token expansion.

