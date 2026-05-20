package openai

// annotationRankingPrompt is the EXACT prompt text mandated by the
// v9.7 spec preamble (PART 1-3 — the annotation ranking template).
// Do not reword, summarise, or "improve". The ranker substitutes the
// four placeholders ({{OUTPUT_LIMIT}}, {{PERIOD}}, {{MARKETS}},
// {{ANNOTATIONS}}, {{FLOW_SUMMARY}}) at build time with structured
// data blocks. The text outside placeholders is load-bearing and
// MUST stay verbatim.
const annotationRankingPrompt = `Ты — political/geopolitical prediction-market intelligence analyst.

Тебе передан список Polymarket event annotations за последний период по выбранным рынкам.

Твоя задача:
выбрать самые важные события для prediction-market оператора.

Ты НЕ пишешь новостной обзор.
Ты НЕ пересказываешь все события.
Ты НЕ выбираешь просто самые свежие.

Выбери события, которые:
- могут materially изменить probability рынка;
- объясняют сильный price repricing;
- могут вызвать volatility;
- связаны с catalyst / blocker / resolution;
- подтверждают или ломают текущий тренд;
- могут объяснять whale-flow / accumulation / cluster;
- могут означать, что рынок underreacting или overreacting;
- важны для политического/geopolitical risk desk.

Отфильтруй:
- noise;
- duplicates;
- stale events;
- generic campaign chatter;
- события без market impact;
- события, не связанные с resolution criteria.

Верни STRICT JSON only.
No markdown.
No prose outside JSON.

Schema:

{
  "selected": [
    {
      "event_slug": "<string>",
      "market_slug": "<string or null>",
      "rank": <integer>,
      "importance": <0.0-1.0>,
      "volatility_potential": <0.0-1.0>,
      "probability_impact": "bullish|bearish|mixed|neutral|unclear",
      "affected_outcome": "<string or null>",
      "title": "<short title>",
      "reason": "<why this matters, one sentence>",
      "market_read": "underreacting|overreacting|already_priced|watch|avoid|unclear"
    }
  ]
}

Rules:
- Select at most {{OUTPUT_LIMIT}} events.
- Prefer fewer high-quality events over filler.
- If nothing important exists, return {"selected":[]}.
- Do not invent facts.
- Use only provided annotations.
- If an event has high priceChange but weak real-world relevance, downgrade it.
- If event is directly resolution-relevant, upgrade it.

Input block before prompt:

Period:
{{PERIOD}}

Markets:
{{MARKETS}}

Annotations:
{{ANNOTATIONS}}

Recent Watchtower flow:
{{FLOW_SUMMARY}}
`
