package metrics

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// httpRequestSeries gathers weatherlookup_http_requests_total, keyed by
// "METHOD path status".
func httpRequestSeries(t *testing.T, m *Metrics) map[string]float64 {
	t.Helper()
	families, err := m.registry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	series := map[string]float64{}
	for _, family := range families {
		if family.GetName() != "weatherlookup_http_requests_total" {
			continue
		}
		for _, metric := range family.GetMetric() {
			labels := map[string]string{}
			for _, label := range metric.GetLabel() {
				labels[label.GetName()] = label.GetValue()
			}
			series[labels["method"]+" "+labels["path"]+" "+labels["status"]] = metric.GetCounter().GetValue()
		}
	}
	return series
}

func TestMetricsMiddlewareAndHandler(t *testing.T) {
	metricSet := New()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /weather", func(response http.ResponseWriter, _ *http.Request) {
		response.WriteHeader(http.StatusNoContent)
	})
	mux.Handle("GET /metrics", metricSet)
	handler := metricSet.Middleware(mux)

	request := httptest.NewRequest(http.MethodGet, "/weather", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", response.Code)
	}

	metricsRequest := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	metricsResponse := httptest.NewRecorder()
	handler.ServeHTTP(metricsResponse, metricsRequest)
	body := metricsResponse.Body.String()
	if !strings.Contains(body, `weatherlookup_http_requests_total{method="GET",path="/weather",status="204"} 1`) {
		t.Fatalf("metrics missing request counter:\n%s", body)
	}
	if !strings.Contains(body, `weatherlookup_http_request_duration_seconds_bucket{method="GET",path="/weather",status="204",le="+Inf"} 1`) {
		t.Fatalf("metrics missing histogram:\n%s", body)
	}
	if strings.Contains(body, `path="/metrics"`) {
		t.Fatalf("metrics scrape should not be recorded:\n%s", body)
	}
}

func TestHTTPLabelsHaveBoundedCardinality(t *testing.T) {
	m := New()
	mux := http.NewServeMux()
	for _, path := range []string{"/weather", "/v1/weather", "/healthz"} {
		mux.HandleFunc("GET "+path, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	}
	handler := m.Middleware(mux)
	for i := 0; i < 5000; i++ {
		for _, method := range []string{"GET", fmt.Sprintf("CUSTOM%d", i)} {
			handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(method, fmt.Sprintf("/missing/%d", i), nil))
		}
	}
	series := httpRequestSeries(t, m)
	if len(series) != 2 {
		t.Fatalf("unmatched paths/methods created %d series, want 2: %v", len(series), series)
	}
	for _, method := range []string{"GET", "OTHER"} {
		if got := series[method+" unmatched 404"]; got != 5000 {
			t.Fatalf("unmatched series for %s = %v, want 5000", method, got)
		}
	}
	for _, path := range []string{"/weather", "/v1/weather", "/healthz"} {
		for _, method := range []string{"GET", "HEAD", "POST"} {
			handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(method, path, nil))
			status := 200
			label := path
			if method == "POST" {
				status = 405
				label = "unmatched" // ServeMux does not match a pattern on 405.
			}
			if _, ok := httpRequestSeries(t, m)[method+" "+label+" "+strconv.Itoa(status)]; !ok {
				t.Fatalf("missing known route/method/status: %s %s %d", method, path, status)
			}
		}
	}
	if series := httpRequestSeries(t, m); len(series) != 9 {
		t.Fatalf("series = %d, want 9: %v", len(series), series)
	}
	scrape := httptest.NewRecorder()
	m.ServeHTTP(scrape, httptest.NewRequest("GET", "/metrics", nil))
	if strings.Contains(scrape.Body.String(), "/missing/") || strings.Contains(scrape.Body.String(), "CUSTOM") {
		t.Fatal("raw client labels leaked into metrics")
	}
}

