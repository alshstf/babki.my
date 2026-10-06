// Package metrics keeps the program's numbers for Prometheus (decision
// Р-22): how requests and background jobs go. They are always counted — it
// costs next to nothing — and published only when BABKI_METRICS is on, at
// /metrics. Nothing here carries an amount, a paper or a person.
package metrics

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Registry is the process's one registry; tests read it.
var Registry = prometheus.NewRegistry()

var (
	httpRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "babki_http_requests_total",
		Help: "HTTP requests by method, matched route and status code.",
	}, []string{"method", "route", "status"})
	httpDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "babki_http_request_duration_seconds",
		Help:    "How long HTTP requests took, by method and matched route.",
		Buckets: prometheus.DefBuckets,
	}, []string{"method", "route"})
	jobDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "babki_job_duration_seconds",
		Help: "How long background job attempts took, by job kind and outcome (ok or failed).",
		// A quote refresh takes a second; a broker's full history, minutes.
		Buckets: []float64{0.1, 0.5, 1, 5, 15, 30, 60, 120, 300, 600, 900},
	}, []string{"kind", "outcome"})
)

func init() {
	Registry.MustRegister(httpRequests, httpDuration, jobDuration,
		collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
}

// ObserveRequest counts one answered request. route is the pattern the
// request matched ("GET /api/v1/accounts/{accountId}"), never its path, so ids
// do not make a series each.
func ObserveRequest(method, route string, status int, took time.Duration) {
	if route == "" {
		route = "unmatched"
	}
	httpRequests.WithLabelValues(method, route, strconv.Itoa(status)).Inc()
	httpDuration.WithLabelValues(method, route).Observe(took.Seconds())
}

// ObserveJob counts one background job attempt.
func ObserveJob(kind string, took time.Duration, failed bool) {
	outcome := "ok"
	if failed {
		outcome = "failed"
	}
	jobDuration.WithLabelValues(kind, outcome).Observe(took.Seconds())
}

// Handler serves the registry in Prometheus's text format.
func Handler() http.Handler {
	return promhttp.HandlerFor(Registry, promhttp.HandlerOpts{})
}
