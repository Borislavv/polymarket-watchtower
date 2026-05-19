# Tuning methodology — building `.env.prod` from real DB evidence

The product brief explicitly forbids intuition-only tuning. This
document is the procedure. Output: a `.env.prod` file derived from
your DB distributions and validated against `diagnose-alerts`.

**Why this lives in a doc instead of pre-filled values:** the
right thresholds depend on YOUR market mix (Politics-heavy vs
Sports-heavy), YOUR baseline depth, YOUR operator attention
budget. A pre-filled `.env.prod` from someone who hasn't seen
your data is exactly the "looks safer" anti-pattern the brief
calls out.

---

## Step 0 — Snapshot the current state

Before changing anything, capture what the existing config
produces. This is your baseline-of-comparison.

```sh
# Source the current env (e.g. balanced.env or your .env)
set -a && source .env

# Save a snapshot
mkdir -p tuning/$(date +%Y-%m-%d)
go run ./cmd/cli diagnose-alerts -lookback 24h -show-candidates 50 \
  > tuning/$(date +%Y-%m-%d)/diagnose-24h.txt
go run ./cmd/cli diagnose-alerts -lookback 7d  -show-candidates 50 \
  > tuning/$(date +%Y-%m-%d)/diagnose-7d.txt
```

Read both files. Note Info / Warning / Critical / Hard projected
fire counts and the strongest blocking gates.

---

## Step 1 — Quantitative DB research

The queries below are the **load-bearing evidence** for every
threshold in `.env.prod`. Run them; paste the output into your
tuning notes; reference the numbers when you set thresholds.

### Q1. Per-bucket notional distribution (trades only)

```sql
WITH t AS (
  SELECT
    notional_usd,
    NTILE(100) OVER (ORDER BY notional_usd) AS pctile
  FROM polymarket_trades
  WHERE traded_at >= NOW() - INTERVAL '30 days'
)
SELECT
  percentile_cont(0.50) WITHIN GROUP (ORDER BY notional_usd) AS p50,
  percentile_cont(0.90) WITHIN GROUP (ORDER BY notional_usd) AS p90,
  percentile_cont(0.95) WITHIN GROUP (ORDER BY notional_usd) AS p95,
  percentile_cont(0.99) WITHIN GROUP (ORDER BY notional_usd) AS p99,
  max(notional_usd)                                        AS max
FROM polymarket_trades
WHERE traded_at >= NOW() - INTERVAL '30 days';
```

**Use this to set:** `ALERT_INFO_MIN_NOTIONAL_USD`,
`ALERT_WARNING_MIN_NOTIONAL_USD`, `ALERT_CRITICAL_MIN_NOTIONAL_USD`.

**Rule:** Info ≈ p95, Warning ≈ p99, Critical ≈ p99.5+ —
**not** p99 across all trades but p99 of the trades that already
clear the lifecycle + readiness gates. The "trades that survive
filtering" distribution is much narrower than the all-trades
distribution.

### Q2. p95/p99 multiplier distribution (over baseline median)

```sql
-- This requires running the existing diagnose-alerts which
-- exposes ratio histograms; piped via:
go run ./cmd/cli diagnose-alerts -lookback 7d -show-candidates 200 \
  | grep -E 'p95_ratio|p99_ratio' | sort -n
```

**Use this to set:** `ALERT_INFO_MIN_MARKET_P95_RATIO`,
`ALERT_WARNING_MIN_MARKET_P95_RATIO`,
`ALERT_CRITICAL_MIN_MARKET_P95_RATIO`.

### Q3. Accumulation-line length distribution

```sql
SELECT
  trader_id, market_id, outcome_token, side,
  count(*) AS trades,
  sum(notional_usd) AS total_usd,
  EXTRACT(EPOCH FROM (max(traded_at) - min(traded_at))) / 3600 AS span_h,
  percentile_cont(0.5) WITHIN GROUP (ORDER BY notional_usd) AS median_trade
FROM polymarket_trades
WHERE traded_at >= NOW() - INTERVAL '30 days'
GROUP BY trader_id, market_id, outcome_token, side
HAVING count(*) >= 3
ORDER BY total_usd DESC LIMIT 100;
```

**Use this to set:** `ACCUMULATION_*` tier floors. Compare your
top-100 lines to the existing `presets/balanced.env`
accumulation thresholds.

### Q4. Ownership concentration histogram

