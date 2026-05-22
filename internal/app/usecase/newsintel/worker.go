package newsintel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/Borislavv/polymarket-watchtower/internal/infra/ai/openai"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/metrics"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/repository"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/telegram"
)

// --- Seams (interfaces over infra) ----------------------------------------

// AnnotationSource lists raw Polymarket event-page annotations that
// were first/last seen on or after `since`, capped at `limit`.
// Implemented by *repository.EventPageRepository.ListAnnotationsSince.
type AnnotationSource interface {
	ListAnnotationsSince(ctx context.Context, since time.Time, limit int32) ([]repository.EventAnnotation, error)
}

// EventMarketSource returns the per-event market list (newest snapshot
// per market_id) — the v11.0 worker uses this to attach affected
// markets to each news item without hitting Polymarket. Implemented
// by *repository.EventPageRepository.ListLatestEventMarkets.
type EventMarketSource interface {
	ListLatestEventMarkets(ctx context.Context, eventSlug string) ([]repository.EventPageMarketRow, error)
}

// IntelStore wraps the v11.0 persistence layer. Implemented by
// *repository.NewsIntelRepository.
type IntelStore interface {
	InsertRun(ctx context.Context, in repository.NewsIntelRunInsert) (int64, error)
	FinishRun(ctx context.Context, in repository.NewsIntelRunFinish) error
	InsertDecision(ctx context.Context, d repository.NewsIntelDecision) error
	FilterUnprocessed(ctx context.Context, itemHashes []string) ([]string, error)
	MarkProcessed(ctx context.Context, itemHash, eventSlug, title string, runID int64) error
	TouchProcessed(ctx context.Context, itemHash string) error
}

// Analyzer dispatches the single AI call per cycle. Implemented by
// *openai.Client.EvaluateHourlyNewsIntel.
type Analyzer interface {
	EvaluateHourlyNewsIntel(ctx context.Context, req openai.NewsIntelAIRequest) (openai.NewsIntelAIResult, error)
}

// TelegramSender delivers HTML-parse-mode chunks via the v11.3 typed
// router. The worker passes SurfaceNewsIntelActionable on every send
// so the router maps it to the signal chat regardless of what other
// config the wiring layer may have.
type TelegramSender interface {
	Send(ctx context.Context, msg telegram.Message) (telegram.SendResult, error)
}

// --- Worker ---------------------------------------------------------------

// Worker is the hourly cycle driver.
type Worker struct {
	cfg          Config
	annotations  AnnotationSource
	markets      EventMarketSource
	store        IntelStore
	ai           Analyzer
	tg           TelegramSender
	met          *metrics.Metrics
	log          *zerolog.Logger
	lastInputFP  string
	lastCycleEnd time.Time
}

// New wires the worker. Any nil dependency is treated as "feature
// unavailable" and the worker degrades — see Tick() for the rules.
func New(
	cfg Config,
	annotations AnnotationSource,
	markets EventMarketSource,
	store IntelStore,
	ai Analyzer,
	tg TelegramSender,
	met *metrics.Metrics,
	log *zerolog.Logger,
) *Worker {
	cfg.applyDefaults()
	return &Worker{
		cfg:         cfg,
		annotations: annotations,
		markets:     markets,
		store:       store,
		ai:          ai,
		tg:          tg,
		met:         met,
		log:         log,
	}
}

// Run blocks until ctx cancels. Ticks at cfg.Interval. When
// cfg.StartupRun is true the first tick fires immediately.
func (w *Worker) Run(ctx context.Context) {
	if !w.cfg.Enabled {
		if w.log != nil {
			w.log.Info().Msg("news intel: disabled by config")
		}
		return
	}
	if w.cfg.StartupRun {
		w.Tick(ctx)
	}
	ticker := time.NewTicker(w.cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.Tick(ctx)
		}
	}
}

