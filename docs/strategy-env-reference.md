# Watchtower Strategy ENV Reference

Source of truth for every active strategy/worker env key as of v11.10.

This document is **derived from**:
- `internal/app/strategy_config.go` (env tag + envDefault)
- `internal/app/config.go` (root-level env wiring + `staleEnvKeys{}`)
- `.env.example` (operator-facing baseline)
- detector packages in `internal/app/usecase/analytics/`

Every key listed below is read by code today. Keys that appear in old
docs but are absent here are either renamed or `staleEnvKeys`-rejected
(boot fails loud if set).

## How to read this document

| Column | Meaning |
|---|---|
| `KEY` | Env variable name |
| `DEFAULT` | Default from `envDefault:` tag |
| `TYPE` | Go type / parser (bool, int, float64, string, time.Duration) |
| `USED IN` | Package or worker that reads this value |
| `SAFE RANGE` | Operator-tunable range with no architectural change |
| `DANGEROUS` | Values that violate Watchtower safety invariants |
| `EFFECT ↑` | What raising the value does |
| `EFFECT ↓` | What lowering the value does |
| `WATCH` | Metric / table to monitor when tuning |

## 0. Global strategy gates (load-bearing — handle with care)

| KEY | DEFAULT | TYPE | USED IN | SAFE | DANGEROUS | EFFECT ↑ | EFFECT ↓ | WATCH |
|---|---|---|---|---|---|---|---|---|
| `STRATEGY_LEARNING_LOOP_VERSION` | `v11.5-shadow` | string | `strategybus.Bus` writes this on every shadow row | bump only when scoring formula changes | renaming mid-run resets promotion sample window | n/a | n/a | `polymarket_strategy_shadow_decisions.strategy_version` |
| `STRATEGY_LEARNING_LOOP_PROMOTION_ALLOWED` | `false` | bool | `strategybus.Bus.Record` → forces ShadowOnly=true when false | **`false` while N<50 firings per strategy** | `true` without a passing promotion review = unvalidated live alerts | enables live promotion path | locks all strategies into shadow | `watchtower_strategy_shadow_decisions_total{shadow_only}` |
| `STRATEGY_PROMOTION_BYPASS_EXPLICIT` | `false` | bool | `strategypromotion.Worker.Allow` returns false when true | always `false` | `true` = gate returns false for everything (kill-switch); also blocks legitimate promotion | kills promotion regardless of state | n/a | `watchtower_strategy_promotion_reviews_total{eligible}` |
| `STRATEGY_SHADOW_MAX_DECISIONS_PER_TRADE` | `20` | int | `detect.Loop.recordStrategyShadow` budget | 10..30 | 0 disables shadow writes; 100+ row blowup | more strategies record per alert | fewer recordings per alert | `watchtower_strategy_eval_total` |
| `STRATEGY_SHADOW_RECORD_NOFIRE` | `false` | bool | `recordStrategyShadow` writes structural Tag even when no fire | `false` during burn-in; `true` only for calibration weeks | `true` in production = row-volume blowup | adds no-fire audit rows | shadow rows only on real fires | row count delta on `polymarket_strategy_shadow_decisions` |

## 1. Promotion review

`strategypromotion.Worker` evaluates eligibility every `Interval` over `Lookback`.

| KEY | DEFAULT | TYPE | EFFECT ↑ | EFFECT ↓ | WATCH |
|---|---|---|---|---|---|
| `STRATEGY_PROMOTION_REVIEW_INTERVAL` | `1h` | duration | slower review cycle, less DB pressure | faster review cycle | `strategypromotion` worker latency |
| `STRATEGY_PROMOTION_REVIEW_LOOKBACK` | `336h` (14d) | duration | wider sample, slower to react to recent calibration | narrower sample, may miss old patterns | `sample_size` column |
| `STRATEGY_PROMOTION_MIN_SAMPLE` | `50` | int | stricter — fewer eligible strategies | looser — promotion possible on small samples (RISKY) | `eligible` column |
| `STRATEGY_PROMOTION_MIN_SIGNED_MOVE_6H_CENTS` | `1.5` | float | demands higher uplift before promotion | accepts lower uplift (more spurious wins) | `median_signed_move_6h` |
| `STRATEGY_PROMOTION_MAX_REVERSAL_15M_RATIO` | `0.5` | float | tolerates more reversals | tighter quality | `reversal_15m_ratio` |
| `STRATEGY_PROMOTION_MAX_ALERTS_PER_DAY` | `40` | float | wider alert budget | tighter budget — promotes only sparse-but-good strategies | `alerts_per_day` |

