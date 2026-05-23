# Watchtower Strategy Tuning SQL Pack

Operator-facing SQL queries for tuning Watchtower strategy parameters.
All queries are read-only and safe against the production DB.

## 1. Strategy eval / firing

### 1.1 Shadow decisions by strategy (last 24h)
```sql
SELECT strategy_name,
       COUNT(*)                                                                                       AS total,
       COUNT(*) FILTER (WHERE decision_kind = 'standalone')                                           AS standalone,
       COUNT(*) FILTER (WHERE decision_kind = 'boost')                                                AS boost,
       COUNT(*) FILTER (WHERE decision_kind = 'suppress')                                             AS suppress,
       COUNT(*) FILTER (WHERE decision_kind = 'degrade')                                              AS degrade,
       COUNT(*) FILTER (WHERE decision_kind = 'tag')                                                  AS tag
FROM polymarket_strategy_shadow_decisions
WHERE fired_at >= NOW() - INTERVAL '24 hours'
GROUP BY strategy_name
ORDER BY total DESC;
```

### 1.2 Shadow decisions per hour (rate sanity check)
```sql
SELECT date_trunc('hour', fired_at)                          AS hour,
       strategy_name,
       COUNT(*)                                              AS n
FROM polymarket_strategy_shadow_decisions
WHERE fired_at >= NOW() - INTERVAL '48 hours'
GROUP BY 1, 2
ORDER BY 1 DESC, 3 DESC;
```

### 1.3 Per-strategy decision-level distribution
```sql
SELECT strategy_name, decision_level, COUNT(*) AS n
FROM polymarket_strategy_shadow_decisions
WHERE fired_at >= NOW() - INTERVAL '7 days'
GROUP BY 1, 2
ORDER BY 1, 2;
```

### 1.4 Skip reasons (Prometheus → Grafana)
Watchtower exports `watchtower_strategy_eval_skipped_total{strategy,reason}`.
Per Prometheus:
```promql
sum by (strategy, reason) (rate(watchtower_strategy_eval_skipped_total[1h]))
```

## 2. CLV / value evaluator

### 2.1 CLV coverage per strategy
```sql
SELECT strategy_name,
       COUNT(*)                                              AS total,
       COUNT(*) FILTER (WHERE clv_15m IS NOT NULL)           AS has_15m,
       COUNT(*) FILTER (WHERE clv_1h  IS NOT NULL)           AS has_1h,
       COUNT(*) FILTER (WHERE clv_6h  IS NOT NULL)           AS has_6h,
       COUNT(*) FILTER (WHERE clv_24h IS NOT NULL)           AS has_24h
FROM polymarket_strategy_shadow_decisions
WHERE fired_at >= NOW() - INTERVAL '30 days'
GROUP BY strategy_name
ORDER BY total DESC;
```

### 2.2 Median CLV uplift per strategy (key promotion metric)
```sql
SELECT strategy_name,
       COUNT(*) FILTER (WHERE clv_6h IS NOT NULL)                                  AS evaluated,
       percentile_cont(0.5) WITHIN GROUP (ORDER BY clv_6h)                         AS median_6h_cents,
       percentile_cont(0.75) WITHIN GROUP (ORDER BY clv_6h)                        AS p75_6h_cents,
       percentile_cont(0.25) WITHIN GROUP (ORDER BY clv_6h)                        AS p25_6h_cents
FROM polymarket_strategy_shadow_decisions
WHERE fired_at >= NOW() - INTERVAL '14 days'
  AND clv_6h IS NOT NULL
GROUP BY strategy_name
ORDER BY median_6h_cents DESC NULLS LAST;
```

### 2.3 Reversal rate per strategy (15m signed move flipped vs 1h)
```sql
SELECT strategy_name,
       COUNT(*) FILTER (WHERE clv_15m IS NOT NULL AND clv_1h IS NOT NULL)            AS evaluated,
       AVG(CASE WHEN clv_15m > 0 AND clv_1h <= 0 THEN 1.0 ELSE 0.0 END)              AS reversal_ratio
FROM polymarket_strategy_shadow_decisions
WHERE fired_at >= NOW() - INTERVAL '14 days'
GROUP BY strategy_name
ORDER BY reversal_ratio ASC;
```

