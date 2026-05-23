# Watchtower Strategy Admin/User Telegram Flow

How strategy decisions reach Telegram, and what each surface should
and should not contain.

## 1. Two surfaces only

There are exactly two strategy-related Telegram surfaces:

| Surface | Receives | When | Volume target |
|---|---|---|---|
| **Admin** | shadow + skip summaries + worker health + promotion review | always (operator-facing) | 50–500 / day |
| **User** | promoted high-confidence actionable signals | only after triple-gate passes | 1–10 / day |

No other Telegram surface receives strategy output. Watchtower stats,
market_intel, daily_intel, prediction-update-blocked, top-annotations
generic reports are disabled via `staleEnvKeys{}` and runtime tests.

## 2. Config keys

| KEY | Default | Purpose |
|---|---|---|
| `TELEGRAM_STRATEGY_ADMIN_FLOW_ENABLED` | `true` | admin surface on/off |
| `TELEGRAM_STRATEGY_USER_FLOW_ENABLED` | `false` | user surface on/off |
| `TELEGRAM_STRATEGY_SHADOW_TO_ADMIN` | `true` | shadow rows surface to admin |
| `TELEGRAM_STRATEGY_PROMOTED_TO_USER` | `true` | promoted decisions reach user (still gated below) |
| `TELEGRAM_STRATEGY_MIN_USER_CONFIDENCE` | `0.75` | confidence floor for user |
| `TELEGRAM_STRATEGY_MIN_USER_LEVEL` | `warning` | level floor (warning/critical/hard) |
| `TELEGRAM_STRATEGY_USER_DEDUPE_WINDOW` | `12h` | user dedupe |
| `TELEGRAM_STRATEGY_ADMIN_DEDUPE_WINDOW` | `1h` | admin dedupe |

## 3. Routing rules (defined in `StrategyTelegramFlowConfig`)

### Shadow decisions
```
if !cfg.AdminEnabled            → drop
if !cfg.ShadowToAdmin           → drop
if dedupe(admin, dedup_key, 1h) → drop
→ admin Telegram
```

### Promoted decisions (live)
A promoted decision is one that survived ALL three gates:
1. `cfg.GlobalPromotionAllowed=true`
2. `<STRATEGY>_SHADOW_ONLY=false`
3. `strategypromotion.Worker.Allow(strategy_name)=true`

```
if !cfg.UserEnabled                                  → admin only
if confidence < cfg.MinUserConfidence                → admin only
if level < cfg.MinUserLevel                          → admin only
if dedupe(user, dedup_key, 12h)                      → admin only
→ user Telegram + admin Telegram (if PromotedToUser=true)
```

## 4. Message UX

### Admin message format (shadow row)
```
STRATEGY SHADOW · {strategy} · {decision_level}

Type:     strategy_shadow
Flow:     admin
Strategy: {strategy_name_human}
Decision: {decision_kind}   ({decision_level})
Shadow:   true
Score:    {score:.2f}
Conf:     {confidence:.2f}

Why:
• {reason_1}
• {reason_2}
• {reason_3}

Features:
• breadth = {features.breadth}
• aligned_usd = {features.aligned_usd}
• consistency = {features.consistency}
  …

Market:
• Polymarket event: <link>
• Market: <link>
• Grafana: <link>
```

### User message format (promoted signal)
```
WATCHTOWER SIGNAL

Strategy: {strategy_name_human}
Market:   {question}

Why it matters:
{1–2 sentence operator-facing thesis}

Signal:
• {primary_evidence_1}
• {primary_evidence_2}

Evidence:
• Flow:     {flow_summary if applicable}
• Holders:  {holder_summary if applicable}
• Book:     {book_summary if applicable}
• Catalyst: {catalyst_summary if applicable}

Risk:
• {ambiguity / opposed_exposure / known_caveat}

Links:
• Polymarket event: <link>
• Market: <link>
```

The user message NEVER contains: `eval_skipped`, debug score numbers,
internal feature names, no-edge analysis, internal Grafana links,
or text like "shadow row", "promotion review", or "AI analysis".

## 5. Quality rules (hard requirements)

1. **Every user alert must answer "what / why / evidence / risk"** in ≤ 6 short lines.
2. **No user alert without a Polymarket market link**.
3. **No user alert during dedupe window** (12h default).
4. **No user alert with `confidence < 0.75`** — even if every other gate passes.
5. **No user alert during operator-flagged maintenance** (set `TELEGRAM_STRATEGY_USER_FLOW_ENABLED=false`).

## 6. What never reaches user

| Surface | Why disabled |
|---|---|
| Watchtower stats (every-2h) | High volume, low signal |
| Market intelligence reports | Generic commentary, no actionable thesis |
| Daily political intel | Same |
| Prediction-update blocked | Internal pipeline state, no operator action |
| Top annotations report | Already covered by per-alert annotation context |
| Shadow rows that didn't pass gates | By definition not promotion-eligible |
| Skip-reason summaries | Operator concern, not user |

## 7. Verification

```bash
# 1. Pinned defaults test (must pass):
go test ./internal/app/ -run TestEnvFiles_DangerousDefaultsBlocked -count 1

# 2. Legacy surfaces still disabled:
go test ./internal/app/ -run TestLegacyTelegramSurfaces -count 1

# 3. Stale env keys still rejected:
go test ./internal/app/ -run TestStaleEnvKeys -count 1

# 4. Audit env sync:
bash scripts/audit-env.sh
```

If any of these fail, user-flow Telegram is **NOT safe to enable**.

## 8. Common operator mistakes (and what catches them)

| Mistake | Detection |
|---|---|
| Setting `TELEGRAM_STRATEGY_USER_FLOW_ENABLED=true` without promotion | dangerous-defaults test fails locally |
| Setting `STRATEGY_PROMOTION_BYPASS_EXPLICIT=true` to "see what would happen" | dangerous-defaults test fails |
| Setting `STRATEGY_LEARNING_LOOP_PROMOTION_ALLOWED=true` without checking promotion eligibility | bus still forces `shadow_only=true` when no strategy is eligible |
| Re-enabling `WATCHTOWER_STATS_TELEGRAM_ENABLED=true` | dangerous-defaults test fails |
| Setting `MARKET_INTEL_ENABLED=true` | boot fails (`staleEnvKeys{}` rejection) |
| Lowering `TELEGRAM_STRATEGY_MIN_USER_CONFIDENCE` to 0.5 | dedupe + level gates still hold; risk of user-alert noise; metric to monitor: alert volume in admin chat compared with previous day |
| Removing `TELEGRAM_STRATEGY_USER_DEDUPE_WINDOW` | parser keeps default 12h via envDefault tag |
