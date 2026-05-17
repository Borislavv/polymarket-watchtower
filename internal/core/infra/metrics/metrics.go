// Package metrics owns the Prometheus registry and the collectors used by the
// pipeline. Keeping the registry private prevents accidental use of the default
// global registry from random packages.
package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Registry is the type the /metrics HTTP handler consumes.
type Registry = *prometheus.Registry

// Metrics is the fixed set of collectors emitted by the app.
type Metrics struct {
	registry Registry

	UpstreamRequests *prometheus.CounterVec   // api, endpoint, status
	UpstreamLatency  *prometheus.HistogramVec // api, endpoint
	MarketsTracked   prometheus.Gauge

	TradesIngested   *prometheus.CounterVec // market
	NotionalIngested *prometheus.CounterVec // market

	WindowTradeRate    *prometheus.GaugeVec // market, window
	WindowNotionalRate *prometheus.GaugeVec // market, window
	WindowAvgSize      *prometheus.GaugeVec // market, window

	Anomalies         *prometheus.CounterVec // scope, metric, severity
	AnomalyMultiplier *prometheus.GaugeVec   // scope, metric — last observed
}

func New() *Metrics {
	reg := prometheus.NewRegistry()
	m := &Metrics{registry: reg}

	m.UpstreamRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "upstream", Name: "requests_total",
		Help: "HTTP requests issued to Polymarket upstreams, by api/endpoint/status.",
	}, []string{"api", "endpoint", "status"})

	m.UpstreamLatency = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "watchtower", Subsystem: "upstream", Name: "request_duration_seconds",
		Help:    "Latency of Polymarket upstream HTTP calls.",
		Buckets: prometheus.ExponentialBuckets(0.01, 2, 10),
	}, []string{"api", "endpoint"})

	m.MarketsTracked = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "watchtower", Subsystem: "discover", Name: "markets_tracked",
		Help: "Number of markets currently in the active set.",
	})

	m.TradesIngested = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "collect", Name: "trades_total",
		Help: "Trades ingested per market.",
	}, []string{"market"})

	m.NotionalIngested = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "collect", Name: "notional_usd_total",
		Help: "Notional USD ingested per market.",
	}, []string{"market"})

	m.WindowTradeRate = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "watchtower", Subsystem: "window", Name: "trade_rate_per_min",
		Help: "Rolling trade rate (trades/minute) by market and window label.",
	}, []string{"market", "window"})

	m.WindowNotionalRate = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "watchtower", Subsystem: "window", Name: "notional_rate_usd_per_min",
		Help: "Rolling notional rate (USD/minute) by market and window label.",
	}, []string{"market", "window"})

	m.WindowAvgSize = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "watchtower", Subsystem: "window", Name: "avg_size",
		Help: "Rolling average trade size by market and window label.",
	}, []string{"market", "window"})

	m.Anomalies = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "anomaly", Name: "findings_total",
		Help: "Anomaly findings emitted, by scope/metric/severity.",
	}, []string{"scope", "metric", "severity"})

	m.AnomalyMultiplier = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "watchtower", Subsystem: "anomaly", Name: "last_multiplier",
		Help: "Last observed recent/baseline ratio by scope/metric.",
	}, []string{"scope", "metric"})

	reg.MustRegister(
		m.UpstreamRequests,
		m.UpstreamLatency,
		m.MarketsTracked,
		m.TradesIngested,
		m.NotionalIngested,
		m.WindowTradeRate,
		m.WindowNotionalRate,
		m.WindowAvgSize,
		m.Anomalies,
		m.AnomalyMultiplier,
	)
	return m
}

// Registry exposes the underlying registry for the HTTP handler.
func (m *Metrics) Registry() Registry { return m.registry }

// UpstreamObserver returns a callback that increments UpstreamRequests +
// observes UpstreamLatency for the given API label. Designed to plug into
// httpx.Config.Observe.
func (m *Metrics) UpstreamObserver(api string) func(endpoint string, status int, dur time.Duration) {
	return func(endpoint string, status int, dur time.Duration) {
		m.UpstreamRequests.WithLabelValues(api, endpoint, statusLabel(status)).Inc()
		m.UpstreamLatency.WithLabelValues(api, endpoint).Observe(dur.Seconds())
	}
}

func statusLabel(status int) string {
	switch {
	case status == 0:
		return "net_err"
	case status >= 200 && status < 300:
		return "2xx"
	case status >= 300 && status < 400:
		return "3xx"
	case status == 429:
		return "429"
	case status >= 400 && status < 500:
		return "4xx"
	case status >= 500:
		return "5xx"
	default:
		return "other"
	}
}
