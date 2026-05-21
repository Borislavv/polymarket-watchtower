// eventpage_metrics.go — adapter from the central metrics struct to
// the small eventpage.MetricsSink interface. The adapter lives in
// internal/app so the infra/polymarket/eventpage package stays out
// of the infra/metrics import graph.
package app

import (
	"github.com/Borislavv/polymarket-watchtower/internal/infra/metrics"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/polymarket/eventpage"
)

type eventPageMetricsSink struct {
	m *metrics.Metrics
}

// newEventPageMetricsSink returns a sink that forwards into the
// supplied *metrics.Metrics. nil input is safe — every method is
// guarded.
func newEventPageMetricsSink(m *metrics.Metrics) eventpage.MetricsSink {
	if m == nil {
		return nil
	}
	return &eventPageMetricsSink{m: m}
}

func (s *eventPageMetricsSink) ObserveRedirect(status string) {
	if s == nil || s.m == nil || s.m.EventPageRedirects == nil {
		return
	}
	s.m.EventPageRedirects.WithLabelValues(status).Inc()
}

func (s *eventPageMetricsSink) ObserveRedirectFailure(reason string) {
	if s == nil || s.m == nil || s.m.EventPageRedirectFailures == nil {
		return
	}
	s.m.EventPageRedirectFailures.WithLabelValues(reason).Inc()
}

func (s *eventPageMetricsSink) ObserveBuildIDRefresh(reason string) {
	if s == nil || s.m == nil || s.m.EventPageBuildIDRefresh == nil {
		return
	}
	s.m.EventPageBuildIDRefresh.WithLabelValues(reason).Inc()
}

func (s *eventPageMetricsSink) ObserveSlugAlias() {
	if s == nil || s.m == nil || s.m.EventPageSlugAlias == nil {
		return
	}
	s.m.EventPageSlugAlias.Inc()
}

func (s *eventPageMetricsSink) ObserveContextStale(reason string) {
	if s == nil || s.m == nil || s.m.EventPageContextStale == nil {
		return
	}
	s.m.EventPageContextStale.WithLabelValues(reason).Inc()
}
