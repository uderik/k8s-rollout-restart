.PHONY: all build test clean

# Build settings
BINARY_NAME=k8s-rollout-restart
GO=go
GOFLAGS=-v

# Version information
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "none")
DATE ?= $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")
LDFLAGS=-ldflags "-s -w -X github.com/uderik/k8s-rollout-restart/cmd.version=$(VERSION) -X github.com/uderik/k8s-rollout-restart/cmd.commit=$(COMMIT) -X github.com/uderik/k8s-rollout-restart/cmd.date=$(DATE)"

all: build

build:
	$(GO) build $(GOFLAGS) $(LDFLAGS) -o $(BINARY_NAME)

test:
	$(GO) test ./... -v

clean:
	rm -f $(BINARY_NAME)
	$(GO) clean

# Development helpers
fmt:
	$(GO) fmt ./...

vet:
	$(GO) vet ./...

lint:
	golangci-lint run

tidy:
	$(GO) mod tidy

# Install dependencies
deps:
	$(GO) mod download

.DEFAULT_GOAL := all 