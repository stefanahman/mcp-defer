BIN ?= $(HOME)/.local/bin
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: build install test lint

build:
	go build -ldflags "-X main.version=$(VERSION)" -o mcp-defer .

install:
	mkdir -p $(BIN)
	go build -ldflags "-X main.version=$(VERSION)" -o $(BIN)/mcp-defer .

test:
	go test -race -count=1 ./...

lint:
	test -z "$$(gofmt -l .)" || { gofmt -l .; exit 1; }
	go vet ./...
