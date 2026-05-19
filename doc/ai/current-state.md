# Current state snapshot

> Refresh this file whenever the answers change. Date of last
> verified-by-tests state: see git log for this file.

## Active strategies

All currently wired:

- single-trade whale flow (`analytics/score`)
- recent accumulation (`analytics/accumulation`)
- lifetime accumulation (same package, different window)
- multi-wallet cluster / HARD (`analytics/cluster`)
- ownership concentration (`analytics/ownership`)
- stable favorite (`analytics/stablefavorite` + `usecase/stablefavorite` worker)
- new-wallet / dormant-wallet context (in `detect.Loop`)
- low-baseline severity cap (`analytics/score`)
- quiet-market wake-up tag (`analytics/quietmarket`)
- MM / bidirectional suppression (`analytics/mmfilter`)
- AI Analyst note (`aianalysis.Service`)
- 2h market intelligence report (`marketintel.Worker`)
- AI outcome postmortem (`outcomeai.Worker`)

## Active AI infrastructure

- OpenAI Chat Completions transport (`internal/infra/ai/openai`)
- typed `*ProviderError` with categories: quota_exceeded /
  rate_limited / timeout / provider_5xx / bad_request / etc.
- v8.1 minimal output validation (empty / provider-error JSON only)
- `polymarket_alert_analyses` — SUCCESSFUL AI answers only
- `polymarket_ai_request_logs` — every AI call, success or failure
- `period_key UNIQUE` on `polymarket_market_intelligence_reports`
- `partial_api_limit` cooldown via `BACKFILL_PARTIAL_RETRY_AFTER`

## Intentionally NOT implemented

- live trading / order placement
- OpenAI Responses API + web_search transport (scaffolded only;
  needs live-API verification before enable)
- replay-against-historical-outcomes harness
- vector DB / embeddings / RAG
- ML ranking layer
- websocket-based real-time ingest (HTTP polling is sufficient)
- backtesting framework

## Current production risks

| Risk | Mitigation status |
|---|---|
| OpenAI quota exhaustion mid-day | metric `watchtower_ai_quota_exceeded_total` + alert |
| Detection queue backlog growth | per-tick summary log + pending-count metric |
| Telegram rate limiting | sequential `ALERT_SENDER_WORKERS=1` |
| Backfill burning API quota | `partial_api_limit` cooldown (v8) |
| Wallet false positives (MM) | `mmfilter` enabled by default |
| Tiny-liquidity multiplier traps | `LOW_BASELINE_CAP_ENABLED=true` |
| Stale `StrategyIdentity = informed-flow-v6` lagging scorer changes | not mitigated; future bump is a one-time re-alert wave |
| Cross-flow context is tag-only, not hard suppressor | accepted tradeoff; AI prompt biases toward Watch |

## Current weak areas

- **Ownership share is trade-flow approximation.** No upstream
  holders endpoint. Labelled in Telegram body.
- **AI prompt does not include polling/news.** Web context is
  scaffolded but off. The model receives `public_context: NOT
  checked` and must include the canonical disclosure sentence.
- **No backtesting against past resolutions.** Tuning is forward-
  measured via PAL dashboards.
- **`StrategyIdentity` lag.** v7/v8 scoring changes did not bump
  the dedup namespace. A future bump re-alerts deduped trades.

## Current tuning philosophy

Two profiles, distinct purposes:

| Profile | File | Purpose |
|---|---|---|
| Testing | `presets/testing.env` | pipeline validation, Telegram UX, prompt tuning |
| Balanced (default) | `presets/balanced.env` | day-to-day operator channel |
| Conservative | `presets/conservative.env` | pager-grade |
| Aggressive | `presets/aggressive.env` | local exploration |
| Production template | `presets/prod.env.template` | placeholders only; fill from DB evidence per `doc/project/tuning-methodology.md` |

**Tuning rule:** edit ONE threshold, run `diagnose-alerts`,
compare projected fire counts, then apply. No batch changes.

## Current known noisy patterns (suppressed by design)

