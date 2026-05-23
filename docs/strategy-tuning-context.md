# Watchtower Strategy Tuning Context Pack

Audience: a separate planning/analysis model that will help an operator tune
Watchtower strategy parameters. The model does NOT have direct codebase
access — this document is the source of truth.

## 1. What Watchtower is, in one paragraph

Watchtower is a Polymarket informed-flow surveillance engine. It ingests
trades, holders, orderbook depth, news annotations, and catalysts; runs 9
pure detectors (`internal/app/usecase/analytics/`); writes their
verdicts to a single `polymarket_strategy_shadow_decisions` table;
periodically scores their value (CLV uplift, reversal rate); and gates
live alerts behind a triple-locked promotion review. There is NO direct
Telegram from detectors; everything flows through a centralized router
with explicit admin and user surfaces.

## 2. Hard invariants (do not violate)

| Invariant | Where enforced |
|---|---|
| No detector accesses SQL / HTTP / Telegram / OpenAI | `internal/app/usecase/analytics/*` are pure `New(Config) + Decide(Input) Verdict` |
| All strategy decisions go to one table | `polymarket_strategy_shadow_decisions` via `strategybus.Bus.Record` |
| No live promotion without N≥50 firings + uplift criteria | `strategypromotion.Worker.Allow` gate |
| No user-flow Telegram until eligible | `TELEGRAM_STRATEGY_USER_FLOW_ENABLED=false` default + dangerous-defaults guard test |
| No legacy noise surfaces | `staleEnvKeys{}` rejection at boot |
| Detect.Loop never calls external APIs | hot path reads only from staged Postgres readers (bounded + cached) |

A correctly-tuned Watchtower **never increases noise to make a strategy
look better**. Tuning targets are: shadow row volume, skip-reason mix,
CLV distribution, reversal rate, and promotion eligibility timeline.

## 3. End-to-end data flow

```
┌─────────────────────────────────────────────────────────────────────┐
│ External sources (read-only adapters)                              │
├─────────────────────────────────────────────────────────────────────┤
│  Gamma /markets, /events, /tags, /public-search                    │
│  Data API /trades, /holders                                        │
│  CLOB /book, /books, /midpoint, /prices-history                    │
│  WS market channel (book / price_change / last_trade / BBA)        │
│  Polymarket Next.js event-page JSON (annotations)                  │
└─────────────────────────────────────────────────────────────────────┘
           │
           ▼
┌─────────────────────────────────────────────────────────────────────┐
│ Ingest workers (write to canonical tables)                         │
├─────────────────────────────────────────────────────────────────────┤
│  discover/collect ─► polymarket_markets, polymarket_market_outcomes │
│  trades collector ─► polymarket_trades                              │
│  eventpage.Provider ─► polymarket_event_page_snapshots, _annotations│
│  catalyst importer ─► polymarket_event_catalysts                    │
│  realtime worker (WS) ─► ws_events, live_market_state, work_queue   │
│  holdersync ─► polymarket_holder_snapshots                          │
│  bookbars ─► polymarket_book_feature_bars                           │
│  marketlinks ─► polymarket_market_links                             │
│  thesislines ─► polymarket_wallet_thesis_lines                      │
│  walletgraph ─► polymarket_wallet_graph_edges                       │
│  riskscore ─► polymarket_market_risk_scores                         │
│  repricing.Worker ─► polymarket_repricing_windows (open + close)    │
└─────────────────────────────────────────────────────────────────────┘
           │
           ▼
┌─────────────────────────────────────────────────────────────────────┐
│ detect.Loop hot path (per trade + per alert)                        │
├─────────────────────────────────────────────────────────────────────┤
│  Observe(market, trade)                                             │
│   ├── baseline + score + tier                                       │
│   ├── persistAlert() → polymarket_alerts (dedup)                    │
│   └── recordStrategyShadow()                                        │
│        │                                                             │
│        ├── rulesrisk  (pure, no staged input)                       │
│        │                                                             │
│        ├── stagedinputs.Readers (cached, bounded)                   │
│        │     ├── MarketLinksByEvent                                  │
│        │     ├── CatalystsByEvent                                    │
│        │     ├── RiskScoreForCondition                               │
│        │     ├── WalletEdgesForWallet                                │
│        │     ├── ClosedRepricingWindowsForCondition                  │
│        │     ├── RecentDecisionsForCondition                         │
│        │     └── WalletThesisLinesForEvent (v11.10)                  │
│        │                                                             │
│        └── per-strategy hooks (catalystwindow, walletcohort,        │
│              conflictresolve, cheaptail, repricinglag,              │
│              thesisaccum, holderdelta, bookvacuum)                  │
│             └── strategybus.Bus.Record                              │
│                   └── polymarket_strategy_shadow_decisions          │
└─────────────────────────────────────────────────────────────────────┘
           │
           ▼
┌─────────────────────────────────────────────────────────────────────┐
│ Async value + promotion + outcome (background workers)              │
├─────────────────────────────────────────────────────────────────────┤
│  strategyvalue.Worker ─► fills clv_15m/1h/6h/24h on shadow rows     │
│  strategypromotion.Worker ─► polymarket_strategy_promotion_reviews  │
│  strategyoutcome.Worker ─► fills outcome_status                     │
└─────────────────────────────────────────────────────────────────────┘
           │
           ▼ (only when triple-gated)
┌─────────────────────────────────────────────────────────────────────┐
│ Telegram router (admin / user surfaces)                             │
├─────────────────────────────────────────────────────────────────────┤
│  alertsender.Worker reads polymarket_alerts                         │
│  typed router routes by surface → destination → chat_id             │
│  Admin: shadow + skip + health + features                           │
│  User:  promoted + high-confidence + Min level + dedupe             │
│  Old surfaces (stats, market_intel, daily_intel, …) DISABLED        │
└─────────────────────────────────────────────────────────────────────┘
```

