// Package metrics implements the proxy's Prometheus instrumentation. It
// is the only package that depends on a metrics library; the proxy talks
// to it through a small interface, so metrics stay fully optional.
package metrics

import (
	"net/http"
	"strconv"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics holds the collectors and the /metrics HTTP handler. It
// satisfies the proxy's Recorder interface.
type Metrics struct {
	requests        *prometheus.CounterVec
	duration        *prometheus.HistogramVec
	responseBytes   prometheus.Counter
	inFlight        prometheus.Gauge
	upstreamLatency prometheus.Histogram
	upstreamErrors  *prometheus.CounterVec
	coalesced       prometheus.Counter
	handler         http.Handler
}

// New registers the collectors on a private registry (also including Go
// runtime and process metrics) and returns a ready Metrics.
func New() *Metrics {
	m := &Metrics{
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "egresszero_requests_total",
			Help: "Total requests served, by method and final status.",
		}, []string{"method", "status"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "egresszero_request_duration_seconds",
			Help:    "Request duration in seconds, by method.",
			Buckets: prometheus.ExponentialBuckets(0.001, 4, 10), // 1ms .. ~260s
		}, []string{"method"}),
		responseBytes: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "egresszero_response_bytes_total",
			Help: "Total object body bytes streamed to clients.",
		}),
		inFlight: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "egresszero_in_flight_requests",
			Help: "Requests currently being served.",
		}),
		upstreamLatency: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "egresszero_upstream_request_duration_seconds",
			Help:    "Seconds until S3 returned response headers (time to first byte).",
			Buckets: prometheus.ExponentialBuckets(0.005, 2, 12), // 5ms .. ~10s
		}),
		upstreamErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "egresszero_upstream_errors_total",
			Help: "Non-success outcomes by classified HTTP status (4xx/5xx and 499).",
		}, []string{"status"}),
		coalesced: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "egresszero_coalesced_requests_total",
			Help: "Requests served from a shared upstream call instead of their own.",
		}),
	}

	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		m.requests, m.duration, m.responseBytes,
		m.inFlight, m.upstreamLatency, m.upstreamErrors, m.coalesced,
	)
	m.handler = promhttp.HandlerFor(reg, promhttp.HandlerOpts{})
	return m
}

// Handler serves the Prometheus exposition format at /metrics.
func (m *Metrics) Handler() http.Handler { return m.handler }

func (m *Metrics) IncInFlight() { m.inFlight.Inc() }
func (m *Metrics) DecInFlight() { m.inFlight.Dec() }

func (m *Metrics) ObserveUpstreamLatency(seconds float64) {
	m.upstreamLatency.Observe(seconds)
}

func (m *Metrics) IncUpstreamError(status int) {
	m.upstreamErrors.WithLabelValues(strconv.Itoa(status)).Inc()
}

func (m *Metrics) ObserveRequest(method string, status int, bytes int64, seconds float64) {
	m.requests.WithLabelValues(method, strconv.Itoa(status)).Inc()
	m.duration.WithLabelValues(method).Observe(seconds)
	if bytes > 0 {
		m.responseBytes.Add(float64(bytes))
	}
}

func (m *Metrics) IncCoalesced() { m.coalesced.Inc() }
