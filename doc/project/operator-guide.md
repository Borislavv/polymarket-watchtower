# Operator guide

Day-to-day runbook. SQL is the source of truth; metrics are the
fast view.

---

## Required env (production minimum)

```
POSTGRES_DSN=postgres://...
TELEGRAM_BOT_TOKEN=...
TELEGRAM_CHAT_ID=...
CATEGORY_WHITELIST=Politics

# AI (optional but recommended)
AI_ANALYSIS_ENABLED=true
OPENAI_API_KEY=...
AI_ANALYSIS_TELEGRAM_ALERTS_ENABLED=true
AI_ANALYSIS_REPORTS_ENABLED=true

# Periodic intelligence
AI_MARKET_INTELLIGENCE_ENABLED=true
AI_MARKET_INTELLIGENCE_INTERVAL=2h

# Stable favorite (separate strategy)
STABLE_FAVORITE_ENABLED=true
```

Pair with one of `presets/{conservative,balanced,aggressive}.env`
sourced FIRST, then your overrides.

## Startup checklist

After a restart, grep stdout for these lines in order:

```sh
grep '"detection pipeline wired"' watchtower.log    # detect_mode=db_queue
grep '"detection worker: enabled"' watchtower.log   # workers, claim_limit
grep '"backfill: enabled"' watchtower.log           # partial_retry_after
grep '"ai analysis (enabled|disabled|configured)"' watchtower.log
grep '"alertsender: ai enricher wired"' watchtower.log
```

If `detect_mode=inline_memory` appears in production, the wiring
is wrong (Postgres is unreachable or DSN is empty). Fix before
anything else.

## SQL health checks

### Detection queue

```sql
-- Distribution by status
SELECT detection_status, detection_skip_reason, count(*)
  FROM polymarket_trades
 GROUP BY detection_status, detection_skip_reason
 ORDER BY count(*) DESC;

-- Throughput (last 30m)
SELECT count(*)
  FROM polymarket_trades
 WHERE detected_at >= NOW() - INTERVAL '30 minutes';

-- Pending backlog
SELECT count(*) FROM polymarket_trades WHERE detection_status='pending';

-- Stale claimed (should be 0; the worker reclaims after ClaimTTL)
SELECT count(*) FROM polymarket_trades
 WHERE detection_status='claimed' AND detection_claimed_at < NOW() - INTERVAL '10 minutes';

-- Lag (now - newest detected_at)
SELECT NOW() - MAX(detected_at) AS detection_lag FROM polymarket_trades;
```

### Alerts

```sql
-- Distribution by status / severity / kind
SELECT status, severity, kind, count(*)
  FROM polymarket_alerts
 GROUP BY status, severity, kind
 ORDER BY count(*) DESC;

-- Creation rate (last 30m)
SELECT count(*) FROM polymarket_alerts
 WHERE created_at >= NOW() - INTERVAL '30 minutes';

-- Latest 20 rows
SELECT id, created_at, severity, kind, status,
       COALESCE(last_send_error,'') AS err
  FROM polymarket_alerts
 ORDER BY id DESC LIMIT 20;

-- Stuck pending (alertsender not draining?)
SELECT count(*) FROM polymarket_alerts
 WHERE status='pending' AND created_at < NOW() - INTERVAL '5 minutes';
```

### AI

```sql
-- Latest 20 SUCCESSFUL analyses
SELECT alert_id, version, model,
       left(analysis_text, 120) AS preview, verdict, created_at
  FROM polymarket_alert_analyses
 WHERE legacy_provider_failure = FALSE
   AND status = 'ok'
 ORDER BY created_at DESC LIMIT 20;

-- AI provider call telemetry (success + failure)
SELECT created_at, target_kind, status, error_category,
       http_status, left(error_message, 120) AS msg, latency_ms
  FROM polymarket_ai_request_logs
 ORDER BY created_at DESC LIMIT 50;

-- Quota burn last 24h (operator action — billing)
SELECT count(*) FROM polymarket_ai_request_logs
 WHERE error_category='quota_exceeded'
   AND created_at >= NOW() - INTERVAL '24 hours';

-- Validation-rejected (v8.1 should be near-zero — provider regression signal)
SELECT count(*), error_category FROM polymarket_ai_request_logs
 WHERE error_category LIKE 'validation_failed:%'
   AND created_at >= NOW() - INTERVAL '24 hours'
 GROUP BY error_category;
```

### Market intelligence

```sql
-- 2h report cadence + delivery state
SELECT period_key, status, sent_at, model, prompt_tokens,
       length(report_text) AS analysis_chars
  FROM polymarket_market_intelligence_reports
 ORDER BY period_start DESC LIMIT 20;

-- Period-dedup sanity (should always be 1)
SELECT period_key, count(*) FROM polymarket_market_intelligence_reports
 GROUP BY period_key HAVING count(*) > 1;
```

### Backfill

```sql
SELECT backfill_status, count(*) FROM polymarket_markets
 GROUP BY backfill_status;

-- partial_api_limit cooldown working?
SELECT count(*) FROM polymarket_markets
 WHERE backfill_status='partial_api_limit'
   AND backfill_completed_at >= NOW() - INTERVAL '6 hours';
```

