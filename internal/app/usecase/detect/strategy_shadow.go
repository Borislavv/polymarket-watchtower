// strategy_shadow.go — v11.12-insider-prior detect↔strategybus
// bridge. Wires the production staged readers + typed StrategyConfig
// into the hot path so prod ENV is the single source of truth for
// every detector threshold.
//
// Surface (per alert):
//
//	rulesrisk        — pure, no staged input
//	catalystwindow   — staged catalysts
//	walletcohort     — staged wallet edges
//	conflictresolve  — staged recent shadow decisions
//	cheaptail        — staged catalysts + staged risk score
//	repricinglag     — staged closed repricing windows
//	thesisaccum      — staged market_links + staged wallet thesis lines
//	holderdelta      — staged holder snapshots (current + previous)
//	bookvacuum       — staged recent book feature bars + rolling baseline
//	marketregime     — pure tag (no staged input)
//
// Safety:
//   - bus.Record forces shadow_only=true while promotion is not
//     allowed. No Telegram, no AI.
//   - Bridge fails open: any non-nil error from a reader or writer
//     is logged + metric'd but never blocks the live alert.
//   - All reads are bounded by ctx + staged-inputs query timeout
//     (default 250ms).
//   - StrategyShadowMaxPerTrade caps per-trade rows.
//
// All previously-hardcoded thresholds now come from cfg.Strategy*.
// Zero-value runtime configs fall through to per-detector defaults
// (applyDefaults in each analytics package).
package detect

import (
	"context"
	"fmt"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/analytics/bookvacuum"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/analytics/catalystwindow"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/analytics/cheaptail"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/analytics/conflictresolve"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/analytics/holderdelta"
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
// directly — avoids a future import cycle.
type StrategyShadowSink interface {
	ShouldEvaluate(name string) bool
	Record(ctx context.Context, d shadowdecisions.Decision) (int64, error)
}

// recordStrategyShadow is the per-alert v11.12 hook. Called once for
// every alert that survived dedup. Returns the number of shadow rows
// written. Sequence is deterministic: rulesrisk first (its
// AmbiguityScore is consumed by cheaptail and repricinglag downstream).
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

	// 0. marketregime — pure, no staged input. Runs first so the
	// regime label is available for downstream features (today it
	// is stamped on its own KindTag row; future work attaches it
	// to every shadow features_json under "market_regime").
	var regime string
	if l.cfg.StrategyMarketRegime != nil && written < maxPerTrade && bus.ShouldEvaluate("marketregime") {
		w, r := l.shadowMarketRegime(ctx, m, t, f, dedupKey)
		written += w
		regime = r
	}

	// 1. rulesrisk — pure ambiguity scorer. Score consumed
	// downstream by cheaptail + repricinglag.
	var ambiguity float64
	if bus.ShouldEvaluate("rulesrisk") {
		if l.cfg.StrategyRulesRisk != nil && written < maxPerTrade {
			w, a := l.shadowRulesRisk(ctx, m, t, f, dedupKey, regime)
			written += w
			ambiguity = a
		} else {
			l.observeStrategySkipped("rulesrisk", "no_detector")
		}
	}

	staged := l.cfg.StrategyStagedInputs
	if staged == nil || !staged.Enabled() {
		for _, name := range []string{"thesisaccum", "holderdelta", "walletcohort", "catalystwindow", "bookvacuum", "repricinglag", "cheaptail", "conflictresolve"} {
			if bus.ShouldEvaluate(name) {
				l.observeStrategySkipped(name, "staged_inputs_disabled")
			}
		}
		return written
	}

	// 2. catalystwindow — booster.
	if written < maxPerTrade && bus.ShouldEvaluate("catalystwindow") {
		written += l.shadowCatalystWindow(ctx, m, t, f, dedupKey, staged, regime)
	}
	// 3. walletcohort — booster (edge_density + fresh_wallet_burst).
	if written < maxPerTrade && bus.ShouldEvaluate("walletcohort") {
		written += l.shadowWalletCohort(ctx, m, t, f, dedupKey, staged, regime)
	}
	// 4. conflictresolve — arbitration.
	if written < maxPerTrade && bus.ShouldEvaluate("conflictresolve") {
		written += l.shadowConflictResolve(ctx, m, t, f, dedupKey, staged, regime)
	}
	// 5. cheaptail — primary, blocked by rulesrisk ambiguity.
	if written < maxPerTrade && bus.ShouldEvaluate("cheaptail") {
		written += l.shadowCheapTail(ctx, m, t, f, dedupKey, staged, ambiguity, regime)
	}
	// 6. repricinglag — primary, blocked by rulesrisk ambiguity.
	if written < maxPerTrade && bus.ShouldEvaluate("repricinglag") {
		written += l.shadowRepricingLag(ctx, m, t, f, dedupKey, staged, ambiguity, regime)
	}
	// 7. thesisaccum — primary.
	if written < maxPerTrade && bus.ShouldEvaluate("thesisaccum") {
		written += l.shadowThesisAccum(ctx, m, t, f, dedupKey, staged, regime)
	}
	// 8. holderdelta — primary, real staged snapshots.
	if written < maxPerTrade && bus.ShouldEvaluate("holderdelta") {
		written += l.shadowHolderDelta(ctx, m, t, f, dedupKey, staged, regime)
	}
	// 9. bookvacuum — booster, real staged bars.
	if written < maxPerTrade && bus.ShouldEvaluate("bookvacuum") {
		written += l.shadowBookVacuum(ctx, m, t, f, dedupKey, staged, regime)
	}
	return written
}

