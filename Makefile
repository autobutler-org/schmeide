SHELL := /usr/bin/env
.SHELLFLAGS = bash -e -o pipefail -c
.DEFAULT_GOAL := help
.NOTPARALLEL:
.SILENT: # use set -v to print commands executed
.ONESHELL:

ifndef VERBOSE
MAKEFLAGS += --no-print-directory
endif

ifneq (,$(wildcard ./.env))
    include .env
    export
endif

export GOTOOLCHAIN=go1.26.0+auto
export GOPROXY ?= https://proxy.golang.org,direct
GO := $(shell which go)

MAIN ?= ./cmd/schmeide
EXE ?= ./build/schmeide
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
GH ?= gh
LDFLAGS := -X github.com/autobutler-org/schmeide/internal/release.Version=$(VERSION)

# Runs go test with $(1) as its arguments and, on failure, repeats what failed
# at the end, where a gate that keeps only the last lines still shows it.
define go_test
	log="$$(mktemp)"
	trap 'rm -f "$$log"' EXIT
	if $(GO) test $(1) 2>&1 | tee "$$log"; then exit 0; fi
	echo
	echo "make $@ FAILED:"
	bash scripts/test-summary.sh "$$log"
	exit 1
endef

##@ Development Environment

.PHONY: setup
setup: setup/gotools

.PHONY: setup/gotools
setup/gotools: ## Install go tools
	$(GO) install golang.org/x/tools/gopls@v0.23.0
	$(GO) install github.com/cweill/gotests/gotests@v1.6.0
	$(GO) install github.com/josharian/impl@v1.4.0
	$(GO) install github.com/haya14busa/goplay/cmd/goplay@v1.0.0

##@ Build

.PHONY: build
build: build/go ## Build codebase

.PHONY: build/go
build/go: ## Build Go codebase
	mkdir -p ./build
	$(GO) build -ldflags "$(LDFLAGS)" -o $(EXE) $(MAIN)

.PHONY: install
install: ## Install schmeide into GOBIN (GOPATH/bin when unset)
	$(GO) install -ldflags "$(LDFLAGS)" $(MAIN)

.PHONY: clean
clean: ## Clean build space
	rm -rf \
		./build \
		./tmp

.PHONY: run
run: build ## Build and run the application
	$(EXE)

.PHONY: test
test: ## Run tests
	$(call go_test,./...)

.PHONY: test/race
test/race: ## Run tests with the race detector (PKGS="./internal/control" to pick some, COVERPROFILE=f to write coverage)
	# The control node and runner share state across goroutines (NATS
	# subscriptions, the status store, pool runners on one job), so a race
	# here is a real bug.
	$(call go_test,-race -covermode=atomic $(if $(COVERPROFILE),-coverprofile=$(COVERPROFILE)) $(or $(PKGS),./...))

.PHONY: test/scripts
test/scripts: ## Test the shell scripts under scripts/
	for t in scripts/*_test.sh; do
		echo "== $$t"
		bash "$$t"
	done

.PHONY: test/e2e
test/e2e: ## Build the binary and run the end-to-end journeys (build tag e2e)
	start=$$(date +%s)
	$(GO) test -tags e2e -count=1 ./internal/e2e
	echo "internal/e2e took $$(( $$(date +%s) - start ))s"

COVER_OUT ?= ./build/coverage.out
COVER_HTML ?= ./build/coverage.html

.PHONY: bench
bench: ## Run the control benchmarks
	$(GO) test -bench . -benchmem ./internal/control

.PHONY: test/cover
test/cover: ## Run tests and write a coverage report under ./build
	mkdir -p ./build
	$(GO) test -coverprofile=$(COVER_OUT) ./...
	$(GO) tool cover -html=$(COVER_OUT) -o $(COVER_HTML)
	echo "Coverage report: $(COVER_HTML)"

.PHONY: watch
watch: ## Watch for changes and rebuild
	$(GO) tool air \
		--build.cmd "$(MAKE) build" \
		--build.entrypoint "$(EXE)" \
		--build.exclude_dir ".github,build,docs"

##@ Release

.PHONY: changelog
changelog: ## Write CHANGELOG.md from merged pull requests (needs gh logged in)
	if ! command -v $(GH) >/dev/null || ! $(GH) auth status >/dev/null 2>&1; then
		echo "make changelog needs the GitHub CLI: install gh (https://cli.github.com), then run \`gh auth login\`"
		exit 1
	fi
	{
		echo "# Changelog"
		echo
		$(GH) pr list --state merged --limit 1000 --json number,title,mergedAt \
			--jq 'sort_by(.mergedAt) | reverse | .[] | "- \(.title) (#\(.number), \(.mergedAt[:10]))"'
	} > CHANGELOG.md
	echo "Wrote CHANGELOG.md"

##@ Dependencies

.PHONY: deps
deps: ## Install dependencies for Go
	$(GO) mod download

.PHONY: tidy
tidy: ## Go deps (go mod tidy)
	$(GO) mod tidy

.PHONY: upgrade
upgrade: upgrade/go ## Upgrade dependencies

.PHONY: upgrade/go
upgrade/go: ## Upgrade Go dependencies
	$(GO) get -u ./...
	$(MAKE) tidy

##@ Code Quality

GOLINT_ARGS ?= --verbose --config .golangci.yml

.PHONY: check
check: check/format check/tidy check/vet check/lint test test/scripts ## Check code quality without changing files

.PHONY: check/format
check/format: ## Fail if any Go file needs gofmt
	unformatted="$$(gofmt -l .)"
	if [ -n "$$unformatted" ]; then
		echo "These files need formatting (run \`make format\`):"
		echo "$$unformatted"
		exit 1
	fi

.PHONY: check/tidy
check/tidy: ## Fail if go mod tidy would change go.mod or go.sum
	$(GO) mod tidy -diff || { echo "go.mod/go.sum are not tidy (run \`make tidy\`)"; exit 1; }

.PHONY: check/vet
check/vet: ## Run go vet
	$(GO) vet ./...

.PHONY: check/lint
check/lint: ## Run golangci-lint without fixing
	$(GO) tool golangci-lint run $(GOLINT_ARGS) ./...

.PHONY: check/vuln
check/vuln: export GOTOOLCHAIN = local
check/vuln: ## Fail on reachable known vulnerabilities (needs network, so not part of make check)
	# Scans with the go on PATH rather than go.mod's toolchain, so the result
	# reflects the patched Go that CI installs. Pinning an old go makes every
	# standard library fix since then show up as reachable.
	$(GO) tool govulncheck ./...

.PHONY: format
format: format/go ## Format code

.PHONY: format/go
format/go: ## Format Go code
	$(GO) tool golangci-lint fmt $(GOLINT_ARGS)

##@ Helpers

.PHONY: help
help: ## Display this help
	awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z0-9_/-]+:.*?##/ { printf "  \033[36m%-17s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

env-%: ## Check if env var is defined
	if [ -z "$($*)" ]; then \
		echo "Error: Environment variable '$*' is not set."; \
		exit 1; \
	fi
