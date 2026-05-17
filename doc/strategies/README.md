# Detection strategies

Watchtower exposes two anomaly-detection modes via `ANOMALY_MODE`. The product
default is `single_cluster`; `volume` is kept for operators who want the
legacy aggregate-rate behaviour.

| Mode             | Unit of detection         | Alert kinds                              | Driven by                       |
|------------------|---------------------------|------------------------------------------|---------------------------------|
| `single_cluster` | individual trade          | `LargeRareBet`, `WhaleClusterDetected`   | per-trade scorer + cluster      |
| `volume`         | per-market 1-minute slice | aggregate `trade_rate`/`notional_rate`   | rolling-bucket aggregate engine |

The two modes are mutually exclusive — only one detector is wired into the
collect loop at process start.

## Documents

- [single-cluster.md](single-cluster.md) — the primary product path: how
  individual trades are scored, how single-trade alerts and category-cluster
  HARDs interact, and how the split-wallet sub-cluster fills the gap below
  the absolute floor.
- [volume.md](volume.md) — the legacy rate-based detector and when (rarely)
  it is the right tool.
- [presets.md](presets.md) — the three opinionated env overlays
  (`conservative`, `balanced`, `aggressive`), what they tune, and how to
  pick one.
- [test-scenarios.md](test-scenarios.md) — the canonical numeric table the
  detector test suite asserts against. Use it to sanity-check any threshold
  change before shipping.

## Shared concepts

- **Severity ladder:** `info < warning < critical < hard`. HARD is the
  human-review escalation; everything below is informational.
- **Conservative-MIN composition:** when both the absolute (notional+odds)
  and multiplier (notional/baseline-median) ladders qualify a trade, the
  emitted severity is the *lower* of the two — see `score.Score` and
  `anomaly.ConservativeMin`.
- **Override stacking:** on top of conservative-MIN, three escalation rules
  can promote the final severity — `HardPromotion` (A/B branches → Hard),
  `HugeWhale` (→ Critical if not already), `MegaWhale` (→ Hard). They stack
  softest-to-hardest so the strongest signal wins.
- **Baseline window is a cap:** `BASELINE_WINDOW` bounds how much per-bucket
  history is kept; it is *never* a minimum-age requirement on the market. A
  1-month market with the default 1-year window is scored using the 1 month
  of available history. The `BASELINE_MIN_READY_WINDOW` gate is the actual
  freshness check (newest-minus-oldest sample span).
- **Lifecycle gating:** alerts only fire when the market has cleared
  `MARKET_MIN_AGE` *and* is past `LIFECYCLE_ALERT_FROM_PCT` of its lifetime.
  Markets without start/end dates are silenced by default
  (`ALLOW_UNKNOWN_MARKET_LIFECYCLE=false`, fail-closed).
- **Two-list category filtering:** `CATEGORY_BLACKLIST` matches category
  slugs/labels; `MARKET_KEYWORD_BLACKLIST` matches market titles, slugs, and
  event slugs. They are independent on purpose — adding a term to one cannot
  silence the other.
