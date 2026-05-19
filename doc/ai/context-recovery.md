# Context recovery — bootstrapping a fresh AI session

> If you're an AI agent landing on this repo without memory: this
> is your fast-path. The script `scripts/generate-chatgpt-context.sh`
> bundles these for paste.

---

## Minimum viable context (paste into the agent)

```
doc/ai/chatgpt-handoff.md
CLAUDE.md (alerting strategy + decisions sections)
```

That alone gets you to ~80% of correct behaviour on most tasks.

## Recommended reading order (cold start, ~10 minutes)

```
1. doc/ai/chatgpt-handoff.md       — AI-to-AI compact
2. doc/ai/project-philosophy.md    — WHY this exists
3. doc/ai/strategy-philosophy.md   — which signals matter and why
4. doc/ai/runtime-mental-model.md  — runtime behaviour
5. doc/ai/current-state.md         — what is true today
6. doc/strategies/current-strategy-map.md  — per-strategy gates + matrix
7. doc/project/operator-guide.md   — SQL + metrics runbook
8. doc/project/tuning-methodology.md — DB-evidence-based tuning
9. CLAUDE.md                        — decisions log
```

## Deep onboarding (cold start, ~30 minutes)

After the above, read code in this order:

```
1. internal/domain/model/anomaly/finding.go   — the data envelope
2. internal/domain/model/anomaly/rule.go      — reason codes + tiers
3. internal/app/usecase/detect/detect.go      — the fan-out
4. internal/app/usecase/analytics/score/      — single-trade scorer
5. internal/app/usecase/analytics/accumulation/ — line detector
6. internal/app/usecase/analytics/stablefavorite/ — convergence detector
7. internal/app/usecase/aianalysis/            — AI orchestration
8. internal/infra/ai/openai/                   — typed ProviderError
9. internal/app/app.go                         — full wiring
```

## Verifying the live system is healthy

After reading docs, validate against the running binary:

```bash
# Build + tests
go build ./...
go test ./...
go test -race ./...
go vet ./...

# If a Postgres DSN is reachable, smoke-check the queue:
go run ./cmd/cli diagnose-ai -dsn "$POSTGRES_DSN" -limit 20
go run ./cmd/cli diagnose-alerts -dsn "$POSTGRES_DSN" -lookback 24h -show-candidates 20
```

## SQL checks that prove understanding

The operator runbook (`doc/project/operator-guide.md`) lists
every health-check query. The four most diagnostic:

```sql
-- 1. Detection queue distribution
SELECT detection_status, count(*) FROM polymarket_trades GROUP BY 1;

-- 2. Alert mix today
SELECT severity, kind, count(*) FROM polymarket_alerts
 WHERE created_at >= NOW() - INTERVAL '24 hours' GROUP BY 1,2;

-- 3. AI call success ratio last hour
SELECT status, count(*) FROM polymarket_ai_request_logs
 WHERE created_at >= NOW() - INTERVAL '1 hour' GROUP BY status;

-- 4. Period dedup proof (must always be 1)
SELECT period_key, count(*) FROM polymarket_market_intelligence_reports
 GROUP BY period_key HAVING count(*) > 1;
```

## Metrics worth pulling first

```
sum(rate(watchtower_telegram_alerts_sent_total[5m]))     -- delivery health
sum by (reason) (rate(watchtower_market_intelligence_skipped_total[1h]))
sum by (status) (rate(watchtower_ai_analysis_requests_total[5m]))
sum(increase(watchtower_ai_quota_exceeded_total[1h]))    -- billing alarm
sum by (reason) (rate(watchtower_filter_alert_mm_suppressed_total[5m]))
```

## Logs worth grepping first

```
"detection pipeline wired"            -- must show detect_mode=db_queue
"alertsender: ai enricher wired"      -- AI is hooked up
"ai alert analysis: completed"        -- AI is producing real output
"ai alert analysis: no note attached" -- with reason= field, why elided
"market intelligence skipped: ai_unavailable"
"backfill: tick summary"              -- backfill drain rate
"detection: tick summary"             -- detection queue drain rate
```