## 2. Staged inputs (detect.Loop hot path)

`stagedinputs.Readers` are the bounded Postgres readers detect.Loop fans out to.

| KEY | DEFAULT | TYPE | EFFECT ↑ | EFFECT ↓ | WATCH |
|---|---|---|---|---|---|
| `STRATEGY_STAGED_INPUTS_ENABLED` | `true` | bool | n/a | false = all non-rulesrisk strategies skip with `staged_inputs_disabled` | `watchtower_strategy_eval_skipped_total` |
| `STRATEGY_STAGED_CACHE_ENABLED` | `true` | bool | enables TTL cache | n/a (per-trade DB amplification if false) | DB query rate |
| `STRATEGY_STAGED_CACHE_TTL` | `60s` | duration | fresher reads, higher load | staler reads, lower load | strategy_eval skip reasons |
| `STRATEGY_STAGED_MAX_QUERY_ROWS` | `200` | int | richer context, slower queries | tighter context, faster | per-query latency |
| `STRATEGY_STAGED_QUERY_TIMEOUT` | `250ms` | duration | tolerates slow DB | aggressive timeouts | `reader_error` skip count |

## 3. Per-strategy gates

Every strategy has `*_ENABLED` and (where applicable) `*_SHADOW_ONLY`. All default to `enabled=true, shadow_only=true`.

### 3.1 thesisaccum (`internal/app/usecase/analytics/thesisaccum`)

| KEY | DEFAULT | TYPE | EFFECT ↑ | EFFECT ↓ |
|---|---|---|---|---|
| `THESIS_ACCUM_ENABLED` | `true` | bool | n/a | strategy off |
| `THESIS_ACCUM_SHADOW_ONLY` | `true` | bool | shadow-only | unlocks promotion path (still triple-gated) |
| `THESIS_ACCUM_LOOKBACK_RECENT` | `72h` | duration | wider recent-conviction window | narrower window |
| `THESIS_ACCUM_LOOKBACK_LIFETIME` | `8760h` (1y) | duration | wider lifetime window | narrower |
| `THESIS_ACCUM_MIN_BREADTH` | `2` | int | requires more linked markets — fewer fires | accepts single-market lines (RISKY) |
| `THESIS_ACCUM_MIN_CONSISTENCY` | `0.75` | float | demands more aligned-vs-opposed exposure | accepts mixed-direction lines |
| `THESIS_ACCUM_MIN_ALIGNED_SCORE` | `1.5` | float | higher aligned-USD floor | lower floor — more fires |
| `THESIS_ACCUM_CATALYST_BOOST_MAX` | `0.4` | float | catalyst proximity adds up to +0.4 to score | smaller catalyst contribution |
| `THESIS_ACCUM_LIQUIDITY_FLOOR_USD` | `500` | float | requires more market liquidity to count | tolerates illiquid markets |
| `THESIS_ACCUM_MAX_LINKED_MARKETS` | `32` | int | larger graph evaluation | smaller graph |

### 3.2 holderdelta / OWNERSHIP_V2 (`internal/app/usecase/analytics/holderdelta`)

| KEY | DEFAULT | TYPE | EFFECT ↑ | EFFECT ↓ |
|---|---|---|---|---|
| `OWNERSHIP_V2_ENABLED` | `true` | bool | n/a | strategy off |
| `OWNERSHIP_V2_SHADOW_ONLY` | `true` | bool | shadow-only | unlocks promotion path |
| `OWNERSHIP_V2_MIN_PCT_OI_INFO` | `0.03` | float | demands ≥3% OI — fewer fires | lower bar |
| `OWNERSHIP_V2_MIN_PCT_OI_WARN` | `0.08` | float | demands ≥8% for warning | lower bar |
| `OWNERSHIP_V2_MIN_PCT_OI_CRIT` | `0.15` | float | demands ≥15% for critical | lower bar |
| `OWNERSHIP_V2_TOPK` | `5` | int | tracks top-5 holders | tracks fewer |
| `OWNERSHIP_V2_MIN_SHARES_DELTA` | `500` | float | requires more delta to fire | accepts smaller deltas |
| `OWNERSHIP_V2_FRESH_SNAPSHOT_MAX_AGE` | `2h` | duration | tolerates older snapshots | demands fresher snapshots |
| `OWNERSHIP_V2_DENOMINATOR_PENALTY_OI` | `0.3` | float | penalises OI collapses harder | less penalty |
| `OWNERSHIP_V1_APPROX_ENABLED` | `true` | bool | keeps legacy approximate ownership for comparison | disables v1 |

