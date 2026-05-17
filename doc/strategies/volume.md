# `volume` — legacy aggregate-rate detector

`ANOMALY_MODE=volume` keeps the watchtower's original signal: per-market
rolling-window rate-of-volume comparisons against an `AGG_BASELINE_WINDOW`
baseline. It is retained because some operators want a coarse "this market
is suddenly busy" detector that does not depend on per-trade scoring or any
notion of insider shape.

## When to pick it

- Rarely. The unit of detection is the *market* not the *trade*, which is the
  wrong unit for spotting individual abnormal bets.
- Useful for diagnostics: confirming the aggregate engine is wired and the
  rolling buckets behave sanely.
- Useful as a sanity sink for a separate Grafana dashboard that wants to
  show "categories suddenly seeing 30× / 100× / 1000× their typical volume".

## How it fires

The detector watches `recent / baseline` ratios for each market in the
configured `AGG_RECENT_WINDOWS`. A market fires when:

- `recent_window` rate ≥ one of `VOLUME_MULTIPLIERS` × baseline rate, AND
- `recent_window` notional ≥ `VOLUME_MIN_NOTIONAL_USD`, AND
- `recent_window` trade count ≥ `VOLUME_MIN_TRADES`.

Per-market cooldown (`VOLUME_COOLDOWN`) prevents flapping. There is no
category cluster, no sub-cluster, no per-trade enrichment — the alert is a
raw "market X just spiked" signal.

## Trade-offs

- **Pro:** zero dependency on baseline-median quality; works on cold-start
  markets.
- **Con:** no shape information. A burst of small retail bets looks identical
  to a few well-placed whale bets. The product cares about whale shape, so
  `single_cluster` is the default.
- **Memory cost:** sized by `AGG_BASELINE_WINDOW / AGG_BUCKET × MAX_MARKETS`.
  At defaults (168h × 1m × 500 markets) ≈ 5M buckets — fine in RAM but it is
  the reason `AGG_BASELINE_WINDOW` is capped at 168h rather than the
  `single_cluster` reservoir's 1y.

## Don't confuse the two windows

- `BASELINE_WINDOW` — per-(category, market, outcome) reservoir for the
  per-trade detector, capped by `BASELINE_MAX_SAMPLES`. Safe to widen to 1y.
- `AGG_BASELINE_WINDOW` — sizes the in-memory rolling-bucket engine. Widen
  this only after measuring the memory cost.
