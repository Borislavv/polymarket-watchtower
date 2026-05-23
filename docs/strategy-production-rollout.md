# Watchtower Strategy Production Rollout Profiles

Three reference profiles for `.env`. None contains secrets. None
unlocks the triple-lock by itself.

## Profile A — Production-Safe (recommended starting point)

Goal: all 9 strategies enabled, shadow-only, admin-visible, user-flow
silent until per-strategy promotion eligibility is proven.

```bash
# === Strategy core ===
STRATEGY_LEARNING_LOOP_VERSION=v11.5-shadow
STRATEGY_LEARNING_LOOP_PROMOTION_ALLOWED=false  # MUST be false until eligible
STRATEGY_PROMOTION_BYPASS_EXPLICIT=false        # MUST be false
STRATEGY_SHADOW_MAX_DECISIONS_PER_TRADE=20
STRATEGY_SHADOW_RECORD_NOFIRE=false             # row volume guard

# === All 9 strategies enabled, shadow-only ===
THESIS_ACCUM_ENABLED=true
THESIS_ACCUM_SHADOW_ONLY=true
OWNERSHIP_V2_ENABLED=true
OWNERSHIP_V2_SHADOW_ONLY=true
CATALYST_WINDOW_ENABLED=true
CATALYST_WINDOW_SHADOW_ONLY=true
BOOK_VACUUM_ENABLED=true
BOOK_VACUUM_SHADOW_ONLY=true
REPRICING_LAG_ENABLED=true
REPRICING_LAG_SHADOW_ONLY=true
WALLET_COHORT_ENABLED=true
WALLET_COHORT_SHADOW_ONLY=true
CONFLICT_RESOLVE_ENABLED=true
CONFLICT_RESOLVE_SHADOW_ONLY=true
RULES_RISK_ENABLED=true
CHEAPTAIL_ENABLED=true
CHEAPTAIL_SHADOW_ONLY=true

# === Promotion review (defaults match CLAUDE.md targets) ===
STRATEGY_PROMOTION_REVIEW_INTERVAL=1h
STRATEGY_PROMOTION_REVIEW_LOOKBACK=336h         # 14d window
STRATEGY_PROMOTION_MIN_SAMPLE=50
STRATEGY_PROMOTION_MIN_SIGNED_MOVE_6H_CENTS=1.5
STRATEGY_PROMOTION_MAX_REVERSAL_15M_RATIO=0.5
STRATEGY_PROMOTION_MAX_ALERTS_PER_DAY=40

# === Staged inputs (hot path) ===
STRATEGY_STAGED_INPUTS_ENABLED=true
STRATEGY_STAGED_CACHE_ENABLED=true
STRATEGY_STAGED_CACHE_TTL=60s
STRATEGY_STAGED_MAX_QUERY_ROWS=200
STRATEGY_STAGED_QUERY_TIMEOUT=250ms

# === Workers ===
MARKETLINKS_ENABLED=true
MARKETLINKS_INTERVAL=30m
MARKETLINKS_BATCH_SIZE=100
MARKETLINKS_MIN_CONFIDENCE=0.3

# Holdersync — Polymarket /holders. Be polite.
HOLDERSYNC_WORKER_ENABLED=true
HOLDERSYNC_SOURCE_MODE=dataapi
HOLDERSYNC_INTERVAL=10m
HOLDERSYNC_MAX_MARKETS=100               # start conservative; raise if cost OK
HOLDERSYNC_TOPK=25
HOLDERSYNC_PER_MARKET_TIMEOUT=5s
HOLDERSYNC_RATE_LIMIT_RPS=2
HOLDERSYNC_REQUIRE_OPEN_INTEREST=true

# Riskscore
RISKSCORE_ENABLED=true
RISKSCORE_INTERVAL=15m
RISKSCORE_BATCH_SIZE=100
RISKSCORE_VERSION=1
RISKSCORE_REFRESH_OLDER_THAN=24h

# Repricing
REPRICING_WORKER_ENABLED=true
REPRICING_WORKER_INTERVAL=60s
REPRICING_WORKER_OPEN_LOOKBACK=15m
REPRICING_WORKER_MAX_OPEN_WINDOWS=500
REPRICING_WORKER_CLOSE_AFTER=2h
REPRICING_CLOSE_ENABLED=true
REPRICING_MIN_PEER_COUNT=2
REPRICING_MIN_LAG_CENTS=3
REPRICING_PRICE_SOURCE=trades

# Walletgraph
WALLETGRAPH_ENABLED=true
WALLETGRAPH_INTERVAL=1h
WALLETGRAPH_COTRADE_WINDOW=30m
WALLETGRAPH_MIN_SHARED_EVENTS=3
WALLETGRAPH_BATCH_SIZE=5000

# Thesis lines
THESIS_LINES_WORKER_ENABLED=true
THESIS_LINES_LOOKBACK=720h               # 30d
THESIS_LINES_INTERVAL=10m
THESIS_LINES_MAX_EVENTS=500
THESIS_LINES_MAX_WALLETS=10000
THESIS_HOTPATH_MAX_LINKED_MARKETS=25
THESIS_HOTPATH_QUERY_TIMEOUT=250ms

# Bookbars — CLOB /book. THIS IS THE COST HOTSPOT.
BOOK_FEATURE_BARS_ENABLED=true
BOOK_FEATURE_BARS_INTERVAL=15s           # safer than 5s default
BOOK_FEATURE_BARS_TOPN=5
BOOK_FEATURE_BARS_MAX_MARKETS=100        # start conservative
BOOK_FEATURE_BARS_REQUIRE_DEPTH_FOR_VACUUM=true
BOOK_FEATURE_BARS_RETENTION=720h

# Value + outcome
STRATEGY_OUTCOME_EVALUATOR_ENABLED=true
STRATEGY_OUTCOME_EVALUATOR_INTERVAL=1h
STRATEGY_OUTCOME_EVALUATOR_BATCH_SIZE=1000
STRATEGY_OUTCOME_STANDALONE_ENABLED=true
STRATEGY_OUTCOME_STANDALONE_BATCH_SIZE=1000
STRATEGY_OUTCOME_STANDALONE_RESOLVE_SIDE=true

# === Telegram strategy flow ===
TELEGRAM_STRATEGY_ADMIN_FLOW_ENABLED=true
TELEGRAM_STRATEGY_USER_FLOW_ENABLED=false       # MUST be false until promoted
TELEGRAM_STRATEGY_SHADOW_TO_ADMIN=true
TELEGRAM_STRATEGY_PROMOTED_TO_USER=true
TELEGRAM_STRATEGY_MIN_USER_CONFIDENCE=0.75
TELEGRAM_STRATEGY_MIN_USER_LEVEL=warning
TELEGRAM_STRATEGY_USER_DEDUPE_WINDOW=12h
TELEGRAM_STRATEGY_ADMIN_DEDUPE_WINDOW=1h

# === Legacy noise — MUST stay false ===
WATCHTOWER_STATS_TELEGRAM_ENABLED=false
TELEGRAM_STATS_ENABLED=false
PREDICTION_UPDATE_TELEGRAM_ENABLED=false
PREDICTION_BLOCKED_TELEGRAM_ENABLED=false
PREDICTION_STATE_TRANSITION_TELEGRAM_ENABLED=false
```