### 2.4 Matched-control uplift
```sql
-- Strategy CLV vs the control bucket median for the same bucket.
SELECT s.strategy_name,
       s.control_bucket_key,
       COUNT(*)                                                                     AS n,
       percentile_cont(0.5) WITHIN GROUP (ORDER BY s.clv_6h)                        AS strat_median_6h,
       c.control_median_6h,
       percentile_cont(0.5) WITHIN GROUP (ORDER BY s.clv_6h) - c.control_median_6h  AS uplift_cents
FROM polymarket_strategy_shadow_decisions s
JOIN (
    SELECT control_bucket_key,
           percentile_cont(0.5) WITHIN GROUP (ORDER BY clv_6h) AS control_median_6h
    FROM polymarket_strategy_shadow_decisions
    WHERE strategy_name IN ('rulesrisk')   -- baseline strategy = control
      AND clv_6h IS NOT NULL
    GROUP BY control_bucket_key
) c USING (control_bucket_key)
WHERE s.clv_6h IS NOT NULL
GROUP BY 1, 2, c.control_median_6h
HAVING COUNT(*) >= 10
ORDER BY uplift_cents DESC NULLS LAST
LIMIT 25;
```

## 3. Promotion review

### 3.1 Latest review per strategy
```sql
SELECT DISTINCT ON (strategy_name)
    strategy_name, strategy_version, sample_size,
    median_signed_move_6h, reversal_15m_ratio, alerts_per_day,
    eligible, reasons_json, reviewed_at
FROM polymarket_strategy_promotion_reviews
ORDER BY strategy_name, reviewed_at DESC;
```

### 3.2 Eligibility timeline
```sql
SELECT strategy_name,
       date_trunc('day', reviewed_at) AS day,
       BOOL_OR(eligible)              AS eligible_today,
       MAX(sample_size)               AS max_sample
FROM polymarket_strategy_promotion_reviews
WHERE reviewed_at >= NOW() - INTERVAL '30 days'
GROUP BY 1, 2
ORDER BY 1, 2;
```

## 4. Worker freshness / health

### 4.1 Single freshness snapshot
```sql
SELECT 'holder_snapshots'    AS t, COUNT(*) AS rows, MAX(snapshot_at)    AS newest FROM polymarket_holder_snapshots UNION ALL
SELECT 'book_feature_bars',     COUNT(*),           MAX(bar_start)       FROM polymarket_book_feature_bars UNION ALL
SELECT 'wallet_thesis_lines',   COUNT(*),           MAX(refreshed_at)    FROM polymarket_wallet_thesis_lines UNION ALL
SELECT 'market_links',          COUNT(*),           MAX(updated_at)      FROM polymarket_market_links UNION ALL
SELECT 'wallet_graph_edges',    COUNT(*),           MAX(last_seen_at)    FROM polymarket_wallet_graph_edges UNION ALL
SELECT 'market_risk_scores',    COUNT(*),           MAX(computed_at)     FROM polymarket_market_risk_scores UNION ALL
SELECT 'repricing_windows',     COUNT(*),           MAX(opened_at)       FROM polymarket_repricing_windows UNION ALL
SELECT 'ws_events',             COUNT(*),           MAX(received_at)     FROM polymarket_ws_events UNION ALL
SELECT 'live_market_state',     COUNT(*),           MAX(updated_at)      FROM polymarket_live_market_state UNION ALL
SELECT 'event_annotations',     COUNT(*),           MAX(first_seen_at)   FROM polymarket_event_annotations UNION ALL
SELECT 'event_catalysts',       COUNT(*),           MAX(updated_at)      FROM polymarket_event_catalysts UNION ALL
SELECT 'shadow_decisions',      COUNT(*),           MAX(fired_at)        FROM polymarket_strategy_shadow_decisions UNION ALL
SELECT 'promotion_reviews',     COUNT(*),           MAX(reviewed_at)     FROM polymarket_strategy_promotion_reviews;
```

### 4.2 Repricing windows by status
```sql
SELECT status, COUNT(*) AS n, MIN(opened_at) AS oldest, MAX(opened_at) AS newest
FROM polymarket_repricing_windows
GROUP BY status
ORDER BY n DESC;
```

