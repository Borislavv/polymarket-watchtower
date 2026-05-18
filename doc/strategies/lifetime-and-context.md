# Strategies A, B, E — lifetime accumulation, new-wallet context, ownership concentration

These three strategies extend the v4 baseline (single-trade, cluster,
24h accumulation, quiet-market wake-up, MM/arb suppression, outcome
tracking, CLV-lite drift) with one new primary alert kind, one context
booster, and one widened existing detector. They are designed to work
**together** — context boosters never spam, primary alerts never
double-emit, and the MM filter still applies upstream.

This document is the operator's reference for what's implemented vs
what is deferred until missing data lands.

## Terminology

The strategies surface **surveillance signals**, not claims about
intent. The right operator vocabulary:

- "informed-flow candidate" — a trade or line whose shape matches the
  informed-trader literature
- "suspicious wallet-market pair" — a wallet whose activity on one
  (market, outcome) clears multiple gates simultaneously
- "anomalous line" — same-trader accumulation that clears the line-level
  multiplier ladder
- "high-conviction wallet" — wallet whose stored alert outcomes show
  > 80% correct over ≥ 5 resolved alerts (reason code; no persisted
  watchlist yet — see "Deferred" below)
- "market ownership concentration" — wallet has accumulated a
  meaningful percentage of the outcome's recorded BUY-side flow

The word "insider" is intentionally never used.

## Strategy A — lifetime accumulation

**What it catches.** A wallet that drips into the same
(market, outcome, side) over weeks or months. Each individual trade is
modest, the daily volume looks ordinary, but the cumulative line is
qualitatively large. The 24h accumulation detector misses this by
construction (its window is too short to see the line).

**How it's implemented.** The accumulation detector now evaluates BOTH
windows for every ingested trade:

- **`recent`** — `since = now - ACCUMULATION_WINDOW` (default 24h).
  Cooldown-bucket dedup: emissions on the same line collapse inside
  one cooldown window.
- **`lifetime`** — `since = NULL` on the SQL side. Per-(line, severity
  tier) dedup: each tier emits exactly one alert per line per the life
  of the market. Severity upgrades emit one new alert at each tier.

The math is unchanged. The window kind is carried through `Line.Window`
→ `Verdict.Window` → `Finding.Accumulation.Window` → the Telegram
header. Per-window dedup keys live in `detect.go::accumulationDedupKey`:

```
accumulation:<sv>:recent:<wallet>:<mid>:<token>:<side>:<bucket>
accumulation:<sv>:lifetime:<wallet>:<mid>:<token>:<side>:<severity>
```

**Severity behaviour.** Same Info/Warning/Critical thresholds as the
24h path — there is no parallel severity universe. Hard promotion
follows the same rule (line total ≥ HardMultiplier × Critical AND
HOT lifecycle).

**Telegram header.** The accumulation alert renders the window tag in
the Why block:

```
• window: recent
```

Operators reading the alert see whether they're looking at a 24h burst
or a 90-day slow drip without having to derive it from Span.

**Tests.** `detect_v4_accumulation_test.go`:
`TestAccumulation_LifetimeBothWindowsFire`,
`TestAccumulation_LifetimeDedupesAtSameTier`,
`TestAccumulation_LifetimeUpgradeEmitsAtHigherTier`,
`TestAccumulation_DedupKeyShape` (updated to assert both shapes).

## Strategy B — new-wallet context booster

**What it catches.** A wallet first seen recently (or with very thin
stored history) placing a meaningful trade or building a meaningful
line. The shape is qualitatively more suspicious than the same trade
from a long-history wallet.

**How it's implemented.** Reason codes — never a standalone alert.
After scoring qualifies a single-trade or accumulation alert, the
detector reads `polymarket_traders.first_seen_at` (via
`cfg.Traders.GetByWallet`) and the wallet's trade-count from
`traderStats`. If EITHER `age < NEW_WALLET_MAX_AGE` OR
`historyTrades ≤ NEW_WALLET_MAX_HISTORY_TRADES`, the helper attaches:

- `NEW_WALLET_LARGE_BET` (on single-trade Findings) or
  `NEW_WALLET_ACCUMULATION` (on accumulation Findings)
- `LOW_TRADER_HISTORY` (when the history-count gate tripped, regardless
  of age)

The `Finding.NewWallet` ref carries `FirstSeenAt`, `AgeAtTrade`,
`HistoryTrades`, `IsNew=true`. The Telegram Why block renders a single
context line:

```
• new wallet: first seen 12h0m ago, 7 stored trades
```

**Crucial property.** The booster never blocks emission and never
promotes severity — it is informational. Disabling it has zero effect
on whether alerts fire.

**Tests.** `detect_v4_accumulation_test.go`:
`TestNewWallet_AccumulationCarriesContextReason`,
`TestNewWallet_OldWalletGetsNoBoost`.

## Strategy E — market-ownership concentration

**What it catches.** A wallet that has accumulated a meaningful
fraction of an outcome's recorded BUY-side flow. Distinct alert kind
`ownership_concentration` with its own visual style in Telegram.

**HOW IT'S IMPLEMENTED — and a critical caveat.**

There is **no Polymarket holders endpoint wired** in the watchtower.
`CLOB_API_URL` is in config but never used to call positions/holders.
The ownership percentage is therefore computed from **trade-flow
share counts**:

```
SharePct = (wallet_BUY_size_shares − wallet_SELL_size_shares)
         / SUM(BUY size_shares across all wallets on this outcome) × 100
```

