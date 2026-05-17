# Project decisions

Accepted decisions for polymarket-watchtower. Update when changing alerting
strategy or terminology. For usage see README.md and presets/README.md.

## Alerting strategy

Goal: surface large risky bets and suspicious wallet clusters near the end
of a market's lifetime. Spam reduction is non-negotiable.

A single-trade alert fires only when **all** are true:
1. Category passes `CATEGORY_BLACKLIST` AND the market title/slug/event-slug
   contain none of the same keywords (catches sports tagged as `Hide From New`).
2. Market lifecycle progress ≥ `LIFECYCLE_ALERT_FROM_PCT` (default 75%).
   Unknown lifecycle → **silent by default** (`ALLOW_UNKNOWN_MARKET_LIFECYCLE=false`).
3. Market absolute age ≥ `MARKET_MIN_AGE` (default 24h).
4. Per-(category, market, outcome) baseline has `Count ≥ SINGLE_MIN_BASELINE_TRADES`,
   `TotalUSD ≥ SINGLE_MIN_BASELINE_NOTIONAL_USD`, `MedianUSD > 0`, and
   `SpanActual ≥ BASELINE_MIN_READY_WINDOW`.
5. Trade clears both absolute tier (notional AND odds) and multiplier tier
   (notional / baseline median).
6. Final severity = `ConservativeMin(absoluteTier, multiplierTier)` —
   **unless** `HardPromotion` fires (see below).

Baseline updates run **before** the alert gates so the reservoir warms
continuously even when alerts are suppressed.

## Detection modes

`ANOMALY_MODE` is validated `oneof=single_cluster volume`. Default
`single_cluster`. The `volume` mode is the legacy aggregate-rate detector;
do not extend it.

## Baseline semantics

- `BASELINE_WINDOW` is the **maximum** lookback. NOT a minimum-age
  requirement. A 1-month-old market with `BASELINE_WINDOW=1y` uses the 1
  month of available history. `0` means "no upper bound" (only
  `BASELINE_MAX_SAMPLES` caps memory).
- `BASELINE_MIN_TRADE_USD` (default $50) drops retail dust at `Baseline.Add`.
- `BASELINE_MIN_READY_WINDOW` (default 24h) requires the observed span
  (newest - oldest sample) to clear this floor before alerts can fire.
- Alerts display the **actual** span (`BaselineRef.Span`), never the
  configured cap.

## Lifecycle gating

- `LifecyclePct = (now - StartDate) / (EndDate - StartDate) * 100`.
- Below `LIFECYCLE_ALERT_FROM_PCT` (default 75) → no alert.
- At or above `LIFECYCLE_HOT_FROM_PCT` (default 90) → `Finding.Hot = true`,
  Telegram header carries `· HOT`.
- Markets with missing `StartDate`/`EndDate` are **silenced by default**.
  Set `ALLOW_UNKNOWN_MARKET_LIFECYCLE=true` to opt in (legacy/debugging).

## Severity rule

Three tiers, each `(MinNotionalUSD, MinOdds, MinMultiplier)`. Both ladders
must qualify at info or above; final = `ConservativeMin` (lower wins).
**HardPromotion** bypasses MIN: a trade matching all three HardPromotion
floors is promoted straight to `Hard` severity ("HumanReviewRequired").

Defaults:

|         | Notional ≥ | Odds ≥ | Multiplier ≥ |
|---------|------------|--------|--------------|
| Info    | $10,000    | 3      | 100×         |
| Warning | $25,000    | 5      | 250×         |
| Critical| $100,000   | 8      | 1,000×       |
| HardPromotion | $100,000 | 8 | 1,000× |

Operators can tune Critical/Warning multipliers downward to taste; the
shipped defaults align with the "shark hunting" goal of catching $100k
@ odds 8 @ 1000× as the canonical insider signal.

## Cluster rule

`Cluster.Detector` accumulates fired `TradeRef`s per category in a sliding
window. Fires HARD `CategoryWatchRequired` when all hold:
- `len(entries) ≥ CLUSTER_MIN_ANOMALOUS_TRADES` (default 3)
- `uniqueWallets ≥ CLUSTER_MIN_UNIQUE_TRADERS` (default 2)
- `totalUSD ≥ CLUSTER_MIN_TOTAL_NOTIONAL_USD` (default $50k)
- Last fire ≥ `CLUSTER_COOLDOWN` ago (default 30m)

