# Runtime flow

What actually happens, in order, when the binary boots and runs.

---

## A. Startup

1. `app.LoadConfig()` reads env. Validation errors → process exits.
2. `metrics.New()` builds the Prometheus registry.
3. If `POSTGRES_DSN` non-empty:
   - `pg.Migrate(DSN)` applies embedded `db/migrations/` (controlled
     by `POSTGRES_AUTO_MIGRATE`, default true).
   - `pg.Open()` creates the pool.
   - Repositories instantiate: market, trade, trader, alert,
     alert-analysis, ai-request-log, etc.
4. `detect.Loop` and per-strategy detectors wire up.
5. Workers are constructed (collect, backfill, sanity, detection,
   alertsender, outcomes, drift, outcomeai, marketintel,
   statsreport, signalreport, stablefavorite).
6. `httpsrv.New` exposes `/metrics`.
7. **Expected log lines** (grep these on every restart):
   - `"detection pipeline wired"` with `detect_mode=db_queue` (or
     `inline_memory` in dev).
   - `"detection worker: enabled"` with workers/claim_limit/interval.
   - `"backfill: enabled"` with workers/page_limit/interval/
     stale_after/partial_retry_after.
   - `"ai analysis enabled"` with provider/model/budget/rate
     OR `"ai analysis disabled"` with reason.
   - `"alertsender: ai enricher wired"` when applicable.

If `POSTGRES_DSN` is empty: dev/memory mode runs, NO detection
queue, NO outcome tracking, NO alert dedup. Boot logs are loud
about this; do not deploy.

## B. Ingestion

### discover (`internal/app/usecase/discover`)
- Periodically calls Gamma `/markets`.
- **Category whitelist filter** runs HERE first — ids not matching
  `CATEGORY_WHITELIST` are stripped BEFORE the registry sees them.
- Persisted via `persist.Sink.UpsertMarket` etc.

### collect (`internal/app/usecase/collect`)
- Snapshots `marketcache`, pulls recent trades from Data API for
  every tradable market.
- `persist.Sink.PersistTrades` writes BEFORE the observer runs (so
  the DB baseline sees the trade by the next query).
- **`l.observer == nil` in Postgres mode** (typed-nil regression
  fixed in v8). Trades sit at `detection_status='pending'` for the
  detection worker.

### backfill (`internal/app/usecase/backfill`)
- Per-tick claims up to `BACKFILL_WORKERS` markets in
  `pending` or `partial_api_limit` status.
- `partial_api_limit` markets are filtered by
  `backfill_completed_at < NOW() - BACKFILL_PARTIAL_RETRY_AFTER`
  (default 6h) — Polymarket 3000-row cap is structural; tight
  retry would burn quota.
- Walks `/trades` offset 0..N until empty page (`completed`) or
  the 3000 cap (`partial_api_limit`). Persists pages as it goes.
- **Per-tick summary log**: `"backfill: tick summary"` with
  claimed/completed/partial/failed/skipped counts.

## C. Detection

`internal/app/usecase/detection.Worker`:

1. `ResetStaleDetectionClaims(ClaimTTL)` returns any rows wedged
   in `claimed` state by a crashed previous process.
2. N parallel `drainLoop` goroutines (per `DETECTION_WORKERS`):
   - `ClaimUndetectedTrades(workerID, claimLimit, ttl)` flips
     `pending → claimed` for a batch.
   - For each row: `handle(ctx, now, row, counters)`.
3. `handle`:
   - market lookup in cache; miss → `mark_skipped('market_unknown')`.
   - `safeObserve(ctx, market, trade)` wraps `detect.Loop.Observe`
     in a `recover()`; panic → `mark_failed` with the recovered
     error.
   - stale-trade gate (`StaleThreshold = LIVE_ALERT_MAX_LAG`) →
     `mark_skipped('too_old_for_live_alert')`.
   - success → `mark_analyzed`.
4. **Per-tick summary log**: `"detection: tick summary"` with
   claimed/analyzed/skipped_too_old/skipped_market_unknown/
   failed_panic/failed_mark counts.

Failure-class table:

| Outcome | DB state | Metric |
|---|---|---|
| analyzer panicked | `detection_status='failed'` + last_error | `watchtower_detection_failed_total{reason="panic"}` |
| market not in cache | `'skipped'` reason `market_unknown` | `..._total{status="skipped", reason="market_unknown"}` |
| trade too old for live | `'skipped'` reason `too_old_for_live_alert` | same metric, different reason |
| `MarkDetectionAnalyzed` failed | warning logged, row stays claimed → reclaimed by ClaimTTL | `..._failed_total{reason="mark_analyzed"}` |
| happy path | `'analyzed'` + `detected_at=NOW()` | `..._total{status="analyzed"}` |

