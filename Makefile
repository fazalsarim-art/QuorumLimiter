GO ?= go
PKG := ./...
BINARY := quorumlimiter
COMPOSE := docker compose -f deploy/compose.dev.yml --env-file deploy/.env

.PHONY: fmt test race lint vuln run build tidy help \
	docker-build compose-up compose-down compose-logs compose-ps

help:
	@echo "Dev:    fmt test race lint vuln run build tidy"
	@echo "Docker: docker-build compose-up compose-down compose-logs compose-ps"

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
