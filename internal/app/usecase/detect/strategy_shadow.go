// strategy_shadow.go — v11.6 detect↔strategybus bridge, extended in
// v11.8 to fan out across all 9 strategies with staged-input readers.
//
// Surface:
//   - detect.Loop calls recordStrategyShadow once per newly-persisted
//     alert (after persistAlert returned true).
//   - For each enabled strategy, the bridge resolves staged inputs
//     from Postgres-backed readers (cached, bounded, timeout-guarded)
//     and runs the pure Decide().
//   - Each fired/tag/boost decision writes one shadow row via bus.
//   - When staged data is missing, the bridge records a precise
//     skip reason rather than firing a synthetic decision.
//
// Safety:
//   - bus.Record forces shadow_only=true while promotion is not
//     allowed (see strategybus.Bus). No Telegram, no AI.
//   - Bridge fails open: any non-nil error from a reader or the
//     writer is logged + metric'd but never blocks the live alert.
//   - Per-trade budget (StrategyShadowMaxPerTrade) caps how many
//     rows a single trade can spawn.
//   - All reads are bounded by ctx + the staged-inputs query
//     timeout (default 250ms).
package detect

import (
	"context"
	"fmt"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/analytics/catalystwindow"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/analytics/cheaptail"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/analytics/conflictresolve"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/analytics/repricinglag"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/analytics/rulesrisk"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/analytics/shadowdecisions"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/analytics/walletcohort"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/stagedinputs"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/anomaly"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/market"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/trade"
)

// StrategyShadowSink is the minimal surface detect.Loop needs from
// strategybus.Bus. Defined here so detect/ doesn't import strategybus
// directly — avoids a future import cycle if the bus ever needs to
// reference detect-public types.
type StrategyShadowSink interface {
	ShouldEvaluate(name string) bool
	Record(ctx context.Context, d shadowdecisions.Decision) (int64, error)
}

// recordStrategyShadow is the per-alert v11.6/v11.8 hook. Called
// once for every alert that survived dedup. Returns the number of
// shadow rows written.
func (l *Loop) recordStrategyShadow(
	ctx context.Context,
	m market.Market,
	t trade.Trade,
	f anomaly.Finding,
	dedupKey string,
) int {
	if l.cfg.StrategyShadowBus == nil {
		return 0
	}
	written := 0
	maxPerTrade := l.cfg.StrategyShadowMaxPerTrade
	if maxPerTrade <= 0 {
		maxPerTrade = 20
	}
	bus := l.cfg.StrategyShadowBus

	// 1. rulesrisk — pure, no staged input.
	if bus.ShouldEvaluate("rulesrisk") {
		if l.cfg.StrategyRulesRisk != nil && written < maxPerTrade {
			written += l.shadowRulesRisk(ctx, m, t, f, dedupKey)
		} else {
			l.observeStrategySkipped("rulesrisk", "no_detector")
		}
	}

	// All remaining strategies need staged inputs.
	staged := l.cfg.StrategyStagedInputs
	if staged == nil || !staged.Enabled() {
		// Single skip metric per strategy when readers are off entirely.
		for _, name := range []string{"thesisaccum", "holderdelta", "walletcohort", "catalystwindow", "bookvacuum", "repricinglag", "cheaptail", "conflictresolve"} {
			if bus.ShouldEvaluate(name) {
				l.observeStrategySkipped(name, "staged_inputs_disabled")
			}
		}
		return written
	}

	// 2. catalystwindow — booster. Needs catalysts for event.
	if written < maxPerTrade && bus.ShouldEvaluate("catalystwindow") {
		written += l.shadowCatalystWindow(ctx, m, t, f, dedupKey, staged)
	}
	// 3. walletcohort — booster. Needs wallet edges.
	if written < maxPerTrade && bus.ShouldEvaluate("walletcohort") {
		written += l.shadowWalletCohort(ctx, m, t, f, dedupKey, staged)
	}
	// 4. conflictresolve — arbitration. Needs recent shadow decisions.
	if written < maxPerTrade && bus.ShouldEvaluate("conflictresolve") {
		written += l.shadowConflictResolve(ctx, m, t, f, dedupKey, staged)
	}
	// 5. cheaptail — primary, requires risk + catalyst context.
	if written < maxPerTrade && bus.ShouldEvaluate("cheaptail") {
		written += l.shadowCheapTail(ctx, m, t, f, dedupKey, staged)
	}
	// 6. repricinglag — primary, needs closed repricing windows.
	if written < maxPerTrade && bus.ShouldEvaluate("repricinglag") {
		written += l.shadowRepricingLag(ctx, m, t, f, dedupKey, staged)
	}
	// 7. thesisaccum — primary, needs market_links presence + wallet aggregates.
	if written < maxPerTrade && bus.ShouldEvaluate("thesisaccum") {
		written += l.shadowThesisAccum(ctx, m, t, f, dedupKey, staged)
	}
	// 8. holderdelta — primary, needs holder snapshots (typically absent).
	if bus.ShouldEvaluate("holderdelta") {
		// No snapshot fetcher reader yet; surface explicit skip reason.
		l.observeStrategySkipped("holderdelta", "no_holder_snapshots_available")
	}
	// 9. bookvacuum — booster, needs book_feature_bars (no producer).
	if bus.ShouldEvaluate("bookvacuum") {
		l.observeStrategySkipped("bookvacuum", "no_book_feature_bars_producer")
	}
	return written
}

