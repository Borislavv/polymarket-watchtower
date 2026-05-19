# AI observability — metrics + log events

The canonical reference for "is AI working, and where is it
failing?". Source code: `internal/infra/metrics/metrics.go` +
`internal/app/usecase/aianalysis/aianalysis.go`.

---

## Metric inventory (current binary)

| Metric | Labels | When it increments |
|---|---|---|
| `watchtower_ai_analysis_requests_total` | `status={ok\|skipped\|error}` | every `analyzer.Analyze*` call |
| `watchtower_ai_analysis_skipped_total` | `reason` | analyzer returned `Status=skipped` |
| `watchtower_ai_analysis_tokens_total` | `kind={prompt\|completion}` | on success |
| `watchtower_ai_analysis_estimated_cost_usd_total` | — | on success |
| `watchtower_ai_analysis_latency_seconds` | — (histogram) | end-to-end |
| `watchtower_ai_request_errors_total` | `kind`, `reason` | non-OK outcome |
| `watchtower_ai_analysis_persisted_total` | `target_kind` | row written to `polymarket_alert_analyses` |
| `watchtower_ai_analysis_rejected_total` | `target_kind`, `reason` | sanity rejection (`empty_text` / `provider_error_text`) |
| `watchtower_ai_quota_exceeded_total` | `provider`, `model` | provider HTTP 429 + `insufficient_quota` |
| `watchtower_market_intelligence_skipped_total` | `reason={empty_report\|duplicate_period\|ai_unavailable}` | 2h scout suppressed without delivery |

### Migration notes (v8 → v8.1)

- `watchtower_ai_analysis_*` (pre-existing) remain the high-level
  call-count, latency, and cost view.
- `watchtower_ai_analysis_persisted_total{target_kind}` and
  `watchtower_ai_analysis_rejected_total{target_kind, reason}`
  are NEW — they answer the operator's "is the data table
  actually getting good rows?" question without joining DB tables.
- `watchtower_ai_quota_exceeded_total{provider, model}` is the
  billing alarm. Quota is operator-actionable, not a transient
  slow-down — surfacing it on its own counter avoids burying it
  in the generic `request_errors_total` stream.
- Existing labels on `*_requests_total` etc. are NOT being
  expanded to add `target_kind/provider/model` (would invalidate
  every existing Grafana panel). Future work: add those labels
  in a new metric family and deprecate the older one.

## Log events

Canonical strings — grep these in production:

| Event | Severity | Fields | Source |
|---|---|---|---|
| `ai alert analysis: started` | info | alert_id, kind, severity | alertsender |
| `ai alert analysis: completed` | info | alert_id, status=ok, verdict, text_len, model, latency_ms | alertsender |
| `ai alert analysis: skipped` | info | alert_id, reason, model, latency_ms | alertsender |
| `ai alert analysis: failed` | warn | alert_id, error, model, latency_ms | alertsender |
| `ai alert analysis: attached to telegram alert` | info | alert_id, text_len | alertsender |
| `ai alert analysis: no note attached` | info | alert_id, reason | alertsender |
| `ai request completed` | info | alert_id, model, latency_ms, prompt_tokens, completion_tokens, text_len | aianalysis |
| `ai request failed` | warn | alert_id, category | aianalysis |
| `ai request skipped` | info | alert_id, reason | aianalysis |
| `ai output failed validation; not persisting as analysis` | warn | alert_id, category | aianalysis |
| `ai analysis enabled` | info | provider, model, timeout, daily_budget_usd, rate_limit_per_min, telegram_alerts_enabled | startup |
| `ai analysis disabled` | info | reason | startup |
| `ai analysis configured but api key missing — falling back to NoopAnalyzer` | warn | model | startup |
| `alertsender: ai enricher wired` | info | ai_telegram_alerts_enabled | startup |
| `market intelligence skipped: ai_unavailable` | warn | period_key, ai_status, ai_category | marketintel |

## Reading the metric stack

Three questions, three views:

**Is AI being called?**
`sum(rate(watchtower_ai_analysis_requests_total[5m]))` should be
roughly the alert-creation rate (one call per alert before
refresh policy kicks in).

**Is AI producing usable output?**
`rate(watchtower_ai_analysis_persisted_total{target_kind="alert"}[1h])`
divided by total AI calls. A healthy production binary should be
well above 0.9 — the AI rarely returns junk; sanity rejection
should be < 1%.

**Is AI failing for billable reasons?**
`increase(watchtower_ai_quota_exceeded_total[1h])` > 0 → billing
attention required.
`increase(watchtower_ai_analysis_skipped_total{reason="rate_limited"}[5m])`
high → raise rate-limit env or scale down callers.

## Cross-table SQL (the slow path)

For deeper investigations, the DB is authoritative:

```sql
-- AI success rate by hour
SELECT date_trunc('hour', created_at) AS hr,
       count(*) FILTER (WHERE status='success') AS ok,
       count(*) FILTER (WHERE status<>'success') AS fail,
       count(*) AS total
  FROM polymarket_ai_request_logs
 WHERE created_at >= NOW() - INTERVAL '24 hours'
 GROUP BY hr ORDER BY hr;

-- Top failure categories last 24h
SELECT error_category, count(*) FROM polymarket_ai_request_logs
 WHERE status <> 'success' AND created_at >= NOW() - INTERVAL '24 hours'
 GROUP BY error_category ORDER BY count(*) DESC;

-- Cost by model (estimated)
SELECT model, sum(estimated_cost_usd) AS spend_usd, count(*) AS calls
  FROM polymarket_ai_request_logs
 WHERE created_at >= NOW() - INTERVAL '24 hours'
 GROUP BY model;
```
