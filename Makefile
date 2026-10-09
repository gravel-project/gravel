# gravel — build, test and run the hub. Every tool is pinned and installed into .bin/ by `make tools`,
# so a clean machine needs only Go and podman. CI runs the same targets.
SHELL := /usr/bin/env bash
.SHELLFLAGS := -eu -o pipefail -c
.DEFAULT_GOAL := help

GO ?= go
BIN := $(CURDIR)/.bin
export PATH := $(BIN):$(PATH)

# Pinned tool versions. Bump here; `make tools` reinstalls what changed.
BUF_VERSION ?= v1.73.0
PROTOC_GEN_GO_VERSION ?= v1.36.12
PROTOC_GEN_CONNECT_GO_VERSION ?= v1.21.0
PROTOC_GEN_CONNECT_OPENAPI_VERSION ?= v0.28.0
GOLANGCI_LINT_VERSION ?= v2.14.0
GOVULNCHECK_VERSION ?= v1.8.0
KO_VERSION ?= v0.19.1
TEMPL_VERSION ?= v0.3.1070
# Vendored into internal/web/static/vendor/ by `make vendor-htmx`; the pages load it from the binary.
HTMX_VERSION ?= 2.0.11
# The accessibility check (`make a11y`) runs this through npx; CI pins it the same way.
AXE_CLI_VERSION ?= 4.13.0
# The hub's Postgres image: the official image plus WAL-G (deploy/postgres/Containerfile).
POSTGRES_IMAGE_REPO ?= localhost/gravel-postgres

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
export VERSION
LDFLAGS := -s -w -X main.version=$(VERSION)
IMAGE_REPO ?= localhost/gravel-hub
IMAGE_TAG ?= dev
# Host ports the compose stack publishes on 127.0.0.1 (override when 8080/9090 are taken).
GRAVEL_PORT ?= 8080
GRAVEL_INTERNAL_PORT ?= 9090
export GRAVEL_PORT GRAVEL_INTERNAL_PORT
# The dev store (versitygw) takes its root key from the environment; the compose file reads it from here.
COMPOSE := GRAVEL_STORE_SECRET="$$(cat $(CURDIR)/deploy/secrets/store-secret 2>/dev/null || echo unset)" podman compose -f $(CURDIR)/deploy/compose.yaml
QUADLET := $(firstword $(wildcard /usr/libexec/podman/quadlet /usr/lib/podman/quadlet /usr/lib/systemd/system-generators/podman-system-generator))

## ---- tools -------------------------------------------------------------------------------------

TOOLS := $(BIN)/buf $(BIN)/protoc-gen-go $(BIN)/protoc-gen-connect-go $(BIN)/protoc-gen-connect-openapi \
         $(BIN)/golangci-lint $(BIN)/govulncheck $(BIN)/ko $(BIN)/templ

.PHONY: tools
tools: $(TOOLS) ## Install the pinned CLIs into .bin/

$(BIN)/buf: Makefile
	GOBIN=$(BIN) $(GO) install github.com/bufbuild/buf/cmd/buf@$(BUF_VERSION)
$(BIN)/protoc-gen-go: Makefile
	GOBIN=$(BIN) $(GO) install google.golang.org/protobuf/cmd/protoc-gen-go@$(PROTOC_GEN_GO_VERSION)
$(BIN)/protoc-gen-connect-go: Makefile
	GOBIN=$(BIN) $(GO) install connectrpc.com/connect/cmd/protoc-gen-connect-go@$(PROTOC_GEN_CONNECT_GO_VERSION)
$(BIN)/protoc-gen-connect-openapi: Makefile
	GOBIN=$(BIN) $(GO) install github.com/sudorandom/protoc-gen-connect-openapi@$(PROTOC_GEN_CONNECT_OPENAPI_VERSION)
$(BIN)/golangci-lint: Makefile
	GOBIN=$(BIN) $(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
$(BIN)/govulncheck: Makefile
	GOBIN=$(BIN) $(GO) install golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION)
$(BIN)/ko: Makefile
	GOBIN=$(BIN) $(GO) install github.com/google/ko@$(KO_VERSION)
$(BIN)/templ: Makefile
	GOBIN=$(BIN) $(GO) install github.com/a-h/templ/cmd/templ@$(TEMPL_VERSION)

## ---- code --------------------------------------------------------------------------------------

