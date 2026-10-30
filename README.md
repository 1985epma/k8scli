# K8sCLI

[![CI](https://github.com/1985epma/k8scli/actions/workflows/ci.yml/badge.svg)](https://github.com/1985epma/k8scli/actions/workflows/ci.yml)
[![Release](https://github.com/1985epma/k8scli/actions/workflows/release.yml/badge.svg)](https://github.com/1985epma/k8scli/actions/workflows/release.yml)
[![SAST](https://github.com/1985epma/k8scli/actions/workflows/sast.yml/badge.svg)](https://github.com/1985epma/k8scli/actions/workflows/sast.yml)
[![OWASP Top 10](https://github.com/1985epma/k8scli/actions/workflows/owasp-top-10.yml/badge.svg)](https://github.com/1985epma/k8scli/actions/workflows/owasp-top-10.yml)

Kubernetes CLI helper tool for managing clusters, pods, deployments, and more.

See [install.md](install.md) for a full installation guide.

Official repository: https://github.com/1985epma/k8scli

Homebrew tap notes: [homebrew-k8scli/README.md](homebrew-k8scli/README.md)

Windows MSI packaging: [windows/msi/README.md](windows/msi/README.md)

## Installation

### macOS

```bash
brew install 1985epma/k8scli/k8scli
```

### Linux

```bash
VERSION=0.1.1   # replace with the release you want: https://github.com/1985epma/k8scli/releases
ARCH=amd64      # or arm64
BASE="https://github.com/1985epma/k8scli/releases/download/v${VERSION}"

curl -fsSLO "${BASE}/k8scli_${VERSION}_linux_${ARCH}.deb"
curl -fsSLO "${BASE}/checksums.txt"
sha256sum --ignore-missing -c checksums.txt

# Debian/Ubuntu
sudo dpkg -i "k8scli_${VERSION}_linux_${ARCH}.deb"

# RHEL/Fedora: download the .rpm the same way, then
# sudo rpm -i "k8scli_${VERSION}_linux_${ARCH}.rpm"
```

### Windows

Download the MSI installer (it adds `k8scli` to the `PATH`) or the portable
`k8scli_<version>_windows_amd64.exe` from the [releases](https://github.com/1985epma/k8scli/releases).

### Docker

```bash
docker run --rm -v ~/.kube:/home/nonroot/.kube:ro ghcr.io/1985epma/k8scli:latest pods
```

### Verify a release

Releases published with the supply-chain pipeline ship a keyless
[cosign](https://github.com/sigstore/cosign) signature of `checksums.txt`, SPDX SBOMs and
a build provenance attestation:

```bash
cosign verify-blob \
  --bundle checksums.txt.sigstore.json \
  --certificate-identity-regexp 'https://github.com/1985epma/k8scli/\.github/workflows/release\.yml@refs/tags/.*' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  checksums.txt

gh attestation verify k8scli_${VERSION}_linux_${ARCH}.deb --repo 1985epma/k8scli
```

## Security

- `CI` workflow: build, `go vet`, tests (Linux, macOS, Windows), `golangci-lint`, ShellCheck, a GoReleaser snapshot build, the Docker image and the Windows MSI
- `SAST` workflow runs `govulncheck` and `gosec` on `main`, on pull requests and weekly
- `OWASP Top 10` workflow runs Semgrep rules for OWASP, Go, and secrets, plus dependency review on pull requests
- Third-party GitHub Actions are pinned by commit SHA and kept up to date by Dependabot
- See [SECURITY.md](SECURITY.md) to report a vulnerability

## Release

- Pushing a `v*` tag runs `CI` and `SAST` first; only if both pass does GoReleaser publish
- Assets: `tar.gz`/`zip` archives, standalone binaries, `.deb`/`.rpm`, a Windows MSI, `checksums.txt` (signed with cosign), SBOMs and a container image on GHCR
- Homebrew tap: [1985epma/homebrew-k8scli](https://github.com/1985epma/homebrew-k8scli)
- Official releases: [1985epma/k8scli releases](https://github.com/1985epma/k8scli/releases)

## Local development

```bash
# Show available automation targets
make help

# Build the local binary (version info is injected via ldflags)
make build

# Run the CLI locally
make run ARGS="help"
make run ARGS="contexts"

# Everything the CI checks: gofmt, go vet, golangci-lint, tests with -race
make ci

# Install into GOPATH/bin
make install

# Generate shell completions
make completions

# Install the binary and zsh completion locally
make install-local

# Build snapshot release artifacts with GoReleaser
make package

# Build the container image from source
make docker
```

Cluster commands such as `pods`, `pods-all`, `logs`, `set`, `scale`, and `cluster`
require a valid kubeconfig. Local commands such as `help`, `contexts`, `use` and `version`
work directly with the kubeconfig file. See [CONTRIBUTING.md](CONTRIBUTING.md) for the
contribution workflow and [docs/adr](docs/adr) for design decisions.

## Commands

Local commands:

- `help` - Show the built-in help screen
- `contexts` - List contexts from the kubeconfig file
- `use <context>` - Switch the current kubeconfig context
- `completion [bash|zsh|fish|powershell]` - Generate shell completion scripts
- `version` - Show the version, commit and build date

Cluster commands:

- `pods` - List pods in the selected namespace (status, restarts, age; `-o wide|json|yaml`)
- `pods-all` - List pods across all namespaces
- `logs <pod> [container]` - Read pod logs (`-c`, `--since`, `--timestamps`)
- `set <deployment> <replicas>` - Quick scale to `2`, `4`, `6`, or `8` (same flags as `scale`)
- `scale <deployment> <replicas>` - Scale to any replica count (`--dry-run`, `--yes`)
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

# Test the Homebrew formula from the local workspace
./scripts/test-homebrew-tap.sh
```

## Usage

```bash
# Show built-in help
k8scli help

# Show the version
k8scli version

# List contexts
k8scli contexts

# Switch context
k8scli use docker-desktop

# Generate completion for zsh
k8scli completion zsh > ~/.zsh/completion/_k8scli

# List pods (uses the current context's namespace unless -n is given)
k8scli pods

# More columns, or machine-readable output
k8scli pods -o wide
k8scli pods -o json

# List pods in all namespaces
k8scli pods-all

# Get logs
k8scli logs my-pod -f

# Pick a container, only the last 10 minutes, with timestamps
k8scli logs my-pod -c sidecar --since 10m --timestamps

# Get previous logs and last 50 lines
k8scli logs my-pod -p -l 50

# Quick scale (2, 4, 6, or 8 pods)
k8scli set myapp 4

# Scale to custom number, previewing first
k8scli scale myapp 10 --dry-run
k8scli scale myapp 10

# Show cluster info
k8scli cluster
```

Before scaling, `k8scli` prints the context and namespace it is about to change.
When the context name contains `prod`, it asks for confirmation unless `--yes` is given.

## Flags

- `-n, --namespace` - Namespace (default: the current context's namespace, or `default`)
- `--kubeconfig` - Path to kubeconfig file (default: `$KUBECONFIG` or `~/.kube/config`)
- `--request-timeout` - Timeout for requests to the cluster (default: `30s`, `0` disables it)
- `-o, --output` - `table`, `wide`, `json` or `yaml` (on `pods`, `pods-all`, `cluster`, `contexts`)

### Logs flags

- `-f, --follow` - Stream logs continuously
- `-p, --previous` - Show logs from the previous container instance
- `-l, --lines` - Limit the number of log lines returned (negative shows everything)
- `-c, --container` - Container name (default: the first container)
- `--since` - Only logs newer than a relative duration such as `5s`, `2m` or `3h`
- `--timestamps` - Prefix each line with its timestamp

### Scale flags (`scale` and `set`)

- `--dry-run` - Show what would change without scaling
- `-y, --yes` - Skip the confirmation asked for production-looking contexts
