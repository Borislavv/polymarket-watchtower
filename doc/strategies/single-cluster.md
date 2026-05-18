# `single_cluster` — per-trade scoring + cluster

The product's primary detector. The unit of detection is the **individual
trade**: every freshly ingested trade is scored against the per-(category,
market, outcome) baseline and either fires a single-trade alert or doesn't.
The cluster detector then aggregates fired alerts to spot multiple sharks
converging on one category.

Two signal kinds reach Telegram:

1. **Single-trade alert (`LargeRareBet`)** — `info` / `warning` / `critical`.
2. **Cluster alert (`WhaleClusterDetected`)** — `hard`. Composed of already-
   fired single-trade alerts that converge on one category.

`hard` is **only** ever emitted by the cluster detector. Single-trade
severity caps at `critical` so the four-rung scale stays meaningful:
`hard` is the "two or more sharks agreeing" escalation, qualitatively
different from any single bet.

## Single-trade scoring (`score.Score`)

For each trade the detector evaluates two ladders:

```
Absolute    : trade USD notional AND 1/price (implied odds)
Multiplier  : trade USD notional / max(marketMedian, traderMedian)
```

A trade fires only when **both** ladders qualify at info or above. The
emitted severity is the conservative MIN of the two tier outcomes
(`anomaly.ConservativeMin`). Below info on either side ⇒ no alert.

### Two multiplier axes (v2)

`score.Score` now consumes **two** baselines:

- **Market axis** — per-(category, market, outcome) distribution. The v1
  signal: "is this trade big relative to the market's normal trades?"
- **Trader axis** — the wallet's full-history distribution across all its
  trades. The v2 addition: "is this trade big relative to *this wallet's*
  normal trades?"

The effective multiplier is `max(marketMultiplier, traderMultiplier)` and
the multiplier-ladder tier is evaluated on that. A trade qualifies if it is
anomalous on **either** axis — surveillance literature treats the two as
complementary, not redundant. The classic informed-flow candidate (a
$200-history wallet placing a $25k bet on a busy market) is invisible on
the market axis (multiplier ~5) but lights up on the trader axis
(multiplier ~125). v1 missed it; v2 catches it.

The `Finding.MultiplierAxis` field names which axis contributed
(`market`, `trader`, `both`, or empty when neither was ready) — it's
surfaced in the Telegram "Why" section so an operator can tell at a glance
why the alert fired.

### Readiness

Each axis has independent readiness gates:

- **Market** — `SINGLE_MIN_BASELINE_TRADES`, `SINGLE_MIN_BASELINE_NOTIONAL_USD`,
  `BASELINE_MIN_READY_WINDOW`. When the market axis is unready the trader
  axis can still fire alone.
- **Trader** — `TRADER_MIN_HISTORY_TRADES` count gate. Below this the
  trader axis is silently disabled and v1 market-only scoring applies.
  Set to `0` to disable the trader axis entirely.

If **both** axes are unready the trade is silently dropped — the detector
refuses to rank rarity without at least one meaningful baseline.

### Severity table (defaults)

| Tier     | notional ≥ | odds ≥ | multiplier ≥ |
|----------|------------|--------|--------------|
| Info     | $10,000    | 3      | 100×         |
| Warning  | $25,000    | 5      | 1,000×       |
| Critical | $100,000   | 8      | 10,000×      |

A trade qualifies at the tier where BOTH dimensions clear. Conservative-MIN
of the two ladders is the final severity. Examples:

| Scenario                                      | Absolute | Multiplier | Final     |
|-----------------------------------------------|----------|------------|-----------|
| $100k @ odds 8, median $100 (mul 1,000)       | Critical | Warning    | Warning   |
| $26,999 @ odds 5.66, median $100 (mul 270)    | Warning  | Info       | **Info**  |
| $700k @ odds 8, median $60 (mul 11,666)       | Critical | Critical   | Critical  |
| $1M @ odds 3, median $60 (mul 16,666)         | Info     | Critical   | Info      |
| $300k @ odds 6, median $60 (mul 5,000)        | Warning  | Warning    | Warning   |

