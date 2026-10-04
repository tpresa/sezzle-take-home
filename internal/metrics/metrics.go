// Package metrics exposes the service's Prometheus metrics using
// client_golang, plus the Go runtime and process collectors.
package metrics

import (
	"database/sql"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics records service metrics in its own registry and serves them in
// Prometheus exposition format. HTTP labels are limited to method, route
// pattern, and status code.
type Metrics struct {
	registry *prometheus.Registry
	handler  http.Handler

	httpRequests *prometheus.CounterVec
	httpDuration *prometheus.HistogramVec
	httpInFlight prometheus.Gauge

	cacheLookups   *prometheus.CounterVec
	cacheEntries   prometheus.Gauge
	cacheEvictions atomic.Uint64
	staleResponses prometheus.Counter

	vendorRequests *prometheus.CounterVec
	vendorDuration *prometheus.HistogramVec
	vendorRetries  *prometheus.CounterVec

	responseLogEnqueued    prometheus.Counter
	responseLogDropped     prometheus.Counter
	responseLogDropReasons *prometheus.CounterVec
	responseLogQueueDepth  prometheus.Gauge
	responseLogWrites      *prometheus.CounterVec
	responseLogDuration    prometheus.Histogram
	responseLogRetries     prometheus.Counter
	retentionDeleted       prometheus.Counter

	// sql.DBStats are cumulative snapshots, read at scrape time.
	dbMu    sync.Mutex
	dbStats sql.DBStats

	guards guardMetrics
}

// dropReasons is the bounded set of response-log drop reasons.
var dropReasons = [...]string{"queue_full", "schema_error", "write_failed", "other"}

// New returns metrics backed by a dedicated registry, so multiple instances
// (for example in tests) never collide on the global default registry.
func New() *Metrics {
	m := &Metrics{registry: prometheus.NewRegistry()}
	m.handler = promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{Registry: m.registry})
	f := promauto.With(m.registry)

	m.httpRequests = f.NewCounterVec(prometheus.CounterOpts{
		Name: "weatherlookup_http_requests_total",
		Help: "Total HTTP requests handled by the service.",
	}, []string{"method", "path", "status"})
	m.httpDuration = f.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "weatherlookup_http_request_duration_seconds",
		Help:    "HTTP request duration in seconds.",
		Buckets: prometheus.DefBuckets,
	}, []string{"method", "path", "status"})
	m.httpInFlight = f.NewGauge(prometheus.GaugeOpts{
		Name: "weatherlookup_http_in_flight_requests",
		Help: "Current number of HTTP requests being handled.",
	})
	f.NewGauge(prometheus.GaugeOpts{
		Name:        "weatherlookup_build_info",
		Help:        "Build information for the Weather Lookup service.",
		ConstLabels: prometheus.Labels{"version": "1.0.0"},
	}).Set(1)

	m.cacheLookups = f.NewCounterVec(prometheus.CounterOpts{
		Name: "weatherlookup_cache_requests_total",
		Help: "Cache lookups by result.",
	}, []string{"result"})
	m.cacheEntries = f.NewGauge(prometheus.GaugeOpts{
		Name: "weatherlookup_cache_entries",
		Help: "Current cache entries.",
	})
	f.NewCounterFunc(prometheus.CounterOpts{
		Name: "weatherlookup_cache_evictions_total",
		Help: "Cache entries evicted by the capacity limit.",
	}, func() float64 { return float64(m.cacheEvictions.Load()) })
	m.staleResponses = f.NewCounter(prometheus.CounterOpts{
		Name: "weatherlookup_stale_responses_total",
		Help: "Responses served from expired cache entries.",
	})

	m.vendorRequests = f.NewCounterVec(prometheus.CounterOpts{
		Name: "weatherlookup_vendor_requests_total",
		Help: "Vendor requests by operation and outcome.",
	}, []string{"operation", "outcome"})
	m.vendorDuration = f.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "weatherlookup_vendor_request_duration_seconds",
		Help:    "Vendor request duration.",
		Buckets: prometheus.DefBuckets,
	}, []string{"operation", "outcome"})
	m.vendorRetries = f.NewCounterVec(prometheus.CounterOpts{
		Name: "weatherlookup_vendor_retries_total",
		Help: "Vendor retry attempts.",
	}, []string{"operation"})

	m.responseLogEnqueued = f.NewCounter(prometheus.CounterOpts{
		Name: "weatherlookup_response_log_enqueued_total",
		Help: "Response records queued for persistence.",
	})
	m.responseLogDropped = f.NewCounter(prometheus.CounterOpts{
		Name: "weatherlookup_response_log_dropped_total",
		Help: "Response records lost to queue overflow or database failure.",
	})
	m.responseLogDropReasons = f.NewCounterVec(prometheus.CounterOpts{
		Name: "weatherlookup_response_log_dropped_by_reason_total",
		Help: "Lost response records by bounded reason.",
	}, []string{"reason"})
	// Zero series let increase() see the first drop after startup.
	for _, reason := range dropReasons {
		m.responseLogDropReasons.WithLabelValues(reason)
	}
	m.responseLogQueueDepth = f.NewGauge(prometheus.GaugeOpts{
		Name: "weatherlookup_response_log_queue_depth",
		Help: "Current response persistence queue depth.",
	})
	m.responseLogWrites = f.NewCounterVec(prometheus.CounterOpts{
		Name: "weatherlookup_response_log_writes_total",
		Help: "Response persistence attempts by outcome.",
	}, []string{"outcome"})
	// Outcomes from persistence.outcome(); zero series let the write-failure
	// alert's increase() see the first failure after startup.
	for _, outcome := range [...]string{"success", "error", "timeout"} {
		m.responseLogWrites.WithLabelValues(outcome)
	}
	m.responseLogDuration = f.NewHistogram(prometheus.HistogramOpts{
		Name:    "weatherlookup_response_log_write_duration_seconds",
		Help:    "Response persistence duration.",
		Buckets: prometheus.DefBuckets,
	})
	m.responseLogRetries = f.NewCounter(prometheus.CounterOpts{
		Name: "weatherlookup_response_log_retries_total",
		Help: "Response persistence retry attempts.",
	})
	m.retentionDeleted = f.NewCounter(prometheus.CounterOpts{
		Name: "weatherlookup_retention_deleted_total",
		Help: "Response records removed by retention cleanup.",
	})

	m.registerDatabasePool(f)
	m.registerGuards(f)

	m.registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	return m
}

