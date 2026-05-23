# Watchtower Strategy Interaction Map

How the 9 strategies combine, conflict, and suppress each other.

## 1. Positive reinforcement matrix

A high-conviction signal usually requires **two or more** of the
following to fire together. The table reads: row + column → effect.

| with → | thesisaccum | holderdelta | catalystwindow | bookvacuum | repricinglag | walletcohort | cheaptail |
|---|---|---|---|---|---|---|---|
| **thesisaccum** | self | wallet is real top holder across linked markets → strongest informed-flow signal | aligned exposure inside catalyst window → "thesis primed before event" | depth withdrawing on a thesis-aligned market → "informed actors moving" | thesis market lagged peers → "thesis underreaction" | cohort converging on the thesis → multi-actor confirmation | cheap-tail across thesis graph → "convex bet on thesis" |
| **holderdelta** | — | self | holder concentration before catalyst → "real money positioning" | depth withdrawing where a top holder accumulated → fragile book | n/a | holder is also in a behavioural cohort → cohort-level position | wallet that's a top holder ALSO buys cheap-tail → coherent thesis |
| **catalystwindow** | — | — | self (booster only) | catalyst within bookvacuum window → "informed quote-pull" | catalyst opens repricing window | catalyst proximity boosts cohort score | required for cheaptail by default (`CHEAPTAIL_REQUIRE_CATALYST=true`) |
| **bookvacuum** | — | — | — | self | repricing window + depth vacuum → strongest repricing signal | n/a | n/a |
| **repricinglag** | — | — | — | — | self | n/a | n/a |
| **walletcohort** | — | — | — | — | — | self | cohort cheap-tail → coordinated lottery |
| **cheaptail** | — | — | — | — | — | — | self |

## 2. Suppression / capping (safety layer)

`rulesrisk` is the only strategy that **caps or blocks** others.

| Trigger | Effect | Controlled by |
|---|---|---|
| `rulesrisk.AmbiguityScore ≥ HIGH_THRESHOLD` (0.6) | `cheaptail` blocked | `RULES_RISK_BLOCK_CHEAPTAIL=true` |
| `rulesrisk.AmbiguityScore ≥ HIGH_THRESHOLD` | `repricinglag` blocked | `RULES_RISK_BLOCK_REPRICING=true` |
| `rulesrisk.AmbiguityScore ≥ HIGH_THRESHOLD` | Other strategy severity capped at `HIGH_CAP_SEVERITY` (default `warning`) | `RULES_RISK_HIGH_CAP_SEVERITY` |

Examples of high-ambiguity markers (lexical, deterministic):
"runoff", "recount", "certification" / "certified", "appeal",
"court", "definitive", "by end of", "announced", "official".

## 3. Arbitration (multi-strategy conflict)

`conflictresolve` runs **after** the per-strategy hooks. It reads
recent shadow decisions on the same `condition_id` and computes:

```
dominance = winning_side_quality / max(losing_side_quality, ε)
```

- `dominance ≥ MIN_DOMINANCE` (1.5) → keep winner, degrade loser
- `dominance ≥ SUPPRESS_DOMINANCE` (2.5) → fully suppress loser
- `dominance < MIN_DOMINANCE` → tag both as `unresolved_conflict`

`conflictresolve` itself **never emits a standalone alert** — it is
an arbitration layer that modifies routing of other strategies.

Side quality components per `SideSignal`:
- `WalletQualityScore` (PnL/CLV percentile)
- `HolderStrength` (from holderdelta if available)
- `ThesisBreadth` (from thesisaccum if available)
- `CatalystProximity` (from catalystwindow if available)
- `BookSupport` (from bookvacuum if available)
- `MMLike` boolean (subtracts `MM_PENALTY`)

## 4. Value tracking interaction

Every shadow decision is later evaluated by:

1. `strategyvalue.Worker` — computes `clv_15m`, `clv_1h`, `clv_6h`, `clv_24h`
   relative to `BaselinePrice` and `Side`. Idempotent UPDATE; NULL stays
   NULL when no trade data is available within the window.

2. `strategypromotion.Worker` — aggregates the above per
   `(strategy_name, strategy_version)` over `Lookback` and writes
   `polymarket_strategy_promotion_reviews` rows with eligibility.