### 3.3 catalystwindow (`internal/app/usecase/analytics/catalystwindow`)

| KEY | DEFAULT | TYPE | EFFECT |
|---|---|---|---|
| `CATALYST_WINDOW_ENABLED` | `true` | bool | toggle |
| `CATALYST_WINDOW_SHADOW_ONLY` | `true` | bool | shadow-only |
| `CATALYST_WINDOW_MIN_CONFIDENCE` | `0.5` | float | demands higher catalyst confidence to count |
| `CATALYST_WINDOW_DEBATE_PRE` | `12h` | duration | debate-kind: window before debate |
| `CATALYST_WINDOW_DEBATE_POST` | `4h` | duration | debate-kind: window after |
| `CATALYST_WINDOW_COURT_RULING_PRE` | `24h` | duration | court ruling pre |
| `CATALYST_WINDOW_COURT_RULING_POST` | `12h` | duration | court ruling post |
| `CATALYST_WINDOW_ELECTION_DAY_PRE` | `72h` | duration | election day pre |
| `CATALYST_WINDOW_ELECTION_DAY_POST` | `24h` | duration | election day post |
| `CATALYST_WINDOW_OFFICIAL_STATEMENT_PRE` | `4h` | duration | official statement pre |
| `CATALYST_WINDOW_OFFICIAL_STATEMENT_POST` | `2h` | duration | official statement post |
| `CATALYST_WINDOW_GENERIC_PRE` | (code default 6h) | duration | unknown-kind catalyst pre |
| `CATALYST_WINDOW_GENERIC_POST` | (code default 3h) | duration | unknown-kind catalyst post |

### 3.4 bookvacuum (`internal/app/usecase/analytics/bookvacuum`)

| KEY | DEFAULT | TYPE | EFFECT |
|---|---|---|---|
| `BOOK_VACUUM_ENABLED` | `true` | bool | toggle |
| `BOOK_VACUUM_SHADOW_ONLY` | `true` | bool | shadow-only |
| `BOOK_VACUUM_TOPN` | `5` | int | depth aggregation window |
| `BOOK_VACUUM_MIN_COLLAPSE_PCT` | `0.5` | float | demands ≥50% top-N depth disappearance |
| `BOOK_VACUUM_MAX_RESTORE_SEC` | `30s` | duration | if depth returns within X, suppress |
| `BOOK_VACUUM_MIN_SPREAD_Z` | `1.5` | float | demands spread spike vs baseline |
| `BOOK_VACUUM_MIN_MID_SHIFT_PCT` | `0.01` | float | demands mid shift toward missing side |
| `BOOK_VACUUM_MAX_AGE_BAR` | `5m` | duration | reject stale bars older than X |

### 3.5 repricinglag (`internal/app/usecase/analytics/repricinglag`)

| KEY | DEFAULT | TYPE | EFFECT |
|---|---|---|---|
| `REPRICING_LAG_ENABLED` | `true` | bool | toggle |
| `REPRICING_LAG_SHADOW_ONLY` | `true` | bool | shadow-only |
| `REPRICING_LAG_MIN_CENTS` | `3` | float | minimum lag (cents) to fire |
| `REPRICING_LAG_PEER_MIN_COUNT` | `2` | int | demands ≥N peer prices |
| `REPRICING_LAG_CHECK_WINDOWS` | `5m,15m,1h` | string CSV | check horizons |
| `REPRICING_LAG_MAX_AMBIGUITY` | `0.6` | float | rulesrisk above blocks lag fire |
| `REPRICING_LAG_OPEN_INTERVAL` | `30s` | duration | how often the lag detector polls open windows |
| `REPRICING_LAG_CLOSE_GRACE` | `2m` | duration | grace for window close |

### 3.6 walletcohort (`internal/app/usecase/analytics/walletcohort`)

