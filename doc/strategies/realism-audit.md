# Strategy realism audit

> Brutally skeptical pass on every active strategy. Question the
> code; do not assume defaults are good. Output is opinion +
> evidence-requests, not a tuning patch.

For each strategy:

1. What real behaviour is this trying to detect?
2. Does the implementation actually detect that?
3. Would a professional operator care about this alert?
4. Would this realistically catch informed flow?
5. What false-positive class dominates?
6. Verdict.

---

## 1. Single-trade whale flow (`analytics/score`)

**Trying to detect:** a single trade that is BOTH large in absolute
USD and rare against the per-(category, market, outcome) baseline
AND/OR rare against the wallet's own history.

**Implementation actually does:** dual-axis scoring; tier (Info /
Warning / Critical) is the highest where every non-zero floor
clears (notional, odds, profit, p95/p99 ratios). A trade fires if
it qualifies on EITHER market or trader axis.

**Operator care:** at Warning+ yes, at Info questionable. Info-tier
single-trade alerts often fire on $5k-$10k trades that are p95 on a
sparse market — operator finds them noisy. The brief is correct.

**Catches informed flow:** **partially**. A $50k+ Politics trade at
late lifecycle WITH trader-history p95 displacement is the
canonical "shark" signal. A $5k trade at p95-only-by-market-axis is
noise dressed as signal.

**Dominant false-positive class:** trades where the market axis
fires alone because the per-bucket sample is thin (e.g. 30 trades,
median $200, current trade $4k → "20× market median" — true but
operationally meaningless). The low-baseline cap exists to clamp
this; it works but only after the alert is created.

**Verdict:** Info-tier produces low-value alerts. Two structural
improvements would help:
- raise market-axis floors so a thin baseline cannot dominate;
- require multi-dimensional clearance — at minimum the trader
  axis ALSO clears or ownership is non-trivial.

The brief's "composite scoring" prescription targets exactly this.
See `composite-scoring.md`.

---

## 2. Recent accumulation (`analytics/accumulation`, window=24h)

**Trying to detect:** a single wallet repeatedly building exposure
on one (market, outcome, side) within a short window.

**Implementation actually does:** two size paths (meaningful = big
median + total ≥ multiplier × tier notional; many-smalls = total
alone ≥ ManySmallsMultiplier × tier notional). Plus tier gates
(trades count, avg_odds, line_total / market_median).

**Operator care:** **mixed**. Real "9 BUYs of $4k = $36k" lines are
the canonical accumulation signal — operator cares. But "44 trades
of $140 = $6k" is exactly the false-positive class the brief calls
out: it fires under many-smalls if the total clears the tier floor
× ManySmallsMultiplier even though the average trade is tiny.

**Catches informed flow:** **yes when the line is heavy and clean**;
no when the line is many-smalls with low absolute size.

**Dominant false-positive class:** many-smalls bots — retail
copy-trader bots or MM-adjacent strategies that spray 40-100 small
clicks. The trader history baseline does not gate them because
they have history; the market baseline does not gate them because
each trade is tiny.

**Verdict:** the many-smalls path is the weakest link. Should
require ABSOLUTE total notional floor (not just multiplier × tier),
e.g. `ACCUMULATION_MIN_LINE_TOTAL_USD=50000` for Warning. Without
an absolute floor, click-spam clears.

Composite improvements:
- Ownership share gate: require the line to push wallet's ownership
  share ≥ X% of net-BUY shares on the outcome. A click-spam bot
  rarely dominates ownership.
- Directional purity gate: ratio of (BUYs in window) to (total
  same-(market, outcome) activity by this wallet in window) ≥
  e.g. 0.85. MM/rebalancing wallets have lower purity.
- Median trade size floor in many-smalls path: even if many-smalls
  bypasses the meaningful-path median check, set a soft floor
  (e.g. median ≥ $200) to drop click-spam.

---

## 3. Lifetime accumulation (`analytics/accumulation`, window=lifetime)

**Trying to detect:** slow-drip conviction across the wallet's
entire stored history on one (market, outcome, side).

**Implementation actually does:** same detector as recent, different
window. Per-severity dedup keys.

**Operator care:** **higher than recent**. A 90-day same-side line
is harder to fake.

**Catches informed flow:** **yes**. This is one of the strongest
detectors in the stack.

