VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/silopost/silo-server/internal/version.Version=$(VERSION)

SQLC := github.com/sqlc-dev/sqlc/cmd/sqlc@v1.31.1

.PHONY: build test lint run sqlc openapi

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/silo-server ./cmd/silo-server

test:
	go test -race ./...

lint:
	golangci-lint run ./...

run:
	SILO_LOG_FORMAT=text go run ./cmd/silo-server serve

# Regenerate internal/store/sqlcgen from migrations/ and internal/store/queries/.
sqlc:
	go run $(SQLC) generate

# Regenerate api/openapi.yaml from the huma operations (no database needed).
openapi:
	go run ./cmd/silo-openapi