3. `strategyoutcome.Worker` — fills `outcome_status` (correct/wrong/
   unknown) once the market resolves.

These three workers are **completely decoupled** from the hot path
— they read shadow rows after the fact. If a strategy generates a
shadow row but receives `outcome_status = resolved_wrong` repeatedly,
that strategy will never become promotion-eligible.

## 5. The promotion gate (terminal arbiter)

```
strategybus.Bus.Record(decision):
    if !flag.Enabled → drop silently (unknown_strategy metric)
    decision.StrategyVersion = cfg.StrategyVersion (auto-stamp)
    if !cfg.GlobalPromotionAllowed → force ShadowOnly=true
    if flag.ShadowOnly → force ShadowOnly=true
    if PromotionGate.Allow(strategy_name) == false → force ShadowOnly=true
    writer.Record(decision)
```

Triple-lock guarantees that **even with `GlobalPromotionAllowed=true`
AND `STRATEGY_SHADOW_ONLY=false`**, a strategy still cannot reach
the live alert pipeline unless `strategypromotion.Worker` has flipped
its eligibility to true for the latest review window.

The promotion review writes one row per (strategy, version) per
tick. The Bus reads only the most recent review when deciding.

## 6. Telegram routing interaction

After bus.Record:
- Shadow row → `polymarket_alerts` is NOT touched. Admin Telegram
  surface reads shadow rows directly (when configured via
  `TELEGRAM_STRATEGY_SHADOW_TO_ADMIN=true`).
- Live row → goes through `polymarket_alerts` → `alertsender.Worker`
  → typed Telegram router. User flow is gated by:
  - `TELEGRAM_STRATEGY_USER_FLOW_ENABLED=true`
  - `confidence ≥ TELEGRAM_STRATEGY_MIN_USER_CONFIDENCE`
  - `decision_level ≥ TELEGRAM_STRATEGY_MIN_USER_LEVEL`
  - dedupe `TELEGRAM_STRATEGY_USER_DEDUPE_WINDOW`

Old surfaces (Watchtower stats, market intel, daily intel,
prediction blocked, top annotations) are **disabled at the
config layer** and re-enabling them via env requires bypassing
the `staleEnvKeys{}` boot check (which fails loud).

## 7. Tuning interaction matrix (effect chain)

| If you raise → | thesisaccum fires | holderdelta fires | bookvacuum fires | repricinglag fires | conflictresolve fires |
|---|---|---|---|---|---|
| `MARKETLINKS_INTERVAL` | ↓ (graph staler) | n/a | n/a | ↓ (fewer peers) | ↓ (less context) |
| `HOLDERSYNC_INTERVAL` | n/a | ↓ (snapshots staler) | n/a | n/a | ↓ (less holder context for sides) |
| `BOOK_FEATURE_BARS_INTERVAL` | n/a | n/a | ↓ (bars staler) | ↓ (price source) | ↓ (less book support) |
| `STRATEGY_STAGED_CACHE_TTL` | static during cache window | static during cache window | static during cache window | static during cache window | static during cache window |
| `RULES_RISK_HIGH_THRESHOLD` | n/a | n/a | n/a | ↑ (fewer markets blocked) | n/a |

## 8. Anti-patterns (do not do)

1. **Lowering all thresholds simultaneously** — gives the illusion
   of activity but pollutes promotion review aggregates with low-quality samples.
2. **Setting `STRATEGY_SHADOW_RECORD_NOFIRE=true` permanently** —
   row volume blowup, contaminates per-strategy "fired" rate calculations.
3. **Enabling user flow without per-strategy promotion** —
   bypasses the triple-lock; only safe when ALL gates pass.
4. **Disabling rulesrisk while keeping repricinglag/cheaptail enabled** —
   removes the only ambiguity safety, exposing UMA dispute risk.
5. **Setting `BOOK_FEATURE_BARS_INTERVAL=1s` with `MAX_MARKETS=250`** —
   ~15k requests/min to CLOB; rate-limit risk.
6. **`HOLDERSYNC_REQUIRE_OPEN_INTEREST=false`** — opens path to
   pct_oi=NaN/infinity from zero-OI tokens.