// Tick runs one cycle. Exposed for the CLI smoke tool and tests.
// Recovers from panics so a single bad cycle doesn't crash the
// worker goroutine.
func (w *Worker) Tick(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			if w.log != nil {
				w.log.Error().Interface("panic", r).Msg("news intel: tick panic recovered")
			}
			if w.met != nil {
				w.met.NewsIntelRuns.WithLabelValues("failed").Inc()
			}
		}
	}()

	start := w.cfg.Clock()
	end := start
	startLookback := end.Add(-w.cfg.Lookback)

	if w.met != nil {
		w.met.NewsIntelRuns.WithLabelValues("started").Inc()
	}

	// --- 1. Scan candidate annotations ---------------------------------
	rawAnns, err := w.annotations.ListAnnotationsSince(ctx, startLookback, int32(w.cfg.MaxItems))
	if err != nil {
		w.recordFailedRun(ctx, startLookback, end, 0, 0, "scan_annotations: "+err.Error())
		return
	}

	items := w.buildItems(ctx, rawAnns)
	if w.met != nil {
		w.met.NewsIntelItemsTotal.WithLabelValues("processed").Add(float64(len(items)))
	}

	if len(items) == 0 {
		w.persistSkippedRun(ctx, startLookback, end, 0, 0, "no_candidate_annotations", "", "")
		return
	}

	// --- 2. Dedupe against the processed-items ledger ------------------
	hashes := make([]string, 0, len(items))
	for _, it := range items {
		hashes = append(hashes, it.Hash)
	}
	var newHashes []string
	if w.cfg.DedupeEnabled {
		newHashes, err = w.store.FilterUnprocessed(ctx, hashes)
		if err != nil {
			w.recordFailedRun(ctx, startLookback, end, len(items), 0, "filter_unprocessed: "+err.Error())
			return
		}
	} else {
		newHashes = hashes
	}
	newSet := make(map[string]struct{}, len(newHashes))
	for _, h := range newHashes {
		newSet[h] = struct{}{}
	}
	newItems := make([]openai.NewsItemForAI, 0, len(newHashes))
	for _, it := range items {
		if _, ok := newSet[it.Hash]; ok {
			newItems = append(newItems, it)
		}
	}
	dupCount := len(items) - len(newItems)
	if w.met != nil && dupCount > 0 {
		w.met.NewsIntelItemsTotal.WithLabelValues("duplicate").Add(float64(dupCount))
	}
	if w.met != nil && len(newItems) > 0 {
		w.met.NewsIntelItemsTotal.WithLabelValues("new").Add(float64(len(newItems)))
	}

	if len(newItems) == 0 {
		// Touch the existing items so last_seen_at bumps — useful for
		// the operator to see the annotation pool isn't stale.
		for _, it := range items {
			_ = w.store.TouchProcessed(ctx, it.Hash)
		}
		w.persistSkippedRun(ctx, startLookback, end, len(items), 0, "no_new_items", "", "")
		return
	}

	// --- 3. Semantic cooldown via input fingerprint --------------------
	inputFP := computeInputFingerprint(newItems)
	if w.cfg.DedupeEnabled && w.lastInputFP != "" && inputFP == w.lastInputFP {
		sinceLast := start.Sub(w.lastCycleEnd)
		if sinceLast > 0 && sinceLast < w.cfg.SemanticCooldown {
			w.persistSkippedRun(ctx, startLookback, end, len(items), 0, "input_fingerprint_unchanged", inputFP, "")
			return
		}
	}

	// --- 4. AI dispatch ------------------------------------------------
	if !w.cfg.AIEnabled || w.ai == nil {
		w.persistSkippedRun(ctx, startLookback, end, len(items), 0, "ai_disabled", inputFP, "")
		for _, it := range newItems {
			_ = w.store.MarkProcessed(ctx, it.Hash, it.EventSlug, it.Title, 0)
		}
		return
	}

	runID, err := w.store.InsertRun(ctx, repository.NewsIntelRunInsert{
		LookbackStart:    startLookback,
		LookbackEnd:      end,
		NewsItemsCount:   len(items),
		AICalled:         false,
		AIStatus:         "pending",
		InputFingerprint: inputFP,
	})
	if err != nil {
		w.recordFailedRun(ctx, startLookback, end, len(items), 0, "insert_run: "+err.Error())
		return
	}

	aiCtx, cancel := context.WithTimeout(ctx, w.cfg.AITimeout)
	defer cancel()
	aiStart := w.cfg.Clock()
	result, aiErr := w.ai.EvaluateHourlyNewsIntel(aiCtx, openai.NewsIntelAIRequest{
		LookbackStart: startLookback,
		LookbackEnd:   end,
		MaxSelected:   w.cfg.MaxSelected,
		Items:         newItems,
	})
	if w.met != nil {
		w.met.NewsIntelAILatency.Observe(w.cfg.Clock().Sub(aiStart).Seconds())
	}

	// --- 5. AI status routing ------------------------------------------
	switch {
	case aiErr != nil || result.Status == "failed":
		if w.met != nil {
			w.met.NewsIntelAIRequests.WithLabelValues("failed").Inc()
		}
		errMsg := result.LastError
		if aiErr != nil && errMsg == "" {
			errMsg = aiErr.Error()
		}
		w.finishRun(ctx, runID, repository.NewsIntelRunFinish{
			ID:                runID,
			Status:            "failed",
			NewsItemsCount:    len(items),
			AICalled:          true,
			AIStatus:          "failed",
			AICostUSD:         result.EstimatedCostUSD,
			OutputFingerprint: result.OutputFingerprintHi,
			LastError:         truncate(errMsg, 400),
		})
		if w.met != nil {
			w.met.NewsIntelRuns.WithLabelValues("failed").Inc()
			w.met.NewsIntelCycleLatency.Observe(w.cfg.Clock().Sub(start).Seconds())
		}
		return

	case result.Status == "skipped":
		if w.met != nil {
			w.met.NewsIntelAIRequests.WithLabelValues("skipped").Inc()
		}
		w.finishRun(ctx, runID, repository.NewsIntelRunFinish{
			ID:                runID,
			Status:            "skipped",
			NewsItemsCount:    len(items),
			AICalled:          false,
			AIStatus:          "skipped",
			SentinelCode:      result.LastError,
			AICostUSD:         result.EstimatedCostUSD,
			OutputFingerprint: result.OutputFingerprintHi,
		})
		if w.met != nil {
			w.met.NewsIntelRuns.WithLabelValues("skipped").Inc()
			w.met.NewsIntelCycleLatency.Observe(w.cfg.Clock().Sub(start).Seconds())
		}
		return
	}

	if w.met != nil {
		w.met.NewsIntelAIRequests.WithLabelValues("ok").Inc()
	}

	// --- 6. Sentinel handling — silent, no Telegram --------------------
	if result.Sentinel != "" {
		if w.met != nil {
			w.met.NewsIntelSentinel.WithLabelValues(result.Sentinel).Inc()
		}
		w.finishRun(ctx, runID, repository.NewsIntelRunFinish{
			ID:                runID,
			Status:            "ok",
			NewsItemsCount:    len(items),
			AICalled:          true,
			AIStatus:          "ok",
			SentinelCode:      result.Sentinel,
			AICostUSD:         result.EstimatedCostUSD,
			OutputFingerprint: result.OutputFingerprintHi,
		})
		w.markItemsProcessed(ctx, newItems, runID)
		w.lastInputFP = inputFP
		w.lastCycleEnd = w.cfg.Clock()
		if w.met != nil {
			w.met.NewsIntelRuns.WithLabelValues("ok").Inc()
			w.met.NewsIntelCycleLatency.Observe(w.cfg.Clock().Sub(start).Seconds())
		}
		return
	}

	// --- 7. Decision rows ----------------------------------------------
	filtered := filterByConfidence(result.Selected, w.cfg.MinConfidence)
	itemAffected := indexAffectedMarkets(newItems)
	for i := range filtered {
		d := filtered[i]
		d.Rank = i + 1
		filtered[i] = d
		affected := itemAffected[d.NewsItemHash]
		dec := repository.NewsIntelDecision{
			RunID:                  runID,
			NewsItemHash:           d.NewsItemHash,
			EventSlug:              d.EventSlug,
			ConditionID:            d.ConditionID,
			MarketTitle:            d.MarketTitle,
			Rank:                   d.Rank,
			Decision:               result.Decision,
			Confidence:             d.Confidence,
			ImpactDirection:        d.ImpactDirection,
			ExpectedPriceImpactMin: d.ExpectedPriceImpactMin,
			ExpectedPriceImpactMax: d.ExpectedPriceImpactMax,
			ExpectedWindow:         d.ExpectedWindow,
			WhyItMatters:           d.WhyItMatters,
			WhatMarketMayMiss:      d.WhatMarketMayMiss,
			TriggerCondition:       d.TriggerCondition,
			InvalidatesIf:          d.InvalidatesIf,
			TradeStance:            d.TradeStance,
			TelegramWorthy:         d.TelegramWorthy,
			AffectedMarkets:        affected,
		}
		if err := w.store.InsertDecision(ctx, dec); err != nil {
			if w.log != nil {
				w.log.Warn().Err(err).Str("hash", d.NewsItemHash).Msg("news intel: insert decision failed")
			}
			continue
		}
		if w.met != nil {
			w.met.NewsIntelDecisions.WithLabelValues(result.Decision, boolStr(d.TelegramWorthy)).Inc()
		}
	}

	// --- 8. Telegram send ---------------------------------------------
	telegramSent := false
	if w.cfg.SendTelegram && len(filtered) > 0 && !shouldSuppress(result.Decision, w.cfg.SuppressNoEdge) {
		telegramSent = w.sendTelegram(ctx, result, filtered, itemAffected)
	}

	w.finishRun(ctx, runID, repository.NewsIntelRunFinish{
		ID:                runID,
		Status:            "ok",
		NewsItemsCount:    len(items),
		SelectedCount:     len(filtered),
		AICalled:          true,
		AIStatus:          "ok",
		AICostUSD:         result.EstimatedCostUSD,
		OutputFingerprint: result.OutputFingerprintHi,
		TelegramSent:      telegramSent,
	})
	w.markItemsProcessed(ctx, newItems, runID)
	w.lastInputFP = inputFP
	w.lastCycleEnd = w.cfg.Clock()
	if w.met != nil {
		w.met.NewsIntelRuns.WithLabelValues("ok").Inc()
		w.met.NewsIntelCycleLatency.Observe(w.cfg.Clock().Sub(start).Seconds())
	}
}

