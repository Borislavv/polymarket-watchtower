# Presets

Three opinionated env overlays under `presets/`. Pick one and either source
it before starting the binary or supply it as a docker `--env-file`. The
file `presets/README.md` is the operator-facing quickstart; this document
explains *why* each preset is shaped the way it is.

## At a glance

| Preset         | Notional floor (Info) | Lifecycle gate | Sub-cluster floors            | Intended use |
|----------------|-----------------------|----------------|-------------------------------|--------------|
| `conservative` | $50,000               | 85 / 95        | 10k notional, odds 8, mul 250 | Pager-grade — strongest, latest-stage anomalies only. |
| `balanced`     | $10,000               | 75 / 90        | 3k notional, odds 5, mul 100  | Day-to-day product alerting (default). |
| `aggressive`   | $2,500                | 60 / 85        | 1k notional, odds 3, mul 50   | Local exploration / debugging — expect noise. |

## Design principles

All three presets follow the same shape, just at different magnitudes:

1. **Multiplier floors scale with the absolute floors** — a preset that
   raises `ALERT_INFO_MIN_NOTIONAL_USD` also raises the multiplier floors so
   the conservative-MIN composition keeps the same precision/recall balance.
2. **`HardPromotionA` is volume-led**, `HardPromotionB` is odds-led — the
   two branches express two distinct insider shapes (big bet at moderate
   odds vs. moderate bet at extreme odds). Keeping them parameterised
   separately lets a preset tune precision on each.
3. **`HugeWhale` rescues raw-size**, `MegaWhale` catches absurd-size — the
   rescue floor scales 5–10× the conservative preset's Critical floor.
4. **Lifecycle gates harden with the preset** — `conservative` only fires in
   the final 15% of a market's life, `aggressive` opens the gate at 60%.
5. **Sub-cluster floors track the single-trade floors** — at all three
   presets the sub-cluster admits trades roughly one rung below the
   single-trade `Info` notional. This keeps split-wallet detection
   proportional to the operator's overall sensitivity.
6. **Cluster trigger hardens with the preset** — `conservative` wants 5
   trades from 3 wallets totalling $100k; `aggressive` accepts 2/2/$5k.

## Lifecycle gates and `MARKET_MIN_AGE`

The lifecycle gate is the single biggest knob. The product's working
hypothesis is that real insider activity concentrates in the final stretch
of a market's life — when the resolution event is imminent and the asymmetric
information has not yet been priced in. The conservative preset hard-codes
that hypothesis (85% / 95%); the aggressive preset relaxes it for
exploration. `MARKET_MIN_AGE` is a separate floor that blocks alerts on
freshly-listed markets regardless of lifecycle percent — it exists because
brand-new markets with sparse trade history can produce wild multipliers
that look anomalous but are statistical noise.

## When to switch presets

- **Switch to `conservative`** if Telegram is alerting > 1×/hour and you
  want every alert to be pager-worthy.
- **Switch to `aggressive`** if you are exploring a new category or want to
  validate that the pipeline is producing alerts at all on a low-activity
  market list.
- **Stay on `balanced`** for production. Customise individual env vars
  inline if a specific tier needs tuning rather than swapping presets.

## What presets do *not* tune

`POLYMARKET_*`, `RL_*`, `GAMMA_*`, `TELEGRAM_*`, `GRAFANA_*`,
`MAX_MARKETS`, `DISCOVER_*`, `COLLECT_*` — infrastructure / wiring rather
than detection sensitivity. Set those in `.env` or in deploy YAML.