## Metrics to watch (Grafana)

| Metric | What it tells you |
|---|---|
| `watchtower_detection_claimed_total` | rate at which the queue drains |
| `watchtower_detection_failed_total{reason}` | panics / mark-failures |
| `watchtower_telegram_alerts_sent_total{severity}` | delivery health |
| `watchtower_telegram_alert_errors_total{severity}` | delivery breakage |
| `watchtower_ai_analysis_persisted_total{target_kind}` | AI is producing real output |
| `watchtower_ai_analysis_rejected_total{target_kind,reason}` | sanity rejects (should ≈ 0) |
| `watchtower_ai_quota_exceeded_total{provider,model}` | billing alarm |
| `watchtower_ai_request_errors_total{kind,reason}` | category breakdown |
| `watchtower_market_intelligence_skipped_total{reason}` | empty / dup / ai_unavailable |
| `watchtower_filter_alert_mm_suppressed_total{category,reason}` | MM filter activity |

## Telegram expectations

Every alert HTML contains, in order:
1. Header `{SEV}: x{mul} · ${notional}[ · HOT] · {title}`.
2. `Why` block (multiplier, odds, baseline, tier composition, lifecycle).
3. `Trade` block.
4. `Cluster` block (HARD only).
5. `Links` (Polymarket market / category / trader / Grafana).
6. `Analyst note` (when AI succeeded).
7. `Data` block with dedup key + market_id + outcome_token in `<code>`.

If the Analyst note is missing, grep:

```sh
grep 'ai alert analysis: no note attached' watchtower.log | tail -5
```

The `reason=` field on those lines is the canonical category
(`enricher_not_wired`, `service_error`, `latest_text_status_not_ok:...`,
etc.).

## "No alerts but healthy" vs "broken"

**Healthy quiet:**
- detection queue drains (`pending` < few thousand);
- `watchtower_detection_claimed_total` rising;
- `polymarket_alerts.created_at` last row within last few hours;
- no `pending` alerts stuck more than 5 minutes;
- AI request logs show recent `success` rows.

→ The market is quiet. The thresholds are doing their job.

**Broken:**
- pending backlog grows without bound;
- `polymarket_alerts` empty for >24h on a busy day;
- `ai_request_logs` shows `quota_exceeded` repeatedly;
- detection worker emits `claimed=0` for hours despite `pending`
  > 0 → claim query is wrong.

## Inspecting AI failures fast

```sh
# CLI shortcut
go run ./cmd/cli diagnose-ai -dsn "$POSTGRES_DSN" -limit 20

# Or the targeted queries above
```

Look for:
- `error_category='quota_exceeded'` → top up billing.
- `error_category='rate_limited'` → bump `AI_ANALYSIS_RATE_LIMIT_PER_MIN` or check parallel workers.
- `error_category='timeout'` → raise `AI_ANALYSIS_TIMEOUT`.
- `error_category='validation_failed:provider_error_text'` → the
  provider returned 200 with an error JSON body. Investigate
  upstream.
- `error_category='unauthorized'` → key revoked.

## Detection queue health

```sql
SELECT
  count(*) FILTER (WHERE detection_status='pending')        AS pending,
  count(*) FILTER (WHERE detection_status='claimed')        AS claimed,
  count(*) FILTER (WHERE detection_status='analyzed')       AS analyzed,
  count(*) FILTER (WHERE detection_status='skipped')        AS skipped,
  count(*) FILTER (WHERE detection_status='failed')         AS failed
FROM polymarket_trades;
```

Healthy ratios depend on volume; what matters:
- `pending` not growing forever;
- `failed` not growing forever (= recurring panic);
- `skipped` mostly `too_old_for_live_alert` after a restart, NOT
  `market_unknown` (which means discover and detect disagree).

## Market-intelligence health

```sql
-- Are reports being SENT?
SELECT status, count(*) FROM polymarket_market_intelligence_reports
 WHERE created_at >= NOW() - INTERVAL '24 hours' GROUP BY status;

-- Are they UNIQUE per period?
SELECT period_key, count(*) FROM polymarket_market_intelligence_reports
 GROUP BY period_key HAVING count(*) > 1;
```

Metric: `watchtower_market_intelligence_skipped_total{reason}`
distribution tells you why a period was suppressed.

## Safe tuning methodology

DO NOT eyeball thresholds.

1. **Capture baseline.** Run `diagnose-alerts -lookback 24h
   -show-candidates 50` against the CURRENT env. Save output.
2. **Measure.** Run the SQL queries in this doc against 7 days
   of data. Note Info/Warning/Critical rates per day.
3. **Predict.** Edit ONE threshold; re-run `diagnose-alerts`;
   compare projected fire counts.
4. **Apply.** Update env. Restart. Watch the first 24h.
5. **Iterate.** Repeat against the next bottleneck (= the noisiest
   strategy + reason).

The `presets/{conservative,balanced,aggressive}.env` files are
your reference points. Conservative = pager-grade, balanced =
operator-channel, aggressive = local exploration.

Tuning the production profile (`.env.prod`) is a separate
investment — see `doc/project/tuning-methodology.md`.