## Profile B — Aggressive Testing (short-term, weeks 1–2 of calibration)

Goal: maximise data volume to populate promotion-review samples
faster. **NOT for production user-facing alerting.**

Differences from Profile A:

```bash
# Capture more rows for value calibration
STRATEGY_SHADOW_RECORD_NOFIRE=true              # OK only during testing weeks
STRATEGY_SHADOW_MAX_DECISIONS_PER_TRADE=30

# Tighter staged-input cache for fresher reads
STRATEGY_STAGED_CACHE_TTL=30s

# More aggressive worker cadence
HOLDERSYNC_INTERVAL=5m
HOLDERSYNC_MAX_MARKETS=200
BOOK_FEATURE_BARS_INTERVAL=10s
BOOK_FEATURE_BARS_MAX_MARKETS=150
THESIS_LINES_INTERVAL=5m

# Slightly looser thresholds — pure data collection
THESIS_ACCUM_MIN_BREADTH=2
THESIS_ACCUM_MIN_CONSISTENCY=0.65
OWNERSHIP_V2_MIN_PCT_OI_INFO=0.02
BOOK_VACUUM_MIN_COLLAPSE_PCT=0.4

# Keep promotion locked
STRATEGY_LEARNING_LOOP_PROMOTION_ALLOWED=false
TELEGRAM_STRATEGY_USER_FLOW_ENABLED=false
```

