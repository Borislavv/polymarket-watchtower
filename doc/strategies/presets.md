# Presets

Three opinionated env overlays under `presets/`. Pick one and either source
it before starting the binary or supply it as a docker `--env-file`.

## At a glance

| Preset         | Notional floor (Info) | Lifecycle gate | Cluster trigger             | Intended use |
|----------------|-----------------------|----------------|-----------------------------|--------------|
| `conservative` | $50,000               | 85% / 95% HOT  | 5 trades, 3 wallets, $100k  | Pager-grade — strongest signals only. |
| `balanced`     | $10,000               | 75% / 90% HOT  | 3 trades, 2 wallets, $50k   | Day-to-day product alerting (default). |
| `aggressive`   | $2,500                | 60% / 85% HOT  | 2 trades, 2 wallets, $5k    | Local exploration / debugging — expect noise. |

## Design principles

All three presets share the same shape, just at different magnitudes:

1. **Multiplier floors scale with the absolute floors.** Raising the
   notional floor without raising the multiplier floor would let medium-
   bet-on-quiet-markets noise slip through.
2. **Lifecycle gates harden with the preset.** `conservative` only fires
   in the final 15% of a market's life; `aggressive` opens the gate at 60%.
3. **Cluster trigger hardens with the preset.** `conservative` wants 5
   trades from 3 wallets totalling $100k; `aggressive` accepts 2/2/$5k.
4. **No promotion overrides.** Earlier iterations stacked HardPromotion +
   HugeWhale + MegaWhale on top of conservative-MIN to escalate single-
   trade severity. That gave 4 ways to reach `hard` for a single trade
   and made tuning opaque. The simplified model caps single-trade severity
   at `critical` and reserves `hard` for the cluster detector. If you want
   a single $1M trade to wake you up, set the Critical thresholds to a
   shape it clears.

## Lifecycle gates and `MARKET_MIN_AGE`

The lifecycle gate is the single biggest knob. The product's working
hypothesis is that real insider activity concentrates in the final stretch
of a market's life — when the resolution event is imminent and the
asymmetric information has not yet been priced in. `MARKET_MIN_AGE` is a
separate floor that blocks alerts on freshly-listed markets regardless of
lifecycle percentage — brand-new markets with sparse trade history can
produce wild multipliers that look anomalous but are statistical noise.

## When to switch presets

- **Switch to `conservative`** if Telegram is alerting more than once an
  hour and you want every alert to be pager-worthy.
- **Switch to `aggressive`** if you are exploring a new category or want
  to validate that the pipeline is producing alerts on a low-activity
  market list.
- **Stay on `balanced`** for production. Tune individual env vars inline
  if a specific tier needs adjustment rather than swapping presets.

## What presets do NOT tune

`POLYMARKET_*`, `RL_*`, `GAMMA_*`, `TELEGRAM_*`, `GRAFANA_*`,
`MAX_MARKETS`, `DISCOVER_*`, `COLLECT_*`, `CATEGORY_BLACKLIST` — these are
infrastructure / wiring rather than detection sensitivity. Set them in
`.env` or in deploy YAML.
