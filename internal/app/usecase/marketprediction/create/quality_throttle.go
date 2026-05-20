package create

import (
	"net/url"
	"strings"

	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/eventflow"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/eventpagecontext"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/repricing"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/analysis"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/repository"
)

// passesQuality runs the deterministic gate the v10.1 PART 7 spec
// pins. The first return is whether the prediction should be shipped
// to Telegram; the second is a short reason code that's emitted on
// the quality_gate metric and a structured log line. The reason
// codes are stable so operators can chart them.
//
// Reason codes:
//
//	low_confidence  — confidence below MinConfidence.
//	low_summary     — summary body shorter than MinSummaryChars.
//	neutral_no_signal — side_bias=neutral and SendNeutral=false.
//	no_signal       — RequireSignal=true and we found no catalyst,
//	                  no repricing direction, no flow signal, no
//	                  fresh annotation.
//	ok              — all checks passed (caller emits as "ok").
func (w *Worker) passesQuality(
	res analysis.PredictionCreationResponse,
	page eventpagecontext.Summary,
	cats []repository.EventCatalyst,
	flow eventflow.EventFlowSummary,
	sig *repricing.Signal,
) (bool, string) {
	if res.Confidence < w.cfg.MinConfidence {
		return false, "low_confidence"
	}
	if len(strings.TrimSpace(res.Summary)) < w.cfg.MinSummaryChars {
		return false, "low_summary"
	}
	if !w.cfg.SendNeutral && strings.EqualFold(res.SideBias, "neutral") {
		return false, "neutral_no_signal"
	}
	if w.cfg.RequireSignal {
		hasCatalyst := len(cats) > 0
		hasRepricing := sig != nil && sig.RepricingStatus != "" && sig.RepricingStatus != "unclear"
		hasFlow := flow.RecentAlerts > 0 || flow.SameSideNotionalUSD > 0
		hasAnnotation := len(page.Annotations) > 0
		if !(hasCatalyst || hasRepricing || hasFlow || hasAnnotation) {
			return false, "no_signal"
		}
	}
	return true, "ok"
}

// canSendTelegram reports whether the worker is allowed to ship a
// Telegram message for `eventSlug` in this cycle. Returns "" on a
// green light; otherwise a stable short reason code suitable for
// metric labels + log emission:
//
//	startup_suppressed   — first cycle of process + SendOnStartup=false.
//	cooldown             — per-event cooldown still ticking.
//	max_per_run_reached  — already shipped MaxTelegramPerRun.
//
// The current per-run counter is reset at the top of Tick.
func (w *Worker) canSendTelegram(eventSlug string) string {
	w.tgMu.Lock()
	defer w.tgMu.Unlock()
	if !w.startupDone && !w.cfg.SendOnStartup {
		return "startup_suppressed"
	}
	if w.cfg.MaxTelegramPerRun > 0 && w.tgSentThisRun >= w.cfg.MaxTelegramPerRun {
		return "max_per_run_reached"
	}
	if last, ok := w.lastTGSentAt[eventSlug]; ok && w.cfg.TelegramCooldown > 0 {
		if w.cfg.Clock().Sub(last) < w.cfg.TelegramCooldown {
			return "cooldown"
		}
	}
	return ""
}

// markTelegramSent stamps the per-event lastTGSentAt + bumps the
// per-run counter. Called only on a successful send.
func (w *Worker) markTelegramSent(eventSlug string) {
	w.tgMu.Lock()
	defer w.tgMu.Unlock()
	w.lastTGSentAt[eventSlug] = w.cfg.Clock()
	w.tgSentThisRun++
}

// resetPerRunCounter is called at the top of Tick so the run-level
// MaxTelegramPerRun cap is exact (not cumulative across cycles).
func (w *Worker) resetPerRunCounter() {
	w.tgMu.Lock()
	defer w.tgMu.Unlock()
	w.tgSentThisRun = 0
}

// markStartupDone flips the gate that suppresses Telegram on the
// very first cycle when SendOnStartup=false. Called from Tick after
// processing completes (success or not).
func (w *Worker) markStartupDone() {
	w.tgMu.Lock()
	defer w.tgMu.Unlock()
	w.startupDone = true
}

