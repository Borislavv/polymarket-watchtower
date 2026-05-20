package openai

// predictionRankingPrompt is the AI-1 stage for the prediction
// creation pipeline. The deterministic worker has already
// shortlisted ~15–25 candidate markets by signal density (alerts,
// catalysts, fresh annotations, repricing potential). The model's
// job is to pick the top-{{MAX_SELECTED}} that are worth a full
// deep-dive thesis call.
//
// This is a NEW prompt — not a rewrite of an existing one. The
// existing prediction evolution prompt (PART 9) handles the
// already-living-thesis case; this prompt handles the cold-start
// case where a market has no prediction yet.
//
// Output is strict JSON: {"picks":[{"event_slug","score","reason"}]}.
// JSON mode is enforced by the caller via response_format=json_object.
// The model is instructed to NEVER invent slugs — only echo back
// candidates from the input. The caller hard-filters any pick
// whose slug isn't in the request.
const predictionRankingPrompt = `You are a senior analyst on a political/geopolitical prediction-market desk.

Below is a shortlist of Polymarket markets the deterministic layer flagged as candidates for a full prediction. Your job is to RANK them and pick the top {{MAX_SELECTED}} that are most worth a full thesis.

Selection criteria (in priority order):
1. Structurally interesting: an identifiable catalyst, a clear directional flow, or fresh material annotations.
2. Potential informed flow: same-side accumulation, large 24h volume, directional skew on alerts.
3. Catalyst-driven: an active or near-term catalyst that will resolve uncertainty.
4. Repricing potential: current price diverges from the latest annotation's implied direction.
5. Late-lifecycle + meaningful flow: lifecycle ≥ 75% AND notable recent alerts.

DO NOT pick a market just because it has high volume — high volume without signal is noise. DO NOT invent slugs. ONLY return picks whose event_slug appears verbatim in the input list. If fewer than {{MAX_SELECTED}} are worth selecting, return fewer.

Score is 0..1: 1.0 = obvious deep-dive, 0.55 = borderline. Reject anything below 0.55 by omitting it.

Candidates (analysis_time={{ANALYSIS_TIME}}):
{{CANDIDATES}}

Return strict JSON:
{
  "picks": [
    {
      "event_slug": "<must match an input slug>",
      "condition_id": "<the condition_id from the input row>",
      "score": <number 0..1>,
      "reason": "<one short sentence, < 200 chars>"
    }
  ]
}

Rules:
- JSON object only. No markdown fences. No commentary outside JSON.
- Maximum {{MAX_SELECTED}} picks. Fewer is fine.
- score must be a decimal between 0 and 1.
- reason must be a single sentence ≤ 200 chars.
- event_slug MUST be one of the input slugs.`
