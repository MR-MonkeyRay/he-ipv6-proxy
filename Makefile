GO ?= go
BINARY := bin/he-ipv6-proxy

.PHONY: all build test vet check clean

all: check build

build:
	CGO_ENABLED=0 $(GO) build -trimpath -o $(BINARY) ./cmd/he-ipv6-proxy

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

check: test vet

clean:
	rm -rf bin dist coverage.out