**Dominant false-positive class:** wallets that systematically
provide liquidity over months — total notional looks like
conviction, but they were buying and selling around it. The MM
filter helps but is windowed (`MM_LOOKBACK=24h`), so a wallet that
churns weekly slips through.

**Verdict:** strong detector. Should be paired with a LIFETIME MM
proxy: ratio of total BUY notional to total SELL notional on the
same (market, outcome, side family) — if < e.g. 2.5×, treat as
liquidity-provider, not conviction.

---

## 4. Cluster / HARD (`analytics/cluster`)

**Trying to detect:** multi-wallet directional convergence on a
single category window.

**Implementation actually does:** sliding window with floors on
unique-wallet count, total notional, and per-(category) cooldown.

**Operator care:** **highest when it fires correctly** — multiple
sharks agreeing is the strongest signal in the stack.

**Catches informed flow:** **yes when the wallets are truly
independent**.

**Dominant false-positive class:** 3 wallets where 2 are the same
operator or 2 are MMs on the same market. Wallet independence is
NOT verified — only wallet address distinctness is.

**Verdict:** the cluster gate is too easy at default
`CLUSTER_MIN_UNIQUE_TRADERS=2`. A 3-trade / 2-wallet / $50k cluster
fires on noise. Brief is right: should be `≥ 3 unique wallets,
≥ 4 anomalous trades, ≥ $100k total, p99 displacement per trade,
no MM overlap`. Cluster alerts should feel rare and serious — 0-2
per day target.

Composite improvements:
- Wallet entropy check: if two wallets in the cluster account for
  > 80% of notional, treat as `2-wallet cluster`, not 3.
- Per-trade p99 displacement: every trade in the cluster sample
  must be p99 against its own bucket, not just one outlier
  dragging the others along.
- No-MM overlap: none of the cluster wallets currently match the
  MM-filter shape on this (market, outcome).

---

## 5. Ownership concentration (`analytics/ownership`)

**Trying to detect:** a single wallet has accumulated a meaningful
fraction of net BUY-share count on a given outcome.

**Implementation actually does:** trade-flow approximation
`(wallet_net_BUY / market_total_BUY) × 100`. Tier on the share
percentage.

**Operator care:** **high** when share ≥ 10% on a Politics market.
This is the closest the product gets to "holdings".

**Catches informed flow:** **yes**. The brief is correct that
ownership should be PRIMARY in production. A 15% share with $40k
total is a stronger signal than 44 small trades at $6k.

**Dominant false-positive class:** small markets where 5 BUYers
exist. A $5k trade can look like 20% ownership because the
denominator is tiny. The "Approximate" flag exists but is not a
suppression — operators still see the alert.

**Verdict:** the percentage alone is insufficient; needs an
ABSOLUTE-shares floor. A 30% share of an outcome that has $1k
total BUY volume is data noise; a 10% share of an outcome with
$500k total BUY volume is a real signal. Add
`OWNERSHIP_MIN_MARKET_BUY_VOLUME_USD` gate (e.g. $25k for Info,
$100k for Warning, $250k for Critical).

Brief's prescription "ownership should dominate severity" matches.
Specifically: when ownership share is in Critical tier AND market
BUY volume clears the floor, force severity = Critical regardless
of single-trade scoring.

---

## 6. Stable favorite (`analytics/stablefavorite`)

**Trying to detect:** a late-stage market that has converged on a
favorite in the configurable band with low recent volatility.

**Implementation actually does:** state-driven (per-market scan,
not per-trade). Score = weighted blend (lifecycle / stability /
return / liquidity / no-reversal / cross-market). Severity gates
on score + confidence + lifecycle.

**Operator care:** **medium**. The strategy explicitly does not
claim "safe"; the operator reads it as "late-stage convergence
candidate".

**Catches informed flow:** **no — this is not an informed-flow
detector**. It's a market-state detector. Fine in its lane.

**Dominant false-positive class:** markets with cross-market
ambiguity — e.g. Polymarket says 0.78 YES on candidate X but the
Kalshi paired market says 0.62. v7 removed cross-market as a hard
gate; now the score component is only 5% weight. Could be
under-weighting.

**Verdict:** different lane from the rest. The relaxation in v7
(stricter cooldowns / shorter stability window) was prompted by
the operator finding it noisy. Today's defaults look reasonable.

---

## 7. New-wallet / dormant-wallet context

**Trying to detect:** wallet-age signals that are themselves
ambiguous (could be a sophisticated wallet just funded, could be
retail).

