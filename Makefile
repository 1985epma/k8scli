BIN := k8scli
GO ?= go
ARGS ?= help
PREFIX ?= /usr/local
SHELL_NAME ?= zsh

.PHONY: help build run install install-local completions package release-snapshot clean fmt msi-help

help:
	@printf "Targets available:\n"
	@printf "  make build              Build the CLI binary\n"
	@printf "  make run ARGS=\"help\"   Run the CLI locally with custom args\n"
	@printf "  make install            Install the binary into GOPATH/bin\n"
	@printf "  make completions        Generate shell completion files in dist/completions\n"
	@printf "  make install-local      Install binary and shell completion locally\n"
	@printf "  make package            Build release artifacts with GoReleaser snapshot\n"
	@printf "  make release-snapshot   Alias for package\n"
	@printf "  make msi-help           Show how to build the Windows MSI\n"
	@printf "  make fmt                Format the Go source\n"
	@printf "  make clean              Remove the generated binary\n"

build:
	$(GO) build -o $(BIN) .

run:
	$(GO) run . $(ARGS)

install:
	$(GO) install .

install-local:
	./scripts/install.sh --prefix "$(PREFIX)" --shell "$(SHELL_NAME)"

completions:
	mkdir -p dist/completions
	$(GO) run . completion bash > dist/completions/k8scli.bash
	$(GO) run . completion zsh > dist/completions/_k8scli
	$(GO) run . completion fish > dist/completions/k8scli.fish

package: release-snapshot

release-snapshot:
	./scripts/release.sh snapshot

msi-help:
	@printf "Build the MSI on Windows with PowerShell:\n"
	@printf "  pwsh -File scripts/build-msi.ps1 -Version 0.1.0\n"

fmt:
	$(GO) fmt ./...

clean:
	rm -f $(BIN)