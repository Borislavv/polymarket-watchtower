# Polymarket Politics — Strategy Candidates for Watchtower

**Status:** v11.4 research input, not committed code.
**Audience:** Watchtower operators + strategy reviewers.
**Scope:** Politics / Geopolitics / Elections markets on Polymarket.
**Author:** ChatGPT/Claude research synthesis, edits required before promotion.

---

## Executive summary

Watchtower today catches the **loudest** surveillance patterns: large single trades,
single-wallet accumulation lines, multi-wallet cluster convergence, and crude
ownership concentration. These are the easy half of the informed-flow / insider-like
detection problem. The recent **CRS legal explainer** [^crs] and the
**Harvard Corporate Governance** post [^hcg] make the inverse painfully clear:
the most operationally interesting Polymarket surveillance patterns are the **quiet**
ones — pre-event positioning that doesn't show up as a single $50k bet, cohort
behaviour across markets that aren't obviously linked, and underreaction / repricing
lag after public information lands. **dopaminemarkets.com's composite-score writeup** [^dop]
documents a 60+ standard-deviation excess win rate across ~210k flagged
(wallet, market) pairs by combining five signals — bet size cross-section, within-trader
bet size, profitability, pre-event timing, and directional concentration. Watchtower
covers two of those five well, two partially, and one not at all.

**Strongest existing strategies (keep, tune lightly):**
- `single_trade_whale` (the load-bearing detector)
- `accumulation`
- `cluster_convergence` (still high-precision when MM-filtered)

