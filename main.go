package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"weatherlookup/internal/cache"
	"weatherlookup/internal/httpapi"
	"weatherlookup/internal/metrics"
	"weatherlookup/internal/openmeteo"
	"weatherlookup/internal/persistence"
	"weatherlookup/internal/response"
	"weatherlookup/internal/telemetry"
	"weatherlookup/internal/weather"
)

func main() {
	if err := run(); err != nil {
		os.Exit(1)
	}
}

// run wires up the service and blocks until it stops. Deferred cleanup
// (persistence queue drain, tracer flush, signal-notify stop) always runs
// before run returns, so main can os.Exit only after they have completed.
func run() error {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	port := envOrDefault("PORT", "8080")
	address := ":" + port
	requestTimeout := durationOrDefault("REQUEST_TIMEOUT", 5*time.Second)
	vendorTimeout := durationOrDefault("OPEN_METEO_TIMEOUT", 4*time.Second)
	cacheTTL := durationOrDefault("CACHE_TTL", 5*time.Minute)
	cacheMaxStaleAge := durationOrDefault("CACHE_MAX_STALE_AGE", 15*time.Minute)
	cacheFillTimeout := durationOrDefault("CACHE_FILL_TIMEOUT", 5*time.Second)
	cacheMaxEntries := intOrDefault("CACHE_MAX_ENTRIES", 10000)
	vendorMaxAttempts := intOrDefault("VENDOR_MAX_ATTEMPTS", 2)
	openMeteoBaseURL := envOrDefault("OPEN_METEO_BASE_URL", "https://api.open-meteo.com")
	openMeteoGeocodingBaseURL := envOrDefault("OPEN_METEO_GEOCODING_BASE_URL", "https://geocoding-api.open-meteo.com")

	traceEndpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	serviceName := envOrDefault("OTEL_SERVICE_NAME", "weatherlookup")

	traceProvider, err := telemetry.NewTracerProvider(context.Background(), traceEndpoint, serviceName)
	if err != nil {
		logger.Error("tracing disabled because configuration is invalid", "error", err)
	}
	if traceProvider != nil {
		defer func() {
			shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := traceProvider.Shutdown(shutdown); err != nil {
				logger.Error("tracing shutdown failed", "error", err)
			}
		}()
	}

	vendor := openmeteo.NewClient(openmeteo.Config{
		BaseURL:          openMeteoBaseURL,
		GeocodingBaseURL: openMeteoGeocodingBaseURL,
		HTTPClient: &http.Client{
			Timeout:   vendorTimeout,
			Transport: telemetry.HTTPTransport(traceProvider),
		},
	})
	metricSet := metrics.New()
	weatherCache := cache.New[weather.Result](cacheMaxEntries, cacheTTL)
	service := weather.NewServiceWithOptions(vendor, weather.Options{
		Cache:                   weatherCache,
		MaxStaleAge:             cacheMaxStaleAge,
		FillTimeout:             cacheFillTimeout,
		MaxAttempts:             vendorMaxAttempts,
		Observer:                metricSet,
		MaxVendorInFlight:       intOrDefault("VENDOR_MAX_IN_FLIGHT", 8),
		BreakerFailureThreshold: intOrDefault("VENDOR_BREAKER_FAILURE_THRESHOLD", 5),
		BreakerCooldown:         durationOrDefault("VENDOR_BREAKER_COOLDOWN", 30*time.Second),
	})

	var responseSink response.Sink
	databaseDSN := os.Getenv("DATABASE_URL")
	if databaseDSN == "" {
		databaseDSN = persistence.DSNFromConfig(
			os.Getenv("DATABASE_HOST"),
			os.Getenv("DATABASE_PORT"),
			os.Getenv("DATABASE_NAME"),
			os.Getenv("DATABASE_USER"),
			os.Getenv("DATABASE_PASSWORD"),
		)
	}
	responseLogger, err := persistence.New(persistence.Config{
		DSN:       databaseDSN,
		QueueSize: intOrDefault("DATABASE_QUEUE_SIZE", 1000),
		Workers:   intOrDefault("DATABASE_WORKERS", 1),
		Retention: durationOrDefault("DATABASE_RETENTION", 7*24*time.Hour),
		Logger:    logger,
		Metrics:   metricSet,
	})
	if err != nil {
		logger.Error("response persistence disabled because database setup failed", "error", err)
	} else if responseLogger != nil {
		responseSink = responseLogger
		defer func() {
			shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := responseLogger.Close(shutdown); err != nil {
				logger.Error("response persistence shutdown failed", "error", err)
			}
		}()
	}

	handler := httpapi.NewHandlerWithOptions(service, requestTimeout, logger, httpapi.Options{
		Metrics:      metricSet,
		ResponseSink: responseSink,
		MaxInFlight:  intOrDefault("REQUEST_MAX_IN_FLIGHT", 64),
	})
	handler = telemetry.HTTPHandler(handler, traceProvider, serviceName)

	server := &http.Server{
		Addr:              address,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	serverErrors := make(chan error, 1)
	go func() {
		logger.Info("weather service listening", "address", address, "vendor", openMeteoBaseURL)
		serverErrors <- server.ListenAndServe()
	}()

	shutdownContext, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	select {
	case err := <-serverErrors:
		if !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server stopped unexpectedly", "error", err)
			return err
		}
	case <-shutdownContext.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			logger.Error("graceful shutdown failed", "error", err)
			return err
		}
	}

	return nil
}

func envOrDefault(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func durationOrDefault(key string, fallback time.Duration) time.Duration {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}

	duration, err := time.ParseDuration(value)
	if err != nil || duration <= 0 {
		return fallback
	}
	return duration
}

func intOrDefault(key string, fallback int) int {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}