// buildItems converts raw annotations into NewsItemForAI rows with
// linked affected markets. AffectedMarkets is capped at
// cfg.MaxMarketsPerItem.
func (w *Worker) buildItems(ctx context.Context, anns []repository.EventAnnotation) []openai.NewsItemForAI {
	out := make([]openai.NewsItemForAI, 0, len(anns))
	marketsByEvent := make(map[string][]openai.NewsAffectedMarketForAI)

	for _, a := range anns {
		title := strings.TrimSpace(a.Title)
		if title == "" {
			continue
		}
		hash := strings.TrimSpace(a.ItemHash)
		if hash == "" {
			hash = computeItemHashFromAnnotation(a)
		}
		affected, ok := marketsByEvent[a.EventSlug]
		if !ok {
			affected = w.resolveAffected(ctx, a.EventSlug)
			marketsByEvent[a.EventSlug] = affected
		}
		if len(affected) == 0 {
			// PART 22: spec says "no affected markets" annotations
			// should not waste an AI slot. Skip silently — they'll
			// re-appear on a later cycle once Polymarket attaches
			// the event-page market rows.
			continue
		}
		cut := affected
		if len(cut) > w.cfg.MaxMarketsPerItem {
			cut = cut[:w.cfg.MaxMarketsPerItem]
		}
		item := openai.NewsItemForAI{
			Hash:            hash,
			EventSlug:       a.EventSlug,
			Title:           title,
			Summary:         strings.TrimSpace(a.Summary),
			Source:          strings.TrimSpace(a.Source),
			Timestamp:       a.Timestamp,
			PriceBefore:     a.PriceBefore,
			PriceAfter:      a.PriceAfter,
			PriceChange:     a.PriceChange,
			AffectedMarkets: cut,
		}
		out = append(out, item)
	}
	// Stable order: newest first, then by event_slug, then by title —
	// keeps fingerprint stable across reorderings.
	sort.SliceStable(out, func(i, j int) bool {
		ti, tj := out[i].Timestamp, out[j].Timestamp
		if !ti.Equal(tj) {
			return ti.After(tj)
		}
		if out[i].EventSlug != out[j].EventSlug {
			return out[i].EventSlug < out[j].EventSlug
		}
		return out[i].Title < out[j].Title
	})
	if len(out) > w.cfg.MaxItems {
		out = out[:w.cfg.MaxItems]
	}
	return out
}

