# Strategy philosophy (AI-oriented)

> WHY each strategy exists, not HOW it's implemented.
> For implementation see `doc/strategies/current-strategy-map.md`.

## Mental model

The system is a **stack of independent detectors** writing to one
queue (`polymarket_alerts`). Each detector targets a distinct
suspicious-flow shape. Together they recover precision through
**diversity of evidence** — a trade that fires on multiple
detectors is more interesting than one that fires on one.

## What is statistically suspicious on Polymarket

Ordered by signal strength (per research consensus + operator
feedback):

1. **Late-market directional accumulation** — one wallet, one side,
   many trades, narrow time window, lifecycle ≥ 85%.
   *Why:* persistence + low time-left + commitment. Hardest to
   fake; most informative.
2. **Trader-tail + market-tail overlap** — trade sits in the
   wallet's OWN history p95 AND the per-(category, market, outcome)
   distribution p95.
   *Why:* rare on both axes simultaneously rules out "this wallet
   always trades big" AND "this market always has big trades".
3. **Ownership concentration** — single wallet holds ≥ 10% of net
   BUY-share count on an outcome.
   *Why:* skin in the game. Cannot be faked without real capital.
4. **Multi-wallet cluster** — ≥ 3 anomalous trades from ≥ 2 unique
   wallets in one category window.
   *Why:* independent agreement, not one wallet being weird.
5. **Stable favorite + meaningful remaining return** — late
   lifecycle, narrow band (0.55-0.85), low recent volatility,
   non-trivial liquidity.
   *Why:* convergence candidate; the market has decided.
6. **Quiet-market wake-up** — historically inactive (market,
   outcome) suddenly sees a sized trade.
   *Why:* asymmetric signal — silence broken on purpose.
7. **Dormant wallet revival** — wallet with stored history but
   long idle period suddenly places a sized trade.
   *Why:* long-lived account "woke up" near a binding event.

## What is NOT a strong signal

- **Isolated large trade** — could be retail FOMO, MM, churn.
  Standalone notional ≠ informed flow. The scorer requires
  baseline-relative rarity, not just size.
- **Meme / celebrity / coinflip markets** — high noise, weak
  asymmetry. Category whitelist exists for this reason.
- **Balanced BUY/SELL same wallet** — market-making, hedging, or
  closing. MM filter exists for this reason.
- **High multiplier on tiny-liquidity market** — `5000×` on a
  market with $200 volume is data noise. Low-baseline severity
  cap exists for this reason.
- **Trade on a market with concurrent opposite-side flow** —
  conflicting flow within 24h drops conviction; the AI prompt
  receives same_market_opposite_side_notional and biases toward
  Watch/Unclear.

## Why directionality matters more than size

A `$100k` trade is **one decision**. A `9 × $4k` accumulation
line over 18h is **nine decisions**, all the same direction, by
the same wallet, near resolution. The probability that nine
independent retail/MM decisions converge on one direction is
much lower than that of one large impulsive bet. Persistence is
the load-bearing signal in informed-flow research.

## Why new wallets are not standalone signals

A 7-day-old wallet placing $20k on a politics market is
suspicious — but only as context, never as the trigger. The
detector layer stamps `NewWalletRef` on existing single-trade
and accumulation Findings; no `kind=new_wallet` alert exists,
intentionally. New-wallet alerts on their own produce too many
false positives (retail with first account funded, MMs splitting
wallets, etc.).

## Why ownership concentration matters

Polymarket's net-BUY share fraction is the closest the system
can get to "holdings" without an upstream holders endpoint. A
single wallet holding 15% of an outcome's net BUY shares has
**committed capital proportional to their conviction**. That
capital cannot be faked, hedged elsewhere, or unwound without
moving the price.

The strategy explicitly labels this an APPROXIMATION (no
upstream holders feed) so the Telegram body never overclaims.

## Why opposite-side flow weakens conviction

If wallet A accumulates YES while wallet B accumulates NO on
the same market in the same 24h window, **at least one of them
is wrong**. The system cannot tell which. The honest answer is
"conflicting flow, treat as Watch" — explicitly surfaced via
the cross-flow context fields the AI receives.

