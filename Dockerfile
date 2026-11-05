# syntax=docker/dockerfile:1

ARG GO_VERSION=1.26

# --- dev: hot reload with air (mount the repo at /src) ---
FROM golang:${GO_VERSION} AS dev
RUN go install github.com/air-verse/air@latest
WORKDIR /src
EXPOSE 8080
CMD ["air", "-c", ".air.toml"]

# --- build: static binary ---
FROM golang:${GO_VERSION} AS build
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath \
    -ldflags="-s -w -X github.com/silopost/silo-server/internal/version.Version=${VERSION}" \
    -o /out/silo-server ./cmd/silo-server

# --- prod: distroless, non-root ---
FROM gcr.io/distroless/static-debian12:nonroot AS prod
COPY --from=build /out/silo-server /silo-server
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/silo-server"]
CMD ["serve"]