**Weakest / risky (keep but tighten or demote to context-only):**
- `ownership_concentration` (approximation — no holders endpoint; tighten thresholds)
- `mm_suppression` (necessary; the parameters bias toward false negatives on
  legitimate two-sided traders that aren't MMs)
- `low_baseline_cap` (boots-strapping helper; not a primary detector)

**Biggest gaps (operator-facing, not theoretical):**
1. No **pre-event timing** detector beyond "trade existed before alert" — the
   detector doesn't quantify "how much earlier than the public information
   timestamp". This is the #1 academic signal for informed trading [^hcg] [^dop].
2. No **post-event underreaction** detector — Watchtower fires on the bet that
   creates the move, never on the market that hasn't moved enough after a
   confirmed catalyst.
3. No **cross-market wallet cohort** detector — repeated wallets that profit
   together across superficially-unrelated markets are invisible.
4. No **orderbook liquidity-shape** detector — Polymarket's public WSS market
   channel [^pmwss] exposes orderbook depth + price updates in real time;
   liquidity withdrawal before a sharp move is a strong informed-flow tell that
   Watchtower currently ignores.
5. No **wallet reputation** layer — every alert treats every wallet as a fresh
   identity; we don't carry forward "this wallet has hit 23/30 cleanly across
   six elections".

**Recommended next build (after the Market Close Review learning loop):**
**Wallet Reputation v0** (P0). Persist per-wallet rolling hit rate + total
realised PnL + first-seen / last-seen across MARKET_CLOSE_REVIEW outcomes. Use
it as a **context booster on existing alerts** before promoting it to its own
strategy. Justification: cheap, deterministic, uses data Watchtower already
collects, and unlocks four of the seven new strategies below with no extra
upstream calls.

---

## Existing strategy audit

Each table row is opinionated. "Verdict" is `keep / tune / context_only / remove / investigate`.

| Strategy | What it detects | Semantic load | Polymarket-Politics fit | Failure modes | Verdict | Learning-loop metrics |
|---|---|---|---|---|---|---|
| `single_trade_whale` | Single trade whose USD notional crossed either an absolute tier or per-bucket displacement ladder | Direct conviction signal: "someone is willing to put real money down NOW" | **Strong.** Politics markets routinely see $50k–$500k single bets near catalysts. | Lottery-ticket whales who lose; coordinated multi-wallet spoofing | **keep** | `kind=trade_anomaly` CLV6h + market-close confirmed_rate from `watchtower_strategy_quality_v` |
| `accumulation` | Same wallet repeatedly building exposure on same (market, outcome, side) within a window | Inferred conviction over time: "this wallet is committing capital with intent" | **Strong.** Politics has long-tail markets where a single conviction wallet quietly stacks $30k over weeks. | Treasury wallets rebalancing; market-maker that's net long but not informed | **keep, tighten readiness** | Reason quality + accumulation row in strategy_quality view |
| `cluster_convergence` | N anomalous trades by M unique wallets in same category within sliding window | Coordination signal: "the smart crowd is suddenly agreeing" | **Strong** when MM-filtered. The category whitelist (`Politics`) already ensures focus. | False clusters from a single information vendor pushing the same trade idea to many subs (still informative but not insider-like) | **keep** | Cluster firing rate vs reviewed market close rate |
| `ownership_concentration` | Wallet's net (BUY−SELL) share-count crossed % of outcome's total flow | "One wallet effectively controls this side" | **Weak.** No upstream holders endpoint — the % is computed from trade flow, biased high in low-liquidity markets. | False positive in early-life low-liquidity markets; misses true holders | **context_only** | Confirmed-rate in `watchtower_strategy_reason_quality_v` should currently be near baseline |
| `mm_suppression` | Two-sided BUY+SELL on same (market, outcome) within tolerance | **Filter**, not detector | **Necessary.** Without it Polymarket MMs blow out the precision floor. | Suppresses legitimate informed two-sided traders; false negatives on directional+hedge wallets | **keep, tune `MM_NEUTRALITY_TOL` against learning-loop data** | Reverse-lookup: count alerts that fired AFTER mm-filter was relaxed in shadow eval |
| `quiet_market_wakeup` | Tags an alert as quiet-market context when baseline activity is low | **Context booster** | **Useful** in long-tail politics markets that suddenly wake up. | None of consequence — purely additive label. | **keep, never standalone** | Reason quality view |
| `new_wallet` / `dormant_wallet` | Wallet's first-seen < N days or last-seen > N days before this trade | Context boosters | **Promising.** New-wallet conviction in a specific catalyst window correlates with insider risk per [^hcg]. | False positives on wallets that simply rotate addresses | **keep, tune `NEW_WALLET_MAX_AGE`** | Reason quality |
| `low_baseline_cap` | Caps the multiplier ladder for very low-baseline markets | Spam filter | **Necessary** for the long tail. | None of consequence | **keep** | n/a |
| `news_intel` | v11.0 hourly News Intelligence (typed) | News context for actionable repricing | **Strong** in elections / geopolitics — exactly the categories with real catalysts. | News with no probability impact ("election day approaches"); already-priced content. The v11.0 prompt already guards against this. | **keep** | News-intel actionable rate vs close-review confirmed rate |
| `market_close_review` | v11.4 post-resolution AI review | Learning loop, not a live detector | n/a | AI invents context; mitigated by strict-JSON + alert_id whitelist | **keep — this is the new core** | Self-documenting via `watchtower_market_close_review_quality_v` |

---

## Gap analysis

Watchtower does **not** catch (today):

1. **Pre-news flow timing.** We tag "alert fired" and the news intel later
   produces affected markets, but we don't compute the explicit
   "Δt = trade.sent_at − public-information.timestamp" feature that's the load-bearing
   pre-event signal in academic detection [^hcg] [^dop].
2. **Post-news underreaction.** When a confirmed catalyst lands (court ruling,
   sanction, endorsement) and the price barely moves, that's an *opportunity* —
   not the same as "informed flow already there". Watchtower currently fires on
   the bet that creates the move, never on the market that hasn't moved enough.
3. **Cross-market wallet positioning.** A wallet that builds correlated positions
   across multiple superficially-unrelated markets (e.g. "Trump wins" + "GOP
   wins house" + "S&P >5500 by EOY") is invisible to single-market strategies.
4. **Repeated wallet hit-rate.** No wallet-level reputation persisted across
   reviews. The fact that wallet `0xABC...` was 23/30 correct on prior
   resolved political markets carries zero weight in today's alert scoring.
5. **Orderbook imbalance / liquidity withdrawal.** Polymarket's WSS market
   channel [^pmwss] exposes orderbook depth + price updates in real time;
   Watchtower today uses it only for repricing context. Liquidity withdrawal
   (a sudden ask-side cancel before a buy) is a well-documented informed-flow
   tell that we ignore.
6. **Cheap tail-odds whale entries.** A $200k bet at 4% on a tail outcome is
   structurally different from a $200k bet at 50% — payoff asymmetry × stake
   × pre-event timing is the signal, but Watchtower's tier ladders mix the two.
7. **Resolution-criteria ambiguity exploitation.** Several Polymarket markets
   have settled on edge-case wording (e.g. "definitive" / "first" / "by EOY").
   Wallets that systematically enter near resolution-criteria edges are a
   distinct, surveillance-relevant pattern.
8. **Price-move without large trade.** When a price moves 8% in an hour with no
   trade >$5k, that's likely book-side conviction (cancels + small clearing
   trades) — not visible in our trade-stream detectors.
9. **Flow before official-statement publication times.** Government / agency
   announcements have known publication times (FOMC schedule, court calendars).
   Trades clustered in the 30–120 minutes BEFORE those publications are a
   higher-prior insider-like cohort than trades at random times [^crs].
10. **Wallet cohort behaviour.** A handful of wallets that systematically *enter*
    the same market within minutes of each other across many resolutions is the
    cluster signal taken to a higher dimension. Today's `cluster_convergence`
    only catches it inside a single market and single category.

---

## New strategies

Each proposal includes: thesis, market fit, required data, detection logic,
features, gates, scoring, severity, false-positive filters, example signal,
failure modes, backtest plan, metrics, complexity, expected value, comparison
to current strategies, and classification (`production / core / booster /
admin-only / research-only`).

The order below is **rough** P0→P3; the ranked roadmap at the bottom is the
operator-facing prioritisation.

---

### 1. Wallet Reputation v0 (booster → core)

- **Thesis.** Wallets are the most stable identity in a Polymarket market. A
  wallet that has resolved 23 of 30 prior alerts correctly is a different
  prior than a fresh address.
- **Market fit.** Politics resolutions are clean (binary; not subject to
  re-pricing surprises post-settlement). Wallets are pseudonymous but
  consistent across markets.
- **Required data.** Already collected: `polymarket_alerts.trader_id` +
  `outcome_status` + `clv_24h` + (from MARKET_CLOSE_REVIEW)
  `verdict + best_alert_ids/worst_alert_ids`.
- **Detection logic.** Daily aggregator over the last `WALLET_REP_LOOKBACK`
  (default 180d) per wallet → `hit_rate, sample_size, avg_clv_6h, total_realised_pnl,
  first_alert_at, last_alert_at`. Persist in `polymarket_wallet_reputation`
  (one row per wallet, upserted daily).
- **Features.**
  - `hit_rate = resolved_correct / (resolved_correct + resolved_wrong)`
  - `wallet_age_days = now - first_alert_at`
  - `recent_streak = last 5 resolved alerts`
  - `realised_avg_clv_6h`
- **Gates.** `sample_size ≥ 5` AND `wallet_age_days ≥ 14`.
- **Scoring.** Confidence weight on existing alerts: `+0.10` when hit_rate ≥ 0.65,
  `−0.05` when hit_rate ≤ 0.40.
- **Severity.** Booster — never standalone for v0. v1 could fire its own alert
  when a top-decile wallet enters a new market.
- **False-positive filters.** Exclude wallets flagged by mm_suppression in
  >50% of recent alerts.
- **Example signal.** "wallet 0xABC opened $25k YES on Trump-2028 at 0.42;
  wallet's prior reputation: 17 reviewed alerts, hit_rate 0.70, avg CLV6h +3.1%
  → stamp booster reason `new_wallet_high_reputation` on alert."
- **Failure modes.** Survivorship bias on early adopters; wallet rotation; bots
  that mimic top wallets. Mitigations: hit-rate weighted by sample size +
  recency decay.
- **Backtest plan.** Replay last 180d of resolved alerts with hit-rate weighting
  vs without; compare CLV6h means by decile of wallet hit_rate.
- **Metrics.** `watchtower_wallet_reputation_alerts_total{decile,direction}`;
  reputation distribution gauge.
- **Implementation complexity.** **Low.** New table + nightly aggregator +
  read-time join in alertsender. No new upstream calls.
- **Expected value.** **High.** Unblocks 3, 4, 8 below. Surveillance research [^dop]
  identifies "repeated profitable wallet" as one of five canonical features.
- **vs current.** Complements every existing detector; not a substitute.
- **Class.** **core / booster v0**.

---

### 2. Pre-news flow timing detector (production, gated)

- **Thesis.** A trade's *timing relative to the first public mention of the
  outcome-relevant event* is the single sharpest informed-flow signal [^hcg] [^dop].
- **Market fit.** Politics catalysts (court rulings, polling cadence,
  endorsements, sanctions, statements) have public timestamps; we already
  ingest those via `polymarket_event_annotations` + the v11 news intel feed.
- **Required data.**
  - `polymarket_trades.traded_at`
  - `polymarket_event_annotations.first_seen_at` (already persisted)
  - news intel item `first_seen_at`
- **Detection logic.** For each alert, compute
  `Δt = min(annotation.first_seen_at, news_intel_item.first_seen_at) − trade.traded_at`.
  When Δt > `PRE_NEWS_MIN_LEAD` (default 30m) **and** the trade direction
  matches the post-news price move, label the alert `pre_news_lead=true` with
  the Δt minutes.
- **Features.** Δt (signed), Δprice_post_news, lead_decile.
- **Gates.** Annotation must have a price_before / price_after move that
  agrees with the alert direction. Reject when news first_seen_at is later
  than the trade's market-close (we're not predicting the past).