Run for 7-14 days, then revert to Profile A before promoting any
strategy.

## Profile C — Emergency Rollback (kill-switches)

Goal: stop everything strategy-related without rolling back the binary.

```bash
# === Kill detect.Loop strategy hook ===
STRATEGY_STAGED_INPUTS_ENABLED=false

# === Kill all strategies individually ===
THESIS_ACCUM_ENABLED=false
OWNERSHIP_V2_ENABLED=false
CATALYST_WINDOW_ENABLED=false
BOOK_VACUUM_ENABLED=false
REPRICING_LAG_ENABLED=false
WALLET_COHORT_ENABLED=false
CONFLICT_RESOLVE_ENABLED=false
RULES_RISK_ENABLED=false
CHEAPTAIL_ENABLED=false

# === Kill workers (cost stops within minutes) ===
HOLDERSYNC_WORKER_ENABLED=false
BOOK_FEATURE_BARS_ENABLED=false
THESIS_LINES_WORKER_ENABLED=false
MARKETLINKS_ENABLED=false
RISKSCORE_ENABLED=false
REPRICING_WORKER_ENABLED=false
WALLETGRAPH_ENABLED=false

# === Kill Telegram surfaces ===
TELEGRAM_STRATEGY_ADMIN_FLOW_ENABLED=false
TELEGRAM_STRATEGY_USER_FLOW_ENABLED=false
TELEGRAM_STRATEGY_SHADOW_TO_ADMIN=false

# === Triple-lock (defence in depth) ===
STRATEGY_LEARNING_LOOP_PROMOTION_ALLOWED=false
STRATEGY_PROMOTION_BYPASS_EXPLICIT=false

# === Existing alert pipeline (UNTOUCHED) ===
# Watchtower's v4 single-trade alerts continue normally.
# Strategy rollback does not affect them.
```

## Rollout sequence (real-world)

1. **T0** — deploy Profile A. `verify-local.sh` passes. Monitor 24h.
2. **T0 + 3d** — review skip-reason mix. If `no_market_links_for_event` or `no_wallet_thesis_lines` dominates, **raise** worker rates (move toward Profile B for those workers only).
3. **T0 + 7d** — check promotion review sample sizes. If any strategy is < 10 firings/week, lower its `MIN_*` thresholds by 10-20%.
4. **T0 + 14d** — check `median_signed_move_6h`. Strategies < 0.5c are NOT promotion candidates regardless of sample size.
5. **T0 + 30d** — first promotion candidate emerges. Operator manually verifies via SQL pack queries, then flips `STRATEGY_LEARNING_LOOP_PROMOTION_ALLOWED=true` AND `<STRATEGY>_SHADOW_ONLY=false` for that one strategy only.
6. **T0 + 32d** — enable `TELEGRAM_STRATEGY_USER_FLOW_ENABLED=true`. Monitor user-facing alert rate for 48h. If noise > acceptable, roll back via `<STRATEGY>_SHADOW_ONLY=true`.

## Per-strategy promotion checklist

Before flipping `<STRATEGY>_SHADOW_ONLY=false`:

- [ ] Latest `polymarket_strategy_promotion_reviews` row has `eligible=true`.
- [ ] `sample_size ≥ 50` over 14d.
- [ ] `median_signed_move_6h ≥ 1.5c`.
- [ ] `reversal_15m_ratio ≤ 0.5`.
- [ ] `alerts_per_day ≤ 40`.
- [ ] Per-strategy diagnostic SQL shows reasonable distribution (no clustering at threshold floor).
- [ ] Matched-control uplift positive.
- [ ] Operator has reviewed sample shadow rows (`features_json`) and confirmed they look sensible.
- [ ] Telegram admin flow has been receiving the shadow alerts and operator finds them informative.

If ANY box is unchecked → keep shadow-only.

## Verification commands

```bash
# Config sanity (always green when env aligns with code)
go test ./internal/app/ -run TestEnvFiles -count 1

# Full local verify (build + vet + test + lint + migrations)
bash scripts/verify-local.sh

# .env / .env.example sync diff
bash scripts/audit-env.sh

# Live-API smoke (only when POLYMARKET_LIVE_SMOKE=1)
POLYMARKET_LIVE_SMOKE=1 POSTGRES_TEST_DSN=$POSTGRES_DSN \
    go test -tags integration -run TestPhaseF ./internal/app -v
```