### 4.3 News pipeline funnel
```sql
SELECT 'event_page_snapshots'         AS stage, COUNT(*) FROM polymarket_event_page_snapshots UNION ALL
SELECT 'event_annotations',                     COUNT(*) FROM polymarket_event_annotations UNION ALL
SELECT 'event_catalysts',                       COUNT(*) FROM polymarket_event_catalysts UNION ALL
SELECT 'news_intel_processed_items',            COUNT(*) FROM polymarket_news_intel_processed_items UNION ALL
SELECT 'news_intel_decisions',                  COUNT(*) FROM polymarket_news_intel_decisions;
```

## 5. Per-strategy diagnostics

### 5.1 thesisaccum — breadth / consistency distribution
```sql
SELECT
    (features_json->>'breadth')::int                              AS breadth,
    ROUND(((features_json->>'consistency')::numeric * 10))/10     AS consistency_bucket,
    COUNT(*)                                                      AS n,
    AVG(clv_6h)                                                   AS avg_clv_6h
FROM polymarket_strategy_shadow_decisions
WHERE strategy_name = 'thesisaccum'
  AND fired_at >= NOW() - INTERVAL '14 days'
GROUP BY 1, 2 ORDER BY 1, 2;
```

### 5.2 holderdelta — pct_oi / rank distribution
```sql
SELECT
    ROUND((features_json->>'pct_oi')::numeric, 2)        AS pct_oi_bucket,
    COUNT(*) AS n,
    AVG(clv_6h)                                          AS avg_clv_6h
FROM polymarket_strategy_shadow_decisions
WHERE strategy_name = 'holderdelta'
  AND fired_at >= NOW() - INTERVAL '14 days'
GROUP BY 1 ORDER BY 1;
```

### 5.3 bookvacuum — collapse / spread distribution
```sql
SELECT
    ROUND(((features_json->>'collapse_pct')::numeric * 20))/20    AS collapse_bucket,
    ROUND(((features_json->>'spread_z')::numeric * 2))/2          AS spread_z_bucket,
    COUNT(*) AS n,
    AVG(clv_6h)                                                   AS avg_clv_6h
FROM polymarket_strategy_shadow_decisions
WHERE strategy_name = 'bookvacuum'
  AND fired_at >= NOW() - INTERVAL '14 days'
GROUP BY 1, 2 ORDER BY 1, 2;
```

### 5.4 repricinglag — lag cents distribution
```sql
SELECT
    ROUND((score)::numeric, 1)  AS lag_cents_bucket,
    COUNT(*) AS n,
    AVG(clv_6h)                 AS avg_clv_6h
FROM polymarket_strategy_shadow_decisions
WHERE strategy_name = 'repricinglag'
  AND fired_at >= NOW() - INTERVAL '14 days'
GROUP BY 1 ORDER BY 1;
```

### 5.5 cheaptail — prob band distribution
```sql
SELECT
    ROUND(((features_json->>'price')::numeric * 100))/100  AS price_bucket,
    COUNT(*)                                               AS n,
    AVG((features_json->>'notional')::numeric)             AS avg_notional,
    AVG(clv_6h)                                            AS avg_clv_6h
FROM polymarket_strategy_shadow_decisions
WHERE strategy_name = 'cheaptail'
  AND fired_at >= NOW() - INTERVAL '14 days'
GROUP BY 1 ORDER BY 1;
```

### 5.6 rulesrisk — score distribution
```sql
SELECT
    ROUND((score)::numeric, 1) AS ambiguity_bucket,
    COUNT(*) AS n
FROM polymarket_strategy_shadow_decisions
WHERE strategy_name = 'rulesrisk'
  AND fired_at >= NOW() - INTERVAL '7 days'
GROUP BY 1 ORDER BY 1;
```

### 5.7 walletcohort — edge similarity distribution
```sql
-- Read the underlying edges table (more useful than features_json here).
SELECT
    ROUND((similarity_score * 10)::numeric)/10 AS sim_bucket,
    COUNT(*)                                   AS n,
    AVG(co_events_count)                       AS avg_co_events
FROM polymarket_wallet_graph_edges
GROUP BY 1 ORDER BY 1;
```

### 5.8 conflictresolve — dominance distribution
```sql
SELECT
    ROUND((score)::numeric, 1) AS dominance_bucket,
    decision_kind,
    COUNT(*) AS n
FROM polymarket_strategy_shadow_decisions
WHERE strategy_name = 'conflictresolve'
  AND fired_at >= NOW() - INTERVAL '7 days'
GROUP BY 1, 2 ORDER BY 1, 2;
```

