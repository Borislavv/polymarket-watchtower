# Signal-quality reporting

This document explains the proof-of-value layer: how the watchtower
measures whether its alerts find informed-flow candidates, how to read
the Telegram reports, and how to use the Grafana dashboard.

## Vocabulary discipline

The watchtower surfaces **directionally correct surveillance signals**.
It does NOT prove insider trading and does NOT predict profit. The
right operator vocabulary:

- **informed-flow candidate** — a trade or wallet whose shape matches
  the academic literature on informed trading; not a legal claim.
- **suspicious wallet–market pair** — a wallet whose activity on a
  specific (market, outcome) clears multiple gates.
- **signal quality** — the directional-correctness rate after
  resolution.
- **resolved alert success rate** — for alerts whose markets have
  resolved, the fraction that turned out directionally correct.
- **public-data anomaly** — the underlying observation: a trade that
  is unusual against the public order/trade flow.

Avoid: "insider", "guaranteed profit", "edge", "proof of fraud".

## Two notification surfaces

| Surface  | Role |
|----------|------|
| Telegram | Ops feed. Per-alert messages, outcome reactions, scheduled summaries. **Action.** |
| Grafana  | Analysis / quality / control plane. Historical signal-quality trends. **Reflection.** |

Treat them as complementary — never read Telegram alone and conclude
the system is working or not. Always cross-check the Grafana
dashboards over a longer horizon.

## Outcome states

`polymarket_alerts.outcome_status` carries one of:

| Status              | Meaning                                                                          |
|---------------------|----------------------------------------------------------------------------------|
| `pending`           | Market hasn't resolved yet. Excluded from success rate denominators.             |
| `resolved_correct`  | Alert direction matched the winning outcome. Counted as success.                 |
| `resolved_wrong`    | Alert direction opposite the winning outcome. Counted as failure.                |
| `unknown`           | Market closed but no token cleared the winning-price threshold (default 0.99). Treated as ambiguous; counted separately. |
| `unavailable`       | Market not in upstream snapshot (archived, expired, no condition_id). Excluded.  |

The success-rate formula is:

```
success_rate = resolved_correct / (resolved_correct + resolved_wrong)
```

`pending`, `unknown`, and `unavailable` are NOT in the denominator.

## Telegram outcome reactions

After classification, the outcomes worker calls Telegram's
`setMessageReaction` on the original alert message with a configured
emoji:

| Outcome             | Default emoji |
|---------------------|---------------|
| `resolved_correct`  | 👍            |
| `resolved_wrong`    | 👎            |
| `unknown`           | 🤔            |

Reaction state on the row is one of:

| Reaction status | Meaning |
|----------------|---------|
| `pending`      | Outcome not yet classified, or reactor hasn't run yet. |
| `applied`      | Reaction posted on the upstream message. |
| `unsupported`  | Telegram refused (channel reactions disabled, bot missing rights, paid reactions). **Terminal.** No retries. |
| `failed`       | Transient error. The next tick retries. |
| `disabled`     | `TELEGRAM_OUTCOME_REACTIONS_ENABLED=false` at the moment the row was eligible, or `TELEGRAM_OUTCOME_DISABLE_AMBIGUOUS=true` blocked an ambiguous outcome. Terminal. |

If your Telegram channel disabled reactions or restricted them to a
custom emoji set, expect a flood of `unsupported` initially. The
reactor stops retrying on those rows automatically — there is no
chat-spam risk.

## Scheduled reports

| Cadence    | Sent at                                  | Covers                    |
|------------|------------------------------------------|---------------------------|
| Daily      | 08:00 GMT+3 the day after period end     | Previous full UTC+3 day   |
| Weekly     | 08:00 GMT+3 the Monday after period end  | Previous ISO week         |
| Monthly    | 08:00 GMT+3 of the 1st of the next month | Previous calendar month   |
| Quarterly  | 08:00 GMT+3 of the 1st of the next quarter | Previous calendar quarter |
| Yearly     | 08:00 GMT+3 **+ 72h delay**              | Previous calendar year    |

The yearly 72h delay lets late upstream resolution settle (markets
that close near year-end often resolve a day or two later). Example:
the 2026 yearly report is sent at **2027-01-04 08:00 GMT+3**.

The reporting timezone is operator-configurable via
`SIGNAL_REPORTS_TIMEZONE`. Default `Etc/GMT-3` = UTC+3 (the IANA sign
convention is inverted; `Europe/Moscow` is the alternative if you
prefer city naming).

### Report content

Each report contains:

