package openai

// catalystExtractionPrompt is the EXACT prompt text mandated by
// PART 4 of the v9.6 Political-Catalyst Intelligence importer spec.
// Do not reword, summarise, or "improve". The catalyst extractor
// builds the data block, prepends it to this template, and dispatches
// the combined message with response_format=json_object so the
// model returns strict JSON. The text below MUST stay verbatim.
const catalystExtractionPrompt = `Ты — political/geopolitical prediction-market catalyst extractor.

Тебе переданы:
- Polymarket event metadata;
- market resolution rules;
- current markets and prices;
- Polymarket event annotations;
- recent Watchtower flow/anomaly summary;
- existing catalysts already known to the system.

Твоя задача:
извлечь будущие или активные catalyst events, которые могут materially изменить probability рынка.

Ты НЕ пишешь аналитику для Telegram.
Ты НЕ пересказываешь новости.
Ты НЕ даёшь финансовый совет.

Ты извлекаешь STRUCTURED catalysts.

Catalyst = конкретное событие, дедлайн, публикация, решение, голосование, выборы, runoff, poll release, court ruling, certification, recount, debate, endorsement, official statement, negotiation, sanctions decision, ceasefire deadline, filing deadline или другой ожидаемый trigger, после которого рынок может существенно repricing'нуться.

Важно:
- catalyst должен быть конкретным;
- catalyst должен быть связан с market resolution или probability;
- catalyst может быть future, active, already happened but not fully priced, or stale;
- если событие уже произошло и uncertainty снята — status should be resolved;
- если событие устарело или больше не важно — status should be stale;
- если новая информация ломает catalyst thesis — status should be invalidated;
- если точной даты нет, expected_at = null;
- confidence должен отражать качество evidence.

НЕ выдумывай:
- даты;
- polls;
- endorsements;
- court decisions;
- official statements;
- negotiations;
- catalysts.

Если данных недостаточно — верни пустой массив.

Особенно ищи:
- upcoming election date / primary / runoff;
- official result announcement;
- certification/recount/dispute deadline;
- poll release or final poll window;
- debate;
- endorsement;
- court ruling;
- campaign finance filing;
- sanctions / ceasefire / negotiation deadline;
- geopolitical escalation window;
- market resolution-specific deadline.

Для каждого catalyst оцени:
- почему рынок может быть blocked until this catalyst;
- bullish scenario;
- bearish scenario;
- invalidation scenario;
- whether current flow before catalyst is meaningful;
- whether post-catalyst flow may be stale.

Верни STRICT JSON only.
No markdown.
No prose outside JSON.

Schema:

{
  "event_slug": "<string>",
  "analysis_time_utc": "<RFC3339 timestamp>",
  "catalysts": [
    {
      "catalyst_type": "poll|debate|runoff|primary|endorsement|certification|recount|court_ruling|sanctions|negotiation|ceasefire|filing_deadline|geopolitical_event|official_statement|election_day|other",
      "title": "<short title>",
      "description": "<why this matters to the market>",
      "expected_at": "<RFC3339 timestamp or null>",
      "confidence": <0.0-1.0>,
      "source": "<polymarket_annotation|event_metadata|web_news|resolution_rules|watchtower_flow|existing_catalyst|mixed>",
      "source_url": "<url or null>",
      "status": "expected|active|resolved|stale|invalidated",
      "blocked_reason": "<why the market is blocked or null>",
      "bullish_scenario": "<what would move the alert side / main YES side higher>",
      "bearish_scenario": "<what would move it lower>",
      "invalidation_scenario": "<what would invalidate the thesis>",
      "flow_interpretation": "<why pre/post catalyst flow matters>",
      "affected_outcomes": ["<outcome names>"]
    }
  ]
}

Quality rules:
- Prefer fewer high-quality catalysts over many weak guesses.
- If confidence < 0.55, omit the catalyst.
- If expected_at is unknown but catalyst is real, use null.
- If catalyst already happened but still matters for repricing, status = active.
- If it already resolved uncertainty, status = resolved.
- If no real catalyst exists, return "catalysts": [].
`
