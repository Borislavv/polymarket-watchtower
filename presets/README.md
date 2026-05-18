# Alerting presets

Three opinionated overlays of the watchtower's anomaly + lifecycle env vars.
Each preset is self-contained — drop it in via `--env-file` or source it
before `make run`.

## At a glance

| Preset         | Info notional ≥ | Info multiplier ≥ | Lifecycle gate | HOT marker | Cluster trigger              | Intended use |
|----------------|-----------------|-------------------|----------------|------------|------------------------------|--------------|
| `conservative` | $50,000         | 500×              | 85%            | 95%        | 5 trades, 3 wallets, $100k   | Pager-grade — strongest signals only. |
| `balanced`     | $5,000          | 75×               | 75%            | 90%        | 3 trades, 2 wallets, $50k    | Day-to-day production alerting after the first-week validation period. |
| `aggressive`   | $2,000          | 20×               | 60%            | 85%        | 3 trades, 2 wallets, $25k    | Live validation profile — Info volume large enough to see end-to-end signal without becoming retail-spam. Projection sized from the live DB via `cli diagnose-alerts`. |

## Worked examples — does this trade alert?

The same trade evaluated against each preset, baseline median = $100,
market at 80% lifecycle, baseline reservoir warmed:

| Trade                                     | conservative | balanced | aggressive |
|-------------------------------------------|--------------|----------|------------|
| $3,000 @ odds 4, mul 30                   | no (below all rungs) | no (mul 30 < 100 → no fire) | **Info** |
| $12,000 @ odds 4, mul 120                 | no (notional 12k < 50k) | **Info** | **Warning** (mul 120 ≥ 100) |
| $30,000 @ odds 6, mul 300                 | no (notional 30k < 50k) | **Info** (mul 300 < 1000, conservative-MIN with abs Warning → Info) | **Warning** |
| $120,000 @ odds 8, mul 1,200              | **Info** (mul 1200 ≥ 500 → Info; conservative-MIN with abs Warning → Info) | **Warning** | **Warning** |
| $600,000 @ odds 10, mul 6,000             | **Warning** (mul 6000 < 10,000 → Warning) | **Critical** is gated by mul 10,000 → so this lands at Warning instead | **Critical** |
| $1,000,000 @ odds 10, mul 10,000          | **Critical** | **Critical** | **Critical** |
| 5 wallets × $30k @ odds 6 in 20 min       | no individual Info → no cluster ingest → no HARD | 5 Info alerts → 5 trades / 5 wallets / $150k → **HARD** | **HARD** |

The same trade evaluated against the lifecycle gate:

| Market state at trade time                 | conservative | balanced | aggressive |
|--------------------------------------------|--------------|----------|------------|
| 50% lifecycle                              | gated — no alert | gated — no alert | gated — no alert |
| 70% lifecycle                              | gated | gated | **alerts (gate is 60%)** |
| 80% lifecycle                              | gated (gate is 85%) | **alerts** | **alerts** |
| 92% lifecycle                              | **alerts (HOT below 95%)** | **alerts (HOT)** | **alerts (HOT)** |
| Missing start/end dates                    | gated (fail-closed) | gated (fail-closed) | **alerts (fail-open opt-in)** |

The same baseline shape evaluated against the readiness gates:

| Baseline state                              | conservative | balanced | aggressive |
|---------------------------------------------|--------------|----------|------------|
| 10 trades / $300 total / span 6h            | gated (count 10 < 50, total $300 < $5000, span 6h < 72h) | gated (count 10 < 20) | **trusted** |
| 25 trades / $1,500 total / span 30h         | gated | **trusted** | **trusted** |
| 60 trades / $6,000 total / span 80h         | **trusted** | **trusted** | **trusted** |

## Operator guidance

- **Default is `balanced`.** Switch away only when you have a specific
  reason (too noisy → `conservative`; nothing fires → `aggressive` to
  diagnose).
- **Tune in place rather than swapping presets** if a single rung needs
  adjustment. The presets are anchor points; the env file is the source of
  truth.
- **Lifecycle gate is the biggest knob.** Moving it 10 points up or down
  changes alert volume dramatically. Touch it last, not first.
- **Cluster trigger is the only path to HARD.** If you want Telegram
  paging on multi-wallet convergence, configure routing on `severity=hard`.

## Common semantics across all three

- `BASELINE_WINDOW` is a **cap on history kept**, not a required market
  age. A 1-month-old market on `BASELINE_WINDOW=1y` is scored using its
  1 month of available history.
- Alerts fire only when:
  - the market has cleared `MARKET_MIN_AGE`, AND
  - the lifecycle is past `LIFECYCLE_ALERT_FROM_PCT`, AND
  - the baseline has `≥ SINGLE_MIN_BASELINE_TRADES` samples totalling
    `≥ SINGLE_MIN_BASELINE_NOTIONAL_USD` over an observed span of at least
    `BASELINE_MIN_READY_WINDOW`.
- Severity is the **conservative MIN** of the absolute (notional+odds) and
  multiplier (notional/baseline-median) ladders. Single-trade severity
  caps at `critical`; `hard` is reserved for the cluster detector.
- `CATEGORY_WHITELIST` is the sole category-selection mechanism. ONLY
  whitelisted categories are monitored; everything else is ignored. Default
  `Politics`. Market titles, event slugs, market slugs, and tags are never
  scanned — a sports-themed market filed under a whitelisted non-sports
  category like `Hide From New` is still analysed normally.

## How to apply

```bash
# local
set -a && source presets/conservative.env && make run

# docker compose (in deploy/docker-compose.yml's watchtower service)
env_file:
  - ../presets/balanced.env
```