// --- per-strategy shadow writers ---------------------------------

func (l *Loop) shadowRulesRisk(
	ctx context.Context,
	m market.Market,
	t trade.Trade,
	f anomaly.Finding,
	dedupKey string,
) int {
	in := rulesrisk.Input{
		ConditionID: string(m.ID),
		Title:       m.Question,
	}
	v := l.cfg.StrategyRulesRisk.Decide(in)
	if !l.cfg.StrategyShadowRecordNoFire && v.AmbiguityScore <= 0 {
		l.observeStrategySkipped("rulesrisk", "no_markers")
		return 0
	}
	return l.writeShadow(ctx, "rulesrisk", shadowdecisions.KindTag, shadowdecisions.LevelNone,
		v.AmbiguityScore, v.DisputeRisk, v.Reasons, v.Features, m, t, f, dedupKey)
}

func (l *Loop) shadowCatalystWindow(
	ctx context.Context,
	m market.Market,
	t trade.Trade,
	f anomaly.Finding,
	dedupKey string,
	staged *stagedinputs.Readers,
) int {
	cats, err := staged.CatalystsByEvent(ctx, m.EventSlug)
	if err != nil {
		l.observeStrategySkipped("catalystwindow", "reader_error")
		return 0
	}
	if len(cats) == 0 {
		l.observeStrategySkipped("catalystwindow", "no_catalysts_for_event")
		return 0
	}
	// Map staged catalysts → detector input.
	detCats := make([]catalystwindow.Catalyst, 0, len(cats))
	for _, c := range cats {
		detCats = append(detCats, catalystwindow.Catalyst{
			Kind:       c.CatalystKind,
			ExpectedAt: c.ExpectedAt,
			Confidence: c.Confidence,
			EventSlug:  c.EventSlug,
		})
	}
	det := catalystwindow.New(catalystwindow.Config{})
	in := catalystwindow.Input{
		SignalTime:    l.now(),
		EventSlug:     m.EventSlug,
		Catalysts:     detCats,
		WindowsByKind: defaultCatalystWindows(),
		MinConfidence: 0.5,
		ParentScore:   1.0,
	}
	v := det.Decide(in)
	if !v.InWindow && !l.cfg.StrategyShadowRecordNoFire {
		l.observeStrategySkipped("catalystwindow", "outside_window")
		return 0
	}
	return l.writeShadow(ctx, "catalystwindow", shadowdecisions.KindBoost, shadowdecisions.LevelNone,
		v.Boost, v.Catalyst.Confidence, v.Reasons, v.Features, m, t, f, dedupKey)
}

func (l *Loop) shadowWalletCohort(
	ctx context.Context,
	m market.Market,
	t trade.Trade,
	f anomaly.Finding,
	dedupKey string,
	staged *stagedinputs.Readers,
) int {
	wallet := walletFromFinding(f)
	if wallet == "" {
		l.observeStrategySkipped("walletcohort", "no_wallet")
		return 0
	}
	edges, err := staged.WalletEdgesForWallet(ctx, wallet, 1)
	if err != nil {
		l.observeStrategySkipped("walletcohort", "reader_error")
		return 0
	}
	if len(edges) == 0 {
		l.observeStrategySkipped("walletcohort", "no_edges_for_wallet")
		return 0
	}
	det := walletcohort.New(walletcohort.Config{
		MinSimilarity: 0.5,
		MinEvents:     2,
		MinCohortSize: 2,
	})
	dEdges := make([]walletcohort.Edge, 0, len(edges))
	for _, e := range edges {
		a, b := wallet, e.Other
		if a > b {
			a, b = b, a
		}
		dEdges = append(dEdges, walletcohort.Edge{
			WalletA:         a,
			WalletB:         b,
			EdgeKind:        e.Kind,
			SimilarityScore: e.SimilarityScore,
			CoEventsCount:   e.CoEvents,
		})
	}
	v := det.Decide(walletcohort.Input{
		ConditionID: string(m.ID),
		EventSlug:   m.EventSlug,
		AlertWallet: wallet,
		AlertSide:   string(t.Side),
		Edges:       dEdges,
	})
	if !v.Converged && !l.cfg.StrategyShadowRecordNoFire {
		l.observeStrategySkipped("walletcohort", "no_convergence")
		return 0
	}
	return l.writeShadow(ctx, "walletcohort", shadowdecisions.KindBoost, shadowdecisions.LevelNone,
		v.Boost, v.SimilarityAvg, v.Reasons, v.Features, m, t, f, dedupKey)
}

