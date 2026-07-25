# Developer tasks for buzz-lakebox. Mirrors the checks in .github/workflows/ci.yml.

GO      ?= go
VERSION ?= dev
PROFILE ?= DEFAULT
BINARY  := buzz-backend-databricks-lakebox
CMD     := ./cmd/$(BINARY)
MODULE  := github.com/IceRhymers/buzz-lakebox
LDFLAGS := -X $(MODULE)/internal/version.Version=$(VERSION) -X $(MODULE)/internal/version.DefaultProfile=$(PROFILE)

.DEFAULT_GOAL := help

.PHONY: help build install symlink test vet lint check clean

help: ## Show available targets
	@awk 'BEGIN {FS = ":.*##"} /^[a-zA-Z_-]+:.*##/ {printf "  %-8s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

build: ## Build the provider binary into the repo root
	$(GO) build -ldflags '$(LDFLAGS)' -o $(BINARY) $(CMD)

install: ## Install into GOBIN; PROFILE=<name> bakes in a default Databricks profile
	$(GO) install -ldflags '$(LDFLAGS)' $(CMD)

SYMLINK_DIR ?= /usr/local/bin

symlink: ## Symlink the installed binary into SYMLINK_DIR (default /usr/local/bin) so GUI-launched Buzz Desktop finds it
	@gobin="$$($(GO) env GOBIN)"; [ -n "$$gobin" ] || gobin="$$($(GO) env GOPATH)/bin"; \
	src="$$gobin/$(BINARY)"; dest="$(SYMLINK_DIR)/$(BINARY)"; \
	[ -x "$$src" ] || { echo "$$src not found; run 'make install' first"; exit 1; }; \
	if [ -e "$$dest" ] || [ -L "$$dest" ]; then \
		echo "$$dest already exists; leaving it in place"; \
	else \
		ln -s "$$src" "$$dest" && echo "linked $$dest -> $$src"; \
	fi

test: ## Run all tests with the race detector (matches CI)
	$(GO) test ./... -race

vet: ## Run go vet
	$(GO) vet ./...

lint: ## Run golangci-lint (CI pins v2.11.4)
	@command -v golangci-lint >/dev/null 2>&1 || { echo "golangci-lint not found; install it from https://golangci-lint.run (CI uses v2.11.4)"; exit 1; }
	golangci-lint run ./...

check: vet lint test ## Run all local verification (vet + lint + test)

clean: ## Remove build artifacts
	rm -f $(BINARY)
	rm -rf dist
	$(GO) clean
