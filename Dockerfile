FROM golang:1.25-alpine AS build

WORKDIR /src

# Copy module manifests first so the dependency layer is cached across
# source-only rebuilds.
COPY go.mod go.sum ./

# Pre-download modules; BuildKit cache mounts speed up repeated builds and
# keep the module/build caches out of the final layers.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go mod download

COPY . ./

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/weatherlookup .

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/weatherlookup /weatherlookup

USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/weatherlookup"]
