# Composite conviction scoring

> Multi-dimensional anomaly logic for production-grade signal
> tuning. Design doc + tuning guidance. NOT an implementation
> patch — this is the philosophy `.env.prod` should encode.

The realism audit found that **single-dimension thresholds
under-perform**: every detector has a false-positive class
defined by "passed one gate, failed the others". The fix is not
"raise the one gate"; it is "require independent dimensions to
align before promoting severity".

---

## The six conviction dimensions

Every alert can be measured against six independent dimensions.
Each is normalised to [0, 1] for a composite "conviction score".

| Dim | What it measures | Already-implemented proxy |
|---|---|---|
| **Statistical rarity** | p99 displacement on market AND/OR trader axis | `MarketP95Ratio`, `MarketP99Ratio`, `TraderP95Ratio`, `TraderP99Ratio` |
| **Persistence** | repeated same-side decisions; line duration | accumulation `TradeCount`, `Span`, `Window=lifetime\|recent` |
| **Ownership impact** | wallet's fraction of outcome BUY-share count | `OwnershipRef.SharePct` |
| **Directional purity** | how clean is the side (BUY vs SELL on same wallet) | NOT directly exposed; needs derivation from mmfilter inputs |
| **Lifecycle timing** | late-stage > early-stage | `Finding.LifecyclePct`, `Hot` |
| **Market quality** | absolute liquidity + baseline maturity | `Market.Volume24hUSD`, `BaselineRef.SampleN`, `BaselineRef.Span` |

A conviction score is the **conservative-MIN** of these dimensions
(weakest dimension dominates), NOT a weighted average. A weighted
average lets one strong axis mask three weak ones; conservative-MIN
forces all dimensions to clear.

---

## Severity gates (proposed for `.env.prod`)

Each severity rung clears when **N out of 6 dimensions** clear
their tier floor:

| Severity | Required dimensions | Threshold philosophy |
|---|---|---|
| Info | 3 of 6 | heartbeat / watchlist |
| Warning | 4 of 6 | "read every one" |
| Critical | 5 of 6 | "page someone" |
| Hard | 6 of 6 (cluster only) | "wake up; multi-wallet convergence with everything aligned" |

This is the **composite gate**. The existing tier thresholds (in
`internal/domain/model/anomaly/rule.go::Thresholds`) remain in
force — composite logic adds the cross-dimensional requirement.

---

## Per-dimension thresholds (tier-anchored)

### 1. Statistical rarity

Tier clears when the trade clears **EITHER** axis at the named p99
ratio:

| Tier | Market p99 ratio | Trader p99 ratio |
|---|---|---|
| Info | ≥ 1.5 | ≥ 1.5 |
| Warning | ≥ 3.0 | ≥ 2.5 |
| Critical | ≥ 5.0 | ≥ 4.0 |

The `.env.prod` candidate values come from the actual p99 ratio
distribution against the live DB (query Q2 in
`distribution-queries.md`).

### 2. Persistence

Tier clears for accumulation Findings when the line shows:

| Tier | Min trades | Min span | Min directional purity |
|---|---|---|---|
| Info | 5 | 4h | 0.75 |
| Warning | 8 | 8h | 0.85 |
| Critical | 12 | 16h | 0.90 |

For single-trade Findings: persistence is N/A — single trades
contribute zero to this dimension.

### 3. Ownership impact

Tier clears when **BOTH** share% AND absolute market BUY volume
clear. The current implementation gates on share% alone — this is
the load-bearing audit finding.

| Tier | Min share% | Min market BUY volume USD |
|---|---|---|
| Info | 3 | 10,000 |
| Warning | 8 | 50,000 |
| Critical | 15 | 200,000 |

Absolute-volume floors come from query Q5 in
`distribution-queries.md`.

### 4. Directional purity

Defined per-wallet per-(market, outcome) in the configured
lookback:

```
purity = |BUY notional − SELL notional| / max(BUY + SELL notional, 1)
```

Tier clears when purity ≥:

| Tier | Min purity |
|---|---|
| Info | 0.75 (= 87.5/12.5 split) |
| Warning | 0.85 (= 92.5/7.5 split) |
| Critical | 0.92 (= 96/4 split) |

