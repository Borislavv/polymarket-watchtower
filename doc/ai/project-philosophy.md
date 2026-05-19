# Project philosophy (AI-oriented)

> Written for another AI coding agent. Dense, no fluff.

## What Watchtower is

A **prediction-market surveillance + signal-intelligence system** on
top of Polymarket public data. It turns raw trade history into a
small feed of operator-decision-grade alerts.

## What it is NOT

- not a trading bot (no orders)
- not an "insider detector" (legal claim; never made)
- not a guarantee engine ("guaranteed/risk-free/sure thing" forbidden in copy)
- not a whale firehose (operator attention budget is the load-bearing limit)
- not a meme alert toy

## Business value

| Layer | What it produces |
|---|---|
| ingest | normalised Polymarket trade DB, single source of truth |
| detect | per-trade scoring against statistical + per-wallet baselines |
| filter | aggressive suppression of MM / meme / coinflip / low-baseline noise |
| explain | AI Analyst note answering "would we consider this side?" |
| measure | post-resolution outcomes + drift → signal-quality dashboards |
| tune | tuning methodology grounded in DB distributions, not gut feel |

The product is **a filter + explainer**, not a generator. The model
is: many trades → few alerts → fewer high-signal alerts → fewer the
operator actually reads → some that move money.

## Why each piece exists

| Piece | Why |
|---|---|
| Postgres persistence | restart safety, dedup, outcome tracking, replay |
| Detection queue (DB-backed) | backfill + future sources analysed exactly once |
| Per-strategy detectors | each catches a different shape; sum > single |
| MM filter | borderline market-makers are the dominant false positive |
| Lifecycle gate | early-market behaviour is noise |
| Low-baseline severity cap | thin baselines produce false-positive multipliers |
| Stable-favorite worker | state-driven late convergence ≠ per-trade flow |
| AI analyst note | numeric alerts are ambiguous without context |
| Cross-flow context fields | opposite-side concurrent flow → AI says Watch/Unclear |
| Outcomes + drift workers | proof-of-edge over time, not gut feel |
| period_key UNIQUE | one 2h scout report per bucket — no spam |
| ai_request_logs (separate table) | provider failures NEVER in analytical table |

## Optimisation targets (ordered)

1. **Precision** > volume. A smaller feed of higher-signal alerts wins.
2. **Persistence** > size. Repeat directional decisions beat one $20k bet.
3. **Evidence** > intuition. Tune from DB distributions, not gut.
4. **Explainability** > black-box. Every alert names which gates cleared.
5. **Determinism** > novelty. Same inputs → same alerts. Dedup is sacred.

## Good alerts vs garbage alerts

**Good (high signal):**
- late-stage (lifecycle ≥ 85%) Politics market
- single wallet, 9 BUYs over 18h, $36k total, median $4k, market median $200
- trader history shows persistent same-side conviction
- no opposite-side flow last 24h
- ownership share ≥ 10% of net-BUY shares
- baseline mature: 200+ trades, $25k+ aggregate, 48h+ span

**Garbage (the noise classes the system suppresses):**
- isolated $50k trade on a 30%-lifecycle meme market
- wallet bought + sold same outcome 12 times today (MM, suppressed)
- "5000× multiplier" on a market with $300 total volume (low-baseline trap)
- opposite-side wallet also accumulating same hour (contradictory)
- coinflip market 0.49 ↔ 0.51 with no convergence
- celebrity-novelty topic with no real-world catalyst

## Why politics markets matter most

Public research (Wired/CFTC, Columbia + Haifa, Polymarket
microstructure) is consistent: **politics markets have meaningful
information asymmetry**. Meme/celebrity/sports markets have higher
noise and weaker informed-flow signals. The default
`CATEGORY_WHITELIST=Politics` is not arbitrary — it is the
highest signal-to-noise category by far.

## Operator mindset (target persona)

- skeptical, not gambler
- reads Warning/Critical carefully; uses Info as watchlist
- runs SQL when something looks off
- tunes one threshold at a time against `diagnose-alerts`
- expects **measurable** signal quality, not promises
- accepts "no alert today" as healthy when market is quiet

## What "success" means here

The product succeeds when:
- the operator does not mute the Telegram channel after one week
- PAL dashboards show resolved_correct > implied probability in low buckets
- AI request_logs show success ratio > 0.9
- the detection queue drains; pending backlog flat
- false-positive rate stays low: spot-check 10 Info alerts → ≥ 7 are plausible
- the operator can articulate WHY any given alert fired, in 10 seconds, from the Telegram body alone

## What "failure" means here

- operator mutes the channel
- Telegram body says "AI summary unavailable" as if it were a real note
- `polymarket_alert_analyses` contains raw provider error JSON
- alerts fire on meme markets / coinflips
- pending detection queue grows without bound
- duplicate market-intelligence reports within a 2h window
- "insider" / "guaranteed" appears anywhere in copy

All of these are pinned by tests or invariants in the code. Drift
from any of them is a regression, not a feature.
