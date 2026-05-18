# Detection strategies

Watchtower exposes two anomaly-detection modes via `ANOMALY_MODE`. The
product default is `single_cluster`; `volume` is retained for operators
who want the legacy aggregate-rate behaviour.

| Mode             | Unit of detection         | Alert kinds                              | Driven by                       |
|------------------|---------------------------|------------------------------------------|---------------------------------|
| `single_cluster` | individual trade          | `LargeRareBet`, `WhaleClusterDetected`   | per-trade scorer + cluster      |
| `volume`         | per-market 1-minute slice | aggregate `trade_rate`/`notional_rate`   | rolling-bucket aggregate engine |

The two modes are mutually exclusive — only one detector is wired into the
collect loop at process start.

## Mental model

For `single_cluster` (the product path):

1. **Filter categories you do not want to monitor.** Sports are excluded
   only by the primary category, never by market metadata.
2. **Only alert on mature markets.** The baseline must have enough real
   data; the market must be at least `MARKET_MIN_AGE` old and in the final
   `100 - LIFECYCLE_ALERT_FROM_PCT`% of its lifetime; the final
   `100 - LIFECYCLE_HOT_FROM_PCT`% is marked `HOT`.
3. **Detect large suspicious single trades.** A trade is suspicious when:
   notional is big enough AND odds are high enough AND it is rare versus
   the per-(category, market, outcome) baseline median. Severity is the
   lower of the absolute (notional+odds) and multiplier (notional/median)
   tier outcomes (`info` / `warning` / `critical`).
4. **Detect suspicious clusters.** When several already-fired single-trade
   alerts converge on one category in a short window (different traders,
   meaningful total notional), the cluster fires `hard` — the highest-
   severity signal, qualitatively different from any single bet.
5. **Alert clearly.** Every alert includes the why, the trade details, the
   trader, and Polymarket / Grafana deep-links.

## Documents

- [single-cluster.md](single-cluster.md) — single-trade scoring, cluster
  rules, gates, the France/FIFA regression case.
- [volume.md](volume.md) — the legacy rate-based detector and when (rarely)
  it is the right tool.
- [presets.md](presets.md) — the three opinionated env overlays
  (`conservative`, `balanced`, `aggressive`).
- [test-scenarios.md](test-scenarios.md) — the canonical numeric table
  every threshold change must agree with before shipping.

## Shared concepts

- **Severity scale** (lowest → highest): `info < warning < critical < hard`.
  Single-trade severity caps at `critical`. `hard` is cluster-only — it
  marks "two or more sharks agreeing", a qualitatively different signal.
- **Conservative-MIN composition:** for a single trade, both the absolute
  (notional+odds) and multiplier (notional/baseline-median) ladders must
  qualify; the emitted severity is the *lower* of the two tier outcomes.
  This keeps precision high: a $1M bet at fair odds isn't `critical` just
  because it's big, and a 10,000× multiplier on a $500 bet isn't
  `critical` just because the ratio is wild.
- **Baseline window is a cap, not a minimum age:** `BASELINE_WINDOW` bounds
  how much per-bucket history is kept; a 1-month-old market with a 1-year
  window is still scored using the 1 month of history available. The
  `BASELINE_MIN_READY_WINDOW` gate is the actual freshness check (newest-
  minus-oldest sample span).
- **Lifecycle gating:** alerts only fire when the market has cleared
  `MARKET_MIN_AGE` AND is past `LIFECYCLE_ALERT_FROM_PCT` of its lifetime.
  Markets with missing start/end dates are silenced by default
  (`ALLOW_UNKNOWN_MARKET_LIFECYCLE=false`, fail-closed).
- **Category whitelist:** `CATEGORY_WHITELIST` matches the category
  `slug + " " + label` and nothing else. ONLY whitelisted categories are
  monitored; everything else is ignored. Market titles, event slugs,
  market slugs, and tags are not scanned. Default: `Politics`.
