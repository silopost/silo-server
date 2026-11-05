VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/silopost/silo-server/internal/version.Version=$(VERSION)

.PHONY: build test lint run

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/silo-server ./cmd/silo-server

test:
	go test -race ./...

lint:
	golangci-lint run ./...

run:
	SILO_LOG_FORMAT=text go run ./cmd/silo-server serve