// --- per-strategy shadow writers ---------------------------------

func (l *Loop) shadowMarketRegime(
	ctx context.Context,
	m market.Market,
	t trade.Trade,
	f anomaly.Finding,
	dedupKey string,
) (int, string) {
	catSlug, catLabel := "", ""
	if f.Category != nil {
		catSlug = f.Category.Slug
		catLabel = f.Category.Label
	}
	v := l.cfg.StrategyMarketRegime.Classify(MarketRegimeInput{
		CategorySlug:  catSlug,
		CategoryLabel: catLabel,
		Title:         m.Question,
		EventSlug:     m.EventSlug,
	})
	features := map[string]any{
		"market_regime":              v.Regime,
		"market_regime_score":        v.Score,
		"requires_dual_confirmation": v.Regime == "oracle_sensitive",
	}
	reasons := append([]string{}, v.Reasons...)
	n := l.writeShadow(ctx, "marketregime", shadowdecisions.KindTag, shadowdecisions.LevelNone,
		v.Score, v.Score, reasons, features, m, t, f, dedupKey, v.Regime)
	return n, v.Regime
}

func (l *Loop) shadowRulesRisk(
	ctx context.Context,
	m market.Market,
	t trade.Trade,
	f anomaly.Finding,
	dedupKey string,
	regime string,
) (int, float64) {
	in := rulesrisk.Input{
		ConditionID: string(m.ID),
		Title:       m.Question,
	}
	v := l.cfg.StrategyRulesRisk.Decide(in)
	if !l.cfg.StrategyShadowRecordNoFire && v.AmbiguityScore <= 0 {
		l.observeStrategySkipped("rulesrisk", "no_markers")
		return 0, 0
	}
	features := copyFeatures(v.Features)
	if regime != "" {
		features["market_regime"] = regime
	}
	n := l.writeShadow(ctx, "rulesrisk", shadowdecisions.KindTag, shadowdecisions.LevelNone,
		v.AmbiguityScore, v.DisputeRisk, v.Reasons, features, m, t, f, dedupKey, regime)
	return n, v.AmbiguityScore
}