- **Scoring.** Promotes the alert's severity by one tier if Δt ≥ 2h AND price
  moved ≥5% in the alerted direction within 1h of news.
- **Severity.** Critical when promotion criteria all clear.
- **False-positive filters.** Wallets in mm_suppression must NOT be promoted.
  Don't promote if the wallet's reputation is below the median (avoids
  rewarding lottery winners).
- **Example signal.** Trump-2028 YES bought at 0.62 → 2h later news intel
  flags endorsement leak → price moves to 0.71 within 30 min → wallet
  promoted to `critical` with reason `pre_news_lead_2h_+9pct`.
- **Failure modes.** News annotations have noisy timestamps; reverse-causation
  (the trade itself spread the news). Mitigation: require the news source to
  be external (not Polymarket annotation derived from the trade).
- **Backtest plan.** Replay v11 news intel items with linked trades; bucket
  CLV6h by Δt decile.
- **Metrics.** `watchtower_pre_news_lead_total{bucket=...}`.
- **Complexity.** **Medium.** New SQL join + label on the existing alert
  pipeline. Reuses everything we already collect.
- **Expected value.** **High.** Direct surveillance ROI; mirrors academic
  detection methodology [^hcg] [^dop].
- **vs current.** Sharpens `single_trade_whale` + `accumulation`. Doesn't
  replace either.