Severity always `SeverityHard`. Payload carries a `Sample []TradeRef`
(capped 5) for per-trader contributor lines. `Cluster.Count(cat)` stamps
`InCluster` + `ClusterPeerCount` on the per-trade alert.

## Category blacklist rule

- One list, `CATEGORY_BLACKLIST`. No allowlist.
- Case-insensitive substring match against `slug + " " + label` at the
  category level.
- **Also** matched against `market.Question + market.Slug + market.EventSlug +
  market.EventTitle` — catches sports markets tagged with non-sports
  categories like Polymarket's `Hide From New`.
- Applied at discover (prune ids before they reach the registry) and at
  detect (defense in depth, increments
  `watchtower_filter_category_skipped_total{stage="detect"}`).

## Telegram alert content

HTML parse mode. Escape via `html.EscapeString` — never hand-roll.

Header: `<b>{SEV}: x{mul} · ${notional}[ · HOT] · {title}</b>`.

Sections (in order):
1. `Why` — multiplier, odds + implied probability, baseline (count + median +
   mean + p95 + **actual span**), tier composition, lifecycle pct (+ HOT
   note), single-vs-cluster context.
2. `Trade` — outcome+side, size+shares@price, trader wallet, category,
   ISO timestamp.
3. `Cluster` (HARD only) — counts, total, window, contributor list.
4. `Links` — bulleted list. Each `<a>` only when URL non-empty. Order:
   Polymarket market → Polymarket category → Trader → Grafana. Never emit
   plain-text "Grafana" — omit the row if the URL is missing.

Link URLs:
- Market: `<polymarket-base>/event/<event-slug>`.
- Category: `<polymarket-base>/predictions/<category-slug>`.
- Trader: `<polymarket-base>/profile/<wallet>`.
- Grafana: deep link with `from/to=±GRAFANA_CONTEXT_WINDOW`, `var-category`,
  `var-market`, `var-severity`. Set `GRAFANA_BASE_URL` to a publicly
  reachable host or recipients can't open it from a phone.

## Standard library for infrastructure primitives

URL building via `net/url`. JSON via `encoding/json`. HTTP via `net/http`
(or `internal/infra/polymarket/httpx` for upstreams — adds rate-limiting,
backoff, typed `APIError`). Time via `time.Duration`. No hand-rolled URL/
JSON/HTTP/retry primitives.

## Testing expectations

Hermetic by default: `go test ./...` must not touch the public internet.
Live integration tests opt-in behind `//go:build integration` and
`POLYMARKET_INTEGRATION=1`.

Required coverage for any alerting/baseline/lifecycle change:
- Baseline behavior across (window > market age, window < market age,
  window=0).
- Lifecycle pct at 50% / 75% / 90% boundaries.
- `MARKET_MIN_AGE` and `BASELINE_MIN_READY_WINDOW` gates.
- `ALLOW_UNKNOWN_MARKET_LIFECYCLE` — fail-closed default.
- Tier composition (`ConservativeMin` + `HardPromotion` override).
- Cluster floors (trades / wallets / total / cooldown).
- Telegram link rendering with each URL present and missing.
- Grafana URL includes `var-severity`.
- Category blacklist via category AND via market title/slug/event-slug.
- Every preset loads and matches its documented strictness.

## Presets

Three opinionated overlays under `presets/` (see `presets/README.md`):
- `conservative.env` — pager-grade; lifecycle ≥85%, multiplier ≥500×,
  notional ≥$50k. HardPromotion at $250k AND odds 8 AND 2500×.
- `balanced.env` — defaults; lifecycle ≥75%, multiplier ≥100×, notional
  ≥$10k. HardPromotion at $100k AND odds 8 AND 1000×.
- `aggressive.env` — local exploration only; lifecycle ≥60%, multiplier
  ≥30×, notional ≥$2.5k. `ALLOW_UNKNOWN_MARKET_LIFECYCLE=true`.

Apply via `set -a && source presets/<name>.env` or `env_file:` in compose.
Preset behaviour pinned by tests in `internal/app/preset_test.go`.
