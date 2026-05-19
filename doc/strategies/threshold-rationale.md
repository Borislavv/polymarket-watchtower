# `.env` v8.3 threshold rationale

Every numeric threshold in the production `.env`, mapped to the DB
distribution that backs it. Measured 2026-05-19 against 1.23M
trades / 4013 markets / last 30 days.

Backup of the previous values: `.env.bak.20260519-200206`.

## Notional floors (Q1)

Q1: per-trade notional distribution for trades passing the
lifecycle ≥ 75% gate.

```
p50 = $9.94   p90 = $149.74   p95 = $300   p99 = $1,132
p99.5 = $2,350   p99.9 = $8,811   n = 381,296
```

| Knob | Old | New | Why |
|---|---|---|---|
| `ALERT_INFO_MIN_NOTIONAL_USD` | 1500 | **1200** | Sits at p99 — top 1% of post-filter trades. |
| `ALERT_WARNING_MIN_NOTIONAL_USD` | 3500 | **2500** | Sits at p99.5. |
| `ALERT_CRITICAL_MIN_NOTIONAL_USD` | 10000 | **9000** | Sits just above p99.9 — top 0.1%. |
| `ALERT_INFO_MIN_PROFIT_USD` | 750 | **1000** | Aligns with new notional floor at odds ≈ 2. |
| `ALERT_WARNING_MIN_PROFIT_USD` | 2500 | **3000** | Aligns with new notional × odds. |
| `ALERT_CRITICAL_MIN_PROFIT_USD` | 7500 | **9000** | Aligns with new notional × odds. |

## Per-tier ratio gates (composite gating)

Previously `*_MIN_MARKET_P99_RATIO` and `*_MIN_TRADER_P99_RATIO`
were set to `0` (disabled). The realism audit identified
single-axis firing as the dominant FP class. **All p99 ratio gates
are now ALWAYS-ON.**

| Knob | Old | New | Why |
|---|---|---|---|
| `ALERT_INFO_MIN_MARKET_P95_RATIO` | 1.0 | **1.5** | Require real displacement, not p95-margin. |
| `ALERT_INFO_MIN_MARKET_P99_RATIO` | 0 | **1.0** | Composite-scoring gate enabled. |
| `ALERT_INFO_MIN_TRADER_P95_RATIO` | 0.5 | **1.5** | Trader axis no longer fires on its own at p95. |
| `ALERT_INFO_MIN_TRADER_P99_RATIO` | 0 | **1.0** | Composite-scoring gate enabled. |
| `ALERT_WARNING_MIN_MARKET_P95_RATIO` | 1.5 | **3.0** | Warning requires 3× p95 displacement. |
| `ALERT_WARNING_MIN_MARKET_P99_RATIO` | 0 | **2.0** | Composite gate. |
| `ALERT_WARNING_MIN_TRADER_P95_RATIO` | 1.0 | **2.0** | Composite gate. |
| `ALERT_WARNING_MIN_TRADER_P99_RATIO` | 0 | **1.5** | Composite gate. |
| `ALERT_CRITICAL_MIN_MARKET_P95_RATIO` | 3 | **5.0** | Critical = top 0.1% on multiple axes. |
| `ALERT_CRITICAL_MIN_MARKET_P99_RATIO` | 0 | **3.0** | Composite gate. |
| `ALERT_CRITICAL_MIN_TRADER_P95_RATIO` | 1.5 | **3.0** | Composite gate. |
| `ALERT_CRITICAL_MIN_TRADER_P99_RATIO` | 0 | **2.0** | Composite gate. |

## Multipliers (anti-thin-baseline)

| Knob | Old | New | Why |
|---|---|---|---|
| `ALERT_INFO_MIN_MULTIPLIER` | 25 | **50** | Q1 p50 = $10; a 25× multiplier was trivial. Real conviction lines (Q3) display total/median ≥ 50×. |
| `ALERT_WARNING_MIN_MULTIPLIER` | 50 | **100** | 2× Info as the rung step. |
| `ALERT_CRITICAL_MIN_MULTIPLIER` | 100 | **250** | Rare-by-design. |

## Baseline readiness

