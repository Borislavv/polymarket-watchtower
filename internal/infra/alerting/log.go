package alerting

import (
	"context"

	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/anomaly"
	"github.com/rs/zerolog"
)

// LogSink writes findings to the zerolog logger. Severity routes to the
// appropriate level so existing log-based alerts and dashboards keep working.
type LogSink struct {
	Logger *zerolog.Logger
}

func (s *LogSink) Name() string { return "log" }

func (s *LogSink) Notify(_ context.Context, f anomaly.Finding) error {
	evt := s.Logger.Warn()
	switch f.Severity {
	case anomaly.SeverityHard:
		evt = s.Logger.Error()
	case anomaly.SeverityCritical:
		evt = s.Logger.Error()
	case anomaly.SeverityWarning:
		evt = s.Logger.Warn()
	case anomaly.SeverityInfo:
		evt = s.Logger.Info()
	}

	evt = evt.
		Str("kind", string(f.Kind)).
		Str("severity", string(f.Severity)).
		Str("reason", f.Reason).
		Time("at", f.At)

	if f.Trade != nil {
		evt = evt.
			Str("market", string(f.Trade.Market)).
			Str("slug", f.Trade.Slug).
			Str("question", f.Trade.Question).
			Str("outcome", f.Trade.Outcome).
			Str("side", string(f.Trade.Side)).
			Str("wallet", f.Trade.Wallet).
			Float64("size_shares", f.Trade.SizeShares).
			Float64("price", f.Trade.Price).
			Float64("notional_usd", f.Trade.NotionalUSD)
	}
	if f.Baseline != nil {
		evt = evt.
			Str("baseline_scope", f.Baseline.Scope).
			Float64("baseline_median_usd", f.Baseline.MedianUSD).
			Float64("baseline_mean_usd", f.Baseline.MeanUSD).
			Float64("baseline_p95_usd", f.Baseline.P95USD).
			Int("baseline_n", f.Baseline.SampleN).
			Dur("baseline_span", f.Baseline.Span).
			Dur("baseline_window_max", f.Baseline.WindowMax)
	}
	if f.Category != nil {
		evt = evt.
			Int64("category_id", int64(f.Category.ID)).
			Str("category_label", f.Category.Label).
			Str("category_slug", f.Category.Slug)
	}
	if f.Cluster != nil {
		evt = evt.
			Dur("cluster_window", f.Cluster.Window).
			Int("cluster_trades", f.Cluster.AnomalousTrades).
			Int("cluster_wallets", f.Cluster.UniqueWallets).
			Float64("cluster_total_usd", f.Cluster.TotalUSD)
	}
	if f.Multiplier > 0 {
		evt = evt.Float64("multiplier", f.Multiplier)
	}
	if f.AbsoluteTier != "" {
		evt = evt.Str("absolute_tier", string(f.AbsoluteTier))
	}
	if f.MultiplierTier != "" {
		evt = evt.Str("multiplier_tier", string(f.MultiplierTier))
	}
	if f.Trade != nil && f.Trade.Odds > 0 {
		evt = evt.Float64("odds", f.Trade.Odds)
	}
	if f.MarketURL != "" {
		evt = evt.Str("market_url", f.MarketURL)
	}
	if f.CategoryURL != "" {
		evt = evt.Str("category_url", f.CategoryURL)
	}
	if f.TraderURL != "" {
		evt = evt.Str("trader_url", f.TraderURL)
	}
	if f.GrafanaURL != "" {
		evt = evt.Str("grafana_url", f.GrafanaURL)
	}
	if f.LifecyclePct > 0 {
		evt = evt.Float64("lifecycle_pct", f.LifecyclePct)
	}
	if f.Hot {
		evt = evt.Bool("hot", true)
	}
	if f.Kind == anomaly.KindTradeAnomaly {
		evt = evt.Bool("in_cluster", f.InCluster).Int("cluster_peer_count", f.ClusterPeerCount)
	}
	evt.Msg("anomaly detected")
	return nil
}
