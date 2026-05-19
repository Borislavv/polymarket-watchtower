# Current strategy map

A single-page reference describing every active alerting strategy in
the running Watchtower binary, the gates each one applies, and how
strategies interact when they fire on overlapping trades.

Strategy identity (code-owned, woven into every dedup key):
`anomaly.StrategyIdentity = "informed-flow-v6"`
(see `internal/domain/model/anomaly/strategy.go`).

Everything below describes **the current implementation**, not the
roadmap. If the doc and the code disagree, the code wins — open a PR
to reconcile.

---

## 1. Single-trade whale flow

**Package:** `internal/app/usecase/analytics/score` (scorer),
fired from `internal/app/usecase/detect.Loop.Observe`.

**Purpose.** Surface one trade that is large in absolute USD AND
sits in the tail of the per-bucket distribution (the per-(category,
market, outcome) reservoir) and/or the trader's own history.

**Input data.**
- One trade (notional USD, price, odds).
- Baseline stats for the (category, market, outcome) bucket from
  `dbbaseline.Provider` (Postgres-backed) or `baseline.Baseline`
  (in-memory dev only).
- Trader-axis stats from `traderbaseline.Provider` when configured
  and the wallet has ≥ `TRADER_MIN_HISTORY_TRADES` history.

**Hard gates** (must ALL pass before scoring runs):
1. Category whitelist on `slug + " " + label` only (see CLAUDE.md
   §category whitelist rule).
2. Lifecycle known AND ≥ `LIFECYCLE_ALERT_FROM_PCT` (default 75).
3. Market absolute age ≥ `MARKET_MIN_AGE` (default 24h).
4. `LIVE_ALERT_MAX_LAG` — trade traded_at within configured staleness.
5. Baseline readiness on whichever axis is being enforced
   (`SINGLE_MIN_BASELINE_TRADES`, `SINGLE_MIN_BASELINE_NOTIONAL_USD`,
   `BASELINE_MIN_READY_WINDOW`).

**Score / tier composition.**
Per-tier (`Info` / `Warning` / `Critical`):
- absolute floors (`MinNotionalUSD`, `MinOdds`, `MinProfitUSD`);
- market-axis tail ratio floor (`MinMarketP95Ratio`,
  `MinMarketP99Ratio`);
- trader-axis tail ratio floor (`MinTraderP95Ratio`,
  `MinTraderP99Ratio`).

Severity is the highest tier whose every non-zero floor clears.
A trade fires if it qualifies on EITHER axis (market OR trader) —
the axes are evaluated independently. `Finding.MultiplierAxis`
records which axis contributed.

Single-trade severity caps at Critical. `hard` is cluster-only.

**Dedup key.**
`single:<strategy>:<trade_id>` (per-trade, so a re-detection on the
same row collapses to one row in `polymarket_alerts`).

**Suppression rules.**
- MM filter (`mmfilter`): wallet shows ≥ N two-sided trades on the
  same (market, outcome) inside `MM_LOOKBACK` and the buy/sell
  imbalance is within `MM_NEUTRALITY_TOL` → suppressed,
  `watchtower_filter_alert_mm_suppressed_total{reason="POSSIBLE_MARKET_MAKER"}`
  bumped. Cluster path is NOT affected.
- `LIVE_ALERT_MAX_LAG`: replayed-from-history trades are dropped
  with reason `too_old_for_live_alert`.

**Telegram output.** Header:
`{SEV}: x{mul} · ${notional}[ · HOT] · {title}`. Sections: Why,
Trade, Cluster (only when HARD), Links, Analyst note (when AI
succeeded). See `internal/infra/alerting/telegram.go`.

**Example alert.** $50k BUY of "Yes" on a 92%-lifecycle politics
market at odds ≥ 5 against a baseline median of $200 → Warning.

**Example no-alert.** $5k BUY at odds 1.5 on the same market →
under Info notional floor; baseline keeps warming.

