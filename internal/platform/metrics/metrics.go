// Package metrics defines the Prometheus metrics the service exports.
//
// A dedicated registry (instead of the global default) keeps tests isolated
// and makes every exported series explicit.
package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type Metrics struct {
	Registry *prometheus.Registry

	HTTPRequests       *prometheus.CounterVec
	HTTPDuration       *prometheus.HistogramVec
	LedgerPostings     *prometheus.CounterVec
	LedgerPostedMinor  *prometheus.CounterVec
	DependencyFailOpen *prometheus.CounterVec
	WebhookEvents      *prometheus.CounterVec
	IdempotentReplays  *prometheus.CounterVec
}

func New() *Metrics {
	reg := prometheus.NewRegistry()
	m := &Metrics{
		Registry: reg,
		HTTPRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "irate_http_requests_total",
			Help: "HTTP requests by method, route pattern and status code.",
		}, []string{"method", "route", "status"}),
		HTTPDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "irate_http_request_duration_seconds",
			Help:    "HTTP request latency by method and route pattern.",
			Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10},
		}, []string{"method", "route"}),
		LedgerPostings: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "irate_ledger_postings_total",
			Help: "Committed ledger transactions by kind.",
		}, []string{"kind", "currency"}),
		LedgerPostedMinor: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "irate_ledger_posted_minor_units_total",
			Help: "Money moved through the ledger, in minor units, by kind.",
		}, []string{"kind", "currency"}),
		DependencyFailOpen: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "irate_dependency_fail_open_total",
			Help: "Times a non-critical check was skipped because a dependency was unavailable.",
		}, []string{"check"}),
		WebhookEvents: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "irate_webhook_events_total",
			Help: "Inbound payment webhooks by provider and outcome.",
		}, []string{"provider", "outcome"}),
		IdempotentReplays: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "irate_idempotent_replays_total",
			Help: "Requests answered from a stored idempotent response, by scope.",
		}, []string{"scope"}),
	}
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		m.HTTPRequests, m.HTTPDuration, m.LedgerPostings, m.LedgerPostedMinor,
		m.DependencyFailOpen, m.WebhookEvents, m.IdempotentReplays,
	)
	return m
}

// Handler serves /metrics. Mount it on the internal listener only — metrics
// reveal traffic patterns and should not be reachable from the internet.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{Registry: m.Registry})
}
