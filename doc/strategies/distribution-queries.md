# Distribution queries — empirical evidence for `.env.prod` tuning

> Every threshold in `.env.prod` should map to a percentile or
> absolute floor from the queries here. Operator runs these against
> the live DB; output drives the threshold rationale doc and the
> final config values.
>
> No threshold is set without a backing query.

Run in PSQL with the production DSN (read-only is fine). Save the
output as `tuning/$(date +%Y-%m-%d)/qN.txt` so future sessions can
diff against today's distributions.

---

## Q1. Per-bucket trade notional distribution

What the production p50/p95/p99/p99.5/p99.9 look like AT TODAY for
the trades that already clear the lifecycle gate (so the per-tier
floors live in the same distribution operators alert against).

```sql
WITH eligible AS (
    SELECT t.notional_usd
      FROM polymarket_trades t
      JOIN polymarket_markets m ON m.id = t.market_id
     WHERE t.traded_at >= NOW() - INTERVAL '30 days'
       AND m.deleted_at IS NULL AND m.purged_at IS NULL
       AND m.start_date IS NOT NULL AND m.end_date IS NOT NULL
       AND (100.0 * EXTRACT(EPOCH FROM (t.traded_at - m.start_date)) /
                    NULLIF(EXTRACT(EPOCH FROM (m.end_date - m.start_date)), 0)) >= 75
       AND t.notional_usd > 0
)
SELECT
    percentile_cont(0.50)  WITHIN GROUP (ORDER BY notional_usd) AS p50,
    percentile_cont(0.90)  WITHIN GROUP (ORDER BY notional_usd) AS p90,
    percentile_cont(0.95)  WITHIN GROUP (ORDER BY notional_usd) AS p95,
    percentile_cont(0.99)  WITHIN GROUP (ORDER BY notional_usd) AS p99,
    percentile_cont(0.995) WITHIN GROUP (ORDER BY notional_usd) AS p99_5,
    percentile_cont(0.999) WITHIN GROUP (ORDER BY notional_usd) AS p99_9,
    count(*)                                                    AS n
  FROM eligible;
```

**Drives:** `ALERT_INFO_MIN_NOTIONAL_USD` (≈ p95-p97),
`ALERT_WARNING_MIN_NOTIONAL_USD` (≈ p99),
`ALERT_CRITICAL_MIN_NOTIONAL_USD` (≈ p99.5-p99.9).

## Q2. p99 ratio distribution (market-axis displacement)

For trades that already fired alerts, what's the ratio of trade
notional to per-bucket p99? Tells us where to set the
`ALERT_*_MIN_MARKET_P99_RATIO` floors.

```sql
-- Latest 90 days of alerts where the payload carries the market
-- p99 ratio. The Finding JSON column is on polymarket_alerts.payload.
SELECT
    a.severity,
    percentile_cont(0.50)  WITHIN GROUP (ORDER BY (a.payload->>'MarketP99Ratio')::float)  AS p50,
    percentile_cont(0.90)  WITHIN GROUP (ORDER BY (a.payload->>'MarketP99Ratio')::float)  AS p90,
    percentile_cont(0.99)  WITHIN GROUP (ORDER BY (a.payload->>'MarketP99Ratio')::float)  AS p99,
    count(*) AS alerts
  FROM polymarket_alerts a
 WHERE a.created_at >= NOW() - INTERVAL '90 days'
   AND (a.payload->>'MarketP99Ratio')::float > 0
 GROUP BY a.severity
 ORDER BY a.severity;
```

**Drives:** `ALERT_INFO_MIN_MARKET_P99_RATIO`,
`ALERT_WARNING_MIN_MARKET_P99_RATIO`,
`ALERT_CRITICAL_MIN_MARKET_P99_RATIO`.

If the median Info-alert p99 ratio is e.g. 1.4, an
Info-floor of 1.5 keeps the top half of current Info-alerts.
Adjust to target volume.

## Q3. Accumulation-line statistics

Today's accumulation lines (≥ 5 trades on a (wallet, market,
outcome, side), last 30 days) — what do the strong ones actually
look like?

