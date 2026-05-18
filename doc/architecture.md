# Architecture

Status: **Phase 1 complete + Phase 2 stages 1–4 shipped.** When
`POSTGRES_DSN` is set the app opens a pool, runs the embedded migrations,
and writes every discovered category/market and every collected trade
through to PostgreSQL alongside the existing in-memory pipeline. The live
detector still reads from in-memory state — the DB-backed read path
(stages 5–8) lands in the next session. See
[doc/persistence.md](persistence.md) for the full stage table.

## Today (Phase 1)

```
                          ┌──────────────────────┐
                          │  gamma /tags         │
                          │  gamma /markets      │
                          │  data-api /trades    │
                          └──────────┬───────────┘
                                     │
   ┌─────────────────────────────────┼─────────────────────────────────┐
   │                                 │                                 │
┌──┴──────────┐                ┌─────┴─────┐                  ┌────────┴────────┐
│ discover    │  registry      │  collect  │  per-trade obs   │  detect.Loop    │
│ (10 m tick) ├───────────────▶│ (60 s     ├─────────────────▶│  baseline rings │
│ whitelist   │  in-process    │  tick)    │                  │  cluster windows│
└─────────────┘                └───────────┘                  └───────┬─────────┘
                                                                     │
                                                          alerts     ▼
                                                          ┌─────────────────────┐
                                                          │ Fanout              │
                                                          │  • LogSink          │
                                                          │  • WebhookSink      │
                                                          │  • TelegramSink     │
                                                          │    (single chat id) │
                                                          └─────────────────────┘
```

- **State is in-process.** Restart loses baselines, cluster windows, and the
  `lastTs` cursor.
- **Backfill is shallow.** First sight of a market pulls the last 24 h of
  trades via the Data API. After that, only deltas since the last seen
  timestamp.
- **Alert dedup is absent.** A restart that re-fetches recent trades will
  re-fire on them.
- **Telegram is single-recipient.** No subscriber discovery, no /getUpdates
  polling. `TELEGRAM_CHAT_ID` is the only recipient.
- **Category selection is a whitelist.** Default `Politics`. Categories not
  in the list are skipped at discover and (defence-in-depth) at detect.

## Tomorrow (Phase 2 — PostgreSQL persistence + worker split)

```
                          ┌──────────────────────┐
                          │  gamma + data-api    │
                          └──────────┬───────────┘
                                     │
        ┌────────────────────────────┼────────────────────────────┐
        ▼                            ▼                            ▼
┌───────────────┐         ┌────────────────────┐       ┌─────────────────────┐
│ CategorySync  │         │ MarketDiscovery    │       │ BackfillWorker      │
│ (10 m)        │         │ (10 m)             │       │ (continuous,        │
│ upsert tags   │         │ upsert markets,    │       │  bounded pool)      │
│ set enabled=  │         │ mark inactive,     │       │ page /trades for    │
│  whitelist    │         │ schedule backfill  │       │ markets with        │
└───────┬───────┘         └─────────┬──────────┘       │ status='pending'    │
        │                           │                  └──────────┬──────────┘
        ▼                           ▼                             ▼
┌──────────────────────────────────────────────────────────────────────────┐
│                              PostgreSQL                                  │
│                                                                          │
│  polymarket_categories  polymarket_markets  polymarket_market_outcomes   │
│  polymarket_traders     polymarket_trades   polymarket_alerts            │
└────────────────────────────────────┬─────────────────────────────────────┘
                                     │
       ┌─────────────────────────────┼─────────────────────────────────┐
       ▼                             ▼                                 ▼
┌──────────────┐         ┌──────────────────────┐         ┌────────────────────┐
│ CollectWorker│         │ DetectorWorker       │         │ AlertSenderWorker  │
│ (60 s)       │         │ (event-driven on     │         │ (1 s drain)        │
│ pull recent  │         │ new-trade)           │         │ ListPending →      │
│ trades, write│         │ load baseline from   │         │ render → Telegram  │
│ to DB        │         │ DB, score, try-insert│         │ → MarkSent         │
│              │         │ dedup-keyed alert    │         │                    │
└──────────────┘         └──────────────────────┘         └────────────────────┘
```

Worker responsibilities are **single-purpose** and communicate via the DB.
That makes each worker individually testable and restart-safe.

## Worker invariants (Phase 2 target)

| Invariant | How |
|---|---|
| context cancellation | every loop selects on `ctx.Done()` |
| bounded concurrency | bounded `chan struct{}` semaphores; no unbounded goroutines |
| backoff on errors | exponential, with caps |
| idempotent writes | `INSERT … ON CONFLICT (dedup_key) DO NOTHING` everywhere |
| no goroutine leaks | wait groups on shutdown |
| upstream rate respected | shared `ratelimit.Limiter` injected per upstream |
| no duplicate alerts | `polymarket_alerts.dedup_key UNIQUE` |
| restart-safe | all state in DB; nothing important in-process |

## Local development

```
docker compose -f deploy/docker-compose.yml up --build
```

Today this starts `watchtower`, `prometheus`, `grafana`. Phase 2 will add a
`postgres` service and run migrations on app boot.

## See also

- [persistence.md](persistence.md) — schema, sqlc layout, repository
  interfaces, migration plan.
- [strategies/single-cluster.md](strategies/single-cluster.md) — single-
  trade severity + cluster rule.
- [strategies/test-scenarios.md](strategies/test-scenarios.md) — canonical
  numeric table the test suite asserts against.