A wallet that transferred shares off-chain, sold to a counterparty
whose trade wasn't ingested, or accumulated on the CLOB before the
watchtower started observing the market is invisible to this signal.
The percentage is **directional, not authoritative**.

Every `OwnershipRef` carries `Approximate=true`. The Telegram renderer
surfaces this as a dedicated italic line:

```
• trade-flow approximation — no holders endpoint wired; figure is
  directional, not authoritative
```

Do not draw position-level inference from the exact percentage. Use it
to triage: "this wallet is concentrating exposure" is reliable; the
exact share fraction is not.

**Harmonization.** The detector is invoked from the accumulation path,
not as a standalone worker. Coupling means:

- Ownership cannot fire on a wallet whose recent activity wouldn't
  also trigger accumulation — so dust-market and micro-position cases
  are blocked at the upstream gate.
- The alert is a SEPARATE Finding (kind `ownership_concentration`)
  with its own dedup key — it does not double up with the
  accumulation row.
- Per-tier dedup: a stable position at 17% emits one Warning
  ownership alert, never again at the same tier. A growth to 30%
  emits one new Critical alert.

Dedup key shape:

```
ownership:<sv>:<wallet>:<mid>:<token>:<severity>
```

**Tiers (defaults).**

| Tier | Threshold | Reason codes |
|---|---|---|
| Info | 10% | `MARKET_OWNERSHIP_CONCENTRATION` |
| Warning | 15% | `MARKET_OWNERSHIP_CONCENTRATION` |
| Critical | 25% | `MARKET_OWNERSHIP_CONCENTRATION`, `WALLET_DOMINATES_OUTCOME` |

Absolute-position floor: `OWNERSHIP_MIN_NOTIONAL_USD` (default
$10,000). A wallet that "owns 50%" of a dust market with $200 of
recorded flow never fires — the floor blocks it.

**Tests.**
- Pure detector: `internal/app/usecase/analytics/ownership/ownership_test.go`
  — `TestDecide_TierLadder`, `TestDecide_BelowFloorNoFire`,
  `TestDecide_DisabledNeverFires`, `TestDecide_SellsReduceNetShares`,
  `TestDecide_CriticalCarriesDominateReason`.
- Integration: `detect_v4_accumulation_test.go`:
  `TestOwnership_FiresWhenAccumulationFiresAndShareIsHigh`,
  `TestOwnership_NoFireWhenBelowFloor`,
  `TestOwnership_PerTierDedupes`.

## Interaction matrix

Two principles harmonize the strategies:

1. **Primary alerts** are `trade_anomaly`, `accumulation`,
   `category_watch` (cluster), `ownership_concentration`. Each has its
   own dedup namespace.
2. **Context boosters** (`new wallet`, `quiet market`, `high-win-rate
   wallet` reasons) attach to a primary alert — they never emit
   standalone.

| Interaction | Behaviour |
|---|---|
| single trade + new wallet | Single alert with `NEW_WALLET_LARGE_BET` reason |
| accumulation + new wallet | Accumulation alert with `NEW_WALLET_ACCUMULATION` reason |
| accumulation (recent) + accumulation (lifetime) on same line | Two distinct alerts, one per window (different dedup namespaces). Operators reading both see the burst-vs-drip difference in the window tag |
| accumulation + ownership concentration | Two distinct alerts (different kinds, different dedup keys). Both reference the same wallet and market — Telegram operators see them within seconds |
| quiet market + accumulation | Accumulation alert with `QUIET_MARKET_WAKEUP` reason |
| MM-like wallet | Single-trade AND accumulation alerts suppressed (logged with `POSSIBLE_MARKET_MAKER`). Ownership still emits — large net positions from MM wallets are still surveillance-relevant |
| cluster | Unchanged; cluster math is independent of the new strategies |

## Deferred — what is NOT shipped in this pass

| Family | Status | Why |
|---|---|---|
| C — new-wallet successful streak | **Deferred (subsumed by D)** | Same data limitation as D; would produce noisy false positives |
| D — high-win-rate watchlist (persisted table + worker) | **Deferred — reason hook only** | The only available data source for per-wallet success rate is `polymarket_alerts.outcome_status`, which is biased toward already-alerted wallets. A persisted watchlist driven by alerts-on-alerts would mislead. When per-trade PnL data lands (e.g. resolved-trade enrichment that joins every stored trade to its market's winning outcome), the persisted `polymarket_wallet_watchlist` table + worker should be built. Until then, no fake watchlist |

If you need any of the deferred behaviours, file an issue describing
the upstream data source — the watchlist requires an indexer that joins
every stored trade to a resolved market outcome.

## Operator checklist

When tuning these strategies:

- **Lifetime accumulation noise too high?** Raise
  `ACCUMULATION_MIN_TRADES` (the floor applies to both windows).
- **Lifetime emitting upgrade alerts too often?** The detector emits
  exactly one upgrade per tier per line. If you're getting a flood,
  it's because new wallets are crossing into higher tiers — that is
  the intended signal. Raise the tier multipliers if you want fewer.
- **New-wallet booster firing on too many alerts?** Tighten
  `NEW_WALLET_MAX_AGE` and/or `NEW_WALLET_MAX_HISTORY_TRADES`. The
  booster has no severity effect, so this affects only how often the
  reason code is attached.
- **Ownership concentration firing on dust positions?** Raise
  `OWNERSHIP_MIN_NOTIONAL_USD`.
- **Ownership concentration percentages don't match what you see on
  Polymarket?** Expected — see the approximation caveat above. The
  percentage is wallet-net-BUY-shares vs market-total-BUY-shares, only
  over trades the watchtower ingested.