```sql
WITH lines AS (
    SELECT
        t.trader_id,
        t.market_id,
        t.outcome_token,
        t.side,
        count(*)                                              AS trades,
        sum(t.notional_usd)                                   AS total_usd,
        EXTRACT(EPOCH FROM (max(t.traded_at) - min(t.traded_at))) / 3600 AS span_h,
        percentile_cont(0.5) WITHIN GROUP (ORDER BY t.notional_usd) AS median_trade,
        avg(t.notional_usd)                                   AS mean_trade
      FROM polymarket_trades t
     WHERE t.traded_at >= NOW() - INTERVAL '30 days'
       AND t.trader_id IS NOT NULL
     GROUP BY t.trader_id, t.market_id, t.outcome_token, t.side
    HAVING count(*) >= 5
)
SELECT
    percentile_cont(0.50) WITHIN GROUP (ORDER BY total_usd) AS p50_total,
    percentile_cont(0.90) WITHIN GROUP (ORDER BY total_usd) AS p90_total,
    percentile_cont(0.99) WITHIN GROUP (ORDER BY total_usd) AS p99_total,
    percentile_cont(0.50) WITHIN GROUP (ORDER BY trades)    AS p50_trades,
    percentile_cont(0.99) WITHIN GROUP (ORDER BY trades)    AS p99_trades,
    percentile_cont(0.99) WITHIN GROUP (ORDER BY median_trade) AS p99_median,
    count(*) AS n
  FROM lines;

-- Also: top-30 strongest lines for eyeball sanity.
SELECT trader_id, market_id, outcome_token, side,
       trades, total_usd::int, span_h::int, median_trade::int, mean_trade::int
  FROM lines
 ORDER BY total_usd DESC
 LIMIT 30;
```

**Drives:** `ACCUMULATION_MIN_LINE_TOTAL_USD` (new gate proposed
in composite-scoring),
`ACCUMULATION_*_TOTAL_MULTIPLIER`, accumulation tier trade-count
floors. The p99 of `total_usd` is the "this is what a real
accumulation looks like" anchor.

## Q4. Ownership-share histogram

How concentrated does ownership actually get? Bucketed.

```sql
WITH per_outcome AS (
    SELECT t.market_id, t.outcome_token, t.trader_id,
           SUM(CASE WHEN t.side='BUY' THEN t.size_shares ELSE -t.size_shares END) AS net_shares
      FROM polymarket_trades t
     WHERE t.traded_at >= NOW() - INTERVAL '60 days'
       AND t.trader_id IS NOT NULL
     GROUP BY t.market_id, t.outcome_token, t.trader_id
), totals AS (
    SELECT market_id, outcome_token,
           SUM(CASE WHEN side='BUY' THEN size_shares ELSE 0 END) AS total_buy_shares
      FROM polymarket_trades
     WHERE traded_at >= NOW() - INTERVAL '60 days'
     GROUP BY market_id, outcome_token
)
SELECT
    CASE
      WHEN 100.0 * p.net_shares / NULLIF(t.total_buy_shares, 0) < 3    THEN '< 3%'
      WHEN 100.0 * p.net_shares / NULLIF(t.total_buy_shares, 0) < 5    THEN '3-5%'
      WHEN 100.0 * p.net_shares / NULLIF(t.total_buy_shares, 0) < 10   THEN '5-10%'
      WHEN 100.0 * p.net_shares / NULLIF(t.total_buy_shares, 0) < 15   THEN '10-15%'
      WHEN 100.0 * p.net_shares / NULLIF(t.total_buy_shares, 0) < 25   THEN '15-25%'
      WHEN 100.0 * p.net_shares / NULLIF(t.total_buy_shares, 0) < 50   THEN '25-50%'
      ELSE '50%+'
    END AS bucket,
    count(*) AS rows
  FROM per_outcome p
  JOIN totals t USING (market_id, outcome_token)
 WHERE p.net_shares > 0 AND t.total_buy_shares > 0
 GROUP BY 1
 ORDER BY 1;
```

**Drives:** `OWNERSHIP_*_MIN_SHARE_PCT`.
Pair with Q5 below to set the new
`OWNERSHIP_MIN_MARKET_BUY_VOLUME_USD` floor.

## Q5. Ownership × market-volume joint distribution

The realism audit's key finding: share% alone is noise; must be
paired with absolute volume.