func (w *Worker) resolveAffected(ctx context.Context, eventSlug string) []openai.NewsAffectedMarketForAI {
	if w.markets == nil || strings.TrimSpace(eventSlug) == "" {
		return nil
	}
	rows, err := w.markets.ListLatestEventMarkets(ctx, eventSlug)
	if err != nil {
		if w.log != nil {
			w.log.Debug().Err(err).Str("event_slug", eventSlug).Msg("news intel: market resolve failed")
		}
		return nil
	}
	out := make([]openai.NewsAffectedMarketForAI, 0, len(rows))
	for _, r := range rows {
		title := strings.TrimSpace(r.GroupItemTitle)
		if title == "" {
			title = strings.TrimSpace(r.Question)
		}
		if strings.TrimSpace(r.ConditionID) == "" {
			continue
		}
		out = append(out, openai.NewsAffectedMarketForAI{
			ConditionID: r.ConditionID,
			EventSlug:   eventSlug,
			MarketTitle: title,
		})
	}
	// Stable order: alphabetical by condition_id for fingerprint
	// determinism.
	sort.SliceStable(out, func(i, j int) bool { return out[i].ConditionID < out[j].ConditionID })
	return out
}

func (w *Worker) markItemsProcessed(ctx context.Context, items []openai.NewsItemForAI, runID int64) {
	for _, it := range items {
		if err := w.store.MarkProcessed(ctx, it.Hash, it.EventSlug, it.Title, runID); err != nil && w.log != nil {
			w.log.Debug().Err(err).Str("hash", it.Hash).Msg("news intel: mark processed failed")
		}
	}
}

