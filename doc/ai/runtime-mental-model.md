# Runtime mental model (AI-oriented)

> How the system behaves at runtime — without reading every package.

## One-paragraph summary

Periodic workers pull Polymarket public data, persist it to
Postgres, claim it through a DB-backed detection queue, run
strategy detectors against it, write alerts to another DB queue,
and a sender worker drains that queue to Telegram with optional
AI enrichment. Outcome workers stamp post-resolution verdicts.
Everything is **eventually consistent**; the only synchronous path
is `alertsender.send` (per-claimed-alert).

## Loops by cadence

| Loop | Cadence | Owns |
|---|---|---|
| discover | minutes | upserting markets (Gamma) |
| collect | seconds-to-minute | recent trades per market (Data API) |
| backfill | 1 min | historical trades (offset paging) |
| sanity | hourly | soft-delete → purge transitions |
| detection | 2-5 sec | claim `pending` trades, run detect.Loop |
| alertsender | 5 sec | claim `pending` alerts, send Telegram |
| outcomes | 15 min | post-resolution verdict stamping |
| drift | 5 min | CLV-lite drift fields |
| outcomeai | 10 min | AI postmortems + Telegram edit |
| marketintel | 2h (period-aligned) | scout report |
| statsreport | 2h | aggregate Telegram summary |
| signalreport | daily / weekly / monthly | signal-quality reports |
| stablefavorite | 15 min | state-driven late convergence sweep |

## What is reactive vs periodic

- **Reactive (per-trade fan-out):** detect.Loop.Observe → all
  per-trade strategies (single-trade, accumulation, cluster
  feed, ownership read, MM filter, low-baseline cap).
- **Periodic (state-driven):** stable-favorite (per-market scan),
  outcomes (per resolved-market scan), drift (per-window
  elapsed), marketintel (per-period scout), statsreport,
  signalreport.

## Where eventual consistency is acceptable

- Backfill catches up over hours/days — historical detection is
  not time-critical.
- Outcome stamping is hours/days behind market resolution — the
  market closes, then we look back.
- Drift columns appear in waves (15m, 1h, 6h, 24h) per alert.
- AI Analyst note may be absent on the first attempt and present
  on a refresh attempt — alertsender re-runs are idempotent.

## Where correctness is non-negotiable

- `polymarket_alerts.dedup_key UNIQUE` — no double-send, ever.
- `polymarket_trades.detection_status` state machine — every
  persisted trade is observed exactly once.
- `polymarket_market_intelligence_reports.period_key UNIQUE` —
  one row per 2h bucket; restarts cannot double-send.
- `polymarket_ai_request_logs` carries every AI call — analytical
  table never sees provider failures.
- Typed `*openai.ProviderError` — quota / rate / 5xx / 400 are
  classified, not stringified.

## State machines worth knowing

### `polymarket_trades.detection_status`
```
pending → claimed → analyzed
                  ↘ skipped (too_old_for_live_alert | market_unknown)
                  ↘ failed (panic in safeObserve)
```
Stale `claimed` rows are reclaimed after `ClaimTTL` (default 5m).

### `polymarket_alerts.status`
```
pending → sending → sent
                  ↘ failed (retryable via RetryPolicy backoff/jitter)
                  ↘ failed (permanent — HTML parse, chat-not-found)
```
`ResetStaleSending(now - StaleSendingAfter)` recovers `sending`
rows orphaned by a crashed sender.

### `polymarket_markets.backfill_status`
```
pending ─► running ─► completed
        ↘          ↘ partial_api_limit (3000-row cap)
         ↘ failed
```
`partial_api_limit` markets are cooled down for
`BACKFILL_PARTIAL_RETRY_AFTER` (default 6h) before re-claim.

## Data flow (one trade end-to-end)

