BIN := k8scli
GO ?= go
ARGS ?= help
PREFIX ?= /usr/local
GOBIN_DIR := $(or $(shell $(GO) env GOBIN),$(shell $(GO) env GOPATH)/bin)
SHELL_NAME ?= zsh

MODULE := github.com/1985epma/k8scli
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT ?= $(shell git rev-parse HEAD 2>/dev/null || echo none)
DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w \
	-X $(MODULE)/cmd.version=$(VERSION) \
	-X $(MODULE)/cmd.commit=$(COMMIT) \
	-X $(MODULE)/cmd.date=$(DATE)

.PHONY: help build run install install-local completions package release-snapshot clean fmt fmt-check vet lint test cover tidy vulncheck gosec ci docker msi-help

help:
	@printf "Targets available:\n"
	@printf "  make build              Build the CLI binary (with version info)\n"
	@printf "  make run ARGS=\"help\"   Run the CLI locally with custom args\n"
	@printf "  make test               Run unit tests with the race detector\n"
	@printf "  make cover              Run tests and print coverage\n"
	@printf "  make vet                Run go vet\n"
	@printf "  make lint               Run golangci-lint\n"
	@printf "  make fmt                Format the Go source\n"
	@printf "  make fmt-check          Fail if any file needs formatting\n"
	@printf "  make tidy               Run go mod tidy\n"
	@printf "  make vulncheck          Run govulncheck\n"
	@printf "  make gosec              Run gosec\n"
	@printf "  make ci                 Everything the CI runs locally (fmt-check, vet, lint, test)\n"
	@printf "  make docker             Build the container image from source\n"
	@printf "  make install            Install the binary (and the k8scli-ui launcher) into GOPATH/bin\n"
	@printf "  make completions        Generate shell completion files in dist/completions\n"
	@printf "  make install-local      Install binary and shell completion locally\n"
	@printf "  make package            Build release artifacts with GoReleaser snapshot\n"
	@printf "  make release-snapshot   Alias for package\n"
	@printf "  make msi-help           Show how to build the Windows MSI\n"
	@printf "  make clean              Remove generated binaries and dist/\n"

build:
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) .

run:
	$(GO) run . $(ARGS)

install:
	$(GO) install -trimpath -ldflags "$(LDFLAGS)" .
	ln -sf $(BIN) "$(GOBIN_DIR)/$(BIN)-ui"

install-local:
	./scripts/install.sh --prefix "$(PREFIX)" --shell "$(SHELL_NAME)"

completions: build
	mkdir -p dist/completions
	./$(BIN) completion bash > dist/completions/k8scli.bash
	./$(BIN) completion zsh > dist/completions/_k8scli
	./$(BIN) completion fish > dist/completions/k8scli.fish

package: release-snapshot

release-snapshot:
	./scripts/release.sh snapshot

msi-help:
	@printf "Build the MSI on Windows with PowerShell (after 'make package'):\n"
	@printf "  pwsh -File scripts/build-msi.ps1 -Version 0.1.0\n"

fmt:
	$(GO) fmt ./...

fmt-check:
	@test -z "$$(gofmt -l .)" || { echo "These files need gofmt:"; gofmt -l .; exit 1; }

vet:
	$(GO) vet ./...

lint:
	@command -v golangci-lint >/dev/null 2>&1 || { echo "golangci-lint not found: https://golangci-lint.run/welcome/install/"; exit 1; }
	golangci-lint run ./...

test:
	$(GO) test -race ./...

cover:
	$(GO) test -race -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out

tidy:
	$(GO) mod tidy

vulncheck:
	@command -v govulncheck >/dev/null 2>&1 || { echo "govulncheck not found: go install golang.org/x/vuln/cmd/govulncheck@latest"; exit 1; }
	govulncheck ./...

gosec:
	@command -v gosec >/dev/null 2>&1 || { echo "gosec not found: go install github.com/securego/gosec/v2/cmd/gosec@latest"; exit 1; }
	gosec ./...

ci: fmt-check vet lint test

docker:
	docker build \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		--build-arg DATE=$(DATE) \
		--tag $(BIN):$(VERSION) .

clean:
	rm -rf $(BIN) dist coverage.out
