.PHONY: all build install clean test fmt lint build-all release test-install test-azure-setup vulncheck run deps setup-hooks check

BINARY_NAME=stradl
# Release tags are vX.Y.Z; binaries carry the bare semver.
VERSION?=$(patsubst v%,%,$(shell git describe --tags --dirty 2>/dev/null || echo 0.0.0-dev))
# GitHub owner/name that `stradl update` checks. Empty keeps the default
# compiled into cmd/claude-sync; release builds always set it.
RELEASE_REPO?=
BUILD_DIR=bin
DIST_DIR=dist
PREFIX?=$(HOME)/.local
GO=go

# Build flags
LDFLAGS=-ldflags "-s -w -X main.version=$(VERSION)$(if $(RELEASE_REPO), -X main.releaseRepo=$(RELEASE_REPO))"

# The macOS menu bar needs cgo; Linux builds are pure Go, like the release.
ifeq ($(shell uname -s),Darwin)
CGO_ENABLED?=1
else
CGO_ENABLED?=0
endif

# Default target
all: build

# Build the binary for the host
build:
	CGO_ENABLED=$(CGO_ENABLED) $(GO) build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME) ./cmd/claude-sync

# Install the host build to $(PREFIX)/bin (default ~/.local/bin, like install.sh)
install: build
	mkdir -p $(PREFIX)/bin
	install -m 755 $(BUILD_DIR)/$(BINARY_NAME) $(PREFIX)/bin/$(BINARY_NAME)

# Clean build artifacts
clean:
	rm -rf $(BUILD_DIR) $(DIST_DIR)

# Run tests
test:
	$(GO) test -v ./...

# Format code
fmt:
	$(GO) fmt ./...

# Lint code (requires golangci-lint and shellcheck)
lint:
	golangci-lint run
	shellcheck install.sh scripts/*.sh

# Report known vulnerabilities the code can reach (CI runs the same check)
vulncheck:
	$(GO) run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...

# Build the release tarballs + checksums.txt into ./dist (macOS only; see
# scripts/build-release.sh). CI runs the same script on tag pushes.
build-all: release

release:
	scripts/build-release.sh "$(VERSION)" "$(or $(RELEASE_REPO),MattCampana/stradl)" $(DIST_DIR)

# End-to-end test of install.sh against locally served release assets
test-install:
	scripts/test-install.sh

# Test scripts/azure-setup.sh against a fake Azure CLI
test-azure-setup:
	scripts/test-azure-setup.sh

# Development: build and run
run: build
	./$(BUILD_DIR)/$(BINARY_NAME)

# Download dependencies
deps:
	$(GO) mod download
	$(GO) mod tidy

# Setup git hooks
setup-hooks:
	git config core.hooksPath .githooks
	@echo "Git hooks installed. Pre-commit will run tests and lint."

# Run all checks (same as pre-commit)
check:
	@echo "Checking formatting..."
	@test -z "$$(gofmt -l .)" || (echo "Run 'make fmt' to fix formatting" && exit 1)
	@echo "Running go vet..."
	$(GO) vet ./...
	@echo "Running tests..."
	$(GO) test ./... -short
	@echo "All checks passed!"
