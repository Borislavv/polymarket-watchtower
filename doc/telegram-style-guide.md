# Telegram Style Guide — v10.5

Every Telegram-bound surface in Watchtower follows this guide. The
goal: an operator reading the chat on a phone can scan ten messages
in fifteen seconds and never wonder what kind of message they're
looking at, what triggered it, or where the live event lives.

## Message types

Stable enum (`internal/infra/alerting.MessageType`):

| Value               | When                                              |
|---------------------|---------------------------------------------------|
| `regular`           | scheduled report (market intel, daily intel, stats) |
| `triggered`         | per-alert flow (whale flow / accumulation / cluster) |
| `prediction`        | prediction creation (new living-thesis row)        |
| `prediction_update` | prediction state transition or AI refresh          |
| `market_intel`      | 2h market intelligence scout                       |
| `daily_intel`       | daily political/geopolitical report                |
| `outcome`           | post-resolution audit / postmortem                 |
| `system`            | operator-only system notices                       |

Use the renderer in `internal/infra/alerting/telegram_header.go`.
Never hand-roll the header.

## Standard section order

Every message:

1. **Meta header** — Type / Trigger / Strategy / Value / AI.
2. **Main title** — one HTML line. STATE only for predictions;
   short summary for alerts. NEVER AI text in the title.
3. **Decision / Status** — what just happened, what stance does the
   operator take.
4. **Market** — title, price, lifecycle, vol24h.
5. **Why now** — one bullet line per reason (flow, anomaly, …).
6. **Flow / Value** — wallet stats / notional / multiplier.
7. **Catalyst / Blocked state** — when the prediction is gated.
8. **Repricing** — only when there's a fresh repricing signal.
9. **AI analysis / AI prediction** — bounded text section.
10. **Latest Polymarket events** — annotation rows with source links.
11. **Matched alerts / related signals** — bullets, capped at 5.
12. **Links** — central `RenderLinksBlock` from
    `internal/infra/alerting/links.go`.

Section is rendered only when its content is non-empty. No orphan
headers.

## Header rules

```
<b>Type:</b> <type>
<b>Trigger:</b> frequency=<dur>, last_posted_at=<rfc3339>, now=<rfc3339>
<b>Trigger:</b> by=<reason>       ← for triggered alerts
<b>Strategy:</b> <human label 1> · <human label 2>
<b>Value:</b> tokens=<…>, usd=$<…>, profit=$<…>
<b>AI:</b> status=<ok|fallback|skipped|error|unknown>, cost=$<…>, tokens=in/out
```

Rules:
- Omit fields that have no value.
- Strategy labels are ALWAYS the human-readable form via
  `alerting.StrategyLabel(...)`. Never dump raw enum keys.
- `AI: status=...` is mandatory for surfaces that COULD call AI.
  Set `skipped` + `cost=$0` when the call was suppressed by
  config / rate limit / budget.

## Link rules

- Use `alerting.RenderLinksBlock(LinksInput)` for the section.
- Use `alerting.RenderLinksInline(LinksInput)` for per-row "links: …".
- Every event-specific surface MUST include the event link when
  `event_slug` is known. The block elides cleanly when no link
  config is wired — there is never an orphan "Links" header.
- Annotation source URLs go through
  `alerting.SanitizeLinkURL` AND a per-row cap
  (`MAX_SOURCE_LINKS=3`). Unsafe / localhost / loopback / non-http
  URLs are silently dropped.

## Length limits

- One chunk ≤ 4000 chars (use `alerting.SafeSplitForTelegram`).
- AI analysis paragraph: 1200–2500 chars normal, up to 4000 only
  for genuinely complex setups.
- Prediction update AI text: ≤ 1800 chars.
- Market intelligence AI text: ≤ 2500 chars.
- Daily intel AI text: per-paragraph 1500–2500 chars; the worker
  splits on `\n\n` boundaries.

## Blocked-state rendering

```
<b>Catalyst</b>
• blocked until: <RFC3339|tbd>
• status: <expected|active|resolved|invalidated|stale>
• event: <one-line, ≤ 140 chars>
• why it matters: <one-line, ≤ 240 chars>
```

`internal/infra/alerting/telegram.go::writeBlockedAlertBlock` and
`evolution/render.go::RenderEvolutionUpdate` both consume this
shape.

## Prediction rendering

Title: `<b>PREDICTION UPDATE</b> · <state>`. The market title
belongs in its own `<b>Market</b>` section underneath. NEVER put AI
text in the title — `worker_test.go::TestTelegram_OnStateChange`
asserts this.

`pred.Summary` is allowed to carry an AI-curated phrase but it must
NOT bleed into the header line. The renderer is the gatekeeper.

## Market-intel rendering

Title: `<b>MARKET INTELLIGENCE</b> · 2h`. The body is sectioned:
Overview → Markets to watch → Important Polymarket events → AI
analysis. Every "Markets to watch" row carries `links: ...`. Every
"Important Polymarket events" row carries `links: Event · Source 1 · …`.

## Daily-intel rendering

Title: `<b>DAILY POLITICAL INTEL</b> · <date>`. Sections: Executive
read → Today's catalysts → Best watchlist → Noise/avoid → Watch
next. Always include the event link under each catalyst.

## Forbidden patterns

- **Long title with AI text.** Title is structural, not narrative.
- **No links.** Every event-specific message MUST surface the event
  link when the slug is known.
- **Empty sections.** Render the header only when its content is
  non-empty.
- **Raw JSON in operator-visible text.** JSON belongs in the
  `polymarket_*_logs` rows, not in Telegram.
- **Duplicate markets.** marketintel and intel reports dedupe by
  `condition_id` before rendering.
- **Huge paragraphs in list items.** Bullets are one line each;
  paragraphs go in the AI analysis section.

## Tests

Every surface MUST have a golden snapshot test under its package.
The golden assertions are:

- `Type`/`Trigger`/`Strategy`/`AI` header lines present.
- Event link present when `event_slug` is in the input.
- No orphan `<b>Links</b>` block.
- No raw `{` / `}` JSON in the body.
- Body chunks ≤ 4000 chars.
- AI text NOT in the title.

See `internal/infra/alerting/telegram_header_test.go` and
`internal/infra/alerting/links_test.go` for the shared assertions.