## 4. Strategy interaction map (rules of combination)

| Pair | Interaction | Controlled by |
|---|---|---|
| thesisaccum + catalystwindow | Aligned cross-market exposure within catalyst window → strongest informed-flow signal | `CATALYST_WINDOW_MIN_CONFIDENCE`, `THESIS_ACCUM_CATALYST_BOOST_MAX` |
| holderdelta + cheaptail | Cheap-tail staging by a wallet that ALSO becomes a real top holder → higher conviction | `CHEAPTAIL_REQUIRE_CATALYST`, `OWNERSHIP_V2_MIN_PCT_OI_INFO` |
| bookvacuum + accumulation/whale | Depth withdrawal + directional flow → fragile book pre-repricing | `BOOK_VACUUM_MIN_COLLAPSE_PCT`, `BOOK_VACUUM_MIN_SPREAD_Z` |
| repricinglag + catalystwindow/news | Catalyst/news opens window → lag detector decides if target lagged peers | `REPRICING_LAG_MIN_CENTS`, `REPRICING_LAG_PEER_MIN_COUNT`, `CATALYST_WINDOW_*` |
| walletcohort + thesisaccum | Multiple wallets converging on the same linked-market thesis → cohort confirmation | `WALLET_COHORT_MIN_SIMILARITY`, `WALLET_COHORT_MIN_EVENTS` |
| rulesrisk + everything | High ambiguity → caps severity or blocks repricinglag / cheaptail | `RULES_RISK_BLOCK_REPRICING`, `RULES_RISK_BLOCK_CHEAPTAIL`, `RULES_RISK_HIGH_CAP_SEVERITY` |
| conflictresolve | Picks winner across opposing same-condition decisions using quality dominance | `CONFLICT_RESOLVE_MIN_DOMINANCE`, `CONFLICT_RESOLVE_MM_PENALTY` |
| strategyvalue + strategypromotion | Value worker fills CLV; promotion review aggregates and gates live | `STRATEGY_PROMOTION_MIN_SAMPLE`, `STRATEGY_PROMOTION_MIN_SIGNED_MOVE_6H_CENTS` |

## 5. Strategy cards

Each card has: role, business purpose, required data, key params,
how to tune, false positives/negatives, metrics, kill-switch.