func (l *Loop) shadowConflictResolve(
	ctx context.Context,
	m market.Market,
	t trade.Trade,
	f anomaly.Finding,
	dedupKey string,
	staged *stagedinputs.Readers,
) int {
	recent, err := staged.RecentDecisionsForCondition(ctx, string(m.ID), l.now().Add(-15*time.Minute))
	if err != nil {
		l.observeStrategySkipped("conflictresolve", "reader_error")
		return 0
	}
	if len(recent) == 0 {
		l.observeStrategySkipped("conflictresolve", "no_recent_decisions")
		return 0
	}
	sides := map[string]*conflictresolve.SideSignal{}
	for _, d := range recent {
		if d.Side == "" {
			continue
		}
		sa, ok := sides[d.Side]
		if !ok {
			sa = &conflictresolve.SideSignal{Side: d.Side}
			sides[d.Side] = sa
		}
		// Use score/confidence as proxies for wallet quality + breadth.
		if d.Score > sa.WalletQualityScore {
			sa.WalletQualityScore = d.Score
		}
		if d.Confidence > sa.ThesisBreadth {
			sa.ThesisBreadth = d.Confidence
		}
	}
	if len(sides) < 2 {
		l.observeStrategySkipped("conflictresolve", "no_opposing_side")
		return 0
	}
	// Build pairwise (A,B) input — pick first two distinct sides.
	var a, b conflictresolve.SideSignal
	picked := 0
	for _, s := range sides {
		if picked == 0 {
			a = *s
		} else if picked == 1 && s.Side != a.Side {
			b = *s
		}
		picked++
		if picked == 2 {
			break
		}
	}
	det := conflictresolve.New(conflictresolve.Config{})
	v := det.Decide(conflictresolve.ConflictInput{A: a, B: b})
	kind := shadowdecisions.KindTag
	switch v.Action {
	case conflictresolve.ActionBoostWinnerDegrade:
		kind = shadowdecisions.KindDegrade
	case conflictresolve.ActionBoostWinnerSuppress:
		kind = shadowdecisions.KindSuppress
	case conflictresolve.ActionTagUnresolved, conflictresolve.ActionKeepBoth:
		kind = shadowdecisions.KindTag
	}
	return l.writeShadow(ctx, "conflictresolve", kind, shadowdecisions.LevelNone,
		v.Dominance, 0.5, v.Reasons, v.Features, m, t, f, dedupKey)
}

func (l *Loop) shadowCheapTail(
	ctx context.Context,
	m market.Market,
	t trade.Trade,
	f anomaly.Finding,
	dedupKey string,
	staged *stagedinputs.Readers,
) int {
	// Cheap-tail probability band = 0.02..0.15
	if t.Price < 0.02 || t.Price > 0.15 {
		l.observeStrategySkipped("cheaptail", "price_outside_band")
		return 0
	}
	cats, _ := staged.CatalystsByEvent(ctx, m.EventSlug)
	risk, _, _ := staged.RiskScoreForCondition(ctx, string(m.ID))
	det := cheaptail.New(cheaptail.Config{
		MinPrice:        0.02,
		MaxPrice:        0.15,
		MinNotionalUSD:  500,
		MinTrades:       1,
		RequireCatalyst: false,
		MaxAmbiguity:    0.7,
	})
	notional := 0.0
	if f.Trade != nil {
		notional = f.Trade.NotionalUSD
	}
	in := cheaptail.Input{
		ConditionID:       string(m.ID),
		Wallet:            walletFromFinding(f),
		HasActiveCatalyst: len(cats) > 0,
		AmbiguityScore:    risk.AmbiguityScore,
		LifecyclePct:      f.LifecyclePct,
		Trades: []cheaptail.Trade{{
			Price:       t.Price,
			NotionalUSD: notional,
			Side:        string(t.Side),
			Timestamp:   t.Timestamp,
		}},
	}
	v := det.Decide(in)
	if !v.Fired && !l.cfg.StrategyShadowRecordNoFire {
		l.observeStrategySkipped("cheaptail", "below_threshold")
		return 0
	}
	return l.writeShadow(ctx, "cheaptail", shadowdecisions.KindStandalone,
		shadowdecisions.DecisionLevel(v.Level), v.Score, v.Convexity, v.Reasons, v.Features, m, t, f, dedupKey)
}

