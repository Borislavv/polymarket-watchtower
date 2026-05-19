# ChatGPT handoff — paste this into a fresh AI session

> Dense. AI-to-AI. Minimal prose. Read top-to-bottom; every line
> carries load.

---

## SYSTEM IDENTITY

Polymarket Watchtower — Go service. Market-surveillance +
signal-intelligence over Polymarket public data. Filters &
explains; does NOT trade. Telegram is the only output channel.

## PRODUCT MISSION

Turn the firehose of Polymarket trades into a small feed of
operator-decision-grade alerts named:
- informed-flow candidate
- whale-like behavior
- late-market convergence
- asymmetric setup
- watchlist-worthy

Never: "insider", "guaranteed", "risk-free", "sure thing".

## NORTH STAR METRICS

- Operator does NOT mute the channel after one week.
- AI request success ratio > 0.9.
- PAL low-probability buckets: resolved_correct > implied_prob.
- Detection queue drains; pending backlog flat.
- False positives on Info spot-check < 30%.

## CORE LOOP

```
Gamma/Data API ─► discover/collect/backfill ─► Postgres
   ─► detection worker (claim FOR UPDATE SKIP LOCKED)
   ─► detect.Loop.Observe ─► strategy detectors
   ─► polymarket_alerts (dedup_key UNIQUE)
   ─► alertsender worker
   ─► AI Analyst note (success → polymarket_alert_analyses;
                       failure → polymarket_ai_request_logs)
   ─► Telegram SendHTML
   ─► outcomes / drift / outcomeai workers
   ─► PAL Grafana dashboards
```

## TECH STACK

- Go (zerolog, pgx/v5, sqlc, prometheus/client_golang, validator)
- Postgres (single source of truth)
- OpenAI Chat Completions (typed `*ProviderError` classifier)
- Telegram Bot API (HTML mode; sendHTML / editMessageText / setMessageReaction)
- Grafana (provisioned dashboards, `watchtower-main` UID)

## NON-NEGOTIABLE INVARIANTS

1. AI failure NEVER blocks alert send.
2. Detection panic NEVER kills the worker.
3. Provider quota/rate/5xx NEVER stored as analysis.
4. Period dedup is content-independent; same window = one row.
5. `collect.observer` is true nil-interface in Postgres mode.
6. MM filter fails OPEN on DB error.
7. Lifecycle-unknown markets are silenced; no env override.
8. `polymarket_alerts.dedup_key` is UNIQUE; never bypass.

## STRATEGY MAP (canonical order)

| ID | Strategy | Trigger shape |
|---|---|---|
| 1 | single-trade whale-flow | one trade in market+trader tail |
| 2 | recent accumulation | same wallet/side/market, line over window |
| 3 | lifetime accumulation | same but full stored history |
| 4 | cluster (HARD) | ≥3 anomalous trades, ≥2 wallets, one category |
| 5 | ownership concentration | wallet net-BUY ≥ tier% of outcome flow |
| 6 | stable favorite | late lifecycle + favored band + low stddev + liq |
| 7 | new wallet context | tag-only; never standalone |
| 8 | dormant wallet context | tag-only; never standalone |
| 9 | low-baseline severity cap | thin baselines → severity clamp |
| 10 | quiet-market wake-up | tag-only context |
| 11 | MM / bidirectional suppress | drops single+accumulation, NOT cluster |

Severity ladder: Info < Warning < Critical < Hard.
Hard = cluster ONLY.

## SUPPRESSION HIERARCHY (strongest first)

1. CATEGORY_WHITELIST (default: Politics only).
2. lifecycle ≥ LIFECYCLE_ALERT_FROM_PCT (default 75).
3. LIVE_ALERT_MAX_LAG (replayed-from-history drop).
4. MM filter (two-sided + neutrality).
5. LOW_BASELINE_CAP (thin reservoir clamp).
6. baseline readiness floors (SINGLE_MIN_BASELINE_*).
7. MARKET_MIN_AGE (default 24h).

## ENV PROFILES

| Profile | Path | Purpose |
|---|---|---|
| balanced | `presets/balanced.env` | default operator channel |
| conservative | `presets/conservative.env` | pager-grade |
| aggressive | `presets/aggressive.env` | local exploration |
| testing | `presets/testing.env` | Info-only loosening for pipeline validation |
| prod TEMPLATE | `presets/prod.env.template` | placeholders only; fill from DB via tuning-methodology |

## DATA MODEL (essentials)

