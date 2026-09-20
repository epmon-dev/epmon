# epmon — single static binary, zero cgo.
#
#   make build          local binary with version stamping
#   make test           full suite (make test-race for -race, as in CI)
#   make check          fmt + vet + tests (what CI's go job runs)
#   make cross          §G1 matrix into dist/
#   make docker-build   image with release metadata (never reports `dev`)
#   make compose-up     detached stack (VERSION/COMMIT/DATE flow through)
#
# Overridables: make build VERSION=0.2.0 / make compose-up VERSION=... / ARGS="..."

BINARY      ?= epmon
DIST        ?= dist
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT      ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE        ?= $(shell date -u +%FT%TZ 2>/dev/null || echo unknown)
LDFLAGS     := -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)
GO          ?= go
DOCKER      ?= docker
# `docker compose` (plugin) or falling back to the standalone binary.
COMPOSE     ?= $(shell if $(DOCKER) compose version >/dev/null 2>&1; then echo "$(DOCKER) compose"; else echo docker-compose; fi)

PLATFORMS   := linux/amd64 linux/arm64 darwin/arm64 windows/amd64

.PHONY: help build test test-race vet fmt lint check check-docs cross clean \
	run init validate docker-build docker-version docker-run \
	compose-up compose-down compose-logs compose-config

help:
	@echo "Targets:"
	@echo "  build           CGO_ENABLED=0 binary ./$(BINARY) with ldflags stamping"
	@echo "  test            go test ./..."
	@echo "  test-race       go test -race ./... (CI parity)"
	@echo "  vet             go vet ./..."
	@echo "  fmt             fail on gofmt drift"
	@echo "  lint            golangci-lint if installed, else warn-and-skip"
	@echo "  check           fmt + vet + test-race"
	@echo "  check-docs      every README yaml block + examples must validate"
	@echo "  cross           static binaries for $(PLATFORMS) into $(DIST)/"
	@echo "  clean           remove ./$(BINARY) and $(DIST)/"
	@echo '  run             go run ./cmd/epmon run $$ARGS'
	@echo '  init            go run ./cmd/epmon init $$ARGS'
	@echo '  validate        go run ./cmd/epmon validate $$ARGS'
	@echo '  docker-build    image epmon:$$VERSION with release metadata'
	@echo "  docker-version  print the image's reported version"
	@echo "  docker-run      run image foreground (override with ARGS)"
	@echo "  compose-up      detached stack (passes VERSION/COMMIT/DATE)"
	@echo "  compose-down    stop the stack"
	@echo "  compose-logs    tail the stack logs"
	@echo "  compose-config  validate compose file parsing"

build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/epmon

test:
	$(GO) test ./...

test-race:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

fmt:
	@test -z "$$($(GO)fmt -l .)" || { $(GO)fmt -l .; echo "gofmt drift — run: gofmt -w ."; exit 1; }

lint:
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run ./...; \
	else \
		echo "golangci-lint not installed — skipping (vet still runs in check)"; \
	fi

check: fmt vet test-race

# Mirrors CI: every ```yaml block in README must load through validate,
# plus the shipped examples. (Script file, not a heredoc: macOS make is
# 3.81 and runs each recipe line in its own shell.)
check-docs:
	EPMON_API_KEY=ci-dummy TOKEN=ci-dummy DEV_TOKEN=ci-dummy python3 scripts/check-docs.py

cross:
	@mkdir -p $(DIST)
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; out=$(DIST)/epmon-$$os-$$arch; \
		test "$$os" = windows && out=$$out.exe; \
		echo "building $$out"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch $(GO) build -trimpath \
			-ldflags "$(LDFLAGS)" -o $$out ./cmd/epmon || exit 1; \
	done

clean:
	rm -f $(BINARY)
	rm -rf $(DIST)

run:
	$(GO) run ./cmd/epmon run $(ARGS)

init:
	$(GO) run ./cmd/epmon init $(ARGS)

validate:
	$(GO) run ./cmd/epmon validate $(ARGS)

docker-build:
	$(DOCKER) build -t epmon:$(VERSION) \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		--build-arg DATE=$(DATE) .

docker-version:
	$(DOCKER) run --rm --entrypoint /epmon epmon:$(VERSION) version

docker-run:
	$(DOCKER) run --rm -p 8080:8080 \
		-v "$(CURDIR)/data:/data:rw" \
		epmon:$(VERSION) $(ARGS)

compose-up:
	VERSION=$(VERSION) COMMIT=$(COMMIT) DATE=$(DATE) $(COMPOSE) up --build -d

compose-down:
	$(COMPOSE) down

compose-logs:
	$(COMPOSE) logs -f

compose-config:
	$(COMPOSE) config
