package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace"

	"weatherlookup/internal/cache"
	"weatherlookup/internal/openmeteo"
	"weatherlookup/internal/response"
	"weatherlookup/internal/weather"
)

type fakeProvider struct{}

func (fakeProvider) Search(context.Context, string) (weather.Location, error) {
	return weather.Location{Name: "Paris", Latitude: 48.86, Longitude: 2.35, Timezone: "Europe/Paris", Country: "France"}, nil
}

func (fakeProvider) CurrentWeather(context.Context, float64, float64) (weather.Location, weather.Current, error) {
	return weather.Location{Latitude: 48.86, Longitude: 2.35, Timezone: "Europe/Paris"}, weather.Current{Time: "2026-09-15T12:00", TemperatureC: 22, WeatherCode: 1, IsDay: true}, nil
}

func TestWeatherEndpointByCoordinates(t *testing.T) {
	handler := NewHandler(weather.NewService(fakeProvider{}), time.Second, nil)
	request := httptest.NewRequest(http.MethodGet, "/weather?latitude=48.86&longitude=2.35", nil)
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", response.Code, response.Body.String())
	}
	var result weather.Result
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Location.Latitude != 48.86 || result.Current.Condition != "partly cloudy" {
		t.Fatalf("result = %+v", result)
	}
}

func TestWeatherEndpointValidation(t *testing.T) {
	handler := NewHandler(weather.NewService(fakeProvider{}), time.Second, nil)
	tests := []struct {
		name string
		path string
	}{
		{name: "missing query", path: "/weather"},
		{name: "only latitude", path: "/weather?latitude=1"},
		{name: "invalid latitude", path: "/weather?latitude=abc&longitude=1"},
		{name: "out of range", path: "/weather?latitude=91&longitude=1"},
		{name: "mixed modes", path: "/weather?location=Paris&latitude=1&longitude=2"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, test.path, nil)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", response.Code)
			}
		})
	}
}

func TestHealthEndpoint(t *testing.T) {
	handler := NewHandler(weather.NewService(fakeProvider{}), time.Second, nil)
	request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Body.String() != "{\"status\":\"ok\"}\n" {
		t.Fatalf("response = %d %q", response.Code, response.Body.String())
	}
}

func TestWeatherEndpointCacheHeaders(t *testing.T) {
	provider := &countingProvider{}
	service := weather.NewServiceWithOptions(provider, weather.Options{
		Cache:      cache.New[weather.Result](10, time.Minute),
		RetryDelay: func(int) time.Duration { return 0 },
	})
	handler := NewHandlerWithOptions(service, time.Second, nil, Options{})

	firstRequest := httptest.NewRequest(http.MethodGet, "/weather?latitude=48.86&longitude=2.35", nil)
	firstResponse := httptest.NewRecorder()
	handler.ServeHTTP(firstResponse, firstRequest)
	if firstResponse.Code != http.StatusOK || firstResponse.Header().Get("X-Weather-Cache") != "miss" {
		t.Fatalf("first response = %d, cache=%q", firstResponse.Code, firstResponse.Header().Get("X-Weather-Cache"))
	}

	secondRequest := httptest.NewRequest(http.MethodGet, "/weather?latitude=48.86&longitude=2.35", nil)
	secondResponse := httptest.NewRecorder()
	handler.ServeHTTP(secondResponse, secondRequest)
	if secondResponse.Code != http.StatusOK || secondResponse.Header().Get("X-Weather-Cache") != "hit" {
		t.Fatalf("second response = %d, cache=%q", secondResponse.Code, secondResponse.Header().Get("X-Weather-Cache"))
	}
	if provider.lookups != 1 {
		t.Fatalf("provider lookups = %d, want 1", provider.lookups)
	}
}

type countingProvider struct {
	lookups int
}

func (p *countingProvider) Search(context.Context, string) (weather.Location, error) {
	return weather.Location{Latitude: 48.86, Longitude: 2.35, Timezone: "Europe/Paris"}, nil
}

func (p *countingProvider) CurrentWeather(context.Context, float64, float64) (weather.Location, weather.Current, error) {
	p.lookups++
	return weather.Location{Latitude: 48.86, Longitude: 2.35, Timezone: "Europe/Paris"}, weather.Current{WeatherCode: 1}, nil
}

type recordingSink struct {
	records []response.Record
}

func (s *recordingSink) Enqueue(record response.Record) {
	s.records = append(s.records, record)
}

func TestHandlerEnqueuesResponseRecord(t *testing.T) {
	sink := &recordingSink{}
	handler := NewHandlerWithOptions(weather.NewService(fakeProvider{}), time.Second, nil, Options{ResponseSink: sink})
	request := httptest.NewRequest(http.MethodGet, "/weather?latitude=48.86&longitude=2.35", nil)
	traceID, _ := trace.TraceIDFromHex("0123456789abcdef0123456789abcdef")
	spanID, _ := trace.SpanIDFromHex("0123456789abcdef")
	state, _ := trace.ParseTraceState("vendor=value")
	sc := trace.NewSpanContext(trace.SpanContextConfig{TraceID: traceID, SpanID: spanID, TraceFlags: trace.FlagsSampled, TraceState: state})
	request = request.WithContext(trace.ContextWithSpanContext(request.Context(), sc))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if len(sink.records) != 1 {
		t.Fatalf("records = %d, want 1", len(sink.records))
	}
	if sink.records[0].Status != http.StatusOK || sink.records[0].Path != "/weather" || len(sink.records[0].Response) == 0 {
		t.Fatalf("record = %+v", sink.records[0])
	}
	record := sink.records[0]
	if !record.SpanContext.Equal(sc) || record.TraceID != traceID.String() || record.SpanID != spanID.String() {
		t.Fatalf("response record lost trace identity, sampling flags, or trace state: %+v", record)
	}
}

func TestWriteServiceErrorMapsUpstreamRateLimitToServiceUnavailable(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantRetry  string
	}{
		{name: "upstream 429", err: &openmeteo.HTTPError{StatusCode: http.StatusTooManyRequests, Message: "Too Many Requests"}, wantStatus: http.StatusServiceUnavailable, wantRetry: "1"},
		{name: "upstream 500 stays 502", err: &openmeteo.HTTPError{StatusCode: http.StatusInternalServerError, Message: "Internal Server Error"}, wantStatus: http.StatusBadGateway, wantRetry: ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			writeServiceError(response, test.err)
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d", response.Code, test.wantStatus)
			}
			if got := response.Header().Get("Retry-After"); got != test.wantRetry {
				t.Fatalf("Retry-After = %q, want %q", got, test.wantRetry)
			}
		})
	}
}