The fourth row is the canonical example of why conservative-MIN matters: a
huge raw bet at near-even odds is not "asymmetric-payoff insider activity"
even though the multiplier is large — the odds gate keeps Info honest.

## MM/arbitrage suppression (v2)

After a candidate fires single-trade, `mmfilter.Filter.Decide` examines the
wallet's two-sided BUY+SELL activity on the same `(market, outcome)` over
`MM_LOOKBACK`. The single-trade alert is **suppressed** when both:

1. `count(BUY) ≥ MM_MIN_TRADES_PER_SIDE` AND `count(SELL) ≥ MM_MIN_TRADES_PER_SIDE`
2. `|buyNotional − sellNotional| / max(buyNotional, sellNotional) ≤ MM_NEUTRALITY_TOL`

(1) guards against suppressing on a single profit-take SELL after a
directional BUY. (2) is the balanced-book check: at or below the
tolerance the activity is two-sided enough to look like liquidity
provision or arbitrage; above it the bias is directional and the alert
passes through.

Cluster alerts are **deliberately not** filtered here. Even if some of the
participating wallets are MMs, multiple wallets converging on one side of
a category is a signal worth paging — the cluster detector's own gates
(unique-wallets, total-notional, cooldown) make that judgement.

The filter fails open: a DB hiccup never causes a real alert to be
swallowed. Suppressions are recorded as
`watchtower_filter_alert_mm_suppressed_total{category=…}` and an info-level
log line with the buy/sell breakdown so a reviewer can audit what was
hidden.

Set `MM_FILTER_ENABLED=false` to disable suppression entirely (useful for
local exploration when you want to see every signal, MM-shaped or not).

## Same-trader accumulation line (v4)

A qualitatively new signal added in v4. The unit of detection is a
**line**: the set of trades from one wallet on a single
`(market, outcome, side)` inside `ACCUMULATION_WINDOW`. The signal catches
the "many-smalls" shape that single-trade scoring cannot see by
construction — 200 × $200 = $40k is invisible trade-by-trade but is a
strong informed-flow candidate as a whole.

### Severity ladder (anchored on existing tiers)

For tier T (Info / Warning / Critical), the line qualifies when **all**
of these hold:

- `trades ≥ tier_min_trades(T)`  →  Info=3, Warning=4, Critical=5
- `avg_odds ≥ T.MinOdds`
- `lineTotal / marketMedian ≥ T.MinMultiplier`
- One of two **size paths** clears:
  - **(meaningful)** `medianTrade ≥ FRACTION × T.MinNotionalUSD` AND
    `lineTotal ≥ TotalMultiplier × T.MinNotionalUSD`
  - **(many-smalls)** `lineTotal ≥ ManySmallsMultiplier × T.MinNotionalUSD`

Hard accumulation: `trades ≥ 5` AND `lineTotal ≥ HardMultiplier ×
Critical.MinNotionalUSD` AND HOT lifecycle. Reserved for the rare case
of a wallet visibly stacking into a near-close market.

### Worked examples (balanced preset defaults)

Balanced has Info $10k, Warning $25k, Critical $100k; accumulation
defaults `TotalMultiplier=2`, `ManySmallsMultiplier=4`, fraction `0.6`.

| Line                          | total  | median | path        | severity |
|-------------------------------|--------|--------|-------------|----------|
| 4 × $6k same outcome          | $24k   | $6k    | meaningful  | Info     |
| 200 × $200 same outcome       | $40k   | $200   | many-smalls | Info     |
| 10 × $5k same outcome         | $50k   | $5k    | meaningful  | Info     |
| 5 × $30k same outcome         | $150k  | $30k   | meaningful  | Warning  |
| 6 × $50k same outcome, HOT    | $300k  | $50k   | meaningful  | Hard*    |
| 3 × $5k same outcome          | $15k   | $5k    | n/a (fails) | (silent) |

