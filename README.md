# Weather Lookup Service

A small Go HTTP service that provides current weather conditions using
[Open-Meteo](https://open-meteo.com/) as the weather vendor. Open-Meteo's
geocoding API is used when a place name is supplied; weather responses are
normalized into a stable service-owned schema.

## Run

```sh
go run .
```

The server listens on `http://localhost:8080` by default.

```sh
curl 'http://localhost:8080/weather?latitude=44.98&longitude=-93.27'
curl 'http://localhost:8080/weather?location=Minneapolis'
curl 'http://localhost:8080/healthz'
```

`/v1/weather` is also available as a versioned alias for `/weather`.

## API

`GET /weather` requires either:

- `latitude` and `longitude`, expressed as decimal WGS84 coordinates; or
- `location`, a place name resolved using Open-Meteo geocoding.

Coordinates must be within latitude `[-90, 90]` and longitude `[-180, 180]`.
Locations must be valid UTF-8 and at most 256 bytes after URL decoding, including
surrounding whitespace. Surrounding whitespace is trimmed; embedded control
characters are rejected. Invalid locations return `400` before cache/vendor use
and are omitted from persisted request metadata, including for rejected requests.

Example response:

```json
{
  "location": {
    "latitude": 44.98,
    "longitude": -93.27,
    "timezone": "America/Chicago"
  },
  "current": {
    "time": "2026-09-15T12:00",
    "temperature_c": 18.4,
    "relative_humidity_pct": 61,
    "apparent_temperature_c": 18.1,
    "is_day": true,
    "precipitation_mm": 0,
    "rain_mm": 0,
    "showers_mm": 0,
    "snowfall_cm": 0,
    "weather_code": 2,
    "condition": "partly cloudy",
    "cloud_cover_pct": 44,
    "wind_speed_kmh": 13.2,
    "wind_direction_deg": 180
  }
}
```

Open-Meteo's current endpoint is requested with Celsius, km/h, millimetres,
and the location's automatically detected timezone. The `weather_code` is the
vendor's WMO interpretation code; `condition` is a human-readable mapping of
that code.

## Configuration

| Variable | Default | Purpose |
| --- | --- | --- |
| `PORT` | `8080` | HTTP listen port |
| `REQUEST_TIMEOUT` | `5s` | Maximum time spent serving a request |
| `REQUEST_MAX_IN_FLIGHT` | `64` | Maximum active weather HTTP requests per process; excess requests fail immediately |
| `OPEN_METEO_TIMEOUT` | `4s` | Timeout for each vendor request |
| `OPEN_METEO_BASE_URL` | `https://api.open-meteo.com` | Override the vendor endpoint, useful for tests or a proxy |
| `OPEN_METEO_GEOCODING_BASE_URL` | `https://geocoding-api.open-meteo.com` | Override the Open-Meteo geocoding endpoint |
| `CACHE_TTL` | `5m` | Freshness window for cached weather responses |
| `CACHE_MAX_STALE_AGE` | `15m` | Maximum total age since cache insertion allowed for stale-if-error responses, including the fresh TTL |
| `CACHE_FILL_TIMEOUT` | `5s` | Independent timeout for a shared cache fill; each caller still obeys `REQUEST_TIMEOUT` |
| `CACHE_MAX_ENTRIES` | `10000` | Maximum in-process cache entries |
| `VENDOR_MAX_ATTEMPTS` | `2` | Initial vendor request plus bounded retries |
| `VENDOR_MAX_IN_FLIGHT` | `8` | Maximum concurrent vendor-backed lookups, including retries, backoff, and abandoned cache fills |
| `VENDOR_BREAKER_FAILURE_THRESHOLD` | `5` | Consecutive transient attempt failures that open an endpoint's circuit |
| `VENDOR_BREAKER_COOLDOWN` | `30s` | Time an open circuit rejects calls before admitting one recovery probe |
| `DATABASE_URL` | empty | URL-form PostgreSQL DSN; takes precedence over component settings |
| `DATABASE_HOST` | empty | PostgreSQL host when `DATABASE_URL` is not set |
| `DATABASE_PORT` | `5432` | PostgreSQL port when component settings are used |
| `DATABASE_NAME` | empty | PostgreSQL database name |
| `DATABASE_USER` | empty | PostgreSQL user |
| `DATABASE_PASSWORD` | empty | PostgreSQL password |
| `DATABASE_QUEUE_SIZE` | `1000` | Maximum asynchronous response-log queue size |
| `DATABASE_WORKERS` | `1` | Number of response-log workers |
| `DATABASE_RETENTION` | `168h` | Response-log retention period |
| `OTEL_SERVICE_NAME` | `weatherlookup` | OpenTelemetry service name for exported traces |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | empty | OTLP/HTTP trace endpoint; `/v1/traces` is added when omitted |

## Design notes

- The Open-Meteo integration is isolated behind a provider interface, so the
  HTTP and domain layers do not depend on vendor-specific response formats.
- Vendor failures are returned as `502 Bad Gateway`; timeouts as `504 Gateway
  Timeout`; invalid input as `400 Bad Request`; and unknown place names as
  `404 Not Found`. By default one retry is allowed for HTTP 429/500/502/503/504,
  timeouts, connection reset/refused, and broken pipe errors. Other HTTP errors,
  malformed/missing vendor data, configuration errors, unknown errors, and
  cancellation are not retried. Retry delays use bounded exponential backoff
  with jitter, within the lookup deadline.
- Vendor HTTP error body previews are capped at 256 bytes (including a truncation
  marker), normalized to valid UTF-8 and a single line, and bounded before reading
  the body into memory. Logs and span errors share this bound; client responses
  remain generic. Previews are diagnostics, not secret-redacted content.
  Unknown-location `404` lookups log at info; upstream failures remain errors.
- Admission control rejects excess weather requests immediately with `503` and
  `Retry-After: 1`; both weather aliases share the same 64-request limit.
  `/healthz` and `/metrics` bypass this limit. Separately, at most eight
  vendor-backed lookups can run, with no waiting queue. Each holds its permit
  through geocoding, forecast, all retries/backoff, and detached cache-fill
  completion. Caller cancellation does not prematurely release a fill's permit.
  Because the vendor operations are sequential, eight fills imply at most eight
  concurrent upstream calls. This is a concurrency bound, not an RPS/daily quota;
  limits and breakers are per process and aggregate across replicas.
- Geocoding and forecast have independent closed/open/half-open circuit breakers,
  checked before each vendor attempt. Five consecutive transient failures open
  a circuit for 30 seconds; then exactly one probe is allowed. A successful or
  non-transient response closes it; a transient probe failure restarts the cooldown.
  Cancellation does not count as a dependency failure, and a cancelled probe
  releases its probe permit. Late completions from older circuit generations
  cannot change the new state. Non-transient responses reset the consecutive
  failure count; malformed data is not treated as a transient availability fault.
  Open/probing circuits return `503` with a `Retry-After` hint. Circuit rejections
  are not retried or counted as actual vendor requests.
- Successful weather results are cached in-process for five minutes, with a
  maximum of 10,000 entries and request de-duplication for concurrent misses.
  Cache state is exposed with `X-Weather-Cache`, `Age`, and stale `Warning`
  headers without changing the JSON response schema.
  Stale fallback is allowed for retryable failures, open circuits, or exhausted
  vendor capacity, and only up to 15 minutes
  total age since insertion (not 15 minutes after expiry). Age is checked after
  the vendor attempts, for each caller. Retained stale entries share the same
  bounded LRU capacity as fresh entries; unrelated inserts do not purge them.
  Setting `CACHE_MAX_STALE_AGE` below `CACHE_TTL` disables stale fallback.
- Concurrent misses share one cache fill with an independent five-second
  deadline. Each caller can cancel or time out without cancelling other callers.
  An abandoned fill may finish and warm the cache until its own deadline.
  Fresh cache hits bypass vendor capacity limits and circuit breakers.
- The server emits JSON request logs, Prometheus metrics, and OpenTelemetry
  spans for HTTP, vendor, and PostgreSQL operations, plus cache metrics.
  `/metrics` is served by `prometheus/client_golang` and also includes Go
  runtime (`go_*`) and process (`process_*`) metrics.
  HTTP metric paths come from `Request.Pattern` after the mux dispatches,
  with the optional method/host removed. Wildcard routes retain their template
  (for example `/weather/{location}`), not individual path values. Requests
  without a matched route (including mux 404/405 responses) use `unmatched`;
  CONNECT redirects also use `unmatched` because the mux can expose a concrete
  redirect path for those. Nonstandard methods are grouped as `OTHER`.
  Raw paths and custom method names cannot create new metric series.
  `/metrics` scrapes are excluded.
  Admission metrics expose current occupancy, configured limits, and rejected
  requests: `weatherlookup_lookup_{in_flight,max_in_flight,rejected_total}` and
  `weatherlookup_vendor_{in_flight,max_in_flight,admission_rejected_total}`.
  `weatherlookup_circuit_state{operation}` is 0=closed, 1=open, 2=half-open;
  `weatherlookup_circuit_rejections_total{operation}` counts suppressed attempts.
  Circuit labels are restricted to `geocoding.search` and `forecast.current`.
- When configured, response records are persisted asynchronously to
  PostgreSQL. Persistence failures do not fail weather requests.
  The queue carries only the originating span context (including sampling flags
  and trace state), not the request context or cancellation. A `response.persist`
  span and its instrumented SQL children remain in the originating request trace,
  even after the HTTP response finishes, with a separate two-second write budget.
  `weatherlookup_response_log_dropped_total` counts records lost to queue
  overflow, schema initialization failure, or exhausted insert retries, once per
  record rather than per attempt. `weatherlookup_response_log_dropped_by_reason_total`
  distinguishes `queue_full`, `schema_error`, and `write_failed`; write-attempt
  and retry counters remain separate. Persistence is best-effort, not durable delivery.

## Test

```sh
go test ./...
go test -race ./...
go vet ./...
```

## Deploy locally with Helm

The chart is in `helm/weatherlookup`. The default chart values use the local
image `weatherlookup:local` and `imagePullPolicy: Never`, which is convenient
for Rancher Desktop's local cluster.

```sh
docker build -t weatherlookup:local .
helm lint helm/weatherlookup
helm upgrade --install weatherlookup-db helm/postgres \
  --namespace weatherlookup --create-namespace
kubectl -n weatherlookup rollout status statefulset/weatherlookup-postgres
helm upgrade --install weatherlookup helm/weatherlookup \
  --namespace weatherlookup --create-namespace
kubectl -n weatherlookup rollout status deployment/weatherlookup
```

The chart exposes the HTTP service through local NodePort `30080`, so it is
available at `http://127.0.0.1:30080`.

## Monitor locally with Grafana LGTM

The repository includes a local observability chart at `helm/observability`.
It runs Grafana's `otel-lgtm` image with Prometheus, Loki, Tempo, and a
provisioned Weather Lookup dashboard and alert rules. Grafana Alloy is
installed separately to ship Kubernetes pod logs to Loki. PostgreSQL is
installed separately for response persistence.

```sh
helm upgrade --install lgtm helm/observability \
  --namespace observability --create-namespace
helm upgrade --install alloy grafana/alloy \
  --namespace observability \
  -f helm/observability/alloy-values.yaml
helm upgrade --install weatherlookup-db helm/postgres \
  --namespace weatherlookup --create-namespace
helm upgrade --install weatherlookup helm/weatherlookup \
  --namespace weatherlookup --create-namespace
kubectl -n observability rollout status deployment/lgtm
```

Grafana's HTTP UI is exposed through local NodePort `30300`; open
`http://127.0.0.1:30300` and sign in with `admin` / `admin`. The
`Weather Lookup / RED` dashboard shows request rate, error rate, latency, and
in-flight requests. The logs panel is populated by Alloy.

The Grafana MCP server can be installed against this Grafana instance after a
Viewer service-account token has been created and stored in
`grafana-mcp-token`. The token is never printed:

```sh
grafana_mcp_account_id=$(curl -fsS -u admin:admin \
  -H 'Content-Type: application/json' \
  -d '{"name":"grafana-mcp","role":"Viewer"}' \
  http://127.0.0.1:30300/api/serviceaccounts | jq -r '.id')
grafana_mcp_token=$(curl -fsS -u admin:admin \
  -H 'Content-Type: application/json' \
  -d '{"name":"mcp-client"}' \
  "http://127.0.0.1:30300/api/serviceaccounts/${grafana_mcp_account_id}/tokens" | jq -r '.key')
kubectl -n observability create secret generic grafana-mcp-token \
  --from-literal=token="$grafana_mcp_token"
unset grafana_mcp_account_id grafana_mcp_token

kubectl -n observability create secret generic grafana-mcp-server-token \
  --from-literal=token="$(openssl rand -hex 32)"
helm upgrade --install grafana-mcp grafana-community/grafana-mcp \
  --namespace observability \
  --version 0.23.1 \
  -f helm/observability/mcp-values.yaml
```

The chart uses Grafana MCP's Streamable HTTP transport at
`http://127.0.0.1:30800/mcp` through a local NodePort. Clients must send the server token as a bearer
token; retrieve it only when configuring a trusted local MCP client with:

```sh
kubectl -n observability get secret grafana-mcp-server-token \
  -o jsonpath='{.data.token}' | base64 -d
```

### Connect local Codex

The shared local Codex configuration at `~/.codex/config.toml` contains an
enabled `grafana` server entry pointing to the Streamable HTTP endpoint. It
uses `scripts/grafana-mcp-headers.sh` as a header helper, so the bearer token
stays in Kubernetes rather than in the Codex config file. Restart the Codex
CLI, desktop app, or IDE extension and confirm the connection with
`codex mcp get grafana` or `/mcp`.

The all-in-one LGTM image is intended for local development, demos, and
testing. Its chart uses ephemeral storage by default, so production use should
replace the storage and credentials with durable, managed configuration.
