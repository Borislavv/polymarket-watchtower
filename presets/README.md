# Alerting presets

Three opinionated overlays of the watchtower's anomaly + lifecycle env vars.
Pick one and either source it before starting the binary or supply it as a
docker `--env-file`.

| Preset | Notional floor (Info) | Multiplier floor (Info) | Lifecycle gate | HOT marker | Intended use |
|---|---|---|---|---|---|
| `conservative` | $50,000 | 500× | 85% | 95% | Pager-grade: only the strongest, latest-stage anomalies. |
| `balanced` (default) | $10,000 | 100× | 75% | 90% | Day-to-day product alerting. |
| `aggressive` | $2,500 | 30× | 60% | 85% | Local exploration / debugging; expect noise. |

Common rules across all three:

- `BASELINE_WINDOW` is a *cap* on how much history to keep, never a
  requirement that a market must be that old. A 1-month market with the
  default 1-year window uses the 1 month of available history.
- Alerts only fire when the market is in the second half of its lifetime,
  cleared the `MARKET_MIN_AGE` gate, and the observed baseline span has at
  least `BASELINE_MIN_READY_WINDOW` of data.
- The Telegram alert shows the **actual** baseline span used, not the cap.
- Severity is the conservative MIN of the absolute and multiplier ladders.
  `ALERT_HARD_A_*` and `ALERT_HARD_B_*` are two independent OR branches that
  promote to HARD on insider-grade trades. `ALERT_HUGE_WHALE_*` rescues raw-
  size bets that conservative-min would under-classify; `ALERT_MEGA_WHALE_*`
  catches absurd-size outliers.
- `SUB_CLUSTER_*` admits sub-threshold trades (below the single-trade absolute
  floor) into a per-category window; enough distinct wallets totalling enough
  USD trigger a HARD `WhaleClusterDetected` alert. Split-wallet patterns that
  no single trade could surface are caught here.
- `CATEGORY_BLACKLIST` filters by category slug+label. `MARKET_KEYWORD_BLACKLIST`
  filters by market title / slug / event-slug — separate from the category
  list so adding a term to one cannot silence the other.

How to apply:

```bash
# local
set -a && source presets/conservative.env && go run ./cmd/app

# docker compose (in deploy/docker-compose.yml's watchtower env_file:)
env_file:
  - ../presets/balanced.env
```