- **Class.** **production** (after backtest).

---

### 3. Post-news underreaction detector (production)

- **Thesis.** When a confirmed catalyst lands and the market price moves less
  than the catalyst implies, the asymmetric edge sits *with* the news, not
  against it. This is the inverse of pre-news flow — there's a window where
  the move hasn't happened yet.
- **Market fit.** Politics has slow-moving markets where retail dominates and
  takes hours to digest a court ruling.
- **Required data.** News intel item with `expected_price_impact_min/max` (already
  persisted in v11.0) + trade flow in the 0–6h after `first_seen_at`.
- **Detection logic.** For each news intel item flagged `actionable`, compute
  realised Δprice in the 0–60min window post-annotation. When
  `realised_Δ < expected_min × UNDERREACTION_FRACTION` (default 0.5) AND
  volume in that window > `UNDERREACTION_MIN_VOLUME_USD`, emit a
  `news_underreaction` alert with the affected market.
- **Features.** expected_Δ, realised_Δ, volume in window, time since news.
- **Gates.** News intel must be `actionable` (not sentinel). Underreaction
  signal expires after `UNDERREACTION_MAX_AGE` (default 4h).
- **Scoring.** Severity = info → warning → critical by the size of the gap
  between expected and realised price moves.
- **Severity.** Up to `warning`; this is an actionable but lower-confidence
  signal class.
- **False-positive filters.** Markets with broken / suspended trading.
- **Example signal.** "Court rules against X → news intel expected YES move
  to 0.60; current price 0.51 with $8k volume in 30min. Watchtower expects
  repricing in next 4h. alert: news_underreaction, warning."
- **Failure modes.** Markets with thin liquidity that genuinely won't reprice;
  the news being wrong / overstated.
- **Backtest plan.** Replay news intel items with `actionable` and measure
  realised post-window Δ against the alert prediction.
- **Metrics.** `watchtower_news_underreaction_total{outcome=...}`.
- **Complexity.** **Medium.** Reuses news intel + trade reads. New worker
  ticking every 5 min on the rolling 4h window.
- **Expected value.** **High.** Different signal class than current — captures
  "Watchtower missed it the first time" cleanly.