func (l *Loop) shadowCatalystWindow(
	ctx context.Context,
	m market.Market,
	t trade.Trade,
	f anomaly.Finding,
	dedupKey string,
	staged StagedReaders,
	regime string,
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
	detCats := make([]catalystwindow.Catalyst, 0, len(cats))
	for _, c := range cats {
		detCats = append(detCats, catalystwindow.Catalyst{
			Kind:       c.CatalystKind,
			ExpectedAt: c.ExpectedAt,
			Confidence: c.Confidence,
			EventSlug:  c.EventSlug,
		})
	}
	rc := l.cfg.StrategyCatalystWindow
	det := catalystwindow.New(catalystwindow.Config{})
	minConf := rc.MinConfidence
	if minConf <= 0 {
		minConf = 0.65
	}
	in := catalystwindow.Input{
		SignalTime:    l.now(),
		EventSlug:     m.EventSlug,
		Catalysts:     detCats,
		WindowsByKind: catalystWindowsFromCfg(rc),
		MinConfidence: minConf,
		ParentScore:   1.0,
	}
	v := det.Decide(in)
	if !v.InWindow && !l.cfg.StrategyShadowRecordNoFire {
		l.observeStrategySkipped("catalystwindow", "outside_window")
		return 0
	}
	features := copyFeatures(v.Features)
	if regime != "" {
		features["market_regime"] = regime
	}
	return l.writeShadow(ctx, "catalystwindow", shadowdecisions.KindBoost, shadowdecisions.LevelNone,
		v.Boost, v.Catalyst.Confidence, v.Reasons, features, m, t, f, dedupKey, regime)
}

func (l *Loop) shadowWalletCohort(
	ctx context.Context,
	m market.Market,
	t trade.Trade,
	f anomaly.Finding,
	dedupKey string,
	staged StagedReaders,
	regime string,
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
	rc := l.cfg.StrategyWalletCohort
	cohortHits := rc.MinCohortHits
	if cohortHits <= 0 {
		cohortHits = 2
	}
	if len(edges) == 0 && rc.FreshWalletMinBurst <= 0 {
		l.observeStrategySkipped("walletcohort", "no_edges_for_wallet")
		return 0
	}
	det := walletcohort.New(walletcohort.Config{
		MinSimilarity:       rc.MinSimilarity,
		MinEvents:           rc.MinEvents,
		MinCohortSize:       cohortHits,
		FreshWalletMinBurst: rc.FreshWalletMinBurst,
		FreshWalletMaxAge:   rc.FreshWalletMaxAge,
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
		Now:         l.now(),
		Edges:       dEdges,
	})
	if !v.Converged && !l.cfg.StrategyShadowRecordNoFire {
		l.observeStrategySkipped("walletcohort", "no_convergence")
		return 0
	}
	features := copyFeatures(v.Features)
	if regime != "" {
		features["market_regime"] = regime
	}
	return l.writeShadow(ctx, "walletcohort", shadowdecisions.KindBoost, shadowdecisions.LevelNone,
		v.Boost, v.SimilarityAvg, v.Reasons, features, m, t, f, dedupKey, regime)
}

func (l *Loop) shadowConflictResolve(
	ctx context.Context,
	m market.Market,
	t trade.Trade,
	f anomaly.Finding,
	dedupKey string,
	staged StagedReaders,
	regime string,
) int {
	rc := l.cfg.StrategyConflictResolve
	window := rc.Window
	if window <= 0 {
		window = 20 * time.Minute
	}
	recent, err := staged.RecentDecisionsForCondition(ctx, string(m.ID), l.now().Add(-window))
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
	// conflictresolve.Config does not surface MinQualitySum today;
	// rc.MinQualitySum is recorded on the row's features_json so
	// promotion review can re-aggregate if the detector later
	// surfaces it. The other two knobs flow through directly.
	det := conflictresolve.New(conflictresolve.Config{
		MinDominance: rc.MinDominance,
		MMPenalty:    rc.MMPenalty,
	})
	v := det.Decide(conflictresolve.ConflictInput{A: a, B: b})
	kind := shadowdecisions.KindTag
	switch v.Action {
	case conflictresolve.ActionBoostWinnerDegrade:
		kind = shadowdecisions.KindDegrade
	case conflictresolve.ActionBoostWinnerSuppress:
		kind = shadowdecisions.KindSuppress
	}
	features := copyFeatures(v.Features)
	if regime != "" {
		features["market_regime"] = regime
	}
	return l.writeShadow(ctx, "conflictresolve", kind, shadowdecisions.LevelNone,
		v.Dominance, 0.5, v.Reasons, features, m, t, f, dedupKey, regime)
}