---

## 2. Same-trader accumulation (recent + lifetime)

**Package:** `internal/app/usecase/analytics/accumulation`, fired
from `detect.Loop` per trade.

**Purpose.** Detect a wallet repeatedly building exposure on a
single `(market, outcome, side)`. v4 introduced this to catch the
"200 × $200 = $40k" shape that single-trade detection misses.

**Two windows, each with its own dedup namespace:**
- `accumulation:<sv>:recent:<wallet>:<mid>:<token>:<side>:<bucket>`
  — `ACCUMULATION_WINDOW` cooldown-bucket dedup.
- `accumulation:<sv>:lifetime:<wallet>:<mid>:<token>:<side>:<severity>`
  — exactly one alert per severity tier per line.

**Input data.** `repository.AccumulationLineSummary` (sqlc-backed)
returning per-wallet line aggregate. Backed by index
`idx_trades_trader_market_outcome_side_time`.

**Hard gates** (all per-tier `T`):
- `trades ≥ tier_min_trades(T)`.
- `avg_odds ≥ T.MinOdds`.
- `lineTotal / marketMedian ≥ T.MinMultiplier`.
- Plus one of:
  - **meaningful**: `medianTrade ≥ FRACTION × T.MinNotionalUSD` AND
    `lineTotal ≥ TotalMultiplier × T.MinNotionalUSD`.
  - **many-smalls**: `lineTotal ≥ ManySmallsMultiplier × T.MinNotionalUSD`.

`Hard` is reserved for very large lines in HOT lifecycle.

**Score formula.** No separate score; severity is the highest tier
that clears.

**Suppression rules.**
- MM filter applies on the same `(wallet, market, outcome)` two-
  sided activity.
- Same lifecycle / age gates as single-trade flow.

**Telegram output.** Same envelope as single-trade; the
`Accumulation` payload renders a per-line block with trade count,
total, mean/median/max, span, and the recent/lifetime window tag.

**Example alert.** 9 BUYs totalling $36k over 18h on a 90%-lifecycle
politics market, median trade $4k vs market median $200 → Warning
recent + Warning lifetime.

**Example no-alert.** Same wallet, $36k total but spread over a
single morning across BUY and SELL → bidirectional → MM filter +
cross-flow context tag downgrade in AI note.

---

## 3. Cluster (HARD)

**Package:** `internal/app/usecase/analytics/cluster`.

**Purpose.** Multiple wallets converging on a single category in a
short window — qualitatively distinct from one big bet, hence the
top severity `Hard`.

**Hard gates.**
- `len(entries) ≥ CLUSTER_MIN_ANOMALOUS_TRADES` (default 3).
- `uniqueWallets ≥ CLUSTER_MIN_UNIQUE_TRADERS` (default 2).
- `totalUSD ≥ CLUSTER_MIN_TOTAL_NOTIONAL_USD` (default $50k).
- Last fire ≥ `CLUSTER_COOLDOWN` ago (default 30m).

**Severity.** Always `Hard`. The single-trade alerts that fed the
cluster window also get `Finding.InCluster` and
`ClusterPeerCount` stamped.

**Suppression rules.** MM filter intentionally does NOT apply —
multiple wallets converging is meaningful even if some are MMs.

**Dedup key.**
`cluster:<sv>:<category_id>:<bucket_window>`.

---

## 4. Ownership concentration

**Package:** `internal/app/usecase/analytics/ownership`.

**Purpose.** Approximate (no holders endpoint upstream) the share of
the outcome's trade-flow a single wallet has accumulated. Surfaces
"one wallet has 30% of all BUY flow on this outcome" patterns.

**Input.** `polymarket_trades` aggregated as `(wallet net_BUY_shares
/ market_total_BUY_shares) × 100`.

**Hard gates.** Tier-based — share thresholds per Info/Warning/
Critical configurable via env. Tier is the highest tier whose
share floor clears.