// buildRenderInput composes the structured input for
// RenderCreationTelegram. The annotations slice is pre-capped to
// AnnotationsLimit and the URL fields are filled from the
// configured base URLs.
//
// `chunkSeq` is unused today but reserved for the eventual paged
// "1/3", "2/3" Telegram header we may want to add when a single
// prediction needs multiple chunks.
func (w *Worker) buildRenderInput(
	cand analysis.PredictionCandidate,
	res analysis.PredictionCreationResponse,
	page eventpagecontext.Summary,
	_ int,
) CreationRenderInput {
	in := CreationRenderInput{
		EventSlug:                cand.EventSlug,
		Question:                 cand.Question,
		Outcome:                  cand.Outcome,
		SideBias:                 res.SideBias,
		Confidence:               res.Confidence,
		Summary:                  res.Summary,
		RiskFactors:              res.RiskFactors,
		MaxAnnotationTitleChars:  w.cfg.AnnotationsMaxTitleChars,
		MaxAnnotationSourceNames: w.cfg.AnnotationsMaxSourceNames,
	}
	if w.cfg.AnnotationsEnabled {
		limit := w.cfg.AnnotationsLimit
		anns := page.Annotations
		if limit > 0 && len(anns) > limit {
			anns = anns[:limit]
		}
		in.Annotations = anns
	}
	if w.cfg.LinksEnabled {
		in.PolymarketEventURL = w.buildPolymarketEventURL(cand.EventSlug)
		in.PolymarketMarketURL = w.buildPolymarketMarketURL(cand.EventSlug, cand.ConditionID)
		in.GrafanaURL = w.buildGrafanaURL(cand.EventSlug)
	}
	return in
}

// buildPolymarketEventURL returns the canonical /event/<slug> URL.
// Empty input → empty (the sanitizer at render time drops it).
func (w *Worker) buildPolymarketEventURL(eventSlug string) string {
	slug := strings.TrimSpace(eventSlug)
	if slug == "" || w.cfg.PolymarketBase == "" {
		return ""
	}
	return strings.TrimRight(w.cfg.PolymarketBase, "/") + "/event/" + slug
}

// buildPolymarketMarketURL returns the more specific market URL when
// a condition_id is available. Polymarket's market deep-link format
// is currently the same /event/<slug> path with a market hash; the
// safer choice is to point at the event page and let the operator
// pick the market in the UI.
func (w *Worker) buildPolymarketMarketURL(eventSlug, conditionID string) string {
	if eventSlug == "" || conditionID == "" || w.cfg.PolymarketBase == "" {
		return ""
	}
	return strings.TrimRight(w.cfg.PolymarketBase, "/") + "/event/" + eventSlug + "?market=" + url.QueryEscape(conditionID)
}

// buildGrafanaURL deep-links into the prediction-engine dashboard
// filtered by event_slug. Returns "" when the Grafana base or UID
// is empty so the sanitizer / render path elides the bullet.
func (w *Worker) buildGrafanaURL(eventSlug string) string {
	base := strings.TrimSpace(w.cfg.GrafanaBaseURL)
	uid := strings.TrimSpace(w.cfg.GrafanaDashUID)
	if base == "" || uid == "" || eventSlug == "" {
		return ""
	}
	u, err := url.Parse(strings.TrimRight(base, "/") + "/d/" + uid)
	if err != nil {
		return ""
	}
	q := u.Query()
	q.Set("var-event_slug", eventSlug)
	u.RawQuery = q.Encode()
	return u.String()
}

// --- metric + log adapters ------------------------------------------------

// tgSentThisRun lives on Worker to keep the per-run cap exact.
// Declared in worker.go beside the other state fields.

func (w *Worker) observeQualityGate(result string) {
	if w.met == nil || w.met.PredictionCreationQualityGate == nil {
		return
	}
	w.met.PredictionCreationQualityGate.WithLabelValues(result).Inc()
}

func (w *Worker) observeTelegramSkipped(reason string) {
	if w.met == nil || w.met.PredictionCreationTelegramSkipped == nil {
		return
	}
	w.met.PredictionCreationTelegramSkipped.WithLabelValues(reason).Inc()
}

func (w *Worker) observeTelegramSent() {
	if w.met == nil || w.met.PredictionCreationTelegramSent == nil {
		return
	}
	w.met.PredictionCreationTelegramSent.Inc()
}

func (w *Worker) observeDedupeSkipped(reason string) {
	if w.met == nil || w.met.PredictionCreationDedupeSkipped == nil {
		return
	}
	w.met.PredictionCreationDedupeSkipped.WithLabelValues(reason).Inc()
}

func (w *Worker) observeMessageChunks(surface string, chunks int) {
	if w.met == nil || w.met.PredictionMessageChunks == nil || chunks <= 0 {
		return
	}
	w.met.PredictionMessageChunks.WithLabelValues(surface).Add(float64(chunks))
}

func (w *Worker) observeStartupSuppressed() {
	if w.met == nil || w.met.PredictionSchedulerStartupSuppressed == nil {
		return
	}
	w.met.PredictionSchedulerStartupSuppressed.WithLabelValues("prediction_creation").Inc()
}

func (w *Worker) logTelegramSkip(eventSlug, reason string) {
	if w.log == nil {
		return
	}
	// Single info-level line per skip — explicit enough for
	// operators, quiet enough not to dominate logs.
	w.log.Info().Str("event_slug", eventSlug).Str("reason", reason).Msg("prediction creation: telegram send suppressed")
	if reason == "startup_suppressed" {
		w.observeStartupSuppressed()
	}
}
