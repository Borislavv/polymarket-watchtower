# Architecture map

```
                                  ┌──────────────────────┐
                                  │ Gamma / Data / CLOB  │  (Polymarket public APIs)
                                  └──────────┬───────────┘
                                             │
                          ┌──────────────────┼──────────────────┐
                          │                  │                  │
                          ▼                  ▼                  ▼
                    ┌─────────┐        ┌──────────┐       ┌──────────┐
                    │discover │        │ collect  │       │ backfill │
                    │worker   │        │ worker   │       │ worker   │
                    └────┬────┘        └─────┬────┘       └─────┬────┘
                         │                   │                  │
                         └─────────┬─────────┴──────────────────┘
                                   ▼
                          ┌──────────────────┐
                          │  Postgres        │   (single source of truth)
                          │  polymarket_*    │
                          └────────┬─────────┘
                                   │
                                   ▼
                          ┌──────────────────┐
                          │ detection worker │   (claims trades with detected_at IS NULL)
                          └────────┬─────────┘
                                   │
                                   ▼
                          ┌──────────────────────────────────────────────┐
                          │   detect.Loop.Observe (per trade)            │
                          │                                              │
                          │   strategies                                 │
                          │   ─ single-trade whale-flow (score)          │
                          │   ─ accumulation (recent + lifetime)         │
                          │   ─ cluster / HARD                           │
                          │   ─ ownership concentration                  │
                          │   ─ new-wallet / dormant context             │
                          │   ─ MM filter (suppress)                     │
                          │   ─ low-baseline severity cap                │
                          │   ─ quiet-market wake-up tag                 │
                          └────────┬─────────────────────────────────────┘
                                   │
                                   ▼
                          ┌──────────────────┐    ┌─────────────────────┐
                          │ polymarket_      │    │ polymarket_alert_   │
                          │ alerts (queue)   │    │ strategy_dimensions │
                          └────────┬─────────┘    └─────────────────────┘
                                   │
                                   ▼
                          ┌──────────────────┐
                          │ alertsender      │
                          │ worker           │
                          └────────┬─────────┘
                                   │  (per claimed alert)
              ┌────────────────────┼─────────────────────┐
              ▼                    ▼                     ▼
        ┌────────────┐     ┌───────────────┐     ┌───────────────┐
        │ aianalysis │     │ attribution   │     │ Telegram      │
        │ Service    │     │ (buckets)     │     │ Bot.SendHTML  │
        └─────┬──────┘     └───────────────┘     └───────────────┘
              │
              │  (OpenAI Chat Completions API)
              ▼
        ┌─────────────────┐  success ─┐
        │ openai.Client   │           │
        │ + ProviderError │  failure ─┘ ──► polymarket_ai_request_logs
        └─────────────────┘
              │
              ▼  on Status=OK + non-empty + sanity-pass
        ┌─────────────────────────────┐
        │ polymarket_alert_analyses   │   (AI answers ONLY — never errors)
        └─────────────────────────────┘

       parallel pipeline:

       ┌─────────────────────────┐
       │ marketintel worker (2h) │
       │ → AI scout report       │
       │ → render Telegram body  │
       │   at send time only     │
       └────────┬────────────────┘
                ▼
       polymarket_market_intelligence_reports (UNIQUE period_key)

       outcome learning:

       outcomes worker  → stamps outcome_status on polymarket_alerts
       drift worker     → stamps clv_15m/1h/6h/24h on polymarket_alerts
       outcomeai worker → AI postmortem → polymarket_alert_outcome_analyses
                                       → Telegram editMessageText
                                       → setMessageReaction

       observability:

       Prometheus /metrics ──► Grafana (deploy/grafana/dashboards/)
       Structured zerolog  ──► stdout
       polymarket_ai_request_logs ──► operator SQL / diagnose-ai CLI
```

---

## Package map