```
polymarket_markets / _market_categories / _market_outcomes / _categories
polymarket_traders
polymarket_trades                         -- detection queue
polymarket_alerts                         -- alert queue (dedup_key UNIQUE)
polymarket_alert_analyses                 -- SUCCESSFUL AI answers only
polymarket_alert_outcome_analyses         -- AI postmortems
polymarket_alert_strategy_dimensions      -- bucketed attribution
polymarket_ai_request_logs                -- every AI call (success/skip/fail)
polymarket_market_intelligence_reports    -- 2h scout (period_key UNIQUE)
polymarket_collect_cursor                 -- per-market traded_at watermark
polymarket_signal_reports                 -- period signal-quality
```

## OPENAI ERROR CLASSIFIER (memorise)

| HTTP | discriminator | category | retryable |
|---|---|---|---|
| 429 | code=insufficient_quota | `quota_exceeded` | false |
| 429 | else | `rate_limited` | true |
| 401/403 | — | `unauthorized` | false |
| 400 | model_not_found | `invalid_model` | false |
| 400 | context_length_exceeded | `prompt_rejected` | false |
| 400 | else | `bad_request` | false |
| 408/504 | — | `timeout` | true |
| 5xx | — | `provider_5xx` | true |
| net/ctx | DeadlineExceeded | `timeout`/`network_error` | true |

## AI METRICS (current binary)

```
watchtower_ai_analysis_requests_total{status}
watchtower_ai_analysis_skipped_total{reason}
watchtower_ai_analysis_tokens_total{kind}
watchtower_ai_analysis_estimated_cost_usd_total
watchtower_ai_analysis_latency_seconds         -- histogram
watchtower_ai_request_errors_total{kind,reason}
watchtower_ai_analysis_persisted_total{target_kind}
watchtower_ai_analysis_rejected_total{target_kind,reason}
watchtower_ai_quota_exceeded_total{provider,model}
watchtower_market_intelligence_skipped_total{reason}
```

## STRUCTURED LOG EVENTS (grep these)

```
"ai alert analysis: started/completed/skipped/failed"
"ai alert analysis: attached to telegram alert" | "no note attached"
"ai request completed/failed/skipped"
"ai output failed validation; not persisting as analysis"
"ai analysis enabled/disabled"
"alertsender: ai enricher wired"
"detection: tick summary"
"backfill: tick summary"
"market intelligence skipped: ai_unavailable"
"detection pipeline wired" detect_mode=db_queue
```

## TUNING SHORT FORM

- DON'T eyeball. RUN `cmd/cli diagnose-alerts -lookback 24h`.
- ONE threshold per iteration.
- Target volume: Info ≤ 15/day, Warning 1-5/day, Critical 0-2/day, Hard 0-1/day.
- Reference queries: `doc/project/tuning-methodology.md` Q1-Q8.

## DEFAULTS WORTH KNOWING

```
CATEGORY_WHITELIST=Politics
LIFECYCLE_ALERT_FROM_PCT=75
LIFECYCLE_HOT_FROM_PCT=90
MARKET_MIN_AGE=24h
LIVE_ALERT_MAX_LAG=2h
SINGLE_MIN_BASELINE_TRADES=100
SINGLE_MIN_BASELINE_NOTIONAL_USD=10000
BASELINE_MIN_READY_WINDOW=24h
LOW_BASELINE_CAP_ENABLED=true
MM_FILTER_ENABLED=true
MM_MIN_TRADES_PER_SIDE=4
MM_NEUTRALITY_TOL=0.30
BACKFILL_WORKERS=48
BACKFILL_PARTIAL_RETRY_AFTER=6h
DETECTION_WORKERS=4
ALERT_SENDER_WORKERS=1
AI_ANALYSIS_DAILY_BUDGET_USD=5
AI_ANALYSIS_RATE_LIMIT_PER_MIN=10
AI_ANALYSIS_WEB_CONTEXT_ENABLED=false   -- scaffolded only, NOT wired transport
```

## STRATEGY IDENTITY

`anomaly.StrategyIdentity = "informed-flow-v6"` — woven into every
dedup_key. Bump = re-alerts on previously deduped trades. Do not
bump without operator sign-off. v7/v8 scoring changes did NOT bump
(known debt).

## OPERATOR INTENT (memorise)

- skeptical, evidence-driven; not gambler.
- reads Warning/Critical carefully, Info as watchlist.
- expects every alert to be explainable in the Telegram body alone.
- expects fewer alerts on a quiet day; "no alerts today" is HEALTHY.
- prefers tuning by DB evidence over intuition.