| Knob | Old | New | Why |
|---|---|---|---|
| `SINGLE_MIN_BASELINE_TRADES` | 15 | **50** | Statistical claims need more samples. 15-trade baselines produced "1000× median" fake-multiplier traps. |
| `SINGLE_MIN_BASELINE_NOTIONAL_USD` | 1500 | **5000** | Aggregate floor: a 15-trade baseline at $100 each ($1500) is not a baseline. |
| `BASELINE_MIN_READY_WINDOW` | 3h | **24h** | Time-span maturity: 3h is one news cycle, not a baseline. |

## Lifecycle gates

| Knob | Old | New | Why |
|---|---|---|---|
| `LIFECYCLE_ALERT_FROM_PCT` | 60 | **75** | Politics late-stage focus per CLAUDE.md §alerting strategy. 60% allowed first-week-validation noise. |
| `LIFECYCLE_HOT_FROM_PCT` | 85 | **90** | HOT reserved for truly final-stretch markets. |

## Cluster (HARD) gates

| Knob | Old | New | Why |
|---|---|---|---|
| `CLUSTER_MIN_TOTAL_NOTIONAL_USD` | 35000 | **100000** | Brief target: 0-2 HARD/day. Q3 real conviction lines = $140k-$292k each; a 3-wallet cluster crossing $100k combined matches operationally credible convergence. |

`CLUSTER_MIN_ANOMALOUS_TRADES=4` and `CLUSTER_MIN_UNIQUE_TRADERS=3`
kept — already strict enough; the loose dimension was notional.

## MM filter

| Knob | Old | New | Why |
|---|---|---|---|
| `MM_NEUTRALITY_TOL` | 0.30 | **0.40** | Q11 purity histogram is bimodal: 2901 MM-like (<0.50) vs 3519 clean (≥0.92), only 296 wallets in the leaky 0.50-0.92 band. 0.40 catches borderline ~70/30 rebalancers without hurting genuinely directional wallets. |

## Trader axis

| Knob | Old | New | Why |
|---|---|---|---|
| `TRADER_MIN_HISTORY_TRADES` | 5 | **20** | A wallet's p95 over 5 trades is "the largest of 5" — noise. 20+ trades give a credible distribution. |

## Accumulation

| Knob | Old | New | Why |
|---|---|---|---|
| `ACCUMULATION_MANY_SMALLS_MULTIPLIER` | 4 | **17** | Q10: 2,373 many-smalls FP lines totalling $4.36M of "fake conviction". Floor = Info × 17 = $1200 × 17 = $20,400 line total. Exactly the threshold suppressing the $6k/44-trade FP pattern while keeping real $25k+ slow-drips. |
| `ACCUMULATION_HARD_MULTIPLIER` | 3 | **4** | HARD accumulation should be rarer than HARD cluster. |

`ACCUMULATION_MIN_TRADES=5` kept as entry gate. The composite
fix is the absolute floor via `MANY_SMALLS_MULTIPLIER`, not the
entry count.

## Ownership concentration

Q5 joint distribution showed the FP cell `<$10k volume × 25%+
share` has 8,219 rows. Real signal (≥$50k volume × ≥10% share)
has 161 rows over 60d ≈ 2.7/day.

| Knob | Old | New | Why |
|---|---|---|---|
| `OWNERSHIP_INFO_PCT` | 10 | **15** | Q5 row count below this gate at the $10-50k volume band was 561 — too many. 15% drops to 339 → 5-6/day. |
| `OWNERSHIP_WARNING_PCT` | 15 | **20** | Tighter band; Q5 $50-250k × 10-25% = 85 rows over 60d ≈ 1.4/day. |
| `OWNERSHIP_CRITICAL_PCT` | 25 | **30** | Q5 $250k+ × 25%+ = 19 rows over 60d ≈ 0.3/day. |
| `OWNERSHIP_MIN_NOTIONAL_USD` | 10000 | **25000** | Single most important change. The <$10k volume FP class (8,219 rows) is now entirely suppressed. |

## Verification

```sh
set -a && source .env && set +a
go run ./cmd/cli diagnose-alerts -dsn "$POSTGRES_DSN" -lookback 24h -show-candidates 5
```

Projected (post-tuning):
- Critical: 2/day (target 0-2 ✓)
- Warning: 3/day (target 1-5 ✓)
- Info: 13/day (target ≤15 ✓)
- Hard: 0/day (target 0-1 ✓)

Top candidates by profit-if-win are real-world Iran-related
Politics markets with p95 ratios 5-63× and trade sizes $5k-$11k —
the shape the realism audit identified as "real whale" structure.

## Rollback

```sh
cp .env.bak.20260519-200206 .env
```