```
Signal quality · Daily · 2026-05-17

Overview
• total alerts sent: 47
• resolved: 28 (success 19 / failure 9)
• success rate: 67.9%
• still pending: 19 (market not yet resolved)

CLV-lite (24h post-trade drift)
• samples: 28
• avg favourable drift: +1.42%
• positive-drift ratio: 64.3% (18 / 28)

By alert kind
• trade_anomaly: 12/18 (66.7%) — unresolved=8
• accumulation: 7/10 (70.0%) — unresolved=11

By severity
• info: 14/22 (63.6%) — unresolved=14
• warning: 5/6 (83.3%) — unresolved=5
```

**Sample-size caveat.** When the resolved count for the period is
below 30, the report includes:

```
⚠ Sample size is small; treat this as directional, not statistically stable.
```

A 7-out-of-10 success rate looks impressive but a 95% binomial CI on
that sample is enormous. The caveat is honest about the noise.

## Grafana dashboard panels

The `Polymarket Watchtower` dashboard adds a "Signal quality" row
with these panels:

| Panel | Read it as |
|-------|------------|
| **Signal quality · directional correctness (24h rolling rate)** | The single headline number. "Of alerts whose markets resolved in the last 24h, what fraction were directionally correct?" Look for trends, not single-day peaks. |
| **Signal quality · resolved vs pending (count / 1h)** | Are alerts converting to resolutions? A pile of `pending` with few `resolved_correct/wrong` means the watchtower is alerting on markets that haven't closed yet — common when LIFECYCLE_ALERT_FROM_PCT is low. |
| **Signal quality by severity (24h success rate)** | Is Critical actually better than Info, or is the severity ladder noise? |
| **Signal quality by alert kind (24h success rate)** | Are accumulation alerts beating single-trade? Is ownership concentration a leading signal? |
| **Telegram outcome reactions (rate / 15m)** | Health of the reaction pass. Spikes in `unsupported` after a Telegram config change → channel restricted reactions. Spikes in `failed` → upstream rate-limit. |
| **Signal reports sent / failed (24h)** | Scheduler health. Anything in `failed` warrants opening the polymarket_signal_reports row — `last_error` carries the upstream message. |

## Backfill of older alerts

`polymarket_signal_reports` is empty at first deploy. Reports for
periods that have already passed BEFORE the worker was enabled will
not be backfilled — the scheduler treats "no row" as "I haven't sent
this yet" only relative to the most recent completed window. If you
want historical reports, run them manually against the aggregator
SQL:

```sql
SELECT * FROM polymarket_alerts
WHERE sent_at >= '2026-01-01' AND sent_at < '2026-02-01';
```

Plus the same projections the worker uses (`SignalQualityAggregate`,
`SignalQualityByKind`, `SignalQualityBySeverity`).

For Telegram reactions on alerts sent BEFORE migration 00007, the
column defaults to `pending`. The reactor picks them up automatically
on its next tick if their outcomes are already classified.

## What to tune

| Symptom | Likely tune |
|---------|-------------|
| Success rate consistently below 50% | Raise the severity thresholds in `.env` (Info notional, multiplier, etc.). Read `doc/strategies/single-cluster.md` for the rationale. |
| All alerts pending forever | Drop `LIFECYCLE_ALERT_FROM_PCT` is too low — you're alerting too early in markets that won't resolve for weeks. Bump it to 80+. |
| `unavailable` outcomes dominating | The market resolution is too old to fetch from Gamma. Cross-check `cfg.Outcomes.WinningPriceThreshold`. |
| Telegram reactions all `unsupported` | The channel disabled reactions or limited them to a custom set. Override the three `TELEGRAM_OUTCOME_*_REACTION` env vars with values from the allowed set, or set `TELEGRAM_OUTCOME_REACTIONS_ENABLED=false`. |
| Yearly report sent the wrong year | `SIGNAL_REPORTS_TIMEZONE` boundary. The yearly window is anchored to local calendar year in the configured zone. |

## Honest limitations

- **Success is directional, not financial.** A "resolved_correct"
  alert means the direction was right at resolution — not that a
  trader following the alert would have profited (entry price,
  slippage, time horizon all matter).
- **The watchlist is alert-biased.** Per-wallet outcome data comes
  from `polymarket_alerts.outcome_status`, which only covers wallets
  we've already alerted on. A wallet that never tripped an alert is
  invisible to this measurement.
- **Ownership concentration is a trade-flow approximation.** No
  Polymarket holders endpoint is wired; the percentage is computed
  from net BUY-side shares vs total BUY-side flow. Directional, not
  authoritative.
- **Sample sizes are small.** Below 30 resolved alerts in a period,
  any percentage is dominated by noise. The renderer flags this; the
  Grafana panels do not — read the panels with the same skepticism.

If you take one thing away: **never report a single-day Telegram
summary as "our signal quality is X%"** without the matching Grafana
trend. The trend is the signal; the daily number is the noise.