func (m *Metrics) CacheLookup(result string) {
	m.cacheLookups.WithLabelValues(result).Inc()
}

func (m *Metrics) CacheStats(entries int, evictions uint64) {
	m.cacheEntries.Set(float64(entries))
	m.cacheEvictions.Store(evictions)
}

func (m *Metrics) VendorRequest(operation, outcome string, duration time.Duration) {
	m.vendorRequests.WithLabelValues(operation, outcome).Inc()
	m.vendorDuration.WithLabelValues(operation, outcome).Observe(duration.Seconds())
}

func (m *Metrics) VendorRetry(operation string) {
	m.vendorRetries.WithLabelValues(operation).Inc()
}

func (m *Metrics) StaleResponse() {
	m.staleResponses.Inc()
}

func (m *Metrics) ResponseLogEnqueued(depth int) {
	m.responseLogEnqueued.Inc()
	m.responseLogQueueDepth.Set(float64(depth))
}

func (m *Metrics) ResponseLogDropped(depth int, reason string) {
	switch reason {
	case "queue_full", "schema_error", "write_failed":
	default:
		reason = "other"
	}
	m.responseLogDropped.Inc()
	m.responseLogDropReasons.WithLabelValues(reason).Inc()
	m.responseLogQueueDepth.Set(float64(depth))
}

func (m *Metrics) ResponseLogQueueDepth(depth int) {
	m.responseLogQueueDepth.Set(float64(depth))
}

