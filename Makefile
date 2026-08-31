GO ?= go
PKG := ./...
BINARY := quorumlimiter

.PHONY: fmt test race lint vuln run build tidy help

help:
	@echo "Targets: fmt test race lint vuln run build tidy"

fmt:
	$(GO) fmt $(PKG)

test:
	$(GO) test $(PKG) -count=1

race:
	$(GO) test -race $(PKG) -count=1 -timeout=10m

lint:
	golangci-lint run $(PKG)

vuln:
	govulncheck $(PKG)

run:
	$(GO) run ./cmd/quorumlimiter

build:
	$(GO) build -trimpath -o bin/$(BINARY) ./cmd/quorumlimiter

tidy:
	$(GO) mod tidy