```
Polymarket Data API
  │  collect.pull (or backfill.runPages)
  ▼
persist.Sink.PersistTrades
  │  INSERT polymarket_trades (detection_status=pending)
  ▼
detection.Worker.ClaimUndetectedTrades
  │  UPDATE … FOR UPDATE SKIP LOCKED
  ▼
detect.Loop.Observe
  │  fan out to: score (single), accumulation, cluster.Observe,
  │  ownership, mmfilter, new/dormant context, low-baseline cap.
  ▼
For each detector that fires:
  TryCreatePending(polymarket_alerts) — ON CONFLICT (dedup_key) DO NOTHING
  │
  ▼
alertsender.Worker.ClaimPending
  │
  ▼
stampAnalystNote (AI enricher)
  │  → polymarket_alert_analyses (on success only)
  │  → polymarket_ai_request_logs (every call)
  ▼
writeAttribution
  │  → polymarket_alert_strategy_dimensions
  ▼
FormatTelegramMessage + tg.SendHTML
  │
  ▼
MarkSent(id, telegram_message_id)
  │
  (parallel pipeline:)
  ▼
outcomes.Worker → stamps outcome_status
drift.Worker → stamps clv_15m / clv_1h / clv_6h / clv_24h
outcomeai.Worker → AI postmortem + Telegram edit + reaction
```

## Failure invariants (memorise these)

1. **AI failure never blocks alert send.** Telegram body omits
   the Analyst-note block; alert ships.
2. **Detection panic never kills the worker.** `safeObserve`
   recovers; row stamped `failed`.
3. **Provider quota / rate / 5xx never persisted as analysis.**
   Typed error → `ai_request_logs` only.
4. **Period dedup is content-independent.** Same 2h window =
   same `period_key` = one row regardless of clock drift.
5. **Typed-nil interface trap fixed in v8.** `collect.observer`
   is a true `nil` interface in Postgres mode; the detection
   queue is the only path to `detect.Observe`.
6. **MM filter fails open on DB error.** A hiccup must not
   swallow a real alert.

## Worker concurrency

- `BACKFILL_WORKERS=48` parallel goroutines per tick (1:1 with
  per-tick claim count).
- `DETECTION_WORKERS=4` parallel drain loops, each calling
  `ClaimUndetectedTrades` with `FOR UPDATE SKIP LOCKED` so
  workers see disjoint batches.
- `ALERT_SENDER_WORKERS=1` by default (Telegram is sequential
  per chat).
- Other workers are single-goroutine periodic.

## Telegram body shape

Every alert HTML, in order:
1. `<b>{SEV}: x{mul} · ${notional}[ · HOT] · {title}</b>`
2. `<b>Why</b>` — multiplier, odds, baseline, tier composition, lifecycle
3. `<b>Trade</b>` — outcome+side, size+shares@price, trader, category, ISO ts
4. `<b>Cluster</b>` — HARD only
5. `<b>Links</b>` — Polymarket market / category / trader / Grafana
6. `<b>Analyst note</b>` — AI text when present
7. `<b>Data</b>` — `<code>dedup_key</code>`, `<code>market_id</code>`, `<code>outcome_token</code>`

Tests pin the order and the forbidden strings (insider /
guaranteed / risk-free).

## Where to look first when something looks wrong

| Symptom | First place to look |
|---|---|
| No alerts overnight | `polymarket_trades.detection_status` distribution |
| Pending alerts piling up | alertsender logs + Telegram credentials |
| "AI summary unavailable" missing context | `polymarket_ai_request_logs` last 50 rows |
| Duplicate Telegram messages | `polymarket_alerts.dedup_key UNIQUE` violation? (impossible if migrations applied) |
| Same backfill markets every minute in logs | `BACKFILL_PARTIAL_RETRY_AFTER` not set / migration 00014 not applied |
| Quota burn | `watchtower_ai_quota_exceeded_total` + billing |
| HARD never fires | `CLUSTER_MIN_*` floors too tight; check `cluster.Detector` state |