func (w *Worker) persistSkippedRun(ctx context.Context, lookbackStart, lookbackEnd time.Time, itemCount, selectedCount int, sentinel, inputFP, outputFP string) {
	_, _ = w.store.InsertRun(ctx, repository.NewsIntelRunInsert{
		LookbackStart:     lookbackStart,
		LookbackEnd:       lookbackEnd,
		NewsItemsCount:    itemCount,
		SelectedCount:     selectedCount,
		AICalled:          false,
		AIStatus:          "skipped",
		SentinelCode:      sentinel,
		InputFingerprint:  inputFP,
		OutputFingerprint: outputFP,
	})
	if w.met != nil {
		w.met.NewsIntelRuns.WithLabelValues("skipped").Inc()
	}
}

func (w *Worker) recordFailedRun(ctx context.Context, lookbackStart, lookbackEnd time.Time, itemCount, selectedCount int, errMsg string) {
	if w.log != nil {
		w.log.Warn().Str("err", errMsg).Msg("news intel: cycle failed")
	}
	_, _ = w.store.InsertRun(ctx, repository.NewsIntelRunInsert{
		LookbackStart:  lookbackStart,
		LookbackEnd:    lookbackEnd,
		NewsItemsCount: itemCount,
		SelectedCount:  selectedCount,
		AICalled:       false,
		AIStatus:       "failed",
	})
	if w.met != nil {
		w.met.NewsIntelRuns.WithLabelValues("failed").Inc()
	}
}

func (w *Worker) finishRun(ctx context.Context, runID int64, in repository.NewsIntelRunFinish) {
	if err := w.store.FinishRun(ctx, in); err != nil && w.log != nil {
		w.log.Warn().Err(err).Int64("run_id", runID).Msg("news intel: finish run failed")
	}
}

