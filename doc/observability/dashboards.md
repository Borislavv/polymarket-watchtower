# Grafana dashboards

The main dashboard JSON lives at
`deploy/grafana/dashboards/watchtower-main.json` (UID
`watchtower-main`). It is provisioned at container startup; the
Telegram alert links deep-link to it via `GRAFANA_DASH_UID`.

For panel details see the dashboard's accompanying README at
`deploy/grafana/dashboards/watchtower-main.md`. This document
covers what panels should exist, not the JSON itself.

---

## Required sections

### Pipeline health

- Trades ingested per minute (collect + backfill).
- Detection queue: pending vs claimed vs analyzed (last 24h).
- Detection lag (`NOW() - MAX(detected_at)`).
- Backfill status mix (pending / running / completed / partial /
  failed).

### Alerts

- Created per minute by severity.
- Sent vs failed by severity.
- Mean time from alert.created_at to alert.sent_at.
- MM suppression rate by category.
- Lifecycle-unknown skips.

### AI · Analysis health (NEW section)

Panels:

1. **AI requests by status (stacked rate)** —
   `sum by (status) (rate(watchtower_ai_analysis_requests_total[5m]))`.
2. **AI failures by category** —
   `sum by (reason) (rate(watchtower_ai_request_errors_total[5m]))`.
3. **Quota exceeded (singlestat / counter)** —
   `sum(increase(watchtower_ai_quota_exceeded_total[1h]))`. Alert
   threshold: > 0 → notify operator (billing).
4. **AI latency p50/p95 (heatmap or two lines)** —
   `histogram_quantile(0.5, sum by (le)(rate(watchtower_ai_analysis_latency_seconds_bucket[5m])))`
   and 0.95.
5. **Token usage by kind (stacked)** —
   `sum by (kind) (rate(watchtower_ai_analysis_tokens_total[5m]))`.
6. **Estimated cost (running total)** —
   `watchtower_ai_analysis_estimated_cost_usd_total`.
7. **Analyses persisted (success counter)** —
   `sum by (target_kind) (rate(watchtower_ai_analysis_persisted_total[5m]))`.
8. **Analyses rejected by sanity check** —
   `sum by (reason) (rate(watchtower_ai_analysis_rejected_total[5m]))`.
   Expected near-zero in v8.1; spike = provider regression.
9. **Alert-AI success ratio** —
   `sum(rate(watchtower_ai_analysis_persisted_total{target_kind="alert"}[5m]))
   / sum(rate(watchtower_ai_analysis_requests_total[5m]))`.
10. **Market-intelligence skip reasons (stacked)** —
    `sum by (reason) (rate(watchtower_market_intelligence_skipped_total[1h]))`.

### Signal quality (PAL)

Existing panels under `PAL · …` headings in the dashboard JSON;
see CLAUDE.md §Grafana dashboard for the source-of-truth list.

### Backfill

`watchtower_backfill_runs_total{status}` and pages-fetched rate
panels — already in `deploy/grafana/dashboards/watchtower-main.json`
ids 50-57.

---

## Where to add the new AI panels

The dashboard JSON currently has ids 0-57. Append the AI panels at
ids 70+ to avoid renumbering. Cluster them under a row titled
`AI · Analysis Health` immediately above the PAL section so an
operator scrolling the dashboard sees pipeline → alerts → AI →
signal quality.

This document intentionally does NOT include the JSON snippets —
adding them inline would couple the doc to the dashboard format
and we want the JSON to be the single artefact maintained.
Operators wiring up the new panels should:

1. Open `watchtower-main.json` in the Grafana UI.
2. Add panels using the queries above.
3. Export to JSON and replace the file.
4. Commit.

A separate `deploy/grafana/dashboards/ai-health.json` would also
be acceptable if the operator wants the AI view as a standalone
dashboard.