**Implementation actually does:** stamps a context flag on an
existing Finding when wallet age < N days OR history ≤ N trades.

**Operator care:** **low to medium** — useful as tag, useless as
standalone (the implementation correctly never emits standalone).

**Catches informed flow:** **only when paired with a real
detector**. Tag-only is correct.

**Dominant false-positive class:** N/A — tag-only. But the tag is
emitted too liberally — `NEW_WALLET_MAX_HISTORY_TRADES=10`
(default) includes a lot of normal retail wallets. Could tighten
to `≤ 3` or to `≤ 5 AND total prior notional < $1000`.

**Verdict:** correctly scoped (tag-only). Tag emission threshold is
generous.

---

## 8. Quiet-market wake-up

**Trying to detect:** a sized trade landing on a historically quiet
(market, outcome) — silence broken on purpose.

**Implementation actually does:** tag-only context on existing
Findings.

**Operator care:** **medium**. The narrative ("sleepy market just
woke up") is compelling; in practice it overlaps heavily with
"market is small / illiquid".

**Catches informed flow:** **occasionally**. More often catches
the first retail trade on a small market.

**Dominant false-positive class:** illiquid Politics markets where
trade-per-day is low ALL the time — any meaningful trade trips the
gate.

**Verdict:** tag-only mitigates the damage. Tag is sometimes
operator-noise.

---

## 9. Low-baseline severity cap

**Trying to detect:** detector decisions made against thin
baselines — clamp severity.

**Implementation actually does:** when `LOW_BASELINE_CAP_ENABLED`,
caps severity at `LowBaselineSingleMaxSeverity` (typically Info)
unless `LowBaselineAllowCriticalAbsolute` and the absolute floor
clears.

**Operator care:** invisibly correct. Operators don't see this fire
directly; they see fewer fake-multiplier Critical alerts.

**Verdict:** load-bearing. Keep on in all profiles.

---

## 10. MM / bidirectional filter

**Trying to detect:** wallets running both sides on a (market,
outcome) inside `MM_LOOKBACK`.

**Implementation actually does:** suppresses single-trade and
accumulation alerts when count(BUY) ≥ N AND count(SELL) ≥ N AND
notional imbalance ≤ `MM_NEUTRALITY_TOL`.

**Operator care:** invisibly correct (suppression metric is the
only visibility).

**Dominant false-positive class:** wallets at the edge of the
neutrality threshold (e.g. 75% BUY / 25% SELL) — the filter does
not suppress; they slip through accumulation as "directional"
when they're actually rebalancing.

**Verdict:** correct in principle, threshold-sensitive.
`MM_NEUTRALITY_TOL=0.30` (default) suppresses up to a 65/35 split.
Production probably wants 0.40 (suppresses up to ~70/30) to catch
more borderline MMs.

---

## 11. AI Analyst note (`aianalysis.Service`)

**Trying to detect:** N/A — it's enrichment, not detection.

**Implementation actually does:** v8.2 dense prompt with operator-
decision sections; success → `polymarket_alert_analyses`; failure
→ `polymarket_ai_request_logs`.

**Operator care:** **high** — the note answers "would I follow this
side?" rather than restating the alert.

**Verdict:** v8.2 prompt is the right shape. The remaining risk is
the model declining to express opinion on weak signals. Prompt now
licenses that explicitly.

---

## 12. 2h market intelligence

**Trying to detect:** periodic operator briefing.

**Implementation actually does:** candidate selection → AI scout →
Telegram (rendered at send time).

**Operator care:** **medium** — useful for regime awareness.

**Verdict:** appropriately scoped; not a detector. Operator can
mute or shorten without losing detection coverage.

---

## Cross-cutting findings

The recurring theme of this audit: **single-dimension thresholds
under-perform**. Across strategies 1, 2, 4, 5, the dominant
false-positive class is "passed one gate, failed the others". The
brief's prescription — composite multi-dimensional conviction —
is the right next layer. See `composite-scoring.md`.

The second recurring theme: **absolute-USD floors matter more
than multipliers**. A 20× market median on a $200-median market is
$4k — operationally tiny. A 4× market median on a $5k-median
market is $20k — operationally real. Multiplier-only gates favour
thin markets.

The third theme: **ownership share without market-volume context
is noise**. 30% of $1k is one trade; 10% of $500k is a position.
Ownership gate should always be paired with an absolute volume floor.