```sql
WITH per_outcome AS (
    SELECT t.market_id, t.outcome_token, t.trader_id,
           SUM(CASE WHEN t.side='BUY' THEN t.size_shares ELSE -t.size_shares END) AS net_shares,
           SUM(CASE WHEN t.side='BUY' THEN t.notional_usd ELSE 0 END) AS total_buy_usd
      FROM polymarket_trades t
     WHERE t.traded_at >= NOW() - INTERVAL '60 days'
       AND t.trader_id IS NOT NULL
     GROUP BY t.market_id, t.outcome_token, t.trader_id
), totals AS (
    SELECT market_id, outcome_token,
           SUM(CASE WHEN side='BUY' THEN size_shares ELSE 0 END) AS total_buy_shares,
           SUM(CASE WHEN side='BUY' THEN notional_usd ELSE 0 END) AS total_buy_usd
      FROM polymarket_trades
     WHERE traded_at >= NOW() - INTERVAL '60 days'
     GROUP BY market_id, outcome_token
)
SELECT
    CASE
      WHEN t.total_buy_usd <  10000 THEN 'vol < $10k'
      WHEN t.total_buy_usd <  50000 THEN '$10-50k'
      WHEN t.total_buy_usd < 250000 THEN '$50-250k'
      ELSE '$250k+'
    END AS volume_band,
    CASE
      WHEN 100.0 * p.net_shares / NULLIF(t.total_buy_shares, 0) < 10  THEN '< 10%'
      WHEN 100.0 * p.net_shares / NULLIF(t.total_buy_shares, 0) < 25  THEN '10-25%'
      ELSE '25%+'
    END AS share_band,
    count(*) AS rows
  FROM per_outcome p
  JOIN totals t USING (market_id, outcome_token)
 WHERE p.net_shares > 0 AND t.total_buy_shares > 0
 GROUP BY 1, 2
 ORDER BY 1, 2;
```

**Drives:** `OWNERSHIP_INFO_MIN_MARKET_BUY_VOLUME_USD`,
`OWNERSHIP_WARNING_MIN_MARKET_BUY_VOLUME_USD`,
`OWNERSHIP_CRITICAL_MIN_MARKET_BUY_VOLUME_USD`.

The cell "vol < $10k AND share 25%+" is THE classic false-positive.
If that cell has many rows, set the volume floor at $25k+.

## Q6. Current severity mix (volume-budget anchor)

What is today's alerting-rate distribution? Anchor for what we
need to suppress to hit `.env.prod` targets.

```sql
SELECT
    severity,
    kind,
    count(*) FILTER (WHERE created_at >= NOW() - INTERVAL '24 hours')   AS day,
    count(*) FILTER (WHERE created_at >= NOW() - INTERVAL '7 days')/7.0 AS day_avg_7d,
    count(*) FILTER (WHERE created_at >= NOW() - INTERVAL '30 days')/30.0 AS day_avg_30d
  FROM polymarket_alerts
 GROUP BY severity, kind
 ORDER BY severity, kind;
```

**Drives:** the volume-headroom calculation. Target for
`.env.prod`: Info ≤ 12/day, Warning 2-6/day, Critical 0-2/day,
Hard 0-1/day. Compare to current; the gap tells us how much
suppression is needed.

## Q7. Alert outcome quality (PAL-style signal quality)

How well do the existing alerts predict resolution? Tells us which
strategies are "operationally real" vs "noise that fires".

```sql
SELECT
    a.severity,
    a.kind,
    count(*) AS total,
    count(*) FILTER (WHERE a.outcome_status='resolved_correct') AS won,
    count(*) FILTER (WHERE a.outcome_status='resolved_wrong')   AS lost,
    count(*) FILTER (WHERE a.outcome_status IN ('pending','unknown','unavailable')) AS open,
    round(100.0 * count(*) FILTER (WHERE a.outcome_status='resolved_correct')::numeric
              / NULLIF(count(*) FILTER (WHERE a.outcome_status IN ('resolved_correct','resolved_wrong')), 0), 1)
                                                              AS win_pct
  FROM polymarket_alerts a
 WHERE a.created_at >= NOW() - INTERVAL '90 days'
 GROUP BY a.severity, a.kind
 ORDER BY a.severity, a.kind;
```

**Drives:** which strategies to tighten most aggressively. A
strategy with win_pct ≤ implied-probability is noise even if its
gates look right.

## Q8. MM suppression rate (filter effectiveness)

```sql
-- From the metric (if a snapshot is on hand) OR from the
-- structured-log mm-suppressed lines. If neither is convenient:
SELECT
    date_trunc('day', a.created_at) AS day,
    count(*) FILTER (WHERE 'POSSIBLE_MARKET_MAKER' = ANY(SELECT jsonb_array_elements_text(a.payload->'Reasons'))) AS mm_tagged,
    count(*) AS total
  FROM polymarket_alerts a
 WHERE a.created_at >= NOW() - INTERVAL '14 days'
 GROUP BY 1 ORDER BY 1;
```

**Drives:** `MM_NEUTRALITY_TOL`, `MM_MIN_TRADES_PER_SIDE`.
If `mm_tagged / total` is < 5% the filter is too strict for
production; if > 25% it's catching genuine flow.

## Q9. Cluster firing distribution

