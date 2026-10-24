package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel/trace"

	"weatherlookup/internal/admission"
	"weatherlookup/internal/metrics"
	"weatherlookup/internal/openmeteo"
	"weatherlookup/internal/response"
	"weatherlookup/internal/validation"
	"weatherlookup/internal/weather"
)

type Handler struct {
	service        *weather.Service
	requestTimeout time.Duration
	logger         *slog.Logger
	requests       *admission.Limiter
	metrics        *metrics.Metrics
}

type Options struct {
	Metrics      *metrics.Metrics
	ResponseSink response.Sink
	MaxInFlight  int
}

const maxLoggedResponseBytes = 64 * 1024

func NewHandler(service *weather.Service, requestTimeout time.Duration, logger *slog.Logger, metricSets ...*metrics.Metrics) http.Handler {
	var metricSet *metrics.Metrics
	if len(metricSets) > 0 {
		metricSet = metricSets[0]
	}
	return NewHandlerWithOptions(service, requestTimeout, logger, Options{Metrics: metricSet})
}

func NewHandlerWithOptions(service *weather.Service, requestTimeout time.Duration, logger *slog.Logger, options Options) http.Handler {
	if requestTimeout <= 0 {
		requestTimeout = 5 * time.Second
	}
	if logger == nil {
		logger = slog.Default()
	}

	handler := &Handler{service: service, requestTimeout: requestTimeout, logger: logger}
	metricSet := options.Metrics
	if metricSet == nil {
		metricSet = metrics.New()
	}
	if options.MaxInFlight <= 0 {
		options.MaxInFlight = 64
	}
	handler.metrics = metricSet
	handler.requests = admission.New(options.MaxInFlight, metricSet.LookupAdmission)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", handler.health)
	mux.HandleFunc("GET /weather", handler.weather)
	mux.HandleFunc("GET /v1/weather", handler.weather)
	mux.Handle("GET /metrics", metricSet)
	return requestLogger(metricSet.Middleware(mux), logger, options.ResponseSink)
}