- MM / bidirectional wallets → `mmfilter` (silent suppress)
- Meme / sports markets → category whitelist (`Politics` only)
- Coinflip markets (price 0.45-0.55, no convergence) → stable-favorite gate
- Low-baseline trap (5 trades, big multiplier) → low-baseline cap
- Lifecycle-unknown markets → fail-closed, no override
- Replay on restart (old trades) → `LIVE_ALERT_MAX_LAG` belt
- Empty 2h intelligence period → skipped, metric, no Telegram
- AI-unavailable 2h intelligence period → skipped, metric, no Telegram

## Current operational assumptions

- Postgres is available; no production mode without it.
- One Telegram chat ID; no broadcast.
- `OPENAI_API_KEY` is optional but recommended. Without it, the
  Noop analyzer returns `skipped`; alerts ship without notes.
- Daily AI budget is operator-tunable; default $5/day.
- `CATEGORY_WHITELIST=Politics` is the default; widening
  trades precision for volume.

## Current technical debt

| Item | Severity | File reference |
|---|---|---|
| Dead `stablefavorite.Config.ReversalVolumeRatio` field | low | `analytics/stablefavorite/detector.go:58` |
| `validateAlertOutput` misnamed after v8.1 relaxation | low | `aianalysis/aianalysis.go` |
| `doc/strategies/volume.md` describes a removed strategy | low | should move to archive |
| `StrategyIdentity` lags scorer generation | medium | see overview.md "Current limitations" |
| AI metric labels not standardised on `target_kind/provider/model` across all vecs | low | additive new vecs added in v8.1; old vecs preserved |
| `sanitizeAndCap` duplicated to avoid import cycle | trivial | documented in code |

## Current next priorities (recommended ordering)

1. **Run `doc/project/tuning-methodology.md` Q1-Q8 against
   production DB.** Fill `presets/prod.env.candidate` with real
   numbers. Validate via `diagnose-alerts`. Promote.
2. **Wire the new AI Grafana panels** described in
   `doc/observability/dashboards.md` — three new metrics
   (`analysis_persisted_total`, `analysis_rejected_total`,
   `quota_exceeded_total`) deserve visibility.
3. **OpenAI Responses API + web_search transport** — scaffolded
   in `internal/infra/ai/openai/web_context.go`. Needs live
   verification of the JSON contract before flipping
   `AI_ANALYSIS_WEB_CONTEXT_ENABLED=true`.
4. **Wire `same_market_*` fields from a repository query.** Today
   the AI receives them at zero unless the caller populates;
   the detector layer should populate from
   `polymarket_alerts` last-24h aggregates.
5. **`StrategyIdentity` bump to `informed-flow-v7`** — only with
   operator sign-off on the one-time re-alert wave.

## What is probably a trap (do not implement without explicit need)

- ML ranking on top of the rule-based detectors. The rule-based
  layer is the **explainable** layer; adding an ML layer
  destroys explainability. Operator feedback strongly opposes.
- Vector DB / RAG for AI analyst note. The structured request
  the model receives is already compact; adding retrieval makes
  prompts longer and slower without clear precision gain.
- A second detector that "improves" the cluster path. The
  cluster gate already produces high-precision HARD alerts;
  adding a second cluster-shaped detector compounds dedup keys
  and operator confusion.
- Auto-betting / auto-trading. Out of product scope; legally
  fraught; explicitly forbidden by the brief.
- Tweet/news ingest. The Web context (Responses API) is the
  documented path; rolling a custom news scraper is overengineered.

## Test posture today

- `go test ./...` and `go test -race ./...` are both green.
- Repository integration tests opt-in via `POSTGRES_TEST_DSN`;
  not run in CI by default.
- Live OpenAI calls are NEVER made in tests; the openai package
  uses `httptest.Server` for the full transport path.
- No mocking framework — fakes are hand-rolled per package.

## Single source of truth references

| Concern | File |
|---|---|
| Product philosophy | `doc/ai/project-philosophy.md` |
| Strategy reasoning | `doc/ai/strategy-philosophy.md` |
| Runtime model | `doc/ai/runtime-mental-model.md` |
| Current state | THIS FILE |
| Per-strategy gates | `doc/strategies/current-strategy-map.md` |
| AI observability | `doc/observability/ai-metrics.md` |
| Operator runbook | `doc/project/operator-guide.md` |
| Tuning procedure | `doc/project/tuning-methodology.md` |
| Decisions log | `CLAUDE.md` |