## D. Alerting

`internal/app/usecase/alertsender.Worker`:

1. `ResetStaleSending(now - StaleSendingAfter)` clears wedged
   `sending` rows.
2. N parallel workers call `ClaimPending(ClaimLimit)` — atomic
   `UPDATE … FOR UPDATE SKIP LOCKED` pattern.
3. Per claimed alert:
   - `unmarshalFinding` deserialises the persisted `Finding`.
   - `stampAnalystNote` calls the optional `AIEnricher`:
     - `AnalyzeAndStore` → returns row + Status + LastError.
     - LOGS one of: `started`, `completed`, `skipped`, `failed`.
     - On success, `LatestText` fetches the row;
       `f.AnalystNote = text` if non-empty.
     - LOGS `attached to telegram alert` OR `no note attached`
       with `reason=<canonical>`.
   - `writeAttribution` lands the bucketed row in
     `polymarket_alert_strategy_dimensions`.
   - `FormatTelegramMessage` builds HTML.
   - `tg.SendHTML(chatID, text)` → SendResult.
     - context cancel → leave pending.
     - any other error → `markFailed(err)` schedules retry per
       `RetryPolicy`.
     - permanent error (HTML parse, chat-not-found) → no retry.
   - `MarkSent(id, messageID)` commits the delivery.

**AI failure NEVER blocks delivery.** Test pinned by
`TestAIEnricherFailureDoesNotBlockSend` and friends.

## E. Market intelligence (2h)

`internal/app/usecase/marketintel.Worker`:

1. `bucketedPeriod(now, Interval)` → deterministic `(end, start)`
   aligned to interval boundary. `period_key = "<start>/<end>"`
   in UTC RFC3339.
2. `candidates = ListIntelligenceCandidates(MaxMarkets)`.
3. `filterAndDedupCandidates`: drops near-degenerate prices
   (≤0.02, ≥0.98) and same-`condition_id` duplicates.
4. If 0 candidates → log `"marketintel: skipping empty periodic
   report"`, metric `..._skipped_total{reason="empty_report"}`,
   return. NO telegram, NO row.
5. `analyzer.AnalyzeMarketReport(req)` → `MarketReportAnalysis`.
6. If `Status != OK` or `ReportText == ""` → log
   `"market intelligence skipped: ai_unavailable"`, metric
   `..._skipped_total{reason="ai_unavailable"}`, return. NO row.
7. **Persist analysis_text only.** `report_text =
   strings.TrimSpace(res.ReportText)`. The rendered Telegram body
   is built later via `renderTelegramBody`; never stored.
8. `store.Insert` with `ON CONFLICT (period_key) DO NOTHING`. If
   not fresh → metric `..._skipped_total{reason="duplicate_period"}`,
   return.
9. `bot.SendHTML(ChatID, renderTelegramBody(req, res))`.

## F. Outcome learning

- **outcomes worker** (`OUTCOMES_INTERVAL=15m`): scans alerts
  whose markets may be resolved, reads Gamma `outcomePrices`,
  stamps `outcome_status` on `polymarket_alerts`.
- **drift worker** (`DRIFT_INTERVAL=5m`): per sent alert, after
  the 15m/1h/6h/24h windows elapse, writes the signed favourable
  fractional drift columns `clv_15m..clv_24h`.
- **outcomeai worker**: takes resolved alerts, asks the AI for a
  one-paragraph postmortem, persists in
  `polymarket_alert_outcome_analyses`, edits the original
  Telegram message via `editMessageText`, applies a
  success/failure reaction via `setMessageReaction`. Falls back
  to a follow-up message when edit is unsupported.

## G. Failure behaviour (the load-bearing invariants)

1. **AI failure NEVER blocks alert send.** The Telegram body just
   omits the Analyst note. Every alert-AI call writes one row to
   `polymarket_ai_request_logs`.
2. **Detection panic NEVER kills the worker.** `safeObserve`
   recovers and stamps the row `failed` with the panic text.
3. **Provider quota / rate-limit / 5xx are NOT stored as
   analysis.** Typed `*openai.ProviderError` is classified at the
   transport layer; the routing layer writes only to
   `ai_request_logs`. Pinned by
   `TestProviderQuotaExceededIsNotStoredAsAnalysis`.
4. **Telegram retry is bounded and jittered.** Permanent errors
   (HTML parse, chat-not-found, bot-kicked) are recognised and
   never retried.
5. **Market-intelligence empty period → no fake report.** Same
   for AI-unavailable. The previous "AI summary unavailable"
   placeholder behaviour was removed in v8.
