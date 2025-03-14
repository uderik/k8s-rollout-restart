.PHONY: all build test clean

# Build settings
BINARY_NAME=k8s-rollout-restart
GO=go
GOFLAGS=-v
LDFLAGS=-ldflags "-s -w"

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