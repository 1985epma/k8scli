# K8sCLI

[![Release](https://github.com/1985epma/k8scli/actions/workflows/release.yml/badge.svg)](https://github.com/1985epma/k8scli/actions/workflows/release.yml)
[![SAST](https://github.com/1985epma/k8scli/actions/workflows/sast.yml/badge.svg)](https://github.com/1985epma/k8scli/actions/workflows/sast.yml)
[![OWASP Top 10](https://github.com/1985epma/k8scli/actions/workflows/owasp-top-10.yml/badge.svg)](https://github.com/1985epma/k8scli/actions/workflows/owasp-top-10.yml)

Kubernetes CLI helper tool for managing clusters, pods, deployments, and more.

See [install.md](install.md) for a full installation guide.

Official repository: https://github.com/1985epma/k8scli

Homebrew tap scaffold for publishing: [homebrew-k8scli/README.md](homebrew-k8scli/README.md)

Windows MSI packaging scaffold: [windows/msi/README.md](windows/msi/README.md)

## Installation

### macOS

```bash
brew install 1985epma/k8scli/k8scli
```

### Linux

```bash
# Debian/Ubuntu
sudo dpkg -i k8scli_*.deb

# RHEL/Fedora
sudo rpm -i k8scli_*.rpm
```

### Windows

Download the MSI installer or portable EXE from the [releases](https://github.com/1985epma/k8scli/releases).

## Security

- `SAST` workflow runs `go test`, `govulncheck`, and `gosec`
- `OWASP Top 10` workflow runs Semgrep rules for OWASP, Go, and secrets
- Security workflows run on `main`, on pull requests, and on a weekly schedule where configured

## Release

- Release assets are published from Git tags via GitHub Actions and GoReleaser
- Homebrew tap: [1985epma/homebrew-k8scli](https://github.com/1985epma/homebrew-k8scli)
- Official releases: [1985epma/k8scli releases](https://github.com/1985epma/k8scli/releases)

## Local development

```bash
# Show available automation targets
make help

# Build the local binary
make build

# Run the CLI locally
make run ARGS="help"
make run ARGS="contexts"

# Install into GOPATH/bin
make install

# Generate shell completions
make completions

# Install the binary and zsh completion locally
make install-local

# Build snapshot release artifacts with GoReleaser
make package
```

Cluster commands such as `pods`, `pods-all`, `logs`, `set`, `scale`, and `cluster`
require a valid kubeconfig. Local commands such as `help`, `contexts`, and `use`
work directly with the kubeconfig file.

## Commands

Local commands:

- `help` - Show the built-in help screen
- `contexts` - List contexts from the kubeconfig file
- `use <context>` - Switch the current kubeconfig context
- `completion [bash|zsh|fish|powershell]` - Generate shell completion scripts

Cluster commands:

- `pods` - List pods in the selected namespace
- `pods-all` - List pods across all namespaces
- `logs <pod> [container]` - Read pod logs
- `set <deployment> <replicas>` - Quick scale to `2`, `4`, `6`, or `8`
- `scale <deployment> <replicas>` - Scale to any replica count
- `cluster` - Show cluster version, nodes, and namespaces

## Shell completion

Generate completion output directly from the CLI:

```bash
# Preview bash completion
k8scli completion bash

# Preview zsh completion
k8scli completion zsh

# Preview fish completion
k8scli completion fish

# Preview PowerShell completion
k8scli completion powershell

# Install zsh completion manually
mkdir -p ~/.zsh/completion
k8scli completion zsh > ~/.zsh/completion/_k8scli
```

If you use the local installer script, it also installs the generated completion file.

## Packaging and local install

```bash
# Install binary + zsh completion under /usr/local
make install-local

# Use a custom prefix and shell
make install-local PREFIX="$HOME/.local" SHELL_NAME=zsh

# Create snapshot release artifacts under dist/
make package
```

## Install details

You can also install from source locally if you prefer:

```bash
make install-local
```

If you want to test the Homebrew formula locally before publishing the tap:

```bash
./scripts/test-homebrew-tap.sh
```

Release downloads are published at [1985epma/k8scli releases](https://github.com/1985epma/k8scli/releases).

## Usage

```bash
# Show built-in help
k8scli help

# List contexts
k8scli contexts

# Switch context
k8scli use docker-desktop

# Generate completion for zsh
k8scli completion zsh > ~/.zsh/completion/_k8scli

# List pods
k8scli pods

# List pods in all namespaces
k8scli pods-all

# Get logs
k8scli logs my-pod -f

# Get previous logs and last 50 lines
k8scli logs my-pod -p -l 50

# Quick scale (2, 4, 6, or 8 pods)
k8scli set myapp 4

# Scale to custom number
k8scli scale myapp 10

# Show cluster info
k8scli cluster
```

## Flags

- `-n, --namespace` - Namespace (default: default)
- `--kubeconfig` - Path to kubeconfig file

### Logs flags

- `-f, --follow` - Stream logs continuously
- `-p, --previous` - Show logs from the previous container instance
- `-l, --lines` - Limit the number of log lines returned