func (l *Loop) shadowCheapTail(
	ctx context.Context,
	m market.Market,
	t trade.Trade,
	f anomaly.Finding,
	dedupKey string,
	staged StagedReaders,
	ambiguity float64,
	regime string,
) int {
	rc := l.cfg.StrategyCheapTail
	minPrice, maxPrice := rc.MinPrice, rc.MaxPrice
	if minPrice <= 0 {
		minPrice = 0.03
	}
	if maxPrice <= 0 {
		maxPrice = 0.25
	}
	if t.Price < minPrice || t.Price > maxPrice {
		l.observeStrategySkipped("cheaptail", "price_outside_band")
		return 0
	}
	ambiguityCutoff := rc.AmbiguityCutoff
	if ambiguityCutoff <= 0 {
		ambiguityCutoff = 0.50
	}
	if ambiguity >= ambiguityCutoff {
		l.observeStrategySkipped("cheaptail", "blocked_by_rulesrisk")
		return 0
	}
	cats, _ := staged.CatalystsByEvent(ctx, m.EventSlug)
	// Fall back to staged risk score if rulesrisk didn't run earlier.
	risk, _, _ := staged.RiskScoreForCondition(ctx, string(m.ID))
	combinedAmbiguity := ambiguity
	if risk.AmbiguityScore > combinedAmbiguity {
		combinedAmbiguity = risk.AmbiguityScore
	}
	det := cheaptail.New(cheaptail.Config{
		MinPrice:        minPrice,
		MaxPrice:        maxPrice,
		MinNotionalUSD:  rc.MinNotionalUSD,
		MinTrades:       rc.MinTrades,
		RequireCatalyst: rc.RequireCatalyst,
		MaxAmbiguity:    ambiguityCutoff,
	})
	notional := 0.0
	if f.Trade != nil {
		notional = f.Trade.NotionalUSD
	}
	in := cheaptail.Input{
		ConditionID:       string(m.ID),
		Wallet:            walletFromFinding(f),
		HasActiveCatalyst: len(cats) > 0,
		AmbiguityScore:    combinedAmbiguity,
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
	features := copyFeatures(v.Features)
	if regime != "" {
		features["market_regime"] = regime
	}
	features["ambiguity_score"] = combinedAmbiguity
	return l.writeShadow(ctx, "cheaptail", shadowdecisions.KindStandalone,
		shadowdecisions.DecisionLevel(v.Level), v.Score, v.Convexity, v.Reasons, features, m, t, f, dedupKey, regime)
}

func (l *Loop) shadowRepricingLag(
	ctx context.Context,
	m market.Market,
	t trade.Trade,
	f anomaly.Finding,
	dedupKey string,
	staged StagedReaders,
	ambiguity float64,
	regime string,
) int {
	rc := l.cfg.StrategyRepricingLag
	maxAmb := rc.MaxAmbiguity
	if maxAmb <= 0 {
		maxAmb = 0.45
	}
	if ambiguity >= maxAmb {
		l.observeStrategySkipped("repricinglag", "blocked_by_rulesrisk")
		return 0
	}
	wins, err := staged.ClosedRepricingWindowsForCondition(ctx, string(m.ID), l.now().Add(-24*time.Hour))
	if err != nil {
		l.observeStrategySkipped("repricinglag", "reader_error")
		return 0
	}
	if len(wins) == 0 {
		l.observeStrategySkipped("repricinglag", "no_closed_windows")
		return 0
	}
	w := wins[0]
	peerMin := rc.PeerMinCount
	if peerMin <= 0 {
		peerMin = 3
	}
	minLag := rc.MinLagCents
	if minLag <= 0 {
		minLag = 4
	}
	det := repricinglag.New(repricinglag.Config{
		MinLagCents:  minLag,
		PeerMinCount: peerMin,
		MaxAmbiguity: maxAmb,
	})
	in := repricinglag.Input{
		ConditionID:       string(m.ID),
		EventSlug:         m.EventSlug,
		ObservedMoveCents: w.ObservedMove,
		PeerMovesCents:    []float64{w.PeerMove},
		AmbiguityScore:    ambiguity,
	}
	v := det.Decide(in)
	if !v.Fired && !l.cfg.StrategyShadowRecordNoFire {
		l.observeStrategySkipped("repricinglag", "below_threshold")
		return 0
	}
	features := copyFeatures(v.Features)
	if regime != "" {
		features["market_regime"] = regime
	}
	features["ambiguity_score"] = ambiguity
	return l.writeShadow(ctx, "repricinglag", shadowdecisions.KindStandalone,
		shadowdecisions.DecisionLevel(v.Level), v.LagScore, v.PeerMedian, v.Reasons, features, m, t, f, dedupKey, regime)
}

func (l *Loop) shadowThesisAccum(
	ctx context.Context,
	m market.Market,
	t trade.Trade,
	f anomaly.Finding,
	dedupKey string,
	staged StagedReaders,
	regime string,
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
	wallet := walletFromFinding(f)
	if wallet == "" {
		l.observeStrategySkipped("thesisaccum", "no_wallet")
		return 0
	}
	rc := l.cfg.StrategyThesisAccum
	lookback := rc.LookbackLifetime
	if lookback <= 0 {
		// v11.12-insider-prior default = 2160h (90d) — matches
		// THESIS_LINES_LOOKBACK so the worker output is consumed
		// in full instead of the old 720h-vs-2160h truncation.
		lookback = 2160 * time.Hour
	}
	lookbackHours := int(lookback.Hours())
	lines, lerr := staged.WalletThesisLinesForEvent(ctx, m.EventSlug, wallet, lookbackHours)
	if lerr != nil {
		l.observeStrategySkipped("thesisaccum", "wallet_lines_reader_error")
		return 0
	}
	if len(lines) == 0 {
		if !l.cfg.StrategyShadowRecordNoFire {
			l.observeStrategySkipped("thesisaccum", "no_wallet_thesis_lines")
			return 0
		}
		reasons := []string{
			fmt.Sprintf("links_found=%d", len(links)),
			"no_wallet_thesis_lines",
		}
		features := map[string]any{"link_count": len(links), "wallet_lookback_h": lookbackHours}
		if regime != "" {
			features["market_regime"] = regime
		}
		return l.writeShadow(ctx, "thesisaccum", shadowdecisions.KindTag, shadowdecisions.LevelNone,
			float64(len(links)), 0.2, reasons, features, m, t, f, dedupKey, regime)
	}
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
	minBreadth := rc.MinBreadth
	if minBreadth <= 0 {
		minBreadth = 2
	}
	if breadth < minBreadth {
		l.observeStrategySkipped("thesisaccum", "below_min_breadth")
		return 0
	}
	minCons := rc.MinConsistency
	if minCons <= 0 {
		minCons = 0.82
	}
	liqFloor := rc.LiquidityFloorUSD
	if liqFloor <= 0 {
		liqFloor = 1000
	}
	score := float64(breadth) * consistency
	level := shadowdecisions.LevelNone
	if consistency >= minCons && aligned >= liqFloor {
		level = shadowdecisions.LevelInfo
		if breadth >= 3 && consistency >= 0.88 && aligned >= 3*liqFloor {
			level = shadowdecisions.LevelWarning
		}
	}
	reasons := []string{
		fmt.Sprintf("breadth=%d", breadth),
		fmt.Sprintf("aligned_usd=%.2f", aligned),
		fmt.Sprintf("opposed_usd=%.2f", opposed),
		fmt.Sprintf("consistency=%.3f", consistency),
		fmt.Sprintf("min_consistency=%.2f liquidity_floor_usd=%.0f", minCons, liqFloor),
	}
	features := map[string]any{
		"link_count":          len(links),
		"breadth":             breadth,
		"aligned_usd":         aligned,
		"opposed_usd":         opposed,
		"consistency":         consistency,
		"wallet_lookback_h":   lookbackHours,
		"min_consistency":     minCons,
		"liquidity_floor_usd": liqFloor,
	}
	if regime != "" {
		features["market_regime"] = regime
	}
	return l.writeShadow(ctx, "thesisaccum", shadowdecisions.KindStandalone, level,
		score, consistency, reasons, features, m, t, f, dedupKey, regime)
}

// shadowHolderDelta is the v11.12-insider-prior real-source holderdelta
// path. Reads (current, previous) snapshot pair via stagedinputs and
// runs the pure detector. Skip reasons are explicit and bounded:
//
//	no_wallet                     — finding had no wallet attached
//	no_outcome_token              — finding lacks an outcome token to scope the snapshot
//	holder_reader_error           — reader returned a non-NoRows error
//	no_holder_snapshots_available — wallet has zero snapshots
//	no_previous_snapshot          — only one snapshot exists (delta undefined)
//	stale_snapshot                — current snapshot older than FreshSnapshotMaxAge
//	below_threshold               — detector did not fire
func (l *Loop) shadowHolderDelta(
	ctx context.Context,
	m market.Market,
	t trade.Trade,
	f anomaly.Finding,
	dedupKey string,
	staged StagedReaders,
	regime string,
) int {
	wallet := walletFromFinding(f)
	if wallet == "" {
		l.observeStrategySkipped("holderdelta", "no_wallet")
		return 0
	}
	token := outcomeTokenFromFinding(f, t)
	if token == "" {
		// Finding may carry only the outcome LABEL ("Yes"/"No"); the
		// detector needs the CLOB token id to scope the snapshot. We
		// could fall back to summing across all tokens but that
		// would inflate PctOI on multi-leg markets — refuse safely.
		l.observeStrategySkipped("holderdelta", "no_outcome_token")
		return 0
	}
	pair, ok, err := staged.HolderSnapshotPairForWallet(ctx, string(m.ID), token, wallet)
	if err != nil {
		l.observeStrategySkipped("holderdelta", "holder_reader_error")
		return 0
	}
	if !ok {
		l.observeStrategySkipped("holderdelta", "no_holder_snapshots_available")
		return 0
	}
	if !pair.PreviousValid {
		l.observeStrategySkipped("holderdelta", "no_previous_snapshot")
		return 0
	}
	rc := l.cfg.StrategyHolderDelta
	freshMax := rc.FreshSnapshotMaxAge
	if freshMax <= 0 {
		freshMax = 45 * time.Minute
	}
	now := l.now()
	if !pair.Current.SnapshotAt.IsZero() && now.Sub(pair.Current.SnapshotAt) > freshMax {
		l.observeStrategySkipped("holderdelta", "stale_snapshot")
		return 0
	}
	det := holderdelta.New(holderdelta.Config{
		MinPctOIInfo:     rc.MinPctOIInfo,
		MinPctOIWarning:  rc.MinPctOIWarning,
		MinPctOICritical: rc.MinPctOICritical,
		TopK:             rc.TopK,
		MinSharesDelta:   rc.MinSharesDelta,
		OIShrinkPenalty:  rc.OIShrinkPenalty,
	})
	in := holderdelta.Input{
		ConditionID:  string(m.ID),
		OutcomeToken: token,
		Wallet:       wallet,
		Now:          now,
		Current:      holderSnapshotToDetector(pair.Current),
		Previous:     holderSnapshotToDetector(pair.Previous),
	}
	v := det.Decide(in)
	if !v.Fired && !l.cfg.StrategyShadowRecordNoFire {
		l.observeStrategySkipped("holderdelta", "below_threshold")
		return 0
	}
	features := copyFeatures(v.Features)
	if regime != "" {
		features["market_regime"] = regime
	}
	return l.writeShadow(ctx, "holderdelta", shadowdecisions.KindStandalone,
		shadowdecisions.DecisionLevel(v.Level), v.Score, v.Confidence, v.Reasons, features, m, t, f, dedupKey, regime)
}

// shadowBookVacuum is the v11.12-insider-prior real-source bookvacuum
// path. Reads recent bars via stagedinputs and runs the pure detector.
// Skip reasons:
//
//	no_outcome_token   — finding lacks an outcome token
//	bookbars_reader_error
//	no_book_feature_bars       — no producer wired for this token (default)
//	stale_bar                  — latest bar older than MaxAgeBar
//	baseline_missing           — only one bar exists, no baseline computable
//	depth_missing              — bid/ask depth not produced (vacuum needs depth)
//	below_threshold            — detector did not fire
func (l *Loop) shadowBookVacuum(
	ctx context.Context,
	m market.Market,
	t trade.Trade,
	f anomaly.Finding,
	dedupKey string,
	staged StagedReaders,
	regime string,
) int {
	token := outcomeTokenFromFinding(f, t)
	if token == "" {
		l.observeStrategySkipped("bookvacuum", "no_outcome_token")
		return 0
	}
	rc := l.cfg.StrategyBookVacuum
	maxAgeBar := rc.MaxAgeBar
	if maxAgeBar <= 0 {
		maxAgeBar = 90 * time.Second
	}
	// Look back 10× the freshness window so the rolling baseline has
	// breathing room. The reader is bounded by MaxRows.
	since := l.now().Add(-10 * maxAgeBar)
	bars, err := staged.RecentBookFeatureBars(ctx, string(m.ID), token, since, 0)
	if err != nil {
		l.observeStrategySkipped("bookvacuum", "bookbars_reader_error")
		return 0
	}
	if len(bars) == 0 {
		l.observeStrategySkipped("bookvacuum", "no_book_feature_bars")
		return 0
	}
	latest := bars[0]
	if !latest.BarStart.IsZero() && l.now().Sub(latest.BarStart) > maxAgeBar {
		l.observeStrategySkipped("bookvacuum", "stale_bar")
		return 0
	}
	if !latest.BidDepthValid || !latest.AskDepthValid {
		l.observeStrategySkipped("bookvacuum", "depth_missing")
		return 0
	}
	if len(bars) < 2 {
		l.observeStrategySkipped("bookvacuum", "baseline_missing")
		return 0
	}
	// Build baseline by averaging the older bars (index 1..N).
	baseline := bookvacuumBaseline(bars[1:])
	det := bookvacuum.New(bookvacuum.Config{
		MinCollapsePct: rc.MinCollapsePct,
		MinSpreadZ:     rc.MinSpreadZ,
	})
	in := bookvacuum.Input{
		Recent:   featureBarToDetector(latest),
		Baseline: baseline,
		SpreadZ:  latest.SpreadZ,
		MMLike:   false, // MM-flagged markets are filtered upstream by MMfilter
	}
	v := det.Decide(in)
	if !v.Detected && !l.cfg.StrategyShadowRecordNoFire {
		l.observeStrategySkipped("bookvacuum", "below_threshold")
		return 0
	}
	// Mid-shift gate: a real vacuum should accompany a mid move.
	minMidShift := rc.MinMidShiftPct
	if minMidShift > 0 && latest.MidPrice > 0 && (absFloat(latest.MidDelta)/latest.MidPrice) < minMidShift {
		l.observeStrategySkipped("bookvacuum", "mid_shift_below_threshold")
		return 0
	}
	features := copyFeatures(v.Features)
	if regime != "" {
		features["market_regime"] = regime
	}
	features["bar_age_seconds"] = l.now().Sub(latest.BarStart).Seconds()
	features["baseline_bars"] = len(bars) - 1
	return l.writeShadow(ctx, "bookvacuum", shadowdecisions.KindBoost, shadowdecisions.LevelNone,
		v.Boost, 0.5, v.Reasons, features, m, t, f, dedupKey, regime)
}

// --- helpers ------------------------------------------------------

// writeShadow is the common Bus.Record wrapper. regime is stamped
// into the control bucket key so promotion can stratify per-regime.
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
	regime string,
) int {
	catLabel := ""
	if f.Category != nil {
		catLabel = f.Category.Label
	}
	if features == nil {
		features = map[string]any{}
	}
	if regime != "" {
		if _, present := features["market_regime"]; !present {
			features["market_regime"] = regime
		}
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
			regime,
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

// outcomeTokenFromFinding returns the CLOB token id for the alert's
// outcome side. Falls through to t.OutcomeToken when the finding
// doesn't carry one (the typical case for non-accumulation findings).
func outcomeTokenFromFinding(f anomaly.Finding, t trade.Trade) string {
	if f.Accumulation != nil && f.Accumulation.OutcomeToken != "" {
		return f.Accumulation.OutcomeToken
	}
	if tok := string(t.Token); tok != "" {
		return tok
	}
	return ""
}

func catalystWindowsFromCfg(rc CatalystWindowRuntimeConfig) map[string]catalystwindow.WindowSpec {
	dflt := map[string]catalystwindow.WindowSpec{
		"debate":             {Pre: 8 * time.Hour, Post: 3 * time.Hour},
		"court_ruling":       {Pre: 18 * time.Hour, Post: 8 * time.Hour},
		"election_day":       {Pre: 48 * time.Hour, Post: 12 * time.Hour},
		"official_statement": {Pre: 6 * time.Hour, Post: 2 * time.Hour},
		"generic":            {Pre: 3 * time.Hour, Post: 2 * time.Hour},
	}
	override := func(name string, pre, post time.Duration) {
		spec := dflt[name]
		if pre > 0 {
			spec.Pre = pre
		}
		if post > 0 {
			spec.Post = post
		}
		dflt[name] = spec
	}
	override("debate", rc.DebatePre, rc.DebatePost)
	override("court_ruling", rc.CourtRulingPre, rc.CourtRulingPost)
	override("election_day", rc.ElectionDayPre, rc.ElectionDayPost)
	override("official_statement", rc.OfficialStatementPre, rc.OfficialStatementPost)
	override("generic", rc.GenericPre, rc.GenericPost)
	return dflt
}

func holderSnapshotToDetector(s stagedinputs.HolderSnapshot) holderdelta.Snapshot {
	return holderdelta.Snapshot{
		SnapshotAt:  s.SnapshotAt,
		Wallet:      s.Wallet,
		Rank:        s.Rank,
		Shares:      s.Shares,
		NotionalUSD: s.NotionalUSD,
		PctOI:       s.PctOI,
		TotalOI:     s.TotalOI,
	}
}

func featureBarToDetector(b stagedinputs.BookFeatureBar) bookvacuum.FeatureBar {
	return bookvacuum.FeatureBar{
		BidDepthTopN:     b.BidDepthTopN,
		AskDepthTopN:     b.AskDepthTopN,
		MidPrice:         b.MidPrice,
		Spread:           b.Spread,
		BidDepthDeltaPct: b.BidDepthDeltaPct,
		AskDepthDeltaPct: b.AskDepthDeltaPct,
		MidDelta:         b.MidDelta,
	}
}

// bookvacuumBaseline averages the depth + spread fields across the
// older bars to produce the rolling-baseline FeatureBar. Delta fields
// stay zero — Recent.*DeltaPct already encodes the change.
func bookvacuumBaseline(bars []stagedinputs.BookFeatureBar) bookvacuum.FeatureBar {
	if len(bars) == 0 {
		return bookvacuum.FeatureBar{}
	}
	var bid, ask, mid, spread float64
	n := 0
	for _, b := range bars {
		if !b.BidDepthValid || !b.AskDepthValid {
			continue
		}
		bid += b.BidDepthTopN
		ask += b.AskDepthTopN
		mid += b.MidPrice
		spread += b.Spread
		n++
	}
	if n == 0 {
		return bookvacuum.FeatureBar{}
	}
	fn := float64(n)
	return bookvacuum.FeatureBar{
		BidDepthTopN: bid / fn,
		AskDepthTopN: ask / fn,
		MidPrice:     mid / fn,
		Spread:       spread / fn,
	}
}

// lifecycleBucket buckets a 0..100 lifecycle pct for control-bucket joins.
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

func copyFeatures(in map[string]any) map[string]any {
	out := make(map[string]any, len(in)+2)
	for k, v := range in {
		out[k] = v
	}
	return out
}

func absFloat(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
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