func (l *Loop) shadowRepricingLag(
	ctx context.Context,
	m market.Market,
	t trade.Trade,
	f anomaly.Finding,
	dedupKey string,
	staged *stagedinputs.Readers,
) int {
	wins, err := staged.ClosedRepricingWindowsForCondition(ctx, string(m.ID), l.now().Add(-24*time.Hour))
	if err != nil {
		l.observeStrategySkipped("repricinglag", "reader_error")
		return 0
	}
	if len(wins) == 0 {
		l.observeStrategySkipped("repricinglag", "no_closed_windows")
		return 0
	}
	// Use the most recent window.
	w := wins[0]
	det := repricinglag.New(repricinglag.Config{
		MinLagCents:  3,
		PeerMinCount: 2,
	})
	in := repricinglag.Input{
		ConditionID:       string(m.ID),
		EventSlug:         m.EventSlug,
		ObservedMoveCents: w.ObservedMove,
		PeerMovesCents:    []float64{w.PeerMove},
		AmbiguityScore:    0,
	}
	v := det.Decide(in)
	if !v.Fired && !l.cfg.StrategyShadowRecordNoFire {
		l.observeStrategySkipped("repricinglag", "below_threshold")
		return 0
	}
	return l.writeShadow(ctx, "repricinglag", shadowdecisions.KindStandalone,
		shadowdecisions.DecisionLevel(v.Level), v.LagScore, v.PeerMedian, v.Reasons, v.Features, m, t, f, dedupKey)
}

func (l *Loop) shadowThesisAccum(
	ctx context.Context,
	m market.Market,
	t trade.Trade,
	f anomaly.Finding,
	dedupKey string,
	staged *stagedinputs.Readers,
) int {
	links, err := staged.MarketLinksByEvent(ctx, m.EventSlug, 1)
	if err != nil {
		l.observeStrategySkipped("thesisaccum", "reader_error")
		return 0
	}
	if len(links) == 0 {
		l.observeStrategySkipped("thesisaccum", "no_market_links_for_event")
		return 0
	}
	// v11.10: consume real wallet thesis lines from
	// polymarket_wallet_thesis_lines (populated by thesislines worker).
	// Hot-path-safe: bounded reader with TTL cache + 250ms timeout.
	wallet := walletFromFinding(f)
	if wallet == "" {
		l.observeStrategySkipped("thesisaccum", "no_wallet")
		return 0
	}
	const lookbackHours = 720 // 30 days, matches default THESIS_LINES_LOOKBACK
	lines, lerr := staged.WalletThesisLinesForEvent(ctx, m.EventSlug, wallet, lookbackHours)
	if lerr != nil {
		l.observeStrategySkipped("thesisaccum", "wallet_lines_reader_error")
		return 0
	}
	if len(lines) == 0 {
		// Wallet has no aggregate exposure in this event — emit a
		// structural Tag (link_count only) so promotion can see the
		// graph was present but the wallet hadn't built lines yet.
		if !l.cfg.StrategyShadowRecordNoFire {
			l.observeStrategySkipped("thesisaccum", "no_wallet_thesis_lines")
			return 0
		}
		reasons := []string{
			fmt.Sprintf("links_found=%d", len(links)),
			"no_wallet_thesis_lines",
		}
		return l.writeShadow(ctx, "thesisaccum", shadowdecisions.KindTag, shadowdecisions.LevelNone,
			float64(len(links)), 0.2, reasons, map[string]any{"link_count": len(links)}, m, t, f, dedupKey)
	}
	// Compute breadth + aligned/opposed exposure relative to the
	// alert's Side. Single-market wallets cannot fire as cross-market
	// thesis (breadth < 2).
	alertSide := string(t.Side)
	breadth := len(lines)
	aligned, opposed := 0.0, 0.0
	for _, ln := range lines {
		if alertSide != "" && ln.Side == alertSide {
			aligned += ln.NotionalUSD
		} else {
			opposed += ln.NotionalUSD
		}
	}
	denom := aligned + opposed
	consistency := 0.0
	if denom > 0 {
		consistency = aligned / denom
	}
	if breadth < 2 {
		l.observeStrategySkipped("thesisaccum", "single_market_line")
		return 0
	}
	// Score: breadth × consistency × log1p(aligned). Bounded by
	// the existing detector's scoring shape; we record the row
	// regardless and let promotion-review decide.
	score := float64(breadth) * consistency
	level := shadowdecisions.LevelInfo
	if consistency >= 0.75 && aligned >= 1500 {
		level = shadowdecisions.LevelWarning
	}
	reasons := []string{
		fmt.Sprintf("breadth=%d", breadth),
		fmt.Sprintf("aligned_usd=%.2f", aligned),
		fmt.Sprintf("opposed_usd=%.2f", opposed),
		fmt.Sprintf("consistency=%.3f", consistency),
	}
	features := map[string]any{
		"link_count":        len(links),
		"breadth":           breadth,
		"aligned_usd":       aligned,
		"opposed_usd":       opposed,
		"consistency":       consistency,
		"wallet_lookback_h": lookbackHours,
	}
	return l.writeShadow(ctx, "thesisaccum", shadowdecisions.KindStandalone, level,
		score, consistency, reasons, features, m, t, f, dedupKey)
}