## Why MM suppression is critical

Polymarket has heavy market-making activity. Two-sided
(BUY+SELL) activity by the same wallet inside a short window
is a near-perfect MM signal:
- `count(BUY) ≥ N AND count(SELL) ≥ N`
- `|buy − sell| / max(buy, sell) ≤ neutrality_tolerance`

Without `mmfilter`, the Info tier is dominated by MM trades.
With it, MM is silently absorbed. The MM filter does NOT apply
to cluster alerts because multi-wallet convergence is meaningful
even when some participants are MMs.

## Why low-baseline traps must be capped

A market with only 5 prior trades has a meaningless median. A
$10k trade is "1000× the median of a sparse list" — the
multiplier is numerically true but operationally garbage.
Without the low-baseline cap (`LOW_BASELINE_CAP_ENABLED=true`)
the Critical tier fills with these traps. With it, severity is
clamped at Info unless the trade clears the Critical absolute
floor on its own merit.

## Expected false positives

- **Backfill replay on restart** — `LIVE_ALERT_MAX_LAG` belt
  drops these. Without it, a restart on a year of trades would
  fire every historical anomaly as if it were new.
- **MM activity that just misses the neutrality threshold** — a
  wallet at 80%/20% BUY/SELL on a (market, outcome) is borderline.
  Some leak through. Tuning `MM_NEUTRALITY_TOL` is the lever.
- **Quiet-market wake-ups that are just retail FOMO** — the tag
  is context only; the underlying alert must qualify on its own.

## Expected false negatives

- **First sized trade by a brand-new wallet** with no history —
  the trader axis is unready. Market axis must catch it alone.
- **Genuine informed flow on a market with shallow stored
  history** — baseline readiness gates may suppress. Operator
  accepts the tradeoff (less precision risk).
- **Slow accumulation that crosses the `recent` window** —
  caught by the `lifetime` window instead.

## Precision/recall tradeoffs

The product is **precision-first**. Each suppression below is
a deliberate recall sacrifice:

| Suppression | Recall sacrificed | Precision gained |
|---|---|---|
| MM filter | borderline market-makers that are also occasionally informed | huge — MM is the noise tier |
| Lifecycle ≥ 75% | early-market signal (rarely informed) | huge — early markets are mostly speculation |
| Low-baseline cap | thin-market dramatic multipliers | huge — eliminates fake-multiplier alerts |
| Category whitelist (Politics) | sports / culture / meme signal | huge — keeps signal density high |
| `LIVE_ALERT_MAX_LAG` | trades older than the lag | non-negotiable; backfill replay is unactionable |

## Known weaknesses (in the product as built)

1. Ownership share is **trade-flow approximation**, not real
   holdings. Explicitly labelled.
2. Cross-flow context fields are **read at AI prompt time but not
   yet wired into a hard suppressor**. A high opposite-side
   notional currently tags but does not block.
3. Web-search public-context check is **scaffolded but disabled**.
   The AI gets `public_context: NOT checked` and includes the
   canonical disclosure.
4. `StrategyIdentity = informed-flow-v6` lags the actual scorer
   generation (v7/v8 scoring changes did not bump the dedup
   namespace). Bumping has a one-time re-alert cost; not done.
5. No replay-against-historical-outcomes harness for hyper-tuning.
   PAL dashboards measure forward; backtesting is manual.

## How a future AI should think about adding a strategy

Before proposing a new strategy, answer in order:

1. What suspicious-flow shape is this?
2. What public-research evidence supports it being a real
   signal vs. noise?
3. What false-positive class will it produce?
4. What interaction does it have with the existing detectors?
   (Will it stack with accumulation? Be eaten by the MM filter?
   Suppress it intentionally?)
5. What dedup namespace?
6. What suppression rules?
7. What test pins the canonical fire shape?
8. What test pins the canonical NO-fire shape?

If any answer is "I'm not sure", do not add the strategy. The
default is **fewer detectors, better tuned**.
