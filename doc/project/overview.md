# Watchtower — project overview

A long-term knowledge base entry. Read this first.

---

## What Watchtower is

A Polymarket **market-surveillance and signal-intelligence** service.
It ingests public trade history, scores trades against statistical
baselines and per-wallet history, persists structured alerts when
unusual directional flow appears, and ships short AI-written
operator notes alongside each alert.

The product is a **filter + explainer**:
- raw Polymarket trades → DB (truth source);
- DB → per-strategy detectors → `polymarket_alerts`;
- alerts → AI note → Telegram;
- post-resolution outcomes → signal-quality tracking.

## What Watchtower is NOT

- **not a trading bot.** No order placement, no auto-betting.
- **not an insider detector.** "Insider trading" is a legal claim;
  we never make it. The alert language is "informed-flow candidate"
  / "unusual directional flow" / "whale-like behavior" /
  "late-market convergence" / "asymmetric setup".
- **not a guarantee engine.** Telegram alerts NEVER use "guaranteed",
  "sure thing", "risk-free", "insider". The Telegram formatter is
  pinned by tests to keep this true.

## Operator persona

A single attentive operator (or a small team) who:
- watches a Telegram channel daily;
- reads Warnings/Criticals carefully, Info as a watchlist;
- runs SQL on the DB when something looks off;
- tunes thresholds against the `presets/` env files.

This is the load-bearing assumption: alert volume must respect a
human attention budget, NOT a "stream every whale" firehose.

## Signal philosophy

| Signal | Strength | Why |
|---|---|---|
| Late-stage directional accumulation | strong | persistence + low time-left |
| Persistent same-side conviction | strong | repeated decision, not luck |
| Ownership concentration | strong | one wallet has skin in the game |
| Trader-tail + market-tail overlap | strong | rare on both axes |
| Multi-wallet cluster | strong | independent agreement |
| Quiet-market wake-up | medium | non-noise, but not unique |
| Stable favorite + low volatility | medium | convergence candidate |
| New-wallet large bet | medium | context, not standalone |
| Isolated large trade | weak | could be retail, MM, churn |
| Meme/coinflip markets | very weak | low informational value |
| Balanced BUY/SELL same wallet | noise | usually MM/rebalancing |

The product **explicitly prefers precision over volume**: a smaller
feed of higher-signal alerts beats a firehose of "whale trade $X"
notifications. The MM filter + lifecycle gate + readiness gates
exist for this reason.

## Why persistence matters

Without Postgres, every restart loses baseline state, dedup
disappears, no replay is possible, no outcome tracking, no AI
budget reconciliation. The DB is the single source of truth; the
in-memory mode exists only for local dev. CLAUDE.md is explicit:
production must run with `POSTGRES_DSN` set.

## Why AI analysis exists

Every numeric alert ("$50k buy @ odds 5x") is ambiguous without
context. The AI layer reads the structured Finding plus optional
cross-flow context (same-market opposite-side notional, same-wallet
bidirectional, etc.) and produces a short operator-decision note
("would we consider following this side? — Watch, conflicting flow
last 24h"). AI failures NEVER block the alert; they just elide the
Analyst-note block and land in `polymarket_ai_request_logs`.

## Why proof-of-edge tracking matters

The outcome workers (`outcomes`, `drift`, `outcomeai`) stamp each
sent alert with its post-resolution verdict. Over time this
populates the signal-quality dashboards (`PAL` panels in Grafana)
so an operator can tune thresholds AGAINST OUTCOMES, not against
gut feel.

## Why accumulation matters more than raw notional

Public research (Wired/CFTC investigations, Columbia + Haifa
anomalous-profit work, Polymarket microstructure studies) is
consistent: directional persistence dominates one-shot size as a
predictor. A wallet that BUYs $200 forty times over 18 hours on a
late-stage politics market is a stronger informed-flow candidate
than a single $20k trade. The accumulation detector (`recent` +
`lifetime` windows) exists to capture exactly this shape.

## Why suppression is critical

Polymarket has heavy MM, retail, and meme-market noise. Without
aggressive suppression — MM filter, category whitelist,
lifecycle/age gates, baseline readiness, low-baseline severity cap
— the alert stream becomes useless. Operator burnout = product
failure.

## Current limitations (be honest with users)

- **No live trading.** Surveillance only.
- **No guarantee of edge.** PAL dashboards measure historical
  signal quality; nothing here predicts the future.
- **AI web-search transport not implemented.** `web_context.go`
  scaffold is gated behind `AI_ANALYSIS_WEB_CONTEXT_ENABLED=false`;
  the Responses API HTTP shape needs live verification before enable.
- **Ownership concentration is approximate.** No upstream holders
  endpoint; the percentage is `(wallet net-BUY shares) / (market
  total BUY shares)`. The Telegram body says "approx" explicitly.
- **Strategy identity (`informed-flow-v6`) lags the actual scorer
  generation.** v7/v8 scoring changes did not bump the dedup
  namespace. Bumping is non-zero risk — see
  `internal/domain/model/anomaly/strategy.go`.