## SUCCESS PATTERNS TO PRESERVE

- Late-stage Politics market.
- Single wallet, 9+ same-side trades, span 12-48h, total ≥ Info tier.
- Trader history shows directional persistence.
- No opposite-side flow within 24h.
- Ownership share ≥ 10%.
- Baseline mature: 200+ trades, $25k+ aggregate, 48h+ span.
- AI Analyst note structured around Thesis/Follow?/Verdict.

## FAILURE PATTERNS TO REJECT

- `polymarket_alert_analyses` with raw provider error JSON in last_error → v8.1 fixed.
- "AI summary unavailable" shipped as a Telegram report → v8 fixed.
- Same market reanalysed every minute despite identical content → v8 period_key.
- Duplicate 2h intelligence reports inside one window → v8 period_key.
- Backfill retrying partial_api_limit every tick → v8 cooldown.
- collect.observer typed-nil regression panicking detect.Loop → v8 fixed.
- Validation rejecting non-empty paid model output for missing labels → v8.1 removed.

## ANTI-PATTERNS (do not propose)

- ML ranking on top of rule-based detectors (destroys explainability)
- Vector DB / RAG for the analyst note (prompts already compact)
- Auto-trading / order placement (out of product scope)
- A second cluster-shaped detector (compounds dedup keys)
- Custom news scraper (Responses API is the documented path)
- Renaming "informed-flow candidate" to "insider" (legal claim)
- Adding env knobs for behavior already enforced in code (e.g. ALLOW_UNKNOWN_LIFECYCLE — removed in v4)

## DEPENDENCY GRAPH (must respect)

```
infra ──► std/external
domain ──► std only (+ small vo)
repository ──► infra/postgres/sqlc (HIDES pgtype)
usecase ──► repository (interfaces) + domain
cmd ──► everything
```

`pgtype.*` MUST NOT leak above repository. `analysis.Analyzer` is
the only AI seam upstream of `infra/ai/openai`.

## TEST POSTURE

- All packages `ok` under `go test ./...` and `go test -race ./...`.
- No live API calls in tests (`httptest.Server` for the openai
  client; fake stores everywhere else).
- Integration tests gated by `POSTGRES_TEST_DSN` env.

## NEXT TASKS (operator-tunable, ordered)

1. Run `doc/project/tuning-methodology.md` Q1-Q8; fill
   `presets/prod.env.candidate`.
2. Wire new AI Grafana panels per `doc/observability/dashboards.md`.
3. Verify OpenAI Responses API JSON shape; enable web_context.
4. Wire `same_market_*` cross-flow fields from a repository query.
5. Decide on `StrategyIdentity` v7 bump (one-time re-alert wave).

## DOC HIERARCHY (read in this order on cold start)

```
1. doc/ai/chatgpt-handoff.md       ← you are here
2. doc/ai/project-philosophy.md    ← WHY
3. doc/ai/strategy-philosophy.md   ← which signals + why
4. doc/ai/runtime-mental-model.md  ← HOW it runs
5. doc/ai/current-state.md         ← state today
6. doc/strategies/current-strategy-map.md  ← per-strategy gates
7. doc/project/operator-guide.md   ← SQL + metrics runbook
8. doc/project/tuning-methodology.md ← DB-evidence procedure
9. CLAUDE.md                        ← decisions log
```

## GLOSSARY (project-specific)

- **Finding** — typed envelope every detector emits; rendered by the formatter
- **dedup_key** — `polymarket_alerts` UNIQUE column; strategy-namespaced
- **period_key** — `polymarket_market_intelligence_reports` UNIQUE column; UTC RFC3339 bucket
- **detect.Loop.Observe** — synchronous fan-out to all per-trade strategies
- **alertsender** — drains `pending` alerts to Telegram with AI + attribution stamps
- **PAL** — "post-alert lifecycle" / signal-quality dashboards
- **NoopAnalyzer** — analysis.Analyzer impl that always returns `Status=skipped`
- **HARD** — top severity, cluster-only
- **HOT** — lifecycle ≥ `LIFECYCLE_HOT_FROM_PCT`; header carries `· HOT`
- **CLV** — closing-line value; the drift worker measures fractional drift at 15m/1h/6h/24h
- **ProviderError** — typed openai error with Category + Retryable
- **StrategyIdentity** — code-owned dedup namespace, currently `informed-flow-v6`
