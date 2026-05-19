# watchtower-main dashboard — operator guide

Dashboard UID: `watchtower-main` (provisioned on container start).
Source JSON: `watchtower-main.json` next to this file.

Open in Grafana:

```
http://<grafana-host>/d/watchtower-main
```

Telegram alerts deep-link operators directly to this dashboard
through `GRAFANA_BASE_URL` + the alert's `var-category` /
`var-market` / `var-severity` parameters.

## What is PAL?

**PAL = Proof of Alert Value.**

Watchtower alerts are only useful if they predict directionally
better than the market itself was pricing at trade time. PAL is the
metric family that measures that — separate from the operational
panels (alert volume, queue depth, upstream health).

PAL is **never** a claim of insider trading, financial PnL, or
"guaranteed profit". Read every PAL number as "directional
correctness against implied probability over RESOLVED alerts only".

## Dashboard layout

| Section | Panels | What it tells you |
|---|---|---|
| **Overview** | Markets tracked, baseline buckets, anomalies & HARD alerts (1h), telegram sent/errors, MM-suppressed, lifecycle-unknown skipped, pending/failed alerts, trades total, backfill USD/min | Live ops health — is the pipeline alive? |
| **Anomaly model** | Multipliers, single-trade axis (market vs trader), high-odds, by-category counters | Is the strategy firing? |
| **Trade size distribution** | TradeSize histogram, anomaly multiplier histogram | Sanity-check the underlying distribution |
| **Upstream + ingest** | API latency / errors, trade ingest rate | Polymarket upstream health |
| **Persistence (ingest)** | Markets/outcomes/traders/trades upsert rates, soft-delete/purge/resume stats | Is the DB ingesting the upstream flow? |
| **Backfill** | Pages fetched, runs by status | Historical fill progress |
| **Strategy A/B/E** | Accumulation by window, ownership concentration, new-wallet reasons | Per-strategy emission rates |
| **Signal quality (legacy)** | Directional correctness (24h), resolved vs pending, by severity, by kind, reactions, signal-report sends | The original signal-quality view |
| **PAL · Proof of Alert Value** | Cumulative edge, avg edge by severity/kind, weighted success, calibration table, CLV-lite proxy, sample size, pending count | **The proof-of-value layer** |

## PAL panels — how to read

### `PAL · cumulative realized edge`

For every resolved alert:

```
edge = success_binary − implied_probability_at_alert
```

The panel plots the cumulative sum of edge over the dashboard's time
range. The reading is straightforward:

- **Up-and-to-the-right** = alerts on average beat the market's
  implied probability. **Signal exists.**
- **Flat** = alerts roughly match implied probability. Noise.
- **Down** = alerts underperform implied probability. Either the
  strategy is misfiring or the wallet shapes we tag aren't informed.

Don't read absolute values; read the **slope**.

### `PAL · avg realized edge by severity`

24h rolling average edge per severity. Two things to look for:

- **Critical / Hard should out-edge Info.** If they don't, the
  severity ladder isn't separating strong from weak signals; the
  thresholds need work.
- **Negative Info edge with positive Critical edge** is acceptable —
  Info is the validation channel.

### `PAL · avg realized edge by alert kind`

Same shape, sliced by alert kind. Tells you whether `accumulation`
beats `trade_anomaly`, whether `ownership_concentration` is adding
edge or just noise, etc.

### `PAL · weighted success rate`

Severity-weighted: each resolved alert contributes `weight ×
success_binary` to the numerator and `weight` to the denominator.

- Info = 1, Warning = 3, Critical = 10, Hard = 25.

This is a sanity check against `success rate` (in the legacy
"Signal quality" row). A flat weighted rate while raw Info volume
spikes means we are **adding noise without adding edge** — tighten
Info.

### `PAL · calibration by implied-probability bucket` (table)

The most decision-useful single panel. For each bucket
(0-10 / 10-20 / 20-30 / 30-40 / 40-50 / 50-70 / 70+), shows
`success` count and `resolved` count over the last 24h.

How to read:

- The market thinks the 10-20% bucket should win **10-20%** of the
  time.
- If `success / resolved` for that bucket is **> 20%**, we have
  signal in the long-shot region.
- If `success / resolved` for 50-70% is **< 50%**, we're worse than
  the market on chalk plays.

The long-shot buckets matter most: a 5-percentage-point edge over
the market on 10-20% alerts is much bigger than a 5pp edge on 80%+
chalks (because the dollar payoff differential is much larger).

### `PAL · resolved sample size (7d)`

Anything below **30** resolved alerts: the dashboard is noise. Below
**100**: directional only. Above **300**: starting to be statistically
meaningful. A 95% binomial confidence interval needs roughly
`1.96 × sqrt(p(1-p)/n)` width, so for p≈0.5 and n=30 the CI is
±18 percentage points — wider than any signal we'd plausibly claim.

