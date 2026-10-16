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

export GOTOOLCHAIN=go1.25.0+auto
export GOPROXY ?= https://proxy.golang.org,direct
GO := $(shell which go)

MAIN ?= ./cmd/schmeide/main.go
EXE ?= ./build/schmeide

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
	$(GO) build -o $(EXE) $(MAIN)

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
	$(GO) test ./...

.PHONY: watch
watch: ## Watch for changes and rebuild
	$(GO) tool air \
		--build.cmd "$(MAKE) build" \
		--build.entrypoint "$(EXE)" \
		--build.exclude_dir ".github,build,docs"

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
check: check/go ## Check code quality

.PHONY: check/go
check/go: ## Check Go code quality
	$(GO) tool golangci-lint run --fix $(GOLINT_ARGS) ./...

.PHONY: format
format: format/go ## Format code

.PHONY: format/go
format/go: ## Format Go code
	$(GO) tool golangci-lint fmt $(GOLINT_ARGS)

##@ Helpers

.PHONY: help
help: ## Display this help
	awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

env-%: ## Check if env var is defined
	if [ -z "$($*)" ]; then \
		echo "Error: Environment variable '$*' is not set."; \
		exit 1; \
	fi
