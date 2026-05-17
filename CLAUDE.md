# Project decisions

Accepted decisions for polymarket-watchtower. Update when changing alerting
strategy or terminology. For usage see README.md and presets/README.md.

## Alerting strategy

Goal: surface large risky bets and suspicious wallet clusters near the end
of a market's lifetime. Spam reduction is non-negotiable.

A single-trade alert fires only when **all** are true:
1. The market's category passes `CATEGORY_BLACKLIST`. Matching is case-
   insensitive substring against the category `slug + " " + label` ONLY —
   market titles, event slugs, market slugs, and tags are not scanned.
2. Market lifecycle progress ≥ `LIFECYCLE_ALERT_FROM_PCT` (default 75%).
   Unknown lifecycle → **silent by default** (`ALLOW_UNKNOWN_MARKET_LIFECYCLE=false`).
3. Market absolute age ≥ `MARKET_MIN_AGE` (default 24h).
4. Per-(category, market, outcome) baseline has `Count ≥ SINGLE_MIN_BASELINE_TRADES`,
   `TotalUSD ≥ SINGLE_MIN_BASELINE_NOTIONAL_USD`, `MedianUSD > 0`, and
   `SpanActual ≥ BASELINE_MIN_READY_WINDOW`.
5. Trade clears both the absolute tier (notional AND odds) and the
   multiplier tier (notional / baseline median).
6. Final severity = `ConservativeMin(absoluteTier, multiplierTier)`. Single-
   trade severity caps at `critical`. `hard` is cluster-only.

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
must qualify at Info or above; final = `ConservativeMin` (lower wins).
Single-trade severity caps at `critical`. `hard` is the cluster detector's
output and is qualitatively different — "multiple sharks agreeing", not
"one very big bet".

Defaults:

|          | Notional ≥ | Odds ≥ | Multiplier ≥ |
|----------|------------|--------|--------------|
| Info     | $10,000    | 3      | 100×         |
| Warning  | $25,000    | 5      | 1,000×       |
| Critical | $100,000   | 8      | 10,000×      |

No promotion-override ladders (HardPromotion, HugeWhale, MegaWhale,
sub-cluster) live in the model anymore. Earlier iterations stacked them on
top of conservative-MIN and produced four overlapping ways to reach `hard`
on a single trade — too many independent knobs to reason about. Operators
who want a single $1M bet to wake them up should set the Critical
thresholds to a shape it clears.

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
- Case-insensitive substring match against the Polymarket category
  `slug + " " + label` and **nothing else**. Market titles, event slugs,
  market slugs, and tags are deliberately NOT scanned.
- A sports-themed market (e.g. "Will France win the 2026 FIFA World Cup?")
  filed under a non-sports category like Polymarket's `Hide From New` is
  still real prediction-market activity and is analysed normally. Sports
  exclusion is a category-identity decision, not a keyword-on-text decision.
- Default value: `sports,sport`. Operators wanting to exclude additional
  categories add their slug or label — never a keyword meant to catch
  market wording.
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
- Tier composition (`ConservativeMin` of absolute and multiplier ladders).
- Cluster floors (trades / wallets / total / cooldown).
- Telegram link rendering with each URL present and missing.
- Grafana URL includes `var-severity`.
- Category blacklist: primary sports category blocked, sports-themed market
  under non-sports category allowed, sports keyword in market metadata only
  is allowed.
- Every preset loads and matches its documented strictness.

## Presets

Three opinionated overlays under `presets/` (see `presets/README.md`):
- `conservative.env` — pager-grade; lifecycle ≥85%, Info multiplier ≥500×,
  Info notional ≥$50k.
- `balanced.env` — defaults; lifecycle ≥75%, Info multiplier ≥100×, Info
  notional ≥$10k.
- `aggressive.env` — local exploration only; lifecycle ≥60%, Info
  multiplier ≥30×, Info notional ≥$2.5k. `ALLOW_UNKNOWN_MARKET_LIFECYCLE=true`.

Apply via `set -a && source presets/<name>.env` or `env_file:` in compose.
Preset behaviour pinned by tests in `internal/app/preset_test.go`.