### 5.1 Cross-market thesis accumulation (`thesisaccum`)
**Role:** primary detector.
**Purpose:** wallet builds aligned exposure across N linked markets of the same political thesis.
**Data:** `polymarket_market_links`, `polymarket_wallet_thesis_lines`.
**Inputs:** breadth (# linked markets), aligned_usd, opposed_usd, consistency = aligned/(aligned+opposed), link_count.
**Output:** KindStandalone, Level Info/Warning, features = breadth+aligned_usd+opposed_usd+consistency.
**Key params:** `THESIS_ACCUM_MIN_BREADTH`, `THESIS_ACCUM_MIN_CONSISTENCY`, `THESIS_ACCUM_MIN_ALIGNED_SCORE`, `THESIS_ACCUM_LIQUIDITY_FLOOR_USD`.
**Tuning:**
- Raise `MIN_BREADTH` (2→3) → fewer fires; needs 3+ linked markets.
- Raise `MIN_CONSISTENCY` (0.75→0.85) → demands purer aligned exposure; less noise from hedgers.
- Lower `MIN_ALIGNED_SCORE` → fires on smaller wallet exposure (RISK: small-USD wallets dominate counts).
**False positives:** market makers active across multiple correlated markets, indexed positions.
**False negatives:** wallet only present on the source condition; aggregate worker not caught up.
**Metrics:** `strategy_eval_total{strategy="thesisaccum"}`, skip reasons `no_wallet_thesis_lines`/`single_market_line`/`no_market_links_for_event`.
**Kill:** `THESIS_ACCUM_ENABLED=false` or `THESIS_LINES_WORKER_ENABLED=false`.

### 5.2 True holder delta concentration (`holderdelta` / OWNERSHIP_V2)
**Role:** primary detector.
**Purpose:** wallet becomes (or strengthens) top holder of an outcome — real on-chain conviction.
**Data:** Polymarket Data API `/holders?market=` → `polymarket_holder_snapshots`; pct_oi derived as `wallet.amount / SUM(holders.amount)`.
**Inputs:** pct_oi, rank, share_delta, denominator_penalty (OI collapse vs share growth).
**Key params:** `OWNERSHIP_V2_MIN_PCT_OI_INFO` (0.03), `_WARN` (0.08), `_CRIT` (0.15), `OWNERSHIP_V2_TOPK` (5), `HOLDERSYNC_*`.
**Tuning:**
- Raise pct_oi thresholds → fewer alerts on smaller holders.
- Raise `OWNERSHIP_V2_TOPK` → tracks more holders per token (DB + API cost ↑).
- Raise `HOLDERSYNC_INTERVAL` → fewer snapshots per day (cost ↓ but staleness ↑).
**False positives:** Polymarket-side market makers / liquidity provisioning wallets that always rank high.
**False negatives:** wallet outside top-K (especially for low-volume markets).
**Metrics:** `polymarket_holder_snapshots` row growth; `strategy_eval_skipped_total{strategy="holderdelta",reason="wallet_not_holder"}`.
**Kill:** `OWNERSHIP_V2_ENABLED=false` or `HOLDERSYNC_WORKER_ENABLED=false`.

### 5.3 Scheduled catalyst window flow (`catalystwindow`)
**Role:** booster (never standalone).
**Purpose:** boosts a parent signal that fires within the configured catalyst window.
**Data:** `polymarket_event_catalysts` (operator-curated + AI-imported).
**Inputs:** signal time, event catalysts, per-kind WindowSpec, MinConfidence.
**Key params:** `CATALYST_WINDOW_MIN_CONFIDENCE` (0.5), per-kind `_PRE`/`_POST` durations.
**Tuning:**
- Raise `MIN_CONFIDENCE` → fewer marginal catalysts contribute.
- Widen `_PRE`/`_POST` per kind → more signals get boosted.
**False positives:** stale catalysts not marked invalidated.
**False negatives:** no catalyst row for event; catalyst confidence below floor.
**Metrics:** `strategy_eval_total{strategy="catalystwindow"}` ratio against alerts in catalysted events.
**Kill:** `CATALYST_WINDOW_ENABLED=false`.

### 5.4 Liquidity withdrawal / book vacuum (`bookvacuum`)
**Role:** booster.
**Purpose:** detects one-sided depth collapse with mid-shift, indicating informed actors withdrew quotes.
**Data:** `polymarket_book_feature_bars` (CLOB `/book` poller).
**Inputs:** bid_depth_top_n, ask_depth_top_n, spread, spread_z, mid_delta, restore latency.
**Key params:** `BOOK_VACUUM_MIN_COLLAPSE_PCT` (0.5 = ≥50%), `_MIN_SPREAD_Z` (1.5σ), `_MAX_RESTORE_SEC` (30s), `_MIN_MID_SHIFT_PCT` (0.01).
**Tuning:**
- Raise collapse% → demands more dramatic vacuum.
- Lower spread_z → catches more borderline events.
- Lower restore_sec → tighter "vacuum stuck" requirement (lower false-positive on MM oscillation).
**False positives:** MM-like oscillation (rapid collapse+restore); already partially suppressed.
**False negatives:** book bars not populated for this token (cost trade-off via `BOOK_FEATURE_BARS_MAX_MARKETS`).
**Metrics:** `polymarket_book_feature_bars` freshness; `strategy_eval_skipped_total{strategy="bookvacuum"}`.
**Kill:** `BOOK_VACUUM_ENABLED=false` or `BOOK_FEATURE_BARS_ENABLED=false`.

### 5.5 Post-news underreaction / repricing lag (`repricinglag`)
**Role:** primary detector.
**Purpose:** target market lagged its peer median move after a catalyst/annotation.
**Data:** `polymarket_repricing_windows` (open by trigger, close by sampler), `polymarket_market_links`, `polymarket_trades` (price sampler).
**Inputs:** observed_move (target price delta), peer_move (median of linked markets), lag = peer_move - observed.
**Key params:** `REPRICING_LAG_MIN_CENTS` (3), `_PEER_MIN_COUNT` (2), `_MAX_AMBIGUITY` (0.6), `_CHECK_WINDOWS=5m,15m,1h`.
**Tuning:**
- Raise MIN_CENTS → demands bigger lag.
- Raise PEER_MIN_COUNT → more peers required (high-bar reliability).
- Lower MAX_AMBIGUITY → blocks more risky markets.
**False positives:** uncorrelated peers; stale peer prices.
**False negatives:** no peer trades during window → `stale_missing_peers`.
**Metrics:** `polymarket_repricing_windows.status` breakdown (closed_lag_detected vs closed_no_lag vs stale_*).
**Kill:** `REPRICING_LAG_ENABLED=false`.

### 5.6 Wallet cohort / shared-funding convergence (`walletcohort`)
**Role:** booster (Phase A: behavioural co-trade; Phase B funding edges not wired).
**Purpose:** multiple wallets repeatedly co-entering same side across distinct events.
**Data:** `polymarket_wallet_graph_edges` (from `polymarket_trades` co-trade aggregation).
**Inputs:** edges (peers + similarity), convergence on alert event/side.
**Key params:** `WALLET_COHORT_MIN_SIMILARITY` (0.5), `_MIN_EVENTS` (3), `_COTRADE_WINDOW` (30m).
**Tuning:**
- Raise MIN_SIMILARITY → fewer noisy edges.
- Raise MIN_EVENTS → demands more shared history.
**False positives:** random co-occurrence on popular markets.
**False negatives:** edge density still low (125 edges as of v11.10).
**Metrics:** `polymarket_wallet_graph_edges` growth, `strategy_eval_skipped_total{strategy="walletcohort",reason="no_edges_for_wallet"}`.
**Kill:** `WALLET_COHORT_ENABLED=false` or `WALLETGRAPH_ENABLED=false`.

### 5.7 Quality-weighted conflict resolution (`conflictresolve`)
**Role:** arbitration (never standalone).
**Purpose:** resolves opposing decisions on same market using weighted side quality.
**Data:** recent shadow decisions on same condition.
**Inputs:** opposing SideSignal pairs (wallet quality, holder strength, thesis breadth, catalyst proximity, book support, MM penalty).
**Key params:** `CONFLICT_RESOLVE_MIN_DOMINANCE` (1.5), `_MM_PENALTY` (0.4), `_WINDOW` (15m).
**Tuning:**
- Raise MIN_DOMINANCE → fewer arbitration fires (more "unresolved" tags).
- Raise MM_PENALTY → punishes MM-like sides harder.
**False positives:** sparse data — small samples lead to spurious dominance.
**False negatives:** decision rows missing → `no_recent_decisions`.
**Kill:** `CONFLICT_RESOLVE_ENABLED=false`.

### 5.8 Resolution ambiguity / dispute-risk score (`rulesrisk`)
**Role:** safety layer (caps/blocks; never alpha).
**Purpose:** deterministic lexical scoring of market resolution ambiguity.
**Data:** market `Question` text + Gamma `description` + catalyst kind hint.
**Inputs:** keyword matches (runoff, certification, court, by-date, definitive…).
**Output:** `KindTag` with score = ambiguity 0..1.
**Key params:** `RULES_RISK_HIGH_THRESHOLD` (0.6), `_BLOCK_REPRICING` (true), `_BLOCK_CHEAPTAIL` (true), `_HIGH_CAP_SEVERITY` (warning).
**Tuning:**
- Raise HIGH_THRESHOLD → fewer markets blocked.
- Disable BLOCK_* → strategies fire on ambiguous markets (RISKY — UMA dispute exposure).
**False positives:** markets with ambiguous wording but actually well-defined.
**False negatives:** non-English ambiguity, ambiguity in catalyst not market wording.
**Kill:** `RULES_RISK_ENABLED=false`.

### 5.9 Cheap-tail catalyst staging (`cheaptail`)
**Role:** primary detector.
**Purpose:** wallet stages non-dust cheap-tail position before a catalyst — informed lottery ticket.
**Data:** trade price (cheap band) + catalysts + risk score.
**Key params:** `CHEAPTAIL_MIN_PROB` (0.02), `_MAX_PROB` (0.15), `_MIN_NOTIONAL_USD` (1000), `_MIN_TRADES` (2), `_REQUIRE_CATALYST` (true), `_AMBIGUITY_CUTOFF` (0.7).
**Tuning:**
- Tighten band (e.g. 0.03..0.10) → fewer dust signals.
- Raise MIN_NOTIONAL_USD → reject lottery-ticket dust.
- Toggle REQUIRE_CATALYST → without catalyst the strategy becomes pure tail-spam detector.
**False positives:** retail lottery wagers (dust).
**False negatives:** cheap trades on markets without staged catalyst.
**Kill:** `CHEAPTAIL_ENABLED=false`.

## 6. Tuning methodology (5 phases)

### Phase 1 — Observation (Day 0 → Day 3)
- All strategies enabled, `*_SHADOW_ONLY=true`.
- `STRATEGY_LEARNING_LOOP_PROMOTION_ALLOWED=false`.
- `TELEGRAM_STRATEGY_USER_FLOW_ENABLED=false`.
- `STRATEGY_SHADOW_RECORD_NOFIRE=false`.
- Watch:
  - `SELECT strategy_name, COUNT(*) FROM polymarket_strategy_shadow_decisions WHERE fired_at > NOW() - INTERVAL '24 hours' GROUP BY 1 ORDER BY 2 DESC;`
  - `SELECT strategy, reason, COUNT(*) FROM watchtower_strategy_eval_skipped_total ...` (via Prometheus)
- Stop conditions to advance:
  - At least 3 strategies have shadow rows
  - Skip reasons make sense
  - No worker errors > 1% of attempts

### Phase 2 — Noise reduction (Day 3 → Day 7)
- Adjust thresholds based on skip-reason distribution.
- If `no_market_links_for_event` dominates → check `MARKETLINKS_INTERVAL` / `MARKETLINKS_BATCH_SIZE`.
- If admin alert volume > 100/h per strategy → raise thresholds.
- Tune `STRATEGY_STAGED_CACHE_TTL` (60s default; can go to 30s if memory headroom).

### Phase 3 — Signal quality (Day 7 → Day 14)
- Let `strategyvalue.Worker` populate CLV columns.
- Query CLV uplift per strategy:
  ```sql
  SELECT strategy_name, COUNT(*) FILTER (WHERE clv_6h IS NOT NULL) AS evaluated,
         percentile_cont(0.5) WITHIN GROUP (ORDER BY clv_6h) AS median_6h
  FROM polymarket_strategy_shadow_decisions
  WHERE fired_at >= NOW() - INTERVAL '7 days'
  GROUP BY 1 ORDER BY median_6h DESC NULLS LAST;
  ```
- Compare to control buckets (`control_bucket_key`).

### Phase 4 — Promotion eligibility review (Day 14 → Day 30)
- Strategies with `sample_size ≥ 50`, `median_signed_move_6h ≥ 1.5c`, `reversal ≤ 0.5`, `alerts/day ≤ 40`.
- Eligibility evaluated by `strategypromotion.Worker` automatically.
- Operator confirms before flipping `STRATEGY_LEARNING_LOOP_PROMOTION_ALLOWED=true`.

### Phase 5 — User-flow rollout (per-strategy)
- For each promoted strategy:
  1. Set `<STRATEGY>_SHADOW_ONLY=false`.
  2. Verify `TELEGRAM_STRATEGY_USER_FLOW_ENABLED=true` + `MIN_USER_CONFIDENCE` set.
  3. Monitor user-flow alert rate for 48h.
  4. Roll back via `<STRATEGY>_SHADOW_ONLY=true` if noise > acceptable.

## 7. What this document deliberately does NOT promise

- That any strategy is profitable — only that the architecture supports calibration.
- That tuning ratios documented above transfer across market regimes — they are starting points.
- That user-flow Telegram will be quiet — promotion gates exist precisely because shadow data must prove this first.
- That all 9 strategies will reach promotion — some may stay shadow-only forever if their signal is weak.