```sql
SELECT
    severity,
    date_trunc('day', created_at) AS day,
    count(*) AS clusters
  FROM polymarket_alerts
 WHERE kind = 'category_watch'
   AND created_at >= NOW() - INTERVAL '30 days'
 GROUP BY severity, day
 ORDER BY day;

-- And the wallet/notional distribution of clusters:
SELECT
    severity,
    percentile_cont(0.5) WITHIN GROUP (ORDER BY (payload->'Cluster'->>'UniqueWallets')::int)    AS p50_wallets,
    percentile_cont(0.5) WITHIN GROUP (ORDER BY (payload->'Cluster'->>'AnomalousTrades')::int)  AS p50_trades,
    percentile_cont(0.5) WITHIN GROUP (ORDER BY (payload->'Cluster'->>'TotalUSD')::float)       AS p50_total,
    percentile_cont(0.9) WITHIN GROUP (ORDER BY (payload->'Cluster'->>'TotalUSD')::float)       AS p90_total
  FROM polymarket_alerts
 WHERE kind = 'category_watch' AND created_at >= NOW() - INTERVAL '30 days'
 GROUP BY severity;
```

**Drives:** `CLUSTER_MIN_UNIQUE_TRADERS`,
`CLUSTER_MIN_ANOMALOUS_TRADES`,
`CLUSTER_MIN_TOTAL_NOTIONAL_USD`. Production target: 0-2 cluster
alerts/day. If today's rate is > 5/day, the gates are too loose.

## Q10. Many-smalls false-positive class

The realism audit's "$6k / 44 trades / median $140" pattern.

```sql
WITH lines AS (
    SELECT
        t.trader_id, t.market_id, t.outcome_token, t.side,
        count(*) AS trades,
        sum(t.notional_usd) AS total_usd,
        percentile_cont(0.5) WITHIN GROUP (ORDER BY t.notional_usd) AS median_trade
      FROM polymarket_trades t
     WHERE t.traded_at >= NOW() - INTERVAL '30 days'
       AND t.trader_id IS NOT NULL
     GROUP BY t.trader_id, t.market_id, t.outcome_token, t.side
    HAVING count(*) >= 20
       AND sum(t.notional_usd) < 25000
       AND percentile_cont(0.5) WITHIN GROUP (ORDER BY t.notional_usd) < 500
)
SELECT count(*) AS many_smalls_lines,
       sum(trades) AS many_smalls_trades,
       sum(total_usd)::int AS many_smalls_usd
  FROM lines;
```

**Drives:** the new `ACCUMULATION_MIN_LINE_TOTAL_USD` per-tier
floor. If this query returns dozens of lines, the many-smalls
path is producing the bulk of accumulation-tier noise.

## Q11. Directional purity per wallet-market-outcome

```sql
WITH wmo AS (
    SELECT t.trader_id, t.market_id, t.outcome_token,
           sum(CASE WHEN t.side='BUY'  THEN t.notional_usd ELSE 0 END) AS buy_usd,
           sum(CASE WHEN t.side='SELL' THEN t.notional_usd ELSE 0 END) AS sell_usd
      FROM polymarket_trades t
     WHERE t.traded_at >= NOW() - INTERVAL '14 days'
       AND t.trader_id IS NOT NULL
     GROUP BY t.trader_id, t.market_id, t.outcome_token
    HAVING sum(t.notional_usd) > 1000
)
SELECT
    CASE
      WHEN purity < 0.50 THEN '< 0.50 (MM-like)'
      WHEN purity < 0.75 THEN '0.50-0.75 (mixed)'
      WHEN purity < 0.85 THEN '0.75-0.85 (leaky)'
      WHEN purity < 0.92 THEN '0.85-0.92 (directional)'
      ELSE '≥ 0.92 (clean)'
    END AS bucket,
    count(*) AS rows
  FROM (SELECT abs(buy_usd - sell_usd) / NULLIF(buy_usd + sell_usd, 0) AS purity FROM wmo) x
 GROUP BY 1 ORDER BY 1;
```

**Drives:** the proposed directional-purity gate (composite
dimension 4). Tells us where the productive floor sits — a band
that captures the top 20-30% rather than the top 80%.

---

## What I need from you

To produce a defensible `.env.prod`, please run Q1, Q3, Q5, Q6,
Q7, Q9, Q10 first. Q2, Q4, Q8, Q11 are useful but Q1+Q3+Q5+Q6+Q7+
Q9+Q10 are the minimum-viable set.

Each query is read-only; none modifies state. Paste the output and
I will:

1. Fill `presets/prod.env.template` placeholders with values
   directly traceable to the query output.
2. Produce a `threshold-rationale.md` line per knob: "value X
   chosen because Q? showed Y".
3. Estimate alerts/day for the new profile based on Q6 + the
   suppression rate implied by the new floors.