- **vs current.** Complementary; doesn't overlap.
- **Class.** **production** (after backtest).

---

### 4. Cross-market wallet positioning (research → admin-only)

- **Thesis.** A wallet that builds correlated positions across multiple
  superficially-unrelated markets is signalling a thesis broader than any one
  market. Coordinated cohort behaviour across markets is the high-dimension
  analogue of `cluster_convergence`.
- **Market fit.** Politics has natural correlation clusters (presidential +
  congressional + economic markets all key off the same prior).
- **Required data.** `polymarket_trades` + wallet identity + market category.
- **Detection logic.** For each wallet, compute their open positions across
  markets in the same category whitelist; flag when the wallet's portfolio
  shows directional consistency above `CROSS_MARKET_CONCENTRATION_MIN`
  (default 0.7).
- **Features.** Number of correlated markets, total notional, average
  directional consistency, time spread.
- **Gates.** Wallet must have ≥3 distinct market positions in the category
  within the window.
- **Scoring.** Booster on existing alerts (initially). Standalone admin-only
  alert at v1.
- **Severity.** Info booster only.
- **FP filters.** Treasury / known MM wallets (mm_suppression hit-rate >50%
  in 30d).
- **Example signal.** Wallet 0xDEF holds $80k YES across 4 different "GOP-wins-X"
  markets — flagged as `cross_market_cohort_4` on each per-market alert.