*Hard requires lifecycle HOT in addition to the size + count gates.

### Reason codes

Each accumulation Finding carries a `Reasons` list:

- `REPEATED_SAME_OUTCOME_ACCUMULATION` — always included
- `LINE_TOTAL_NOTIONAL_HIGH` — total ≥ 2 × `TotalMultiplier` × tier
- `MANY_SMALL_TRADES_SAME_SIDE` — many-smalls path qualified
- `LINE_LARGE_VS_MARKET` — market multiplier ≥ 2 × tier
- `LINE_LARGE_VS_SELF` — trader multiplier ≥ 10
- `LATE_MARKET_ACCUMULATION` — lifecycle ≥ 75%
- `HOT_MARKET_ACCUMULATION` — lifecycle ≥ HotFromPct
- `LOW_SAMPLE_SIZE` — trades within 2 of `MinTrades` floor
- `POSSIBLE_MARKET_MAKER` — reserved (currently not emitted; MM filter
  blocks suppressed lines before the alert is constructed)

### Score and confidence

The detector also emits a 0..100 `Score` (triage heuristic, not a
probability) and a 0..1 `Confidence` (sample-size + baseline-readiness
weighted). They are exposed to Telegram and metrics but do NOT gate
firing — the deterministic tier ladder does.

### Persistence + dedup

Accumulation reads from `polymarket_trades` via
`repository.AccumulationLineSummary` — a single server-side aggregate
backed by `idx_trades_trader_market_outcome_side_time`. The detector
runs per-trade on the existing hot path (post-baseline, post-MM).

Dedup key:

```
accumulation:<strategy_version>:<wallet>:<market_id>:<outcome_token>:<side>:<window_bucket>
```

Where `window_bucket = floor(now / ACCUMULATION_COOLDOWN)`. The cooldown
prevents repeated alerts on the same line as new small trades trickle in
during the bucket. After the cooldown elapses a fresh bucket key allows
the alert to fire again if the line has materially grown.

### Interaction with other signals

- **vs single trade.** Independent. A single $200k bet can fire single-
  trade Critical; the same wallet's prior 200 × $200 line still produces
  its own accumulation Finding if it qualifies — different dedup keys,
  different alerts, complementary information.
- **vs cluster.** Independent. Cluster requires ≥ 2 wallets in one
  category; accumulation is by construction single-wallet single-market.
- **vs MM/arb.** The MM filter applies to accumulation: a wallet running
  balanced two-sided BUY+SELL on the same `(market, outcome)` is
  suppressed. Cluster alerts remain unaffected (multi-wallet shape).

## Cluster alert (`cluster` package)

When a single-trade alert fires it is pushed into the per-category cluster
window. The cluster fires HARD when, inside `CLUSTER_WINDOW`, one category
sees:

- `≥ CLUSTER_MIN_ANOMALOUS_TRADES` anomalous trades, AND
- from `≥ CLUSTER_MIN_UNIQUE_TRADERS` distinct wallets, AND
- totalling `≥ CLUSTER_MIN_TOTAL_NOTIONAL_USD`.

Per-category cooldown (`CLUSTER_COOLDOWN`) prevents spam.

The cluster only sees trades that already fired single-trade alerts — its
job is to escalate when multiple sharks circle the same category at once,
not to surface trades that no single-trade rule caught.

## Gates

These gates **only block alert emission**. They never block baseline updates
— the reservoir must warm continuously so it is ready the moment a market
crosses the lifecycle threshold.

- **Category filter** — `CATEGORY_WHITELIST` is a case-insensitive
  substring match against the category `slug + " " + label`. ONLY
  whitelisted categories are monitored; everything else is ignored. Market
  titles, event slugs, market slugs, and tags are NOT scanned. A market
  whose title mentions FIFA or NBA but whose category is whitelisted (e.g.
  inside Politics) is still analysed normally. Default: `Politics`.