// sendTelegram renders the message + SafeSplits + sends. Returns
// true when at least one chunk landed. The router resolves the
// destination from SurfaceNewsIntelActionable — the worker no longer
// keeps a ChatID, it just labels the surface.
func (w *Worker) sendTelegram(ctx context.Context, result openai.NewsIntelAIResult, selected []openai.NewsIntelAIDecision, affected map[string][]openai.NewsAffectedMarketForAI) bool {
	if w.tg == nil {
		return false
	}
	body := RenderTelegramMessage(result, selected, affected)
	if strings.TrimSpace(body) == "" {
		return false
	}
	chunks := safeSplit(body, w.cfg.TelegramMessageCap)
	anySent := false
	for _, chunk := range chunks {
		if _, err := w.tg.Send(ctx, telegram.Message{
			Surface: telegram.SurfaceNewsIntelActionable,
			HTML:    chunk,
		}); err != nil {
			if w.met != nil {
				w.met.NewsIntelTelegramChunks.WithLabelValues("failed").Inc()
			}
			if w.log != nil {
				w.log.Warn().Err(err).Msg("news intel: telegram chunk failed")
			}
			continue
		}
		if w.met != nil {
			w.met.NewsIntelTelegramChunks.WithLabelValues("sent").Inc()
		}
		anySent = true
	}
	return anySent
}

// --- Helpers --------------------------------------------------------------

func filterByConfidence(in []openai.NewsIntelAIDecision, min float64) []openai.NewsIntelAIDecision {
	if min <= 0 {
		return in
	}
	out := make([]openai.NewsIntelAIDecision, 0, len(in))
	for _, d := range in {
		if d.Confidence >= min {
			out = append(out, d)
		}
	}
	return out
}

func shouldSuppress(decision string, suppressNoEdge bool) bool {
	if !suppressNoEdge {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(decision)) {
	case "ignore":
		return true
	}
	return false
}

// indexAffectedMarkets builds a hash → []affected map for the
// Telegram renderer (needs the original input list, not the AI's
// shorter `selected` array).
func indexAffectedMarkets(items []openai.NewsItemForAI) map[string][]openai.NewsAffectedMarketForAI {
	out := make(map[string][]openai.NewsAffectedMarketForAI, len(items))
	for _, it := range items {
		out[it.Hash] = it.AffectedMarkets
	}
	return out
}

// computeInputFingerprint hashes the (hash, event_slug, title) tuples
// of the supplied items so a re-run with the exact same input list
// hits the semantic cooldown.
func computeInputFingerprint(items []openai.NewsItemForAI) string {
	if len(items) == 0 {
		return ""
	}
	h := sha256.New()
	for _, it := range items {
		_, _ = h.Write([]byte(it.Hash))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(it.EventSlug))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(it.Title))
		_, _ = h.Write([]byte{0xff})
	}
	sum := h.Sum(nil)
	full := hex.EncodeToString(sum)
	if len(full) > 32 {
		return full[:32]
	}
	return full
}

// computeItemHashFromAnnotation falls back when the annotation row
// lacks a Polymarket-supplied item_hash. Stable across cycles for the
// same (event_slug, unix_time, title) triple.
func computeItemHashFromAnnotation(a repository.EventAnnotation) string {
	h := sha256.New()
	_, _ = h.Write([]byte(a.EventSlug))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(time.Unix(a.UnixTime, 0).UTC().Format(time.RFC3339)))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(a.Title))
	sum := h.Sum(nil)
	return hex.EncodeToString(sum)[:32]
}

// safeSplit is a defensive splitter for HTML-parse-mode messages.
// Telegram's hard cap is 4096; we honor the operator-supplied cap
// (default 3500) so headers + delimiters fit. The splitter prefers
// paragraph (\n\n) boundaries, then newline, then a hard cut.
func safeSplit(body string, cap int) []string {
	if cap <= 0 {
		cap = 3500
	}
	if len(body) <= cap {
		return []string{body}
	}
	var out []string
	rest := body
	for len(rest) > cap {
		cut := strings.LastIndex(rest[:cap], "\n\n")
		if cut <= 0 {
			cut = strings.LastIndex(rest[:cap], "\n")
		}
		if cut <= 0 {
			cut = cap
		}
		out = append(out, strings.TrimRight(rest[:cut], " \t\n"))
		rest = strings.TrimLeft(rest[cut:], " \t\n")
	}
	if len(rest) > 0 {
		out = append(out, rest)
	}
	return out
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func truncate(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	return s[:n]
}

// ErrDisabled is returned by some tests when the worker is run while
// disabled and the test expects to fail loud.
var ErrDisabled = errors.New("news intel worker disabled")