| KEY | DEFAULT | TYPE | EFFECT |
|---|---|---|---|
| `WALLET_COHORT_ENABLED` | `true` | bool | toggle |
| `WALLET_COHORT_SHADOW_ONLY` | `true` | bool | shadow-only |
| `WALLET_COHORT_MIN_SIMILARITY` | `0.5` | float | edge similarity floor |
| `WALLET_COHORT_MIN_EVENTS` | `3` | int | shared events floor |
| `WALLET_COHORT_COTRADE_WINDOW` | `30m` | duration | co-trade window |
| `WALLET_COHORT_USE_FUNDING_EDGES` | `false` | bool | Phase B (no funding provider wrapped) |
| `WALLET_COHORT_CONVERGENCE_WINDOW` | `4h` | duration | convergence event window |
| `WALLET_COHORT_MIN_COHORT_HITS` | `2` | int | minimum cohort members |

### 3.7 conflictresolve (`internal/app/usecase/analytics/conflictresolve`)

| KEY | DEFAULT | TYPE | EFFECT |
|---|---|---|---|
| `CONFLICT_RESOLVE_ENABLED` | `true` | bool | toggle |
| `CONFLICT_RESOLVE_SHADOW_ONLY` | `true` | bool | shadow-only |
| `CONFLICT_RESOLVE_WINDOW` | `15m` | duration | conflict observation window |
| `CONFLICT_RESOLVE_MIN_DOMINANCE` | `1.5` | float | dominance ratio for keep-winner |
| `CONFLICT_RESOLVE_MM_PENALTY` | `0.4` | float | MM-side score penalty |
| `CONFLICT_RESOLVE_MIN_QUALITY_SUM` | `1.0` | float | sides below skip |

### 3.8 rulesrisk (`internal/app/usecase/analytics/rulesrisk`)

Safety layer — caps/blocks risky signals. NOT standalone alpha.

| KEY | DEFAULT | TYPE | EFFECT |
|---|---|---|---|
| `RULES_RISK_ENABLED` | `true` | bool | toggle |
| `RULES_RISK_SHADOW_ONLY` | `true` | bool | always shadow (Tag) |
| `RULES_RISK_HIGH_THRESHOLD` | `0.6` | float | above → high ambiguity (cap severity / block) |
| `RULES_RISK_MID_THRESHOLD` | `0.3` | float | mid risk threshold |
| `RULES_RISK_HIGH_CAP_SEVERITY` | `warning` | string | caps other strategies at this severity when high |
| `RULES_RISK_BLOCK_REPRICING` | `true` | bool | high → block repricinglag |
| `RULES_RISK_BLOCK_CHEAPTAIL` | `true` | bool | high → block cheaptail |

### 3.9 cheaptail (`internal/app/usecase/analytics/cheaptail`)

| KEY | DEFAULT | TYPE | EFFECT |
|---|---|---|---|
| `CHEAPTAIL_ENABLED` | `true` | bool | toggle |
| `CHEAPTAIL_SHADOW_ONLY` | `true` | bool | shadow-only |
| `CHEAPTAIL_MIN_PROB` | `0.02` | float | minimum tail probability (2¢) |
| `CHEAPTAIL_MAX_PROB` | `0.15` | float | maximum tail probability (15¢) |
| `CHEAPTAIL_MIN_NOTIONAL_USD` | (code default 1000) | float | non-dust floor |
| `CHEAPTAIL_MIN_TRADES` | `2` | int | minimum staging trades |
| `CHEAPTAIL_REQUIRE_CATALYST` | `true` | bool | require active/near catalyst |
| `CHEAPTAIL_AMBIGUITY_CUTOFF` | (code default 0.7) | float | rulesrisk above blocks |

## 4. Workers (background producers)

### 4.1 marketlinks

| KEY | DEFAULT | TYPE | EFFECT |
|---|---|---|---|
| `MARKETLINKS_ENABLED` | `false`* | bool | *worker enable; v11.10 set true in `.env.example` |
| `MARKETLINKS_INTERVAL` | `30m` | duration | rebuild graph cadence |
| `MARKETLINKS_BATCH_SIZE` | `100` | int | events per cycle |
| `MARKETLINKS_LINK_VERSION` | `1` | int | bump to rebuild graph from scratch |
| `MARKETLINKS_INCLUDE_OPPOSED` | `true` | bool | include explicit mirror outcomes |
| `MARKETLINKS_MIN_CONFIDENCE` | `0.3` | float | floor for persisting edge |

### 4.2 holdersync (v11.10 — live `/holders`)