.PHONY: generate
generate: $(BIN)/buf $(BIN)/protoc-gen-go $(BIN)/protoc-gen-connect-go $(BIN)/protoc-gen-connect-openapi $(BIN)/templ ## Regenerate gen/ from proto/ and the pages' Go from their .templ files
	$(BIN)/buf generate
	$(BIN)/templ generate -path internal/web/templates

.PHONY: generate-check
generate-check: generate ## Fail if gen/ or a generated page is stale
	git diff --exit-code -- gen/ internal/web/templates/ || { echo "generated code is stale: run make generate and commit"; exit 1; }

.PHONY: vendor-htmx
vendor-htmx: ## Fetch htmx $(HTMX_VERSION) into internal/web/static/vendor/ (then commit it)
	curl -sfL https://unpkg.com/htmx.org@$(HTMX_VERSION)/dist/htmx.min.js -o internal/web/static/vendor/htmx.min.js
	curl -sfL https://unpkg.com/htmx.org@$(HTMX_VERSION)/LICENSE -o internal/web/static/vendor/htmx.LICENSE
	printf 'htmx.org %s\nsource: https://unpkg.com/htmx.org@%s/dist/htmx.min.js\nsha256: %s\nlicence: 0BSD (htmx.LICENSE)\nrefresh: make vendor-htmx (HTMX_VERSION in the Makefile)\n' \
	  $(HTMX_VERSION) $(HTMX_VERSION) "$$(sha256sum internal/web/static/vendor/htmx.min.js | cut -d' ' -f1)" > internal/web/static/vendor/htmx.version

.PHONY: a11y
a11y: ## Run axe over the rendered pages (needs node and Chrome; CI does too)
	A11Y=1 AXE_CLI_VERSION=$(AXE_CLI_VERSION) $(GO) test -count=1 -run TestAccessibility ./internal/web/

.PHONY: build
build: ## Build the hub binary into .bin/
	$(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN)/gravel-hub ./cmd/gravel-hub

.PHONY: fmt-check
fmt-check: ## Fail on unformatted Go
	@out="$$(gofmt -l .)"; if [[ -n "$$out" ]]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

.PHONY: vet
vet: ## go vet
	$(GO) vet ./...

.PHONY: test
test: ## Unit tests with the race detector (integration tests skip without GRAVEL_TEST_DATABASE_URL)
	$(GO) test -race -count=1 ./...

.PHONY: test-integration
test-integration: ## All tests against a real Postgres: GRAVEL_TEST_DATABASE_URL must be set
	@[[ -n "$${GRAVEL_TEST_DATABASE_URL:-}" ]] || { echo "set GRAVEL_TEST_DATABASE_URL (see docs/hub.md)"; exit 1; }
	$(GO) test -race -count=1 ./...

.PHONY: lint
lint: $(BIN)/golangci-lint ## golangci-lint (includes the games/ import boundary)
	$(BIN)/golangci-lint run

.PHONY: vuln
vuln: $(BIN)/govulncheck ## govulncheck
	$(BIN)/govulncheck ./...

.PHONY: proto-lint
proto-lint: $(BIN)/buf ## buf lint
	$(BIN)/buf lint

.PHONY: proto-breaking
proto-breaking: $(BIN)/buf ## buf breaking against origin/main (skipped while main has no protos)
	@if git cat-file -e origin/main:buf.yaml 2>/dev/null; then $(BIN)/buf breaking --against '.git#branch=origin/main'; else echo "no buf.yaml on origin/main yet; skipping"; fi

.PHONY: check
check: fmt-check vet proto-lint lint vuln generate-check test quadlet-check ## Everything CI runs, except the integration job

## ---- containers --------------------------------------------------------------------------------

.PHONY: image
image: $(BIN)/ko ## Build the hub image with ko and load it into podman as $(IMAGE_REPO):$(IMAGE_TAG)
	KO_DOCKER_REPO=$(IMAGE_REPO) $(BIN)/ko build --bare --push=false --tarball $(BIN)/gravel-hub.tar -t $(IMAGE_TAG) ./cmd/gravel-hub
	podman load -i $(BIN)/gravel-hub.tar

.PHONY: postgres-image
postgres-image: ## Build the hub's Postgres image (Postgres 17 + WAL-G) as $(POSTGRES_IMAGE_REPO):$(IMAGE_TAG)
	podman build -t $(POSTGRES_IMAGE_REPO):$(IMAGE_TAG) -f $(CURDIR)/deploy/postgres/Containerfile $(CURDIR)/deploy/postgres
	podman run --rm $(POSTGRES_IMAGE_REPO):$(IMAGE_TAG) wal-g --version