**Score / severity.** Pure tier on share percentage. Per-tier dedup;
severity upgrades emit one new alert at each tier.

**Suppression rules.** Documented as a flow approximation —
**never** described as "true holdings" in the Telegram body.

**Dedup key.**
`ownership:<sv>:<wallet>:<mid>:<token>:<severity>`.

---

## 5. Stable favorite (late-market convergence)

**Package:** `internal/app/usecase/stablefavorite` (worker) +
`internal/app/usecase/analytics/stablefavorite` (detector).

**Purpose.** Surface late-stage markets that have converged on a
favorite still inside the favorite-probability band (default
0.55-0.85), with low recent volatility, no adverse drift, and
meaningful remaining payout. State-driven, not per-trade.

**Hard gates** (in order; first miss returns):
- `LifecyclePct ≥ MinLifecyclePct` (default 92).
- price within `[MinProbability, MaxProbability]`.
- remaining-return `(1-p)/p × 100 ≥ MinReturnPct` (default 20).
- `Window24h.VolumeUSD ≥ MinMarketVolumeUSD`.
- `Window24h.Count ≥ MinRecentTrades`.
- `Window24h.Stddev ≤ MaxPriceStddev`.
- `Drawdown ≤ MaxDrawdown`.
- 6h adverse drift within `MaxAdverseMove6h` / `MaxNegativeDrift6h`.

**Score formula** (`scoreOf`):
- 25% lifecycle linear over min..100.
- 25% stability `1 − stddev/maxStddev`.
- 20% remaining-return saturating at 100%.
- 15% liquidity `log10(1 + vol/floor)` clamped.
- 10% no-reversal `1 − |neg drift| / maxAdverse`.
- 5% cross-market (`confirmed=1`, `conflict=0`, `unavailable=0.8`).

**Confidence** (`confidenceOf`):
- 0.40 base + 0.25 sample-count factor + 0.20 volume factor +
  bonus on cross-market `confirmed` / partial on `unavailable`.

**Severity** (`pickSeverity`): Info/Warning/Critical floors on
lifecycle + score + confidence. **Cross-market is NOT a hard gate**
(v7 removal — see CLAUDE.md §post-v7 hardening); it only affects
score/confidence.

**v7 tags / downgrades:**
- `VOLATILITY_EVENT_PENDING` tag (no severity effect): 6h stddev ≥
  1.5× 24h stddev with enough samples.
- `HYPE_MARKET_SUPPRESSION` downgrade by one rung: 24h vol ≥ 5×
  floor AND 6h vol ≥ 50% of 24h.
- `RISK_ADJUSTED_RETURN`: tag always emitted; carries
  `remainingReturnPct / (stddev × 100)`.

**Dedup key.**
`stable_favorite:<sv>:<market_condition_id>:<outcome_token>:<severity>`.
One alert per (market, outcome, severity); severity upgrades emit
one new alert per tier.

**Telegram output.** Carries `StableFavoriteRef` — probability,
remaining-return, stability stats, lifecycle, score/confidence,
cross-market status. **Never described as "safe" or "guaranteed".**

**Example alert.** Lifecycle 96%, price 0.7, stddev 0.015,
volume_24h $120k, 150 samples → Info or Warning depending on score.

**Example no-alert.** Same market but stddev 0.20 → skipped on
`SkipStability`.

---

## 6. New-wallet / dormant-wallet context boosters

**Package:** detector layer in `detect.Loop` (NewWalletConfig +
DormantWallet probe).

**Purpose.** Annotate a single-trade or accumulation Finding when
the firing wallet is suspicious context-wise:
- **new wallet:** age < `NEW_WALLET_MAX_AGE` OR history-trades ≤
  `NEW_WALLET_MAX_HISTORY_TRADES`.
- **dormant wallet:** wallet has been idle ≥
  `DORMANT_WALLET_MIN_IDLE` AND current trade ≥
  `DORMANT_WALLET_MIN_NOTIONAL_USD`.

