// Package attribution maps an anomaly.Finding into the coarse bucket
// labels stored on polymarket_alert_strategy_dimensions. The point of
// these buckets is to let Grafana / SQL answer "which-setups-actually-
// win" without recomputing them from raw payloads each time:
//
//   SELECT strategy_family, lifecycle_bucket, COUNT(*),
//          SUM(CASE WHEN a.outcome_status='resolved_correct' THEN 1 ELSE 0 END)
//     FROM polymarket_alert_strategy_dimensions d
//     JOIN polymarket_alerts a ON a.id = d.alert_id
//    GROUP BY strategy_family, lifecycle_bucket;
//
// Bucket design rules:
//   - Cardinality stays bounded. Lifecycle, odds, notional, return,
//     ownership, drift each map to ≤ ~8 distinct strings.
//   - The labels are stable. Adding new alert kinds appends a new
//     strategy_family value rather than reshuffling existing ones.
//   - Edge values pick the higher band (price 0.50 → "10-25%" return
//     band, not "0-10%"; see ReturnBucket comments).
//   - Pure function. The detector or sender hands in a Finding +
//     alert id; we hand back a StrategyDimensions row. No I/O.
//
// The strategy_family axis is THE primary group-by. Every Finding
// Kind maps to exactly one family string:
//
//	KindTradeAnomaly         → "whale_flow"
//	KindAccumulation         → "accumulation"
//	KindOwnership            → "ownership_concentration"
//	KindStableFavorite       → "stable_favorite"
//	KindCategoryWatch        → "category_cluster"
//
// Subfamilies (e.g. cluster vs single, recent vs lifetime accumulation)
// are recoverable from other bucket columns + the alert's
// severity/kind, so they don't need their own family value.
package attribution

import (
	"strings"

	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/anomaly"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/repository"
)

// FromFinding produces the attribution row for a persisted Finding.
// `alertID` is the polymarket_alerts.id this row attaches to.
// `aiVerdict` is optional — pass "" when the AI verdict is not yet
// known at write time (the row is upsert-friendly, so the worker
// can overwrite later).
func FromFinding(alertID int64, f anomaly.Finding, aiVerdict string) repository.StrategyDimensions {
	d := repository.StrategyDimensions{
		AlertID:         alertID,
		StrategyFamily:  strategyFamily(f),
		LifecycleBucket: lifecycleBucket(f.LifecyclePct),
		AIVerdict:       aiVerdict,
	}
	if f.Category != nil {
		d.Category = strings.ToLower(strings.TrimSpace(f.Category.Slug))
	}
	if f.NewWallet != nil {
		d.NewWallet = f.NewWallet.IsNew
	}
	if f.QuietMarket != nil {
		d.QuietMarket = true
	}
	if f.DormantWallet != nil {
		d.DormantWallet = true
	}

	switch f.Kind {
	case anomaly.KindTradeAnomaly:
		if f.Trade != nil {
			d.OddsBucket = oddsBucket(f.Trade.Odds)
			d.NotionalBucket = notionalBucket(f.Trade.NotionalUSD)
			d.ReturnBucket = returnBucketFromPrice(f.Trade.Price)
		}
	case anomaly.KindAccumulation:
		if f.Accumulation != nil {
			d.OddsBucket = oddsBucket(f.Accumulation.AvgOdds)
			d.NotionalBucket = notionalBucket(f.Accumulation.TotalNotionalUSD)
			d.AccumulationWindow = accumulationWindow(f.Accumulation.Window)
		}
	case anomaly.KindOwnership:
		if f.Ownership != nil {
			d.NotionalBucket = notionalBucket(f.Ownership.NotionalUSD)
			d.OwnershipShareBucket = ownershipShareBucket(f.Ownership.SharePct)
		}
	case anomaly.KindStableFavorite:
		if f.StableFavorite != nil {
			d.OddsBucket = oddsBucket(1.0 / nonZero(f.StableFavorite.Probability))
			d.ReturnBucket = returnBucketFromPct(f.StableFavorite.RemainingReturnPct)
			d.VolatilityRegime = volatilityRegimeFromStddev(f.StableFavorite.PriceStddev)
		}
	case anomaly.KindCategoryWatch:
		if f.Cluster != nil {
			d.NotionalBucket = notionalBucket(f.Cluster.TotalUSD)
		}
	}
	return d
}