| KEY | DEFAULT | TYPE | EFFECT |
|---|---|---|---|
| `HOLDERSYNC_WORKER_ENABLED` | `true` | bool | worker toggle |
| `HOLDERSYNC_SOURCE_MODE` | `dataapi` | string | `dataapi` / `disabled` |
| `HOLDERSYNC_INTERVAL` | `10m` | duration | poll cadence |
| `HOLDERSYNC_MAX_MARKETS` | `250` | int | tokens per tick (cost!) |
| `HOLDERSYNC_TOPK` | `25` | int | top-K holders per token |
| `HOLDERSYNC_PER_MARKET_TIMEOUT` | `5s` | duration | per-HTTP timeout |
| `HOLDERSYNC_RATE_LIMIT_RPS` | `2` | float | upstream throttle |
| `HOLDERSYNC_REQUIRE_OPEN_INTEREST` | `true` | bool | refuses to write rows when OI=0 (no fake denominator) |

### 4.3 riskscore

| KEY | DEFAULT | TYPE | EFFECT |
|---|---|---|---|
| `RISKSCORE_ENABLED` | `false`* | bool | *set true in production |
| `RISKSCORE_INTERVAL` | `15m` | duration | refresh cadence |
| `RISKSCORE_BATCH_SIZE` | `100` | int | markets per cycle |
| `RISKSCORE_VERSION` | `1` | int | bump on rule set change |
| `RISKSCORE_REFRESH_OLDER_THAN` | `24h` | duration | re-score after staleness |

### 4.4 repricing

| KEY | DEFAULT | TYPE | EFFECT |
|---|---|---|---|
| `REPRICING_WORKER_ENABLED` | `false`* | bool | *set true in prod |
| `REPRICING_WORKER_INTERVAL` | `60s` | duration | open/close cycle |
| `REPRICING_WORKER_OPEN_LOOKBACK` | `15m` | duration | how far back to open windows |
| `REPRICING_WORKER_MAX_OPEN_WINDOWS` | `500` | int | concurrent open windows |
| `REPRICING_WORKER_CLOSE_AFTER` | `2h` | duration | auto-close grace |
| `REPRICING_CLOSE_ENABLED` | `true` | bool | enables real close-phase sampler |
| `REPRICING_MIN_PEER_COUNT` | `2` | int | required peer prices to compute median |
| `REPRICING_MIN_LAG_CENTS` | `3` | float | lag threshold for `closed_lag_detected` |
| `REPRICING_PRICE_SOURCE` | `trades` | string | `trades` / `snapshots` / `auto` |

### 4.5 walletgraph

| KEY | DEFAULT | TYPE | EFFECT |
|---|---|---|---|
| `WALLETGRAPH_ENABLED` | `false`* | bool | *set true in prod |
| `WALLETGRAPH_INTERVAL` | `1h` | duration | graph rebuild |
| `WALLETGRAPH_COTRADE_WINDOW` | `30m` | duration | trade-clustering window |
| `WALLETGRAPH_MIN_SHARED_EVENTS` | `3` | int | edges below skipped |
| `WALLETGRAPH_BATCH_SIZE` | `5000` | int | trade rows per cycle |
| `WALLETGRAPH_EDGE_VERSION` | `1` | int | bump to reset |
| `WALLETGRAPH_USE_FUNDING_PROVIDER` | `false` | bool | Phase B |

### 4.6 thesislines (v11.9)

| KEY | DEFAULT | TYPE | EFFECT |
|---|---|---|---|
| `THESIS_LINES_WORKER_ENABLED` | `true` | bool | aggregate matrix worker |
| `THESIS_LINES_LOOKBACK` | `720h` (30d) | duration | aggregate window |
| `THESIS_LINES_INTERVAL` | `10m` | duration | refresh cadence |
| `THESIS_LINES_MAX_EVENTS` | `500` | int | events per cycle |
| `THESIS_LINES_MAX_WALLETS` | `10000` | int | wallets per cycle |
| `THESIS_HOTPATH_MAX_LINKED_MARKETS` | `25` | int | hot-path bound |
| `THESIS_HOTPATH_QUERY_TIMEOUT` | `250ms` | duration | hot-path query cap |

### 4.7 bookbars (v11.10 — CLOB `/books`)

| KEY | DEFAULT | TYPE | EFFECT |
|---|---|---|---|
| `BOOK_FEATURE_BARS_ENABLED` | `true` | bool | producer toggle |
| `BOOK_FEATURE_BARS_INTERVAL` | `5s` | duration | poll cadence (AGGRESSIVE — see tuning guide) |
| `BOOK_FEATURE_BARS_TOPN` | `5` | int | top-N depth aggregation |
| `BOOK_FEATURE_BARS_MAX_MARKETS` | `250` | int | tokens per tick |
| `BOOK_FEATURE_BARS_REQUIRE_DEPTH_FOR_VACUUM` | `true` | bool | refuse to fire vacuum without depth |
| `BOOK_FEATURE_BARS_RETENTION` | `720h` | duration | row retention |