### `PAL · pending outcomes`

Counts alerts that haven't resolved yet. A growing pending pile
means recent alerts are invisible to the edge calculation. Wait for
resolution before re-tuning.

## How to verify the concept

1. **Run the watchtower for 7-30 days** with the current `.env`.
2. **Watch `PAL · cumulative realized edge` slope**. If it trends up,
   alerts are adding value. If it stays flat or drops, they're not.
3. **Cross-check `PAL · calibration by implied-probability bucket`**.
   The 0-10% and 10-20% buckets are the most informative: if their
   `success / resolved` rates exceed their implied probability, the
   strategy has signal in the long-shot region (where prediction
   markets historically have the most exploitable bias).
4. **Read `PAL · weighted success rate` in parallel**. A high
   weighted success is more meaningful than a high raw success rate.
5. **Always check `PAL · resolved sample size (7d)` first.** If it's
   below 30, none of the other PAL panels are trustworthy.

## How to tune

| Symptom | Likely tune |
|---|---|
| Info volume high (>50/day) but edge negative | **Tighten Info**. Raise `ALERT_INFO_MIN_MULTIPLIER`, `ALERT_INFO_MIN_NOTIONAL_USD`, or `SINGLE_MIN_BASELINE_TRADES`. |
| Edge positive but Info volume low (<5/day) | **Loosen readiness/lifecycle**. Drop `SINGLE_MIN_BASELINE_TRADES`, `BASELINE_MIN_READY_WINDOW`, `LIFECYCLE_ALERT_FROM_PCT`. |
| Warning/Critical edge poor | Inspect the strategy. Look at top-20 candidates via `cli diagnose-alerts --show-candidates 20`. Likely false positives. |
| CLV-lite positive but final success poor | Markets reverse late. Operator-side: trust 15m–1h drift more than 24h. Code-side: this is the late-resolution noise — nothing to tune. |
| Calibration "70+" bucket underperforms market | Chalk plays are leaking. Consider raising `ALERT_INFO_MIN_ODDS` to drop near-even-money admissions. |
| Calibration "0-10" bucket strongly beats market | **Don't change anything.** That's exactly what informed-flow detection should produce. |

## How to run

```bash
# Bring up the whole stack
docker compose -f deploy/docker-compose.yml up -d

# Or just the dashboard surface (when the rest is running elsewhere)
docker compose -f deploy/docker-compose.yml up -d prometheus grafana
```

URLs:

- Grafana: <http://localhost:3000/d/watchtower-main>
  (anonymous viewer; `admin/admin` to edit)
- Prometheus: <http://localhost:9091>
- Watchtower /metrics: <http://localhost:9090/metrics>

Telegram and Grafana are complementary surfaces. The user-facing
guide in `doc/observability/signal-quality.md` covers the Telegram
report cadence (daily/weekly/monthly/quarterly/yearly) and outcome
reactions.

## Operator checklist after 24h / 7d / 30d

### After 24h

- Is the pipeline ingesting trades? (Persistence panels >0)
- Are alerts firing? (`watchtower_telegram_alerts_sent_total` > 0)
- Are reactions being applied? (Telegram outcome reactions panel)
- Pending sample size: probably <30. **Don't conclude anything yet.**

### After 7d

- `PAL · resolved sample size (7d)`: should be ≥ 30 for any meaningful
  PAL read.
- `PAL · cumulative realized edge`: directional trend should be
  visible.
- Calibration table: at least one of the three long-shot buckets
  (0-10 / 10-20 / 20-30) should have ≥ 5 resolved alerts.

### After 30d

- The PAL panels are now statistically meaningful (resolved ≥ 200).
- If cumulative edge is positive AND calibration buckets show the
  expected pattern (long-shots out-perform their implied prob), the
  concept is proven for this configuration.
- If cumulative edge is flat/negative, the strategy is not adding
  value over the market's pricing — tighten or change strategy.

## Caveats

1. **Success is directional, not financial.** A `resolved_correct`
   alert means the direction was right; it does NOT mean a trader
   following the alert would have profited (entry price, slippage,
   timing all matter).
2. **The calibration table is descriptive, not prescriptive.** A
   bucket that out-performs market implied probability for one
   month may revert; check 7d and 30d separately.
3. **PAL metrics live on resolved alerts only.** The pending pile
   is invisible to the edge calculation by construction.
4. **Severity weights are constants (1/3/10/25)** — they change the
   weighted-success reading, not the underlying alerts. Operators
   who want a different weighting can compute it in PromQL with
   their own constants.