**Behaviour.** Context-only — never standalone, never promotes
severity. Stamps the appropriate `*Ref` on the Finding so the
formatter renders a "new wallet" / "dormant wallet" line and adds
the corresponding reason code (`NEW_WALLET_LARGE_BET`,
`DORMANT_WALLET_REVIVAL`, etc.).

**Dedup key.** Not its own — the underlying single-trade /
accumulation key remains primary.

---

## 7. Quiet-market wake-up

**Package:** `internal/app/usecase/analytics/quietmarket`.

**Purpose.** Context tag attached when the firing single-trade or
accumulation event lands on a historically quiet (market, outcome).
Surveillance read: "a sleepy market just woke up".

**Hard gates** (per `Decide` in detector):
- baseline `tradesPerDay ≤ MaxTradesPerDay`.
- `notionalPerDay ≤ MaxNotionalPerDayUSD`.
- `now − LastTradedAt ≥ MinIdleDuration`.
- event notional ≥ `MinCurrentNotionalUSD`.
- event notional / marketMedian ≥ `MinMultiplier` (optional).

**Behaviour.** Tag-only — appends `QUIET_MARKET_WAKEUP` to
`Finding.Reasons` and stamps `Finding.QuietMarket`. Cluster Findings
are not tagged.

---

## 8. Low-baseline confidence cap

**Package:** scorer (`internal/app/usecase/analytics/score`).

**Purpose.** When a tail gate was skipped because the baseline
was not ready, prevent thin baselines from producing pager-grade
alerts.

**Behaviour.** When `LowBaselineCapEnabled=true`, severity is
capped at `LowBaselineSingleMaxSeverity` (typically Info) UNLESS
the trade clears the Critical absolute floor and
`LowBaselineAllowCriticalAbsolute=true`. Surfaced via
`SeverityCapped=true` and the `SEVERITY_CAPPED_LOW_BASELINE`
reason code.

---

## 9. MM / bidirectional suppression

**Package:** `internal/app/usecase/analytics/mmfilter`.

**Purpose.** Drop alerts where a wallet's two-sided activity on the
same `(market, outcome)` inside `MM_LOOKBACK` looks like
market-making.

**Hard gates** (both must hold to suppress):
- `count(BUY) ≥ MM_MIN_TRADES_PER_SIDE` AND
  `count(SELL) ≥ MM_MIN_TRADES_PER_SIDE`.
- `|buy − sell| / max(buy, sell) ≤ MM_NEUTRALITY_TOL`.

**Behaviour.** Suppresses single-trade and accumulation alerts.
Cluster alerts are NOT suppressed. Fails OPEN on DB errors —
a hiccup must not swallow a real alert. Metric:
`watchtower_filter_alert_mm_suppressed_total{category, reason}`.

---

## Strategy interaction matrix

How the system behaves when multiple strategies / tags apply to the
same trade. "Emit", "downgrade", "suppress", "tag only", "cap
severity" are the only legal outcomes — anything else is a bug.