## How to verify signal quality quickly

```sql
-- PAL low-bucket calibration (Strategy success ≠ luck)
SELECT
  d.lifecycle_bucket,
  d.strategy_family,
  count(*) AS alerts,
  count(*) FILTER (WHERE a.outcome_status='resolved_correct') AS won,
  count(*) FILTER (WHERE a.outcome_status='resolved_wrong')   AS lost
FROM polymarket_alert_strategy_dimensions d
JOIN polymarket_alerts a ON a.id = d.alert_id
WHERE a.created_at >= NOW() - INTERVAL '60 days'
GROUP BY 1,2 ORDER BY alerts DESC;
```

A healthy build shows `won` > `lost` in the higher
lifecycle_bucket / accumulation strategy_family rows.

## How to understand current tuning quickly

```bash
# What profile is the binary actually using?
ls -la presets/ .env .env.prod

# Which knobs differ from balanced?
diff presets/balanced.env .env

# What would the current config fire if rolled forward 24h?
go run ./cmd/cli diagnose-alerts -lookback 24h -show-candidates 20
```

## What "fast-path" looks like

If the operator asks "is everything OK?":

1. Grep startup logs for `"detection pipeline wired"
   detect_mode=db_queue`.
2. Run query 1 above; pending should not be growing.
3. Run query 3 above; success ratio > 0.9.
4. Run query 4 above; should return zero rows.
5. Open Grafana watchtower-main dashboard; AI · Analysis Health
   row should not show quota_exceeded spikes.

If all five are clean → "system is healthy". Otherwise pick the
broken one and go deep with the operator-guide runbook.

## What "deep onboarding" looks like

If the operator asks for a strategy change or threshold tuning:

1. Read `doc/ai/strategy-philosophy.md` for the conceptual
   framing.
2. Read `doc/strategies/current-strategy-map.md` for the current
   gates.
3. Read `doc/project/tuning-methodology.md` for the procedure.
4. Run the relevant Q1-Q8 query against the live DB.
5. Propose ONE threshold change with explicit reference to the
   query output.
6. Validate via `diagnose-alerts`.
7. Submit.

## Refreshing this context

If you change behaviour, also update:

| Change | File to update |
|---|---|
| New strategy / detector | `doc/strategies/current-strategy-map.md`, `doc/ai/strategy-philosophy.md`, `doc/ai/chatgpt-handoff.md` strategy table |
| New env var | `doc/ai/chatgpt-handoff.md` defaults table + `presets/*.env` |
| New metric | `doc/observability/ai-metrics.md` + `doc/observability/dashboards.md` |
| New table | `doc/project/architecture-map.md` DB table map |
| Architectural change | `doc/ai/runtime-mental-model.md` + `doc/project/runtime-flow.md` |
| Validation / suppression change | `doc/ai/strategy-philosophy.md` known weaknesses |
| Bug fix that's a load-bearing invariant | `doc/ai/chatgpt-handoff.md` invariants list |
| Change in current state | `doc/ai/current-state.md` |

The script `scripts/generate-chatgpt-context.sh` produces two
artifacts (compact + full); regenerate after big updates.

## Risks of context drift

The docs are NOT auto-generated from code, so they CAN go stale.
Mitigations:

1. **Tests pin the load-bearing invariants** — code that changes
   behaviour breaks a test, prompting a doc update.
2. **The current-state.md and chatgpt-handoff.md files are the
   first to update** because they're explicitly snapshot
   documents.
3. **The script output is reviewed before a real handoff** —
   the operator pastes the compact context AND eyeballs it.
4. **No file is "authoritative on its own"** — every claim about
   behaviour is also pinned by a test or a metric, so an AI that
   wants to verify can grep for the test or hit Prometheus.

If you spot a doc/code contradiction: the code wins; open a PR
to reconcile. Do NOT update the code to match a stale doc.