- **Lifecycle gate** — `LIFECYCLE_ALERT_FROM_PCT` (alerts fire) and
  `LIFECYCLE_HOT_FROM_PCT` (alerts marked HOT). Markets without start/end
  dates are silenced when `ALLOW_UNKNOWN_MARKET_LIFECYCLE=false` (default).
- **Market age** — `MARKET_MIN_AGE` blocks alerts on markets younger than
  this in wall-clock terms, regardless of lifecycle percentage.
- **Baseline readiness** — `BASELINE_MIN_READY_WINDOW` requires the observed
  baseline span (newest − oldest sample) to clear this floor. Distinct from
  `BASELINE_WINDOW` (the cap).

## Regression: the France/FIFA case

> *Trade*: $26,999 @ price 0.1768 (odds 5.66)
> *Baseline*: 29 trades, median $100 (multiplier ≈ 270×)
> *Category*: `Hide From New` (NOT sports)
> *Market title*: "Will France win the 2026 FIFA World Cup?"
> *Lifecycle*: ~83% (late stage)
>
> **Expected**: single-trade alert at Info (absolute Warning, multiplier
> Info → conservative-MIN = Info). Pinned by
> `TestFranceFifaHideFromNewStillAlerts`.

A previous (now reverted) version of the watchtower silenced this trade by
also matching `sports`-style keywords against `market.Question`,
`market.Slug`, `market.EventSlug`. That was wrong: a sports-themed market
filed under a non-sports category is still real prediction-market activity
and warrants an alert. Filtering is category-identity-only.

## Telegram payload

Every Finding rendered to Telegram includes:

- Severity badge in the header.
- Category name and market question.
- Notional USD, implied odds, effective multiplier with the **axis** that
  drove the tier (market / trader / both).
- Market baseline line (count, median, mean, p95, span).
- Trader baseline line (count, median, p95, span) when the trader axis
  was available — gives the operator immediate context on "is this wallet
  acting out of character?".
- Lifecycle percent and a HOT marker when applicable.
- Cluster context (`InCluster`, `ClusterPeerCount`) when peers exist.
- Bulleted Links section: Polymarket event page, category page, trader
  profile, Grafana deep-link. **Each entry is a real Telegram HTML
  `<a href>` anchor** rendered via the `renderLink` helper. Entries whose
  URL is empty are omitted entirely — no bare "Grafana" / "Polymarket"
  plain-text label is ever emitted. The whole section is skipped when
  nothing is renderable.
- The **actual** baseline span used, not the configured cap.

### Link rendering contract

The Telegram sink sends messages with `parse_mode=HTML`. URLs containing
`&` (every Grafana deep-link) are escaped to `&amp;` via `html.EscapeString`
so Telegram parses the anchor tag instead of treating the ampersand as
literal text. Test coverage:

- `telegram_test.go::TestRenderLinkBuildsHTMLAnchor` — exact anchor shape.
- `telegram_test.go::TestRenderLinkEscapesLabel` — label escape contract.
- `telegram_test.go::TestLinksSectionExactFormat` — byte-for-byte Links
  block when all four URLs are present.
- `telegram_test.go::TestGrafanaLinkClickableInWirePayload` — end-to-end:
  captures the JSON payload sent to a fake Telegram server, decodes it,
  asserts `parse_mode=HTML` and that the `text` field contains the full
  `<a href="…">Grafana</a>` anchor.
- `telegram_test.go::TestLabelsNeverAppearAsPlainText` — regression guard:
  when any URL is empty, the corresponding label must NOT appear as a bare
  bullet ("• Grafana" must never exist outside of an `<a>`).
- `telegram_test.go::TestSpecialCharsInHrefAreEscaped` — `&`/`<`/`>`/`"`
  in href values are escaped to entities.