func TestMatchedPatternsPreserveRoutesWithoutClientPathValues(t *testing.T) {
	m := New()
	mux := http.NewServeMux()
	for pattern, status := range map[string]int{
		"GET /weather/{location}":    200,
		"GET /objects/{rest...}":     404, // Application 404 still has a matched route.
		"GET /assets/":               204,
		"GET example.com/hosts/{id}": 200,
	} {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) })
	}
	handler := m.Middleware(mux)
	for i := 0; i < 1000; i++ {
		for _, path := range []string{
			fmt.Sprintf("/weather/city-%d?units=%d", i, i),
			fmt.Sprintf("/objects/dir-%d/file-%d", i, i),
			fmt.Sprintf("/assets/image-%d.png", i),
			fmt.Sprintf("/hosts/host-%d", i),
		} {
			handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "http://example.com"+path, nil))
		}
	}
	series := httpRequestSeries(t, m)
	if len(series) != 4 {
		t.Fatalf("route series = %d, want 4: %v", len(series), series)
	}
	for path, status := range map[string]int{"/weather/{location}": 200, "/objects/{rest...}": 404, "/assets/": 204, "/hosts/{id}": 200} {
		if got := series["GET "+path+" "+strconv.Itoa(status)]; got != 1000 {
			t.Fatalf("matched pattern series for %s = %v, want 1000", path, got)
		}
	}
}

func TestMuxRedirectsHaveBoundedLabels(t *testing.T) {
	m := New()
	mux := http.NewServeMux()
	mux.HandleFunc("/folders/{id}/", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	handler := m.Middleware(mux)
	for i := 0; i < 1000; i++ {
		for _, method := range []string{"GET", "CONNECT"} {
			w := httptest.NewRecorder()
			r := httptest.NewRequest(method, fmt.Sprintf("/folders/folder-%d", i), nil)
			handler.ServeHTTP(w, r)
			if w.Code != 301 {
				t.Fatalf("%s: status = %d, want 301", method, w.Code)
			}
		}
	}
	series := httpRequestSeries(t, m)
	if len(series) != 2 {
		t.Fatalf("redirects created %d series, want 2: %v", len(series), series)
	}
	for method, path := range map[string]string{"GET": "/folders/{id}/", "CONNECT": "unmatched"} {
		if got := series[method+" "+path+" 301"]; got != 1000 {
			t.Fatalf("redirect series for %s = %v, want 1000", method, got)
		}
	}
}

func TestDomainMetricsAreExposed(t *testing.T) {
	metricSet := New()
	metricSet.CacheLookup("hit")
	metricSet.CacheStats(3, 2)
	metricSet.VendorRequest("forecast.current", "success", 20*time.Millisecond)
	metricSet.VendorRetry("forecast.current")
	metricSet.StaleResponse()
	metricSet.ResponseLogEnqueued(4)
	metricSet.ResponseLogWrite("success", 10*time.Millisecond)

	response := httptest.NewRecorder()
	metricSet.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := response.Body.String()
	for _, want := range []string{
		`weatherlookup_cache_requests_total{result="hit"} 1`,
		`weatherlookup_cache_entries 3`,
		`weatherlookup_cache_evictions_total 2`,
		`weatherlookup_vendor_requests_total{operation="forecast.current",outcome="success"} 1`,
		`weatherlookup_vendor_retries_total{operation="forecast.current"} 1`,
		`weatherlookup_stale_responses_total 1`,
		`weatherlookup_response_log_enqueued_total 1`,
		`weatherlookup_response_log_writes_total{outcome="success"} 1`,
		`weatherlookup_response_log_write_duration_seconds_bucket{le="+Inf"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics missing %q:\n%s", want, body)
		}
	}
}

func TestRuntimeAndProcessMetricsAreExposed(t *testing.T) {
	response := httptest.NewRecorder()
	New().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := response.Body.String()
	want := []string{"go_goroutines ", "go_memstats_heap_alloc_bytes ", `weatherlookup_build_info{version="1.0.0"} 1`}
	if runtime.GOOS == "linux" {
		want = append(want, "process_resident_memory_bytes ", "process_open_fds ")
	}
	for _, sample := range want {
		if !strings.Contains(body, sample) {
			t.Fatalf("metrics missing %q", sample)
		}
	}
}

func TestMetricsPassPromlint(t *testing.T) {
	m := New()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /weather", func(w http.ResponseWriter, _ *http.Request) {})
	m.Middleware(mux).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/weather", nil))
	m.CacheLookup("hit")
	m.VendorRequest("forecast.current", "success", time.Millisecond)
	m.VendorRetry("forecast.current")
	m.ResponseLogWrite("success", time.Millisecond)

	problems, err := testutil.GatherAndLint(m.registry)
	if err != nil {
		t.Fatalf("lint: %v", err)
	}
	for _, problem := range problems {
		t.Errorf("%s: %s", problem.Metric, problem.Text)
	}
}
