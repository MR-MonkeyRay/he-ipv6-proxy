GO ?= go
BINARY := bin/light-proxy

.PHONY: all build test vet check clean

all: check build

build:
	CGO_ENABLED=0 $(GO) build -trimpath -o $(BINARY) ./cmd/light-proxy

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

check: test vet

clean:
	rm -rf bin dist coverage.out