### 4.8 outcome backfill

| KEY | DEFAULT | TYPE | EFFECT |
|---|---|---|---|
| `STRATEGY_OUTCOME_EVALUATOR_ENABLED` | `true` | bool | worker toggle |
| `STRATEGY_OUTCOME_EVALUATOR_INTERVAL` | `1h` | duration | cycle |
| `STRATEGY_OUTCOME_EVALUATOR_BATCH_SIZE` | `1000` | int | per cycle |
| `STRATEGY_OUTCOME_STANDALONE_ENABLED` | `true` | bool | resolves closed-market non-linked rows |
| `STRATEGY_OUTCOME_STANDALONE_BATCH_SIZE` | `1000` | int | per cycle |
| `STRATEGY_OUTCOME_STANDALONE_RESOLVE_SIDE` | `true` | bool | resolves correct/wrong via Gamma `winning_outcome_label` |

## 5. Telegram strategy flow (v11.10)

| KEY | DEFAULT | TYPE | EFFECT |
|---|---|---|---|
| `TELEGRAM_STRATEGY_ADMIN_FLOW_ENABLED` | `true` | bool | admin flow toggle |
| `TELEGRAM_STRATEGY_USER_FLOW_ENABLED` | `false` | bool | **MUST stay false until promotion eligible per strategy** |
| `TELEGRAM_STRATEGY_SHADOW_TO_ADMIN` | `true` | bool | shadow rows surface to admin |
| `TELEGRAM_STRATEGY_PROMOTED_TO_USER` | `true` | bool | promoted decisions reach user (gated by other flags) |
| `TELEGRAM_STRATEGY_MIN_USER_CONFIDENCE` | `0.75` | float | confidence floor for user flow |
| `TELEGRAM_STRATEGY_MIN_USER_LEVEL` | `warning` | string | level floor (warning / critical / hard) |
| `TELEGRAM_STRATEGY_USER_DEDUPE_WINDOW` | `12h` | duration | user dedupe window |
| `TELEGRAM_STRATEGY_ADMIN_DEDUPE_WINDOW` | `1h` | duration | admin dedupe window |

## 6. Legacy disabled surfaces (must remain false)

Pinned by `TestEnvFiles_DangerousDefaultsBlocked` + `TestLegacyTelegramSurfaces_StayDisabledByDefault` + `staleEnvKeys{}`.

| KEY | Expected value | Risk if true |
|---|---|---|
| `WATCHTOWER_STATS_TELEGRAM_ENABLED` | `false` | reintroduces every-2h stats spam |
| `TELEGRAM_STATS_ENABLED` | `false` | same |
| `PREDICTION_UPDATE_TELEGRAM_ENABLED` | `false` | prediction blocked alerts |
| `PREDICTION_BLOCKED_TELEGRAM_ENABLED` | `false` | same |
| `PREDICTION_STATE_TRANSITION_TELEGRAM_ENABLED` | `false` | same |
| `MARKET_INTEL_ENABLED` | n/a (stale-rejected) | boot fails on set |
| `DAILY_INTEL_ENABLED` | n/a (stale-rejected) | boot fails on set |
| `DAILY_POLITICAL_INTEL_ENABLED` | n/a (stale-rejected) | boot fails on set |
| `MARKET_PREDICTION_CREATION_ENABLED` | n/a (stale-rejected) | boot fails on set |
| `MARKET_PREDICTION_EVOLUTION_ENABLED` | n/a (stale-rejected) | boot fails on set |

## 7. Things NOT controlled by these env keys

- **Watchtower's existing alert pipeline** (`polymarket_alerts → alertsender.Worker`) is governed by AlertSender + Detection + AnomalyConfig blocks documented in `internal/app/config.go`. Tuning that pipeline is OUT OF SCOPE for this document.
- **AI/OpenAI calls** in non-strategy workflows (alert AI analysis, catalyst importer, prediction evolution) are governed by `AI_*` + `OPENAI_*` env keys. Strategy hot path NEVER calls OpenAI.
- **Telegram delivery itself** (router, dedupe, recipient routing) is governed by v11.3 typed Telegram router (`internal/infra/telegram/router.go`).
