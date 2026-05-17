# `single_cluster` — per-trade scoring + cluster

This is the product's primary detector. The unit of detection is the
**individual trade**: a single bet that fires both ladders is the only thing
that ever produces a `LargeRareBet` alert.

Three signals feed Telegram:

1. **Single-trade alert (`LargeRareBet`)** — info / warning / critical / hard.
2. **Cluster alert (`WhaleClusterDetected`)** — HARD. Composed of already-
   firing single-trade alerts that converge on one category.
3. **Sub-cluster alert (`WhaleClusterDetected`)** — HARD. Composed of trades
   that *did not* fire single-trade alerts but look like coordinated splits.

## Single-trade scoring (`score.Score`)

For each freshly ingested trade the detector evaluates two ladders:

```
Absolute    : trade USD notional AND 1/price (implied odds)
Multiplier  : trade USD notional / baseline-median USD
```

A trade fires only when **both** ladders qualify at info or above. The
emitted severity is the conservative MIN of the two tier outcomes
(`anomaly.ConservativeMin`). Anything below info on either side ⇒ no alert.

If the baseline is too thin (`stats.Count < MinBaselineTrades` or
`stats.TotalUSD < MinBaselineNotionalUSD` or `stats.MedianUSD == 0`) the
multiplier ladder cannot be evaluated and the trade is silently dropped —
the detector refuses to rank rarity without a meaningful baseline.

### Override stacking

On top of the conservative-MIN result, three rules can promote the severity.
They stack softest-to-hardest so the strongest signal wins:

| Rule          | Floors (notional, odds, multiplier)        | Promotes to     |
|---------------|--------------------------------------------|-----------------|
| `HugeWhale`   | e.g. `$250k, 5, 1000×`                     | ≥ Critical      |
| `HardPromotionA` | e.g. `$250k, 5, 1000×`                  | Hard            |
| `HardPromotionB` | e.g. `$100k, 10, 2500×`                 | Hard            |
| `MegaWhale`   | e.g. `$1M, 3, 250×`                        | Hard            |

`HardPromotionA` and `HardPromotionB` are two independent OR branches so a
preset can express "either of two distinct insider shapes". The whale rules
exist because conservative-MIN can under-classify raw-size bets (e.g. a
$300k bet on a moderately rich market may rank Warning on the multiplier
ladder; HugeWhale rescues it). A zero-valued tier disables that override.

## Cluster alert (`cluster` package)

When a single-trade alert fires it is also pushed into the per-category
cluster window. The cluster fires HARD when, inside `CLUSTER_WINDOW`, one
category sees ≥ `CLUSTER_MIN_ANOMALOUS_TRADES` anomalous trades from
≥ `CLUSTER_MIN_UNIQUE_TRADERS` distinct wallets totalling
≥ `CLUSTER_MIN_TOTAL_NOTIONAL_USD`. Per-category cooldown prevents spam.

The cluster only sees trades that already fired a single-trade alert — its
job is to escalate to HARD when many sharks circle the same category at once,
not to surface trades that no single-trade rule caught.

## Sub-cluster alert (`subcluster` package)

The sub-cluster catches what the single-trade detector cannot: distributed
insider bets where a single wallet splits its position across many small
addresses. Each individual leg is small enough to slip below
`ALERT_INFO_MIN_NOTIONAL_USD` (so no single-trade alert fires), but the
collective shape — many distinct wallets, high implied odds, large
multiplier above the baseline median — is the unambiguous signature of a
coordinated whale strategy.

A trade is admitted as a sub-cluster candidate when ALL three per-candidate
floors clear:

```
notional   ≥ SUB_CLUSTER_MIN_TRADE_USD
odds       ≥ SUB_CLUSTER_MIN_ODDS
multiplier ≥ SUB_CLUSTER_MIN_MULTIPLIER
```

The sub-cluster fires HARD when ≥ `SUB_CLUSTER_MIN_UNIQUE_TRADERS` distinct
wallets totalling ≥ `SUB_CLUSTER_MIN_TOTAL_NOTIONAL_USD` accumulate inside
`SUB_CLUSTER_WINDOW`. It runs in parallel with the normal cluster (each
detector sees a disjoint set of trades: firing trades go to cluster, non-
firing qualifying trades go to sub-cluster), so a single category can
legitimately produce both signals on the same window — they describe
different patterns.

## Gates

These gates *only block alert emission*. They never block baseline updates —
the reservoir must warm continuously so it is ready the moment a market
crosses the lifecycle threshold.

- **Category filter** — `CATEGORY_BLACKLIST` is a case-insensitive substring
  match against the category `slug + " " + label` and nothing else. Market
  titles, event slugs, market slugs, and tags are NOT scanned. A market that
  *looks* like a sports market because its title or event slug mentions FIFA
  or NBA but lives under a non-sports category (e.g. Polymarket's
  `Hide From New`) is still analysed normally. Default: `sports,sport`.
- **Lifecycle gate** — `LIFECYCLE_ALERT_FROM_PCT` (alerts fire) and
  `LIFECYCLE_HOT_FROM_PCT` (alerts marked HOT). Markets without start/end
  dates are silenced when `ALLOW_UNKNOWN_MARKET_LIFECYCLE=false` (default).
- **Market age** — `MARKET_MIN_AGE` blocks alerts on markets younger than
  this in wall-clock terms, regardless of lifecycle percentage.
- **Baseline readiness** — `BASELINE_MIN_READY_WINDOW` requires the observed
  baseline span (newest − oldest sample) to clear this floor. Distinct from
  `BASELINE_WINDOW` (the cap).

## Telegram payload

Every Finding rendered to Telegram includes:

- Severity badge in the header.
- Category name and market question.
- Notional USD, implied odds, multiplier vs baseline median.
- Lifecycle percent and a HOT marker when applicable.
- Cluster context (`InCluster`, `ClusterPeerCount`) when the trade has peers.
- Bulleted Links section: Polymarket event page, category page, trader
  profile, Grafana deep-link. Links rendered only when the URL is non-empty.
- The **actual** baseline span used, not the configured cap.
