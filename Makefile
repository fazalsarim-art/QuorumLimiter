# Recipes use bash (the smoke/failover/secret-scan scripts and the inline
# fmt-check test are bash). bash is present on git-bash, WSL, Linux and macOS.
SHELL := bash

GO ?= go
PKG := ./...
BINARY := quorumlimiter
COMPOSE := docker compose -f deploy/compose.dev.yml --env-file deploy/.env

.PHONY: fmt fmt-check vet test race lint vuln secret-scan release-check \
	run build tidy help \
	docker-build compose-up compose-down compose-logs compose-ps \
	integration smoke failover load

help:
	@echo "Dev:     fmt fmt-check vet test race lint vuln secret-scan tidy integration"
	@echo "Gate:    release-check  (fmt-check vet test race lint vuln secret-scan)"
	@echo "Build:   run build"
	@echo "Docker:  docker-build compose-up compose-down compose-logs compose-ps"
	@echo "Ops:     smoke failover load  (require a running cluster + deploy/.env)"

fmt:
	$(GO) fmt $(PKG)

# fmt-check does not rewrite files; it fails if anything is unformatted.
fmt-check:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt needed on:"; echo "$$out"; exit 1; fi; echo "gofmt clean"

vet:
	$(GO) vet $(PKG)

test:
	$(GO) test $(PKG) -count=1

race:
	$(GO) test -race $(PKG) -count=1 -timeout=10m

lint:
	golangci-lint run $(PKG)

vuln:
	govulncheck $(PKG)

secret-scan:
	bash scripts/secret-scan.sh

# release-check is the single reproducible quality gate. It runs each step in a
# fixed order (via $(MAKE) so ordering is deterministic even under `make -j`) and
# does not suppress any findings.
release-check:
	$(MAKE) fmt-check
	$(MAKE) vet
	$(MAKE) test
	$(MAKE) race
	$(MAKE) lint
	$(MAKE) vuln
	$(MAKE) secret-scan
	@echo "release-check: ALL GATES PASSED"

run:
	$(GO) run ./cmd/quorumlimiter

build:
	$(GO) build -trimpath -o bin/$(BINARY) ./cmd/quorumlimiter

tidy:
	$(GO) mod tidy

docker-build:
	docker build -t $(BINARY):dev .

# Requires deploy/.env with the four shared secrets (see deploy/.env.example).
compose-up:
	$(COMPOSE) up -d --build

compose-down:
	$(COMPOSE) down

compose-logs:
	$(COMPOSE) logs -f --tail=100

compose-ps:
	$(COMPOSE) ps

integration:
	$(GO) test ./test/integration -count=1 -timeout=120s

smoke:
	bash scripts/smoke.sh

failover:
	bash scripts/failover.sh

# load runs the k6 performance test. Requires BASE_URL, API_KEY and POLICY_ID
# (see scripts/load.js header). k6 may be a local binary or the grafana/k6 image.
#   make load BASE_URL=http://localhost:8080 API_KEY=qlk_xxx POLICY_ID=load
load:
	k6 run -e BASE_URL=$(BASE_URL) -e API_KEY=$(API_KEY) -e POLICY_ID=$(POLICY_ID) scripts/load.js