- **Failure modes.** Index-style wallets (e.g. PEP that buys "every YES below
  0.10"); fund-of-funds.
- **Backtest plan.** Replay 90d of resolved alerts and measure CLV6h
  conditional on `cross_market_cohort_n` flag.
- **Metrics.** `watchtower_cross_market_cohort_total{n}`.
- **Complexity.** **Medium-high.** Needs nightly cohort assembly + per-alert
  read-time join.
- **Expected value.** **Medium.** The category whitelist already narrows the
  hunt; this strategy expands it inside the whitelist.
- **vs current.** Higher-dimension version of `cluster_convergence`.
- **Class.** **research-only initially** → **admin-only** booster → maybe
  production after a successful backtest.

---

### 5. Orderbook liquidity withdrawal detector (production, hard)

- **Thesis.** A sudden ask-side cancel + price tick up (or vice versa) is the
  cleanest informed-flow tell that doesn't show in the trade stream. The
  Polymarket WSS market channel [^pmwss] streams orderbook updates in real
  time; we already have the connection.
- **Market fit.** Politics markets vary widely in book depth; this strategy
  works best on the deeper / more liquid ones.
- **Required data.** Orderbook snapshots from `polymarket_live_market_state`
  (already persisted by the v10.4 WS fast-lane).
- **Detection logic.** For each tracked market, compute rolling
  `best_ask_size_delta` over 30s windows. Flag when `Δsize ≤ −LIQ_WITHDRAW_THRESHOLD`
  (default 50% of pre-window size) AND `Δprice_1m ≥ LIQ_WITHDRAW_MIN_MOVE`.
- **Features.** Pre-withdrawal book depth, post-withdrawal depth, Δprice.
- **Gates.** Market must have base-line depth ≥ `LIQ_WITHDRAW_MIN_DEPTH_USD`
  (default $5k).
- **Scoring.** Booster on any concurrent trade alert. Standalone info-severity
  alert when no trade alert is concurrent.
- **Severity.** Info / warning depending on `Δprice_1m` magnitude.
- **FP filters.** Stale-quote MMs (a MM that pulled because their connection
  dropped); large operator orders being filled.
- **Example signal.** Trump-2028 YES book: best-ask size was $40k @ 0.62;
  $35k cancelled within 30s; price tick to 0.625 in next 60s →
  `liquidity_withdrawal_high` booster on any concurrent alert.
- **Failure modes.** False positives in thin books; legitimate MM rebalancing.
- **Backtest plan.** Replay live_market_state history; correlate withdrawal
  events with subsequent price moves.
- **Metrics.** `watchtower_orderbook_withdraw_total{magnitude=...}`.
- **Complexity.** **High.** Real-time event detection on the WS fast-lane;
  needs care to not blow latency budgets.
- **Expected value.** **Medium-high.** Differentiates Watchtower from
  trade-stream-only surveillance.
- **vs current.** Orthogonal — works when the trade stream is silent.
- **Class.** **production** (after backtest + careful real-time perf testing).

---

### 6. Cheap tail-odds whale conviction (production, easy)

- **Thesis.** A $200k bet at 4% on a tail outcome is structurally different
  from a $200k bet at 50%. The payoff asymmetry is 24:1 vs 1:1 — the
  signal-to-noise ratio of "this wallet expects a tail event" is much
  higher.
- **Market fit.** Politics tail markets ("Will X happen by Y date?", with X
  unlikely) are common.
- **Required data.** `polymarket_trades.price` + `notional`.
- **Detection logic.** Promote `single_trade_whale` severity by one tier when
  `price ≤ TAIL_ODDS_MAX_PRICE` (default 0.10) AND
  `notional ≥ TAIL_ODDS_MIN_NOTIONAL_USD` (default $25k) AND
  `lifecycle_pct ≥ TAIL_ODDS_MIN_LIFECYCLE` (default 50%).
- **Features.** Price, notional, lifecycle %, payoff multiple.
- **Gates.** Standard whale-alert gates + the three above.
- **Scoring.** Severity bump (info → warning, warning → critical).
- **Severity.** Up to `critical` on the existing ladder.
- **FP filters.** Multi-wallet spoofing on the same tail outcome.
- **Example signal.** "$50k YES at 0.06 on `Will-X-resign-before-Y` —
  payoff 17:1, lifecycle 78%, wallet new but with 4 reviewed alerts; promote
  from info to warning."
- **Failure modes.** Lottery-ticket whales; spoof wallets.
- **Backtest plan.** Replay last 90d resolved alerts at price ≤0.10; compare
  hit-rate vs ladder default.
- **Metrics.** `watchtower_tail_odds_promotion_total{from,to}`.
- **Complexity.** **Low.** Pure scoring tweak on the existing single-trade
  detector.
- **Expected value.** **Medium.** Surfaces a class of conviction trade we
  currently under-rate.
- **vs current.** Modifies `single_trade_whale` scoring; not a new strategy
  package.
- **Class.** **production** (small).

---

### 7. Flow-before-official-statement detector (production)

- **Thesis.** Government / agency / court announcements have publication
  schedules. Trades clustered in the 30–120 minutes BEFORE those publications
  are a higher-prior insider-like cohort than trades at random times.
- **Market fit.** Politics is the canonical domain (FOMC, SCOTUS decisions,
  electoral certifications, OFAC releases).
- **Required data.** A small operator-curated table of "official statement
  windows" + existing trade stream.
- **Detection logic.** When trade falls inside a `pre_statement_window` for a
  market whose category overlaps the statement's topic, stamp the alert with
  `pre_statement_flow=true` and the window offset.
- **Features.** Distance to window-end, market category overlap.
- **Gates.** Statement window must be operator-curated (no auto-generated
  windows for v0).
- **Scoring.** Severity bump similar to pre-news flow.
- **Severity.** Up to `critical`.
- **FP filters.** Random alignment with windows; require trade direction to
  match later-published statement direction (via post-hoc review).
- **Example signal.** "$30k YES on `SCOTUS-Affirms-X` 47 min before scheduled
  9:00am opinion drop — flagged `pre_statement_47m`."
- **Failure modes.** Operator curation lag; statements that don't move
  markets.
- **Backtest plan.** Curate 30 high-profile statement windows from 2024–2026
  and replay.
- **Metrics.** `watchtower_pre_statement_total{window_bucket}`.
- **Complexity.** **Low-medium.** Operator-curated table + read-time join.
- **Expected value.** **Medium.** Niche but distinctive surveillance value.
- **vs current.** Complementary; works alongside news intel.
- **Class.** **production** (after operator curates the seed table).

---

### 8. Repeated early wallet hit-rate (production, depends on #1)

- **Thesis.** Wallets that systematically enter early in a market AND
  resolve correctly are the strongest single-wallet informed signal. Combines
  wallet reputation (#1) with pre-event timing (#2).
- **Market fit.** Politics. Strongest in the resolved-market category window
  the MARKET_CLOSE_REVIEW already touches.
- **Required data.** Wallet reputation table (#1) + alert sent_at vs market
  end_date.
- **Detection logic.** Per (wallet, market): compute `lifecycle_pct_at_alert`.
  Roll a per-wallet aggregate over `EARLY_HIT_LOOKBACK` (default 180d): how
  often the wallet's first alert in a market landed at `lifecycle_pct < 50%`
  AND outcome resolved correct.
- **Features.** Early-hit rate, sample size, average lead.
- **Gates.** Sample size ≥ 5.
- **Scoring.** Promote alert severity when wallet `early_hit_rate ≥ 0.65`.
- **Severity.** Up to `critical` (paired with the trade's existing tier).
- **FP filters.** Recency decay; exclude mm_suppression wallets.
- **Example signal.** "Wallet 0xDEF first-ever YES bet on Trump-Wisconsin
  at 0.41 at lifecycle 18% — wallet's historical early-hit rate: 8/11.
  Promote from warning to critical."
- **Failure modes.** Survivorship bias; reflexive copying.
- **Backtest plan.** Same as #1 + correlation with hit-rate by lifecycle
  decile.
- **Metrics.** `watchtower_early_hit_promotion_total{decile}`.
- **Complexity.** **Low** once #1 ships.
- **Expected value.** **High.** Directly addresses the gap in current ladder.
- **vs current.** Substantial uplift on `single_trade_whale` precision.
- **Class.** **production** (after #1 ships).

---

### 9. False-positive MM/liquidity-trap detector (booster)

- **Thesis.** Some MM wallets straddle the boundary between "two-sided"
  (filtered by mm_suppression) and "one-sided informed". When a MM-classified
  wallet's recent NET imbalance grows large in a specific direction, the
  classification has drifted.
- **Market fit.** Politics liquidity providers shift conviction during
  campaigns.
- **Required data.** mm_suppression history per wallet.
- **Detection logic.** Compute rolling NET imbalance for wallets flagged by
  mm_suppression in the last 30d; flag wallets whose recent NET imbalance
  exceeds `MM_DRIFT_NET_MIN_USD`.
- **Features.** NET imbalance, two-sided ratio, days since classification.
- **Gates.** Wallet must currently be in mm_suppression.
- **Scoring.** Promote MM-suppressed wallet's NEXT alert to info-severity (it
  would normally be dropped).
- **Severity.** Info only.
- **FP filters.** Strict — this is correcting a known filter.
- **Example signal.** "Wallet 0xMMA flagged MM-suppressed; recent 30d NET YES
  exposure $180k > $25k baseline. Promote next alert to info."
- **Failure modes.** Real MMs rebalancing.
- **Backtest plan.** Find MM-suppressed wallets whose post-resolution
  performance ≥ 0.6 hit-rate and label them as drift cases.
- **Metrics.** `watchtower_mm_drift_alerts_total`.
- **Complexity.** **Low.**
- **Expected value.** **Low-medium.** Niche recall improvement.
- **vs current.** Counter-balances mm_suppression false negatives.
- **Class.** **booster**.

---

### 10. Missed-signal detector from Market Close Review (admin-only)

- **Thesis.** The MARKET_CLOSE_REVIEW already produces verdicts like
  `missed_signal`. Aggregate these into a strategy-improvement queue — when
  the same kind of miss happens repeatedly the operator should see it as
  a tuning candidate, not as a single review row.
- **Market fit.** Universal.
- **Required data.** `polymarket_market_close_reviews.verdict` + the
  ai_json `tuning_recommendations` array.
- **Detection logic.** Daily aggregator over the last 30d MARKET_CLOSE_REVIEW
  rows; bucket by `tuning_recommendation.area` and count occurrences.
- **Features.** Recurrence count, severity, area.
- **Gates.** Recurrence ≥ 3.
- **Scoring.** Surface as an admin-only digest; never reaches signal chat.
- **Severity.** n/a (admin telemetry).
- **FP filters.** AI hallucination — operator gates.
- **Example signal.** Admin Telegram: "Recurring tuning: ownership area, 7
  recommendations in 30d, all suggesting tightening threshold."
- **Failure modes.** AI tuning suggestions are inconsistent.
- **Backtest plan.** n/a — observational.
- **Metrics.** `watchtower_missed_signal_recurrence_total{area}`.
- **Complexity.** **Low.**
- **Expected value.** **Medium** — closes the loop on the loop.
- **vs current.** Strictly additive; consumes MARKET_CLOSE_REVIEW output.
- **Class.** **admin-only**.

---

## Ranked roadmap

| Priority | Strategy | Why now | Depends on |
|---|---|---|---|
| **P0** | #1 Wallet Reputation v0 | Cheap, deterministic, unlocks #4 #8; uses data we already collect. | MARKET_CLOSE_REVIEW (✓ shipping in v11.4). |
| **P0** | #2 Pre-news flow timing | Highest-evidence academic feature [^hcg] [^dop]; data already present. | None. |
| **P1** | #3 Post-news underreaction | Different signal class; orthogonal to existing detectors. | News intel v11.0 (✓). |
| **P1** | #6 Cheap tail-odds promotion | Trivial implementation; surfaces under-rated conviction trades. | None. |
| **P1** | #8 Repeated early hit-rate | Strong uplift once Wallet Reputation lands. | #1. |
| **P2** | #5 Orderbook liquidity withdrawal | Real-time perf risk; defer until we have learning-loop evidence of need. | WS fast-lane (✓). |
| **P2** | #7 Flow-before-official-statement | Niche; requires operator curation. | Operator-curated table. |
| **P2** | #10 Missed-signal recurrence digest | Closes the loop; admin-only. | MARKET_CLOSE_REVIEW (✓). |
| **P3** | #4 Cross-market wallet cohort | High complexity; research-only first. | Wallet Reputation. |
| **Reject (for now)** | #9 MM drift detector | Tiny ROI; very narrow recall improvement. | n/a. |
| **Reject** | (any AI-only "agent" framework / RL strategy auto-tuner / vector-DB feature store) | Violates "deterministic before AI" + non-negotiable rules. | n/a. |

---

## Data we should start persisting NOW (even before the strategies ship)

1. **Wallet realised PnL per resolved alert.** Already extractable from
   `polymarket_alerts.clv_*` + `outcome_status` + the trade's
   notional. A daily aggregator (cheap) populates the Wallet Reputation
   table that P0 needs.
2. **Lifecycle-pct at alert time** as a first-class column. Today this is
   computable from `polymarket_markets.start_date / end_date / alert.sent_at`
   but it's expensive to re-compute on every read.
3. **Per-alert news-intel linkage.** When news intel fires on a market the
   alert later references, store the link so Δt computation is a single join.

## What to explicitly reject

- **Vector-DB feature store / "agent" framework / RL strategy tuner.** None of
  these are in the spec direction; none reduce evidence-gathering cost; all
  add infrastructure surface for unclear ROI.
- **Customer-facing dashboard for any of these strategies.** The user-facing
  signal feed stays narrow (flow alerts + actionable news). Everything in
  this report is admin / operator-facing first.
- **Strategy auto-tuning from MARKET_CLOSE_REVIEW.** AI tuning recommendations
  are *input* to operator review, not automatic config updates. The
  non-negotiable rule "Do NOT auto-tune .env thresholds" stands.

## Final recommendation

After the Market Close Review learning loop is operational and has produced
≥ 50 succeeded reviews, **build Wallet Reputation v0 (#1) followed
immediately by Pre-news flow timing (#2)**. Both are cheap, both use data
we already collect, both directly address the highest-evidence academic
signals [^hcg] [^dop]. Skip #5 (orderbook liquidity withdrawal) until
the learning loop produces actual evidence that we're missing reprices —
the WS fast-lane perf budget is the bottleneck, not the detector logic.

---

## Citations

[^crs]: Congressional Research Service, "Prediction Markets and Insider Trading
Law", Library of Congress, https://www.congress.gov/crs-product/LSB11406.

[^hcg]: Harvard Law School Forum on Corporate Governance, "From Iran to Taylor
Swift: Informed Trading in Prediction Markets" (2026-03-25),
https://corpgov.law.harvard.edu/2026/03/25/from-iran-to-taylor-swift-informed-trading-in-prediction-markets/.

[^dop]: dopaminemarkets.com, "How to Solve Insider Trading in Prediction
Markets",
https://www.dopaminemarkets.com/p/how-to-solve-insider-trading-in-prediction.

[^pmwss]: Polymarket Documentation, "WSS Overview" + "Market Channel",
https://docs.polymarket.com/developers/CLOB/websocket/wss-overview,
https://docs.polymarket.com/api-reference/wss/market.

NCSU Poole College "Explainer: Insider Trading and Prediction Markets",
https://poole.ncsu.edu/thought-leadership/article/explainer-insider-trading-and-prediction-markets/.

Money.com, "Prediction Markets Have an Insider Trading Problem. Are They
Still Worth the Gamble?",
https://money.com/prediction-markets-insider-trading/.