// --- helpers ------------------------------------------------------

// writeShadow is the common Bus.Record wrapper.
func (l *Loop) writeShadow(
	ctx context.Context,
	strategy string,
	kind shadowdecisions.DecisionKind,
	level shadowdecisions.DecisionLevel,
	score, confidence float64,
	reasons []string,
	features map[string]any,
	m market.Market,
	t trade.Trade,
	f anomaly.Finding,
	dedupKey string,
) int {
	catLabel := ""
	if f.Category != nil {
		catLabel = f.Category.Label
	}
	d := shadowdecisions.Decision{
		StrategyName:        strategy,
		ConditionID:         string(m.ID),
		EventSlug:           m.EventSlug,
		Wallet:              walletFromFinding(f),
		Side:                string(t.Side),
		Kind:                kind,
		Level:               level,
		Score:               score,
		Confidence:          confidence,
		Reasons:             reasons,
		Features:            features,
		LinkedAlertDedupKey: dedupKey,
		ControlBucketKey: shadowdecisions.ControlBucketKey(
			catLabel,
			lifecycleBucket(f.LifecyclePct),
			"",
			notionalBucket(f.Trade),
			"",
		),
	}
	if _, err := l.cfg.StrategyShadowBus.Record(ctx, d); err != nil {
		l.observeStrategyEval(strategy, "write_failed")
		l.log.Err(err).Str("strategy", strategy).Msg("detect: shadow write failed")
		return 0
	}
	l.observeStrategyEval(strategy, "ok")
	return 1
}

func walletFromFinding(f anomaly.Finding) string {
	if f.Trade == nil {
		return ""
	}
	return f.Trade.Wallet
}

func defaultCatalystWindows() map[string]catalystwindow.WindowSpec {
	return map[string]catalystwindow.WindowSpec{
		"debate":             {Pre: 12 * time.Hour, Post: 4 * time.Hour},
		"court_ruling":       {Pre: 24 * time.Hour, Post: 12 * time.Hour},
		"election_day":       {Pre: 72 * time.Hour, Post: 24 * time.Hour},
		"official_statement": {Pre: 4 * time.Hour, Post: 2 * time.Hour},
		"generic":            {Pre: 6 * time.Hour, Post: 3 * time.Hour},
	}
}

// lifecycleBucket buckets a 0..100 lifecycle pct for control-bucket
// joins. Bucket boundaries match the v11.x severity gates.
func lifecycleBucket(p float64) string {
	switch {
	case p < 50:
		return "0-50"
	case p < 75:
		return "50-75"
	case p < 90:
		return "75-90"
	default:
		return "90-100"
	}
}

// notionalBucket buckets a trade's notional USD into compact ranges.
func notionalBucket(ref *anomaly.TradeRef) string {
	if ref == nil {
		return ""
	}
	n := ref.NotionalUSD
	switch {
	case n < 1_000:
		return "<1k"
	case n < 10_000:
		return "1k-10k"
	case n < 100_000:
		return "10k-100k"
	default:
		return ">=100k"
	}
}

func (l *Loop) observeStrategyEval(strategy, status string) {
	if l.metrics == nil || l.metrics.StrategyEvalTotal == nil {
		return
	}
	l.metrics.StrategyEvalTotal.WithLabelValues(strategy, status).Inc()
}

func (l *Loop) observeStrategySkipped(strategy, reason string) {
	if l.metrics == nil || l.metrics.StrategyEvalSkipped == nil {
		return
	}
	l.metrics.StrategyEvalSkipped.WithLabelValues(strategy, reason).Inc()
}