| Combination | Expected behavior | Why |
|---|---|---|
| accumulation + opposite-side flow on same market | **emit** the accumulation alert; AI receives cross-flow fields, prompt encourages Watch/Unclear verdict | Cross-flow is information for the operator, not a hard suppressor — the alert itself is real. |
| accumulation + same-wallet bidirectional flow | **suppress** via MM filter when the two-sided gate clears; otherwise emit with `POSSIBLE_MARKET_MAKER` reason and AI markets-making warning | The MM filter exists to drop the literal "this wallet is making the book" case; suppressed at the source. |
| accumulation + new wallet | **emit + tag** `NEW_WALLET_ACCUMULATION` | Context booster never standalone; the accumulation is the real signal. |
| accumulation + low baseline | **emit + tag** `LOW_MARKET_BASELINE_CONFIDENCE` + (if cap enabled) **cap severity** at `LowBaselineSingleMaxSeverity` | Thin market data cannot justify pager-grade alerts; preserves caution without dropping the alert. |
| stable favorite + high recent volatility | scorer **rejects** on `SkipStability`; no alert | Stability is the load-bearing premise of the strategy; volatility violates the gate. |
| stable favorite + concurrent whale flow on same market | both **emit independently** with distinct dedup namespaces | Different strategies, different decision logic. Operator sees two alerts and the AI note can name the convergence. |
| stable favorite + meme / noise market | **emit** (no novelty gate in stable_favorite); AI prompt receives `NoveltyOrMemeGuess=true` and biases to Avoid/Watch | The scorer doesn't know what "novelty" means; the AI does. Tag-driven downgrade is the right layer. |
| ownership + partial history | **emit** with `Approximate=true`; Telegram body explicitly says "approximate" | Documented limitation; never claim true holdings. |
| ownership + accumulation on same (wallet, market, outcome) | **emit both**; the single-trade / accumulation Finding also gets `OWNERSHIP_FUSION` stamped by detect.Loop | Different read on the same wallet position; both useful. |
| new wallet + large single trade | **emit** the single-trade alert + tag `NEW_WALLET_LARGE_BET` | Context booster only. |
| dormant wallet + large single trade | **emit** + tag `DORMANT_WALLET_REVIVAL` | Context booster only — "long-lived wallet woke up". |
| cluster + individual single trades | **emit individual** alerts AND **emit** HARD cluster alert when gate clears; per-trade alerts get `InCluster=true` + `ClusterPeerCount` stamped | Cluster is qualitatively different — multiple wallets converging. |
| low baseline + critical absolute notional | **emit** with `SeverityCapped=true`; if `LowBaselineAllowCriticalAbsolute=true`, severity reaches Critical | Operator-tunable: enormous notional may justify pager-grade even on thin baseline. |
| quiet market + accumulation | **emit** accumulation + tag `QUIET_MARKET_WAKEUP` | Tag-only — quiet baseline doesn't change the accumulation gate. |
| MM filter + accumulation | **suppress** | Filter fires before the alert is created. |
| MM filter + cluster | **emit** cluster | Documented exception — multi-wallet cluster is not a single-MM signal. |
| stable favorite + hype suppression | **downgrade** by one rung + tag `HYPE_MARKET_SUPPRESSION` | Fresh volume spike is the opposite of orderly convergence. |
| stable favorite + volatility-event-pending | **emit + tag** `VOLATILITY_EVENT_PENDING` (no severity change) | The scorer already considers stability; double-downgrading proved over-engineered (see detector comments). |

---

## What every operator should know

1. **The detection queue is the only path to Observe in Postgres
   mode.** `collect.Loop.observer` is a true nil-interface. If you
   ever see `collect.pull → detect.Observe` in a stack trace,
   that's the typed-nil regression (fixed v8) and the wiring must
   be re-inspected.

2. **AI failures NEVER produce a `polymarket_alert_analyses` row.**
   They produce a `polymarket_ai_request_logs` row. Querying the
   analyses table by `WHERE status='ok'` is now safe (legacy bad
   rows are flagged with `legacy_provider_failure=TRUE`).

3. **Market-intelligence reports persist the AI text only.** The
   Telegram body is rendered at send time; the `report_text` column
   contains the model's answer verbatim, not the rendered HTML.

4. **`backfill_status='partial_api_limit'` markets are not re-tried
   for `BACKFILL_PARTIAL_RETRY_AFTER` (default 6h)** after their
   last completion. The Polymarket 3000-row offset cap is
   structural; tight retry loops would burn quota.

5. **AI structural validation is removed (v8.1).** The prompt asks
   for `Thesis: / Follow?: / Verdict:` structure, but model output
   that lacks the labels is still accepted. Only empty text or
   accidental provider-error JSON is rejected.