## 6. Outcome accuracy

### 6.1 Per-strategy resolved correct/wrong
```sql
SELECT strategy_name,
       COUNT(*) FILTER (WHERE outcome_status = 'resolved_correct')              AS correct,
       COUNT(*) FILTER (WHERE outcome_status = 'resolved_wrong')                AS wrong,
       COUNT(*) FILTER (WHERE outcome_status = 'unknown')                       AS unknown,
       COUNT(*) FILTER (WHERE outcome_status IS NULL)                           AS pending
FROM polymarket_strategy_shadow_decisions
WHERE fired_at >= NOW() - INTERVAL '60 days'
GROUP BY strategy_name
ORDER BY correct DESC NULLS LAST;
```

### 6.2 Hit rate per strategy (resolved only)
```sql
WITH resolved AS (
  SELECT strategy_name, outcome_status
  FROM polymarket_strategy_shadow_decisions
  WHERE outcome_status IN ('resolved_correct', 'resolved_wrong')
)
SELECT strategy_name,
       COUNT(*) AS resolved_n,
       ROUND(100.0 * COUNT(*) FILTER (WHERE outcome_status = 'resolved_correct') / COUNT(*), 1) AS hit_rate_pct
FROM resolved
GROUP BY 1
ORDER BY hit_rate_pct DESC NULLS LAST;
```

## 7. Telegram / surface

### 7.1 Alert volume by strategy (last 24h)
```sql
-- polymarket_alerts is the live alert table.
SELECT kind, COUNT(*) AS n
FROM polymarket_alerts
WHERE sent_at >= NOW() - INTERVAL '24 hours'
GROUP BY kind
ORDER BY n DESC;
```

### 7.2 Strategy decision routes (shadow vs live)
```sql
SELECT strategy_name,
       shadow_only,
       COUNT(*) AS n
FROM polymarket_strategy_shadow_decisions
WHERE fired_at >= NOW() - INTERVAL '7 days'
GROUP BY 1, 2
ORDER BY 1, 2;
```

If any row has `shadow_only=false` while `STRATEGY_LEARNING_LOOP_PROMOTION_ALLOWED=false`,
**that's a code bug or env drift** — should never happen by construction.

## 8. Cost / load guards

### 8.1 holdersync request volume (proxy from snapshot count)
```sql
SELECT date_trunc('hour', created_at) AS hour, COUNT(DISTINCT condition_id) AS distinct_markets
FROM polymarket_holder_snapshots
WHERE created_at >= NOW() - INTERVAL '24 hours'
GROUP BY 1 ORDER BY 1;
```

### 8.2 bookbars request volume
```sql
SELECT date_trunc('minute', created_at) AS minute, COUNT(DISTINCT outcome_token) AS distinct_tokens
FROM polymarket_book_feature_bars
WHERE created_at >= NOW() - INTERVAL '1 hour'
GROUP BY 1 ORDER BY 1 DESC LIMIT 60;
```

## 9. Health audit (red flags)

### 9.1 Are workers stuck?
```sql
SELECT 'holder_snapshots'    AS t, MAX(snapshot_at)  AS newest, NOW() - MAX(snapshot_at)  AS age FROM polymarket_holder_snapshots UNION ALL
SELECT 'book_feature_bars',     MAX(bar_start),                  NOW() - MAX(bar_start)            FROM polymarket_book_feature_bars UNION ALL
SELECT 'wallet_thesis_lines',   MAX(refreshed_at),               NOW() - MAX(refreshed_at)         FROM polymarket_wallet_thesis_lines UNION ALL
SELECT 'market_risk_scores',    MAX(computed_at),                NOW() - MAX(computed_at)          FROM polymarket_market_risk_scores UNION ALL
SELECT 'event_annotations',     MAX(first_seen_at),              NOW() - MAX(first_seen_at)        FROM polymarket_event_annotations;
```

Any `age > 1h` for a worker whose interval is < 1h indicates a stuck worker.

### 9.2 Promotion review staleness
```sql
SELECT strategy_name, MAX(reviewed_at) AS newest, NOW() - MAX(reviewed_at) AS age
FROM polymarket_strategy_promotion_reviews
GROUP BY 1
ORDER BY age DESC;
```