func strategyFamily(f anomaly.Finding) string {
	switch f.Kind {
	case anomaly.KindTradeAnomaly:
		return "whale_flow"
	case anomaly.KindAccumulation:
		return "accumulation"
	case anomaly.KindOwnership:
		return "ownership_concentration"
	case anomaly.KindStableFavorite:
		return "stable_favorite"
	case anomaly.KindCategoryWatch:
		return "category_cluster"
	}
	return "unknown"
}

// lifecycleBucket coarsens the 0..100 lifecycle percentage into the
// bands Grafana panels group by. Markets below the alerting threshold
// (75%) still fire from earlier strategies (e.g. accumulation), so we
// keep a 0-75 band for completeness.
func lifecycleBucket(pct float64) string {
	switch {
	case pct < 75:
		return "0-75%"
	case pct < 80:
		return "75-80%"
	case pct < 85:
		return "80-85%"
	case pct < 90:
		return "85-90%"
	case pct < 95:
		return "90-95%"
	case pct <= 100:
		return "95-100%"
	}
	return "unknown"
}

// oddsBucket coarsens 1/price into multiplier bands. Inputs ≤ 0 (no
// odds available) return "" so the column stays NULL.
func oddsBucket(odds float64) string {
	switch {
	case odds <= 0:
		return ""
	case odds < 2:
		return "<2x"
	case odds < 3:
		return "2-3x"
	case odds < 5:
		return "3-5x"
	case odds < 10:
		return "5-10x"
	case odds < 25:
		return "10-25x"
	case odds < 100:
		return "25-100x"
	}
	return "100x+"
}

// notionalBucket bands the USD size of an alert. The bands track the
// Info / Warning / Critical tiers ($10k / $25k / $100k) so panel
// breakdowns line up with operator mental model.
func notionalBucket(usd float64) string {
	switch {
	case usd <= 0:
		return ""
	case usd < 1_000:
		return "<1k"
	case usd < 5_000:
		return "1-5k"
	case usd < 10_000:
		return "5-10k"
	case usd < 25_000:
		return "10-25k"
	case usd < 100_000:
		return "25-100k"
	case usd < 500_000:
		return "100-500k"
	}
	return "500k+"
}

// returnBucketFromPrice converts a trade price into the remaining-
// return band: (1-p)/p × 100. Degenerate prices (≤0, ≥1) return "".
func returnBucketFromPrice(price float64) string {
	if price <= 0 || price >= 1 {
		return ""
	}
	return returnBucketFromPct(100 * (1 - price) / price)
}

// returnBucketFromPct coarsens a remaining-return percentage. Negative
// inputs (alerts on SELL-side or already-deep-favorite shorts) collapse
// to "<0%" so the band stays cardinality-bounded.
func returnBucketFromPct(pct float64) string {
	switch {
	case pct < 0:
		return "<0%"
	case pct < 10:
		return "0-10%"
	case pct < 25:
		return "10-25%"
	case pct < 50:
		return "25-50%"
	case pct < 100:
		return "50-100%"
	case pct < 500:
		return "100-500%"
	}
	return "500%+"
}

// accumulationWindow normalises the AccumulationRef.Window string.
// The detector emits "recent" or "lifetime"; anything else (a future
// kind) is recorded verbatim, lower-cased, for forward compat.
func accumulationWindow(w string) string {
	w = strings.ToLower(strings.TrimSpace(w))
	if w == "" {
		return ""
	}
	return w
}

// ownershipShareBucket bands the wallet's share of an outcome's net-
// BUY shares. 0 (no data) collapses to "".
func ownershipShareBucket(pct float64) string {
	switch {
	case pct <= 0:
		return ""
	case pct < 1:
		return "<1%"
	case pct < 5:
		return "1-5%"
	case pct < 10:
		return "5-10%"
	case pct < 25:
		return "10-25%"
	case pct < 50:
		return "25-50%"
	}
	return "50%+"
}

// volatilityRegimeFromStddev maps the price stddev observed by the
// stable-favorite worker into a coarse regime label. Stable-favorite
// fires by definition only in low-stddev regimes, but the bucket is
// useful for confirming "we never alert in high-volatility setups".
func volatilityRegimeFromStddev(stddev float64) string {
	switch {
	case stddev <= 0:
		return ""
	case stddev < 0.01:
		return "very_low"
	case stddev < 0.03:
		return "low"
	case stddev < 0.06:
		return "moderate"
	case stddev < 0.12:
		return "elevated"
	}
	return "high"
}

func nonZero(x float64) float64 {
	if x == 0 {
		return 1e-9
	}
	return x
}