```sql
WITH per_outcome AS (
  SELECT market_id, outcome_token,
         trader_id,
         SUM(CASE WHEN side='BUY' THEN size_shares ELSE -size_shares END) AS net_shares
  FROM polymarket_trades
  WHERE traded_at >= NOW() - INTERVAL '30 days'
  GROUP BY market_id, outcome_token, trader_id
), totals AS (
  SELECT market_id, outcome_token,
         SUM(CASE WHEN side='BUY' THEN size_shares ELSE 0 END) AS total_buy
  FROM polymarket_trades
  GROUP BY market_id, outcome_token
)
SELECT
  CASE
    WHEN 100.0*p.net_shares/NULLIF(t.total_buy,0) < 5   THEN '<5%'
    WHEN 100.0*p.net_shares/NULLIF(t.total_buy,0) < 10  THEN '5-10%'
    WHEN 100.0*p.net_shares/NULLIF(t.total_buy,0) < 25  THEN '10-25%'
    WHEN 100.0*p.net_shares/NULLIF(t.total_buy,0) < 50  THEN '25-50%'
    ELSE '50%+'
  END AS bucket,
  count(*)
FROM per_outcome p JOIN totals t USING (market_id, outcome_token)
WHERE p.net_shares > 0 AND t.total_buy > 0
GROUP BY 1 ORDER BY 1;
```

**Use this to set:** ownership tier floors. Note that the
50%+ band should be **rare** — if you see thousands of rows,
the data is noisier than expected (likely small markets with
few BUYers — see Q5).

### Q5. Liquidity vs alert quality

```sql
SELECT
  CASE
    WHEN m.last_24h_volume_usd <  10000 THEN '<10k'
    WHEN m.last_24h_volume_usd <  50000 THEN '10-50k'
    WHEN m.last_24h_volume_usd < 250000 THEN '50-250k'
    ELSE '250k+'
  END AS liquidity_band,
  count(*) AS alerts,
  count(*) FILTER (WHERE a.outcome_status='resolved_correct') AS won,
  count(*) FILTER (WHERE a.outcome_status='resolved_wrong')   AS lost
FROM polymarket_alerts a
JOIN polymarket_markets m ON m.id = a.market_id
WHERE a.created_at >= NOW() - INTERVAL '90 days'
GROUP BY 1 ORDER BY 1;
```

**Use this to set:** `STABLE_FAVORITE_MIN_MARKET_VOLUME_USD`
floor + a candidate filter on liquidity for the high-precision
profile. Markets in the bottom liquidity band tend to produce
noisy p95 ratios.

### Q6. MM suppression rate

```sql
SELECT category,
       sum(value) AS suppressed_count
  FROM (
    -- pull from your Prometheus snapshot or from logs aggregated
    -- by the watchtower_filter_alert_mm_suppressed_total counter
    SELECT 'placeholder' AS category, 0 AS value
  ) x
GROUP BY category;
```

(If your Prometheus is not on hand, grep the logs:
`grep '"alertsender: mm filter suppressed"' watchtower.log`.)

**Use this to validate:** the MM filter is firing. If MM
suppression is 0 on a Polymarket politics dataset, the gates
are too strict and a noise class is leaking through.

### Q7. Opposite-side concurrent flow

```sql
-- Per (market, 24h window), how often does BUY-flow and SELL-
-- flow both look "informed" by the existing alert criteria?
SELECT
  CASE
    WHEN a.severity IN ('warning','critical','hard') THEN 'sharp'
    ELSE 'soft'
  END AS sharpness,
  count(DISTINCT a.id) FILTER (WHERE EXISTS (
    SELECT 1 FROM polymarket_alerts b
    WHERE b.market_id = a.market_id
      AND b.id <> a.id
      AND b.created_at BETWEEN a.created_at - INTERVAL '24 hours'
                           AND a.created_at + INTERVAL '24 hours'
      AND b.payload::jsonb->'Trade'->>'Side' <> a.payload::jsonb->'Trade'->>'Side'
  )) AS with_opposite_flow,
  count(*) AS total
FROM polymarket_alerts a
WHERE a.created_at >= NOW() - INTERVAL '30 days'
GROUP BY 1;
```

**Use this to validate:** the cross-flow context fields in the
AI prompt actually fire on a non-trivial fraction of alerts.

### Q8. Severity mix today vs target

```sql
SELECT severity, count(*)
  FROM polymarket_alerts
 WHERE created_at >= NOW() - INTERVAL '24 hours'
 GROUP BY severity;
```

Compare to the spec target:
- Info: ≤ 15/day
- Warning: 1-5/day
- Critical: 0-2/day
- Hard: 0-1/day

If you are above these limits BEFORE setting `.env.prod`, your
`.env.prod` needs to be stricter than the current profile.

---

## Step 2 — Build `.env.prod` from the evidence

For every numeric threshold:

1. Read the relevant Q1-Q8 output.
2. Pick the value that produces the **target volume**.
3. Annotate the `.env.prod` line with a comment: `# Q1 p99=$X`.

A template file lives at `presets/prod.env.template` (see below).
**Do not commit a `presets/prod.env` until step 3 is done.**

---

## Step 3 — Verify against `diagnose-alerts`

```sh
# Stage the candidate config
set -a && source presets/prod.env.candidate

go run ./cmd/cli diagnose-alerts -lookback 24h -show-candidates 20 \
  > tuning/$(date +%Y-%m-%d)/diagnose-prod-candidate-24h.txt

go run ./cmd/cli diagnose-alerts -lookback 7d -show-candidates 20 \
  > tuning/$(date +%Y-%m-%d)/diagnose-prod-candidate-7d.txt
```