The MM filter already enforces a similar floor on suppression; the
production composite gate should require ≥ Info purity even on the
non-MM path. Wallets at 65/35 are "leaky directional" — not the
target.

### 5. Lifecycle timing

| Tier | Min lifecycle % |
|---|---|
| Info | 70 |
| Warning | 80 |
| Critical | 90 |
| Hot tag | 95 |

This dimension is a **scalar gate**, not normalised — lifecycle is
already an interpretable percentage. The current default
`LIFECYCLE_ALERT_FROM_PCT=75` is the Info floor.

### 6. Market quality

Tier clears when **ALL** of:

| Tier | Min 24h volume USD | Min baseline samples | Min baseline span |
|---|---|---|---|
| Info | 25,000 | 200 | 48h |
| Warning | 100,000 | 500 | 7d |
| Critical | 250,000 | 1,000 | 14d |

This is the strongest brake on the "thin market produces big
multiplier" false-positive class. Setting it tight produces fewer
alerts but each is higher-quality.

---

## Composite scoring example

A trade fires through the existing tier ladder at Warning. Now
check the six dimensions at Warning floor:

| Dim | Trade meets Warning floor? |
|---|---|
| Statistical rarity | ✓ (market p99 ratio = 4.2) |
| Persistence | ✗ (single trade) |
| Ownership impact | ✓ (10% of $80k BUY volume) |
| Directional purity | ✓ (88/12 split last 24h) |
| Lifecycle timing | ✓ (89%) |
| Market quality | ✗ (200 baseline samples, 36h span — under floor) |

**4 of 6 dimensions clear** → composite Warning passes. Alert
ships at Warning.

If the trade had cleared only 3 dimensions → composite Info, even
if the single-tier ladder said Warning. The composite gate
**downgrades** when dimensions disagree.

This is the brief's "rare high-confidence structures" target:
4-of-6 is genuinely uncommon; 5-of-6 even more so.

---

## What composite scoring does NOT do

- It does NOT replace the existing tier ladder. The tier ladder
  is the FIRST filter — composite is the SECOND, applied to
  tier-passing trades to confirm or downgrade.
- It does NOT promote severity. Composite can ONLY downgrade (or
  pass-through). A trade that scores 6-of-6 at Info-level
  thresholds is still an Info alert if the tier ladder said Info.
- It does NOT replace the MM filter, the lifecycle gate, or the
  category whitelist. Those run BEFORE composite.

---

## How `.env.prod` encodes composite scoring

Today's config supports per-tier floors on individual dimensions:

- `ALERT_*_MIN_NOTIONAL_USD` → statistical rarity (absolute)
- `ALERT_*_MIN_MARKET_P95_RATIO` / `MIN_MARKET_P99_RATIO` →
  statistical rarity (relative)
- `ACCUMULATION_*` → persistence
- `OWNERSHIP_*_MIN_SHARE_PCT` → ownership share
- `LIFECYCLE_ALERT_FROM_PCT` → lifecycle floor
- `SINGLE_MIN_BASELINE_*` → market quality (baseline)
- `STABLE_FAVORITE_MIN_MARKET_VOLUME_USD` → market quality (24h vol)

The brief's "composite alignment requirement" maps to:
**setting EVERY one of these floors to its tier value, so a trade
that fails any single dimension cannot reach the tier severity**.
This is composite scoring expressed in the existing knob surface,
not a new mechanism.

Gaps the current env model does NOT cover (would require code):

1. **Directional purity** — no per-wallet directional-purity floor
   exists outside `MM_NEUTRALITY_TOL`. Either add an explicit env
   gate (`ACCUMULATION_MIN_DIRECTIONAL_PURITY`) or tighten
   `MM_NEUTRALITY_TOL` to cover.
2. **Ownership × market-volume gate** — share% gate exists, volume
   floor does not. Add `OWNERSHIP_MIN_MARKET_BUY_VOLUME_USD` per
   tier.
3. **Accumulation absolute-total floor** — `ACCUMULATION_*_TOTAL_MULTIPLIER`
   exists, absolute USD floor does not. Add
   `ACCUMULATION_MIN_LINE_TOTAL_USD` per tier so 44 × $140 = $6k
   cannot fire Warning even when multiplier clears.

These are documented gaps — not implementation work in this pass.
They become candidate follow-up tasks.