| Package | Responsibility | Owns |
|---|---|---|
| `cmd/app` | binary entrypoint | `main.go` |
| `cmd/cli` | operator CLI (migrate, diagnose-alerts, diagnose-ai) | sub-commands |
| `internal/app` | wiring + config | `app.go`, `config.go` |
| `internal/app/usecase/discover` | market discovery | category-filtered upserts |
| `internal/app/usecase/collect` | recent-trade pulls | persist before observe |
| `internal/app/usecase/backfill` | historical pages | partial_api_limit cooldown |
| `internal/app/usecase/detect` | per-trade orchestration | `Loop.Observe`, fan-out to detectors |
| `internal/app/usecase/detection` | DB queue worker | claim → safeObserve → mark |
| `internal/app/usecase/persist` | write-through sink | trade/market/trader upserts |
| `internal/app/usecase/sanity` | soft-delete reaper | resume / purge |
| `internal/app/usecase/analytics/baseline` | in-memory reservoir | dev/test only |
| `internal/app/usecase/analytics/dbbaseline` | Postgres-backed baseline | prod path |
| `internal/app/usecase/analytics/traderbaseline` | wallet history baseline | trader axis |
| `internal/app/usecase/analytics/score` | tier scorer | `Score` returning `Result` |
| `internal/app/usecase/analytics/cluster` | sliding-window cluster detector | HARD path |
| `internal/app/usecase/analytics/accumulation` | line detector | recent + lifetime |
| `internal/app/usecase/analytics/ownership` | share-concentration detector | tier on % |
| `internal/app/usecase/analytics/mmfilter` | two-sided suppression | fail-open on DB error |
| `internal/app/usecase/analytics/quietmarket` | wake-up context tag | nil-safe |
| `internal/app/usecase/analytics/stablefavorite` | late-market convergence | pure detector |
| `internal/app/usecase/stablefavorite` | stable-favorite **worker** | periodic candidate sweep |
| `internal/app/usecase/aianalysis` | AI orchestration | refresh policy, persist split |
| `internal/app/usecase/marketintel` | 2h scout reports | period-key dedup |
| `internal/app/usecase/outcomes` | post-resolution verdict | reads Gamma `outcomePrices` |
| `internal/app/usecase/drift` | CLV-lite enrichment | post-trade fractional drift |
| `internal/app/usecase/outcomeai` | postmortem worker | edit Telegram + reaction |
| `internal/app/usecase/alertsender` | Telegram delivery | retry + AI enrich + attribution |
| `internal/app/usecase/attribution` | bucketing | dashboard group-by axis |
| `internal/app/usecase/statsreport` | periodic Telegram summary | aggregate counts |
| `internal/app/usecase/signalreport` | signal-quality reports | per-period verdicts |
| `internal/domain/model/anomaly` | Finding envelope + reason codes | strategy identity constant |
| `internal/domain/model/analysis` | AI request/response types | `Analyzer` interface |
| `internal/domain/model/{market,trade}` | core domain types | — |
| `internal/domain/vo` | identifiers + small value objects | `MarketID`, `TokenID` |
| `internal/infra/repository` | sqlc wrappers | all DB I/O |
| `internal/infra/postgres/sqlc` | generated SQL bindings | DO NOT IMPORT outside repository |
| `internal/infra/telegram` | Bot HTTP transport | sendHTML / editMessageText / setMessageReaction |
| `internal/infra/alerting` | Telegram formatter | HTML rendering, link sanitization |
| `internal/infra/ai/openai` | Chat Completions client | typed ProviderError + classifier |
| `internal/infra/polymarket/{gamma,dataapi,httpx}` | upstream HTTP | rate-limited |
| `internal/infra/metrics` | Prometheus registry | one place, no duplicate vecs |
| `internal/infra/log` | zerolog setup | structured JSON |
| `internal/infra/http` | metrics server | `/metrics` only |

## DB table map

| Table | Owner package | Purpose |
|---|---|---|
| `polymarket_categories` / `_markets` / `_market_categories` / `_market_outcomes` | discover/persist | market universe |
| `polymarket_traders` | persist | wallet→id, first/last seen |
| `polymarket_trades` | collect/backfill/persist | every public trade; detection queue |
| `polymarket_alerts` | detect (write) + alertsender (read/write) | THE alert queue |
| `polymarket_alert_strategy_dimensions` | alertsender | bucketed attribution |
| `polymarket_alert_analyses` | aianalysis | SUCCESSFUL AI notes |
| `polymarket_alert_outcome_analyses` | outcomeai | AI postmortems |
| `polymarket_market_intelligence_reports` | marketintel | 2h scout, AI text only |
| `polymarket_ai_request_logs` | aianalysis | every AI provider call, success or failure |
| `polymarket_collect_cursor` | collect | per-market traded_at watermark |
| `polymarket_signal_reports` | signalreport | period-level signal quality |

## Ownership boundaries

- **infra/**: external systems. Owns transport, JSON shapes, retries.
  Returns typed errors. Never imports usecase.
- **usecase/**: orchestration. Owns "when to call", "with what
  policy", "what to persist". Never imports infra packages except
  through interfaces defined in the usecase itself.
- **domain/**: types + invariants. No I/O. No package imports
  outside std + small vo helpers.
- **repository/**: sqlc isolation. Wraps generated code in
  domain-friendly types. Pgtype/pgx never leaks outside this layer.