func (h *Handler) health(response http.ResponseWriter, _ *http.Request) {
	writeJSON(response, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *Handler) weather(response http.ResponseWriter, request *http.Request) {
	if !h.requests.TryAcquire() {
		h.metrics.LookupRejected()
		response.Header().Set("Retry-After", "1")
		writeError(response, http.StatusServiceUnavailable, "weather service is busy; retry later")
		return
	}
	defer h.requests.Release()
	query, err := parseQuery(request)
	if err != nil {
		writeError(response, http.StatusBadRequest, err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(request.Context(), h.requestTimeout)
	defer cancel()

	result, metadata, err := h.service.LookupWithMetadata(ctx, query)
	if err != nil {
		if errors.Is(err, openmeteo.ErrLocationNotFound) {
			h.logger.InfoContext(request.Context(), "weather location not found", "error", err)
		} else {
			h.logger.ErrorContext(request.Context(), "weather lookup failed", "error", err)
		}
		writeServiceError(response, err)
		return
	}
	if metadata.CacheStatus != weather.CacheDisabled {
		response.Header().Set("X-Weather-Cache", string(metadata.CacheStatus))
		response.Header().Set("Age", strconv.FormatInt(int64(metadata.Age/time.Second), 10))
		if metadata.CacheStatus == weather.CacheStale {
			response.Header().Set("Warning", `110 - "Response is Stale"`)
		}
	}
	writeJSON(response, http.StatusOK, result)
}

func parseQuery(request *http.Request) (weather.Query, error) {
	values := request.URL.Query()
	location, err := validation.Location(values.Get("location"))
	if err != nil {
		return weather.Query{}, err
	}
	latitudeText := strings.TrimSpace(values.Get("latitude"))
	longitudeText := strings.TrimSpace(values.Get("longitude"))

	if location != "" && (latitudeText != "" || longitudeText != "") {
		return weather.Query{}, errors.New("use either location or latitude/longitude, not both")
	}
	if location != "" {
		return weather.Query{Location: location}, nil
	}
	if latitudeText == "" && longitudeText == "" {
		return weather.Query{}, errors.New("provide location or both latitude and longitude")
	}
	if latitudeText == "" || longitudeText == "" {
		return weather.Query{}, errors.New("latitude and longitude must be provided together")
	}

	latitude, err := parseCoordinate("latitude", latitudeText, -90, 90)
	if err != nil {
		return weather.Query{}, err
	}
	longitude, err := parseCoordinate("longitude", longitudeText, -180, 180)
	if err != nil {
		return weather.Query{}, err
	}
	return weather.Query{Latitude: &latitude, Longitude: &longitude}, nil
}

func parseCoordinate(name, value string, minimum, maximum float64) (float64, error) {
	coordinate, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(coordinate) || math.IsInf(coordinate, 0) {
		return 0, errors.New(name + " must be a valid number")
	}
	if coordinate < minimum || coordinate > maximum {
		return 0, errors.New(name + " is outside its valid range")
	}
	return coordinate, nil
}

func writeServiceError(response http.ResponseWriter, err error) {
	if errors.Is(err, weather.ErrVendorBusy) || errors.Is(err, weather.ErrCircuitOpen) {
		retryAfter := 1
		var circuitError *weather.CircuitOpenError
		if errors.As(err, &circuitError) {
			retryAfter = max(1, int(math.Ceil(circuitError.RetryAfter.Seconds())))
		}
		response.Header().Set("Retry-After", strconv.Itoa(retryAfter))
		writeError(response, http.StatusServiceUnavailable, "weather provider temporarily unavailable; retry later")
		return
	}
	if errors.Is(err, openmeteo.ErrLocationNotFound) {
		writeError(response, http.StatusNotFound, "location not found")
		return
	}

	var vendorError *openmeteo.HTTPError
	if errors.As(err, &vendorError) {
		if vendorError.StatusCode == http.StatusTooManyRequests {
			// Upstream throttling is retryable: 503 (not 502) plus
			// Retry-After, matching the vendor-busy path above.
			response.Header().Set("Retry-After", "1")
			writeError(response, http.StatusServiceUnavailable, "weather provider is rate limited; retry later")
			return
		}
		writeError(response, http.StatusBadGateway, "weather provider request failed")
		return
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		writeError(response, http.StatusGatewayTimeout, "weather provider request timed out")
		return
	}
	writeError(response, http.StatusBadGateway, "weather provider request failed")
}

func writeError(response http.ResponseWriter, status int, message string) {
	writeJSON(response, status, map[string]string{"error": message})
}

func writeJSON(response http.ResponseWriter, status int, value any) {
	response.Header().Set("Content-Type", "application/json")
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(value)
}

func requestLogger(next http.Handler, logger *slog.Logger, sink response.Sink) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		started := time.Now()
		wrapped := &statusRecorder{ResponseWriter: response, status: http.StatusOK}
		next.ServeHTTP(wrapped, request)
		logArguments := []any{
			"method", request.Method,
			"path", request.URL.Path,
			"status", wrapped.status,
			"duration_ms", time.Since(started).Milliseconds(),
		}
		spanContext := trace.SpanContextFromContext(request.Context())
		if spanContext.IsValid() {
			logArguments = append(logArguments, "trace_id", spanContext.TraceID().String(), "span_id", spanContext.SpanID().String())
		}
		logger.Info("http request", logArguments...)
		if sink != nil && request.URL.Path != "/metrics" {
			sink.Enqueue(buildResponseRecord(request, wrapped, started, time.Since(started)))
		}
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
	body        bytes.Buffer
}

func (r *statusRecorder) WriteHeader(status int) {
	if r.wroteHeader {
		return
	}
	r.status = status
	r.wroteHeader = true
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(body []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	_, _ = r.body.Write(body)
	return r.ResponseWriter.Write(body)
}

func buildResponseRecord(request *http.Request, recorder *statusRecorder, started time.Time, duration time.Duration) response.Record {
	record := response.Record{
		OccurredAt:  started,
		Method:      request.Method,
		Path:        request.URL.Path,
		Status:      recorder.status,
		DurationMS:  duration.Milliseconds(),
		CacheStatus: recorder.Header().Get("X-Weather-Cache"),
		Response:    append([]byte(nil), recorder.body.Bytes()...),
	}
	if age, err := strconv.ParseInt(recorder.Header().Get("Age"), 10, 64); err == nil {
		record.CacheAgeSeconds = age
	}
	if len(record.Response) > maxLoggedResponseBytes {
		record.Response = nil
		record.Error = "response body omitted because it exceeded the persistence limit"
	}
	record.QueryMode, record.Location, record.Latitude, record.Longitude = responseQuery(request)
	if errorMessage := responseError(recorder.body.Bytes()); errorMessage != "" {
		record.Error = errorMessage
	}
	spanContext := trace.SpanContextFromContext(request.Context())
	if spanContext.IsValid() {
		record.TraceID = spanContext.TraceID().String()
		record.SpanID = spanContext.SpanID().String()
		record.SpanContext = spanContext
	}
	return record
}

func responseQuery(request *http.Request) (string, string, *float64, *float64) {
	values := request.URL.Query()
	location, err := validation.Location(values.Get("location"))
	latitude, longitude := optionalCoordinate(values.Get("latitude")), optionalCoordinate(values.Get("longitude"))
	if err != nil {
		// Rejected requests are still logged, but their invalid input is not.
		return "location", "", latitude, longitude
	}
	if location != "" {
		return "location", location, latitude, longitude
	}
	if latitude != nil || longitude != nil {
		return "coordinates", "", latitude, longitude
	}
	return "", "", nil, nil
}

func optionalCoordinate(value string) *float64 {
	coordinate, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil {
		return nil
	}
	return &coordinate
}

func responseError(body []byte) string {
	var value struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &value); err != nil {
		return ""
	}
	return value.Error
}
