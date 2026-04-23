# K8sCLI

Kubernetes CLI helper tool for managing clusters, pods, deployments, and more.

See [install.md](install.md) for a full installation guide.

Official repository: https://github.com/1985epma/k8scli

Homebrew tap scaffold for publishing: [homebrew-k8scli/README.md](homebrew-k8scli/README.md)

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

## Shell completion

Generate completion output directly from the CLI:

```bash
# Preview zsh completion
k8scli completion zsh

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

## Installation

### macOS

```bash
brew install 1985epma/k8scli/k8scli
```

You can also install from source locally if you prefer:

```bash
make install-local
```

If you want to test the Homebrew formula locally before publishing the tap:

```bash
./scripts/test-homebrew-tap.sh
```

### Linux

```bash
# Debian/Ubuntu
sudo dpkg -i k8scli_*.deb

# RHEL/Fedora
sudo rpm -i k8scli_*.rpm
```

Or download from the [releases](https://github.com/1985epma/k8scli/releases).

### Windows

Download the MSI installer or portable EXE from the [releases](https://github.com/1985epma/k8scli/releases).

## Usage

```bash
# List contexts
k8scli contexts

# Switch context
k8scli use docker-desktop

# List pods
k8scli pods

# List pods in all namespaces
k8scli pods-all

# Get logs
k8scli logs my-pod -f

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