Inspect:
- Total fires/day per severity matches target?
- The **strongest** suppression gate is biting the noise classes
  you want it to bite (low-baseline / MM / lifecycle-young)?
- The Top-N candidate examples LOOK like the strategies you want
  (late-stage politics accumulation, ownership, stable favorite)?

If yes → promote the candidate file to `presets/prod.env` (or
your `.env.prod`). If no → revisit Step 1 evidence; one
threshold at a time.

---

## What `.env.prod` MUST favor (research-backed)

From public suspicious-flow research (Wired/CFTC, Columbia +
Haifa, Polymarket microstructure studies, unusualwhales heuristics):

| Signal | Why production should weight it |
|---|---|
| Late-market directional accumulation | Strongest empirical predictor; persistence + low time-left. |
| Trader-tail + market-tail overlap | Rare on BOTH axes ⇒ unlikely to be noise. |
| Ownership concentration ≥ 10% | Skin-in-the-game proxy. |
| Multi-wallet cluster | Independent agreement, not one whale being weird. |
| Clean one-sided flow | Opposite-side concurrent flow is THE noise class. |
| Persistent wallet history | Wallets with track-records of directional consistency. |

Translate into env: raise tier multipliers AND p95 ratios AND
accumulation totals; tighten lifecycle floor; raise MM
neutrality tolerance to catch borderline market-makers.

## What `.env.prod` MUST suppress (research-backed)

| Class | Why |
|---|---|
| Meme / joke / celebrity markets | Low informational value. The category whitelist already filters most of these — keep it tight. |
| Coinflip markets (price 0.45-0.55, no convergence) | High variance, no edge. |
| Balanced BUY/SELL same wallet | MM/rebalancing — already covered by `mmfilter`. |
| Tiny-liquidity multipliers | A `5000×` multiplier on a market with $200 total volume is data noise. Add a market-volume floor. |
| Low-baseline traps | The low-baseline severity cap exists for this. Keep `LOW_BASELINE_CAP_ENABLED=true` in prod. |
| Contradictory whale flow | Tag-only today; in prod, consider a hard suppression when same-market opposite-side notional ≥ 1.5× same-side notional within 24h. |

---

## Production profile philosophy (for the `.env.prod` header)

```
# Watchtower production profile — high-precision informed-flow monitoring.
#
# This profile is built from real DB distributions (see
# doc/project/tuning-methodology.md). It is INTENTIONALLY stricter
# than presets/balanced.env. Operator expectation:
#
#   Info     ≤ 15/day  (heartbeat + early watchlist)
#   Warning  1-5/day   (read every one)
#   Critical 0-2/day   (page someone)
#   Hard     0-1/day   (multi-wallet convergence)
#
# Strong signals:
#   - late-market directional accumulation
#   - trader-tail × market-tail overlap
#   - ownership concentration ≥ 10%
#   - multi-wallet cluster
#
# Suppressed classes:
#   - tiny-liquidity multipliers
#   - balanced BUY/SELL wallets (MM filter)
#   - low-baseline-driven multipliers (severity cap)
#   - contradictory same-market flow (AI tag → Watch/Unclear)
#
# Re-tune when the underlying market mix changes (e.g. heavy
# election cycle, new category whitelist entry). See
# doc/project/tuning-methodology.md.
```

---

## What is the SAFE `.env` for aggressive testing?

This profile is intentionally LOOSER than balanced so the
operator can see the pipeline working end-to-end on real flow.
Goals: heartbeat, prompt tuning, Telegram UX, AI note visibility.

Concrete shape (anchored to `presets/balanced.env` minus
calibrated relaxations):

| Variable | Balanced default | `.env` recommendation |
|---|---|---|
| `LIFECYCLE_ALERT_FROM_PCT` | 75 | 65 (more markets eligible) |
| `MARKET_MIN_AGE` | 24h | 12h |
| `ALERT_INFO_MIN_NOTIONAL_USD` | 5000 | 2500 |
| `ALERT_INFO_MIN_MULTIPLIER` | 75 | 30 |
| `ALERT_INFO_MIN_MARKET_P95_RATIO` | 1 | 1 |
| `SINGLE_MIN_BASELINE_TRADES` | 100 | 40 |
| `SINGLE_MIN_BASELINE_NOTIONAL_USD` | 10000 | 2500 |
| `BASELINE_MIN_READY_WINDOW` | 24h | 6h |
| Warning / Critical floors | balanced | unchanged |
| MM filter | enabled | enabled |
| Category whitelist | Politics | Politics (don't widen) |

Target volume (heuristic, depends on data): up to ~240 alerts/day
with Info dominating; Warning/Critical/Hard unchanged from
balanced. Operator USES `.env` for one week of pipeline
validation, then promotes to `.env.prod`.

These values are **starting points** — even the testing profile
should be re-checked against `diagnose-alerts` after one day of
live data.
