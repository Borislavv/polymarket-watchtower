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
Multiplier  : trade USD notional / baseline-median USD
```

A trade fires only when **both** ladders qualify at info or above. The
emitted severity is the conservative MIN of the two tier outcomes
(`anomaly.ConservativeMin`). Below info on either side ⇒ no alert.

If the baseline is too thin (`stats.Count < MinBaselineTrades` or
`stats.TotalUSD < MinBaselineNotionalUSD` or `stats.MedianUSD == 0`) the
multiplier ladder cannot be evaluated and the trade is silently dropped —
the detector refuses to rank rarity without a meaningful baseline.

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

- **Category filter** — `CATEGORY_BLACKLIST` is a case-insensitive substring
  match against the category `slug + " " + label` and nothing else. Market
  titles, event slugs, market slugs, and tags are NOT scanned. A market
  whose title mentions FIFA or NBA but lives under a non-sports category
  (e.g. Polymarket's `Hide From New`) is still analysed normally. Default:
  `sports,sport`.
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
- Notional USD, implied odds, multiplier vs baseline median.
- Lifecycle percent and a HOT marker when applicable.
- Cluster context (`InCluster`, `ClusterPeerCount`) when peers exist.
- Bulleted Links section: Polymarket event page, category page, trader
  profile, Grafana deep-link. Links render only when the URL is non-empty.
- The **actual** baseline span used, not the configured cap.