func (m *Metrics) ResponseLogWrite(outcome string, duration time.Duration) {
	m.responseLogWrites.WithLabelValues(outcome).Inc()
	m.responseLogDuration.Observe(duration.Seconds())
}

func (m *Metrics) ResponseLogRetry() {
	m.responseLogRetries.Inc()
}

func (m *Metrics) DatabasePool(stats sql.DBStats) {
	m.dbMu.Lock()
	m.dbStats = stats
	m.dbMu.Unlock()
}

func (m *Metrics) RetentionDeleted(count int) {
	if count <= 0 {
		return
	}
	m.retentionDeleted.Add(float64(count))
}

func (m *Metrics) registerDatabasePool(f promauto.Factory) {
	read := func(value func(sql.DBStats) float64) func() float64 {
		return func() float64 {
			m.dbMu.Lock()
			defer m.dbMu.Unlock()
			return value(m.dbStats)
		}
	}
	f.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "weatherlookup_database_open_connections",
		Help: "Current open database connections.",
	}, read(func(s sql.DBStats) float64 { return float64(s.OpenConnections) }))
	f.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "weatherlookup_database_in_use_connections",
		Help: "Current in-use database connections.",
	}, read(func(s sql.DBStats) float64 { return float64(s.InUse) }))
	f.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "weatherlookup_database_idle_connections",
		Help: "Current idle database connections.",
	}, read(func(s sql.DBStats) float64 { return float64(s.Idle) }))
	f.NewCounterFunc(prometheus.CounterOpts{
		Name: "weatherlookup_database_wait_count_total",
		Help: "Database connection wait count.",
	}, read(func(s sql.DBStats) float64 { return float64(s.WaitCount) }))
	f.NewCounterFunc(prometheus.CounterOpts{
		Name: "weatherlookup_database_wait_duration_seconds_total",
		Help: "Database connection wait duration.",
	}, read(func(s sql.DBStats) float64 { return s.WaitDuration.Seconds() }))
}

// Middleware records all routes except /metrics itself, preventing scrapes
// from changing the application traffic metrics they are inspecting.
func (m *Metrics) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/metrics" {
			next.ServeHTTP(response, request)
			return
		}

		started := time.Now()
		m.httpInFlight.Inc()
		defer m.httpInFlight.Dec()

		recorder := &responseRecorder{ResponseWriter: response, status: http.StatusOK}
		next.ServeHTTP(recorder, request)
		// ServeMux sets Pattern while dispatching. Read it only after the mux
		// runs; never fall back to URL.Path when no route matched (404/405).
		path := routeLabel(request.Pattern)
		// Go's CONNECT trailing-slash redirects can set Pattern to a concrete
		// request-derived redirect path, rather than a registered route pattern.
		if request.Method == http.MethodConnect && recorder.status == http.StatusMovedPermanently {
			path = "unmatched"
		}

		labels := []string{methodLabel(request.Method), path, strconv.Itoa(recorder.status)}
		m.httpRequests.WithLabelValues(labels...).Inc()
		m.httpDuration.WithLabelValues(labels...).Observe(time.Since(started).Seconds())
	})
}

// Preserve the existing path label and static-route dashboard queries. Strip
// the optional method/host from the registered pattern, retaining wildcards
// such as /weather/{location} instead of the client's concrete path.
func routeLabel(pattern string) string {
	if index := strings.IndexByte(pattern, '/'); index >= 0 {
		return pattern[index:]
	}
	return "unmatched"
}

func methodLabel(method string) string {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
		http.MethodPatch, http.MethodDelete, http.MethodOptions, http.MethodConnect, http.MethodTrace:
		return method
	default:
		return "OTHER"
	}
}

func (m *Metrics) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	m.handler.ServeHTTP(response, request)
}

type responseRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (r *responseRecorder) WriteHeader(status int) {
	if r.wroteHeader {
		return
	}
	r.status = status
	r.wroteHeader = true
	r.ResponseWriter.WriteHeader(status)
}

func (r *responseRecorder) Write(body []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	return r.ResponseWriter.Write(body)
}