.PHONY: up
up: image postgres-image ## Run hub + Postgres (+ a local backup store) with podman compose (dev secrets generated on first run)
	$(CURDIR)/deploy/dev-secrets.sh $(CURDIR)/deploy/secrets
	$(COMPOSE) up -d
	@for i in $$(seq 1 90); do \
	  if curl -sf http://127.0.0.1:$(GRAVEL_PORT)/readyz >/dev/null 2>&1; then echo "hub ready after $$i s"; break; fi; \
	  if [[ $$i -eq 90 ]]; then echo "hub not ready after 90 s:"; $(COMPOSE) ps; $(COMPOSE) logs --tail 20; exit 1; fi; \
	  sleep 1; \
	done
	@echo "hub: http://127.0.0.1:$(GRAVEL_PORT)/healthz   metrics: http://127.0.0.1:$(GRAVEL_INTERNAL_PORT)/metrics   logs: podman compose -f deploy/compose.yaml logs -f hub"

.PHONY: down
down: ## Stop the compose stack (keeps the Postgres volume)
	$(COMPOSE) down

.PHONY: backup
backup: ## Take a base backup of the compose stack's Postgres into the local store
	$(COMPOSE) --profile tools run --rm backup

.PHONY: backup-drill
backup-drill: build postgres-image ## The restore drill: archive, back up, restore to a point in time, verify, timed (podman)
	HUB=$(BIN)/gravel-hub IMAGE=$(POSTGRES_IMAGE_REPO):$(IMAGE_TAG) $(CURDIR)/deploy/backup-drill.sh

.PHONY: quadlet-check
quadlet-check: ## Dry-run the quadlet units, with and without the backup drop-ins
	@[[ -n "$(QUADLET)" ]] || { echo "quadlet generator not found (install podman)"; exit 1; }
	QUADLET_UNIT_DIRS=$(CURDIR)/deploy/quadlet $(QUADLET) -dryrun -user >/dev/null && echo "quadlet units OK"
	@tmp="$$(mktemp -d)"; cp -r $(CURDIR)/deploy/quadlet/*.container $(CURDIR)/deploy/quadlet/*.volume $(CURDIR)/deploy/quadlet/*.network "$$tmp/"; \
	  cp -r $(CURDIR)/deploy/quadlet/backup/. "$$tmp/"; rm -f "$$tmp/README.md"; \
	  QUADLET_UNIT_DIRS="$$tmp" $(QUADLET) -dryrun -user >/dev/null && echo "quadlet units with backups OK"; rm -rf "$$tmp"

.PHONY: quadlet-install
quadlet-install: ## Copy the quadlet units into ~/.config/containers/systemd/ and reload
	install -d $(HOME)/.config/containers/systemd
	install -m 0644 $(CURDIR)/deploy/quadlet/*.container $(CURDIR)/deploy/quadlet/*.volume $(CURDIR)/deploy/quadlet/*.network $(HOME)/.config/containers/systemd/
	systemctl --user daemon-reload
	@echo "units installed; see deploy/README.md for secrets and start order"

.PHONY: quadlet-install-backup
quadlet-install-backup: ## Copy the backup drop-ins, unit and timer beside the installed units and reload (edit the bucket lines first)
	install -d $(HOME)/.config/containers/systemd/gravel-postgres.container.d $(HOME)/.config/containers/systemd/gravel-hub.container.d
	install -m 0644 $(CURDIR)/deploy/quadlet/backup/gravel-postgres.container.d/* $(HOME)/.config/containers/systemd/gravel-postgres.container.d/
	install -m 0644 $(CURDIR)/deploy/quadlet/backup/gravel-hub.container.d/* $(HOME)/.config/containers/systemd/gravel-hub.container.d/
	install -m 0644 $(CURDIR)/deploy/quadlet/backup/gravel-backup.container $(CURDIR)/deploy/quadlet/backup/gravel-backup.volume $(CURDIR)/deploy/quadlet/backup/gravel-backup.timer $(HOME)/.config/containers/systemd/
	systemctl --user daemon-reload
	@echo "backup units installed; restart gravel-postgres and gravel-hub, then: systemctl --user enable --now gravel-backup.timer"

.PHONY: clean
clean: ## Remove build outputs (not the tools)
	rm -f $(BIN)/gravel-hub $(BIN)/gravel-hub.tar
	rm -rf dist

.PHONY: help
help: ## This help
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  %-20s %s\n", $$1, $$2}'
