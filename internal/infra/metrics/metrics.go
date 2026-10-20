// Package metrics owns the Prometheus instrumentation of the process: a
// dedicated registry, the HTTP request counter and latency histogram, and the
// refresh loop that keeps tasks_count in sync with PostgreSQL.
//
// It deliberately does not import Gin: the HTTP layer calls ObserveRequest and
// mounts Handler, so transport concerns stay in internal/infra/server. The
// registry is private to this package, which keeps tests isolated and makes a
// duplicate registration impossible in the process.
package metrics

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const (
	// Path is the public path of the Prometheus exposition endpoint.
	Path = "/metrics"

	// RequestsTotalName counts handled HTTP requests.
	RequestsTotalName = "requests_total"
	// RequestLatencyHistogramName records HTTP request duration in seconds.
	RequestLatencyHistogramName = "request_latency_histogram"
	// TasksCountName reports the number of persisted tasks.
	TasksCountName = "tasks_count"
	// TasksCountRefreshTimestampName reports when tasks_count was last
	// refreshed successfully, in Unix seconds.
	TasksCountRefreshTimestampName = "tasks_count_last_refresh_timestamp_seconds"

	// UnmatchedRoute is the route label used for requests that matched no
	// registered route. It keeps unknown URLs and 405 responses in one bounded
	// series instead of creating a series per requested path.
	UnmatchedRoute = "unmatched"
	// OtherMethod is the method label used for non-standard HTTP methods, which
	// keeps a client-chosen verb from creating a new series.
	OtherMethod = "other"
)

// latencyBuckets covers the range of a small REST API: sub-millisecond handler
// work up to a slow database round trip. Units are seconds.
var latencyBuckets = []float64{
	0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10,
}

// Metrics holds the collectors and the registry they are registered in.
type Metrics struct {
	registry *prometheus.Registry

	requestsTotal *prometheus.CounterVec
	latency       *prometheus.HistogramVec
	tasksCount    *taskCountCollector
	handler       http.Handler
}

// New builds a Metrics with its own registry. Nothing is registered in the
// global default registry, so constructing several instances (for example one
// per test) cannot panic with a duplicate-registration error.
func New() *Metrics {
	registry := prometheus.NewRegistry()

	requestsTotal := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: RequestsTotalName,
		Help: "Total number of HTTP requests handled, by method, route, and status code.",
	}, []string{"method", "route", "status"})

	latency := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    RequestLatencyHistogramName,
		Help:    "HTTP request duration in seconds, by method, route, and status code.",
		Buckets: latencyBuckets,
	}, []string{"method", "route", "status"})

	tasksCount := newTaskCountCollector()

	registry.MustRegister(requestsTotal, latency, tasksCount)

	return &Metrics{
		registry:      registry,
		requestsTotal: requestsTotal,
		latency:       latency,
		tasksCount:    tasksCount,
		// No HandlerOpts.Registry: promhttp's own handler metrics are not
		// registered, so a scrape does not create instrumentation of itself.
		handler: promhttp.HandlerFor(registry, promhttp.HandlerOpts{}),
	}
}

// Handler serves the standard Prometheus text exposition format for this
// registry. The caller is responsible for not routing scrapes through
// ObserveRequest.
func (m *Metrics) Handler() http.Handler {
	return m.handler
}

// ObserveRequest records one completed HTTP request. route must be the
// registered route pattern (for example /tasks/:id), not the concrete path;
// an empty route is recorded as UnmatchedRoute. duration is measured with a
// monotonic clock by the caller and is recorded in seconds.
func (m *Metrics) ObserveRequest(method, route string, status int, duration time.Duration) {
	if route == "" {
		route = UnmatchedRoute
	}
	// The exposition endpoint is not counted as application traffic. The HTTP
	// layer also skips it; this guard keeps the property if a caller forgets.
	if route == Path {
		return
	}

	labels := []string{normalizeMethod(method), route, strconv.Itoa(status)}
	m.requestsTotal.WithLabelValues(labels...).Inc()
	m.latency.WithLabelValues(labels...).Observe(duration.Seconds())
}

// normalizeMethod maps the request verb into a bounded set. Registered routes
// only accept standard verbs, so anything else is a client error and is
// aggregated under OtherMethod.
func normalizeMethod(method string) string {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
		http.MethodPatch, http.MethodDelete, http.MethodConnect,
		http.MethodOptions, http.MethodTrace:
		return method
	default:
		return OtherMethod
	}
}
