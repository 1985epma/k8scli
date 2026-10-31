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
- `serve` - Start the local [web console](#web-console)
- `ui` - Start the web console and open it in your browser (also installed as `k8scli-ui`)
- `dr` - [Disaster recovery](#disaster-recovery): check, export, stop, start and rehearse (`--execute` to change anything)

Cluster commands:

- `pods` - List pods in the selected namespace (status, restarts, age; `-o wide|json|yaml`)
- `pods-all` - List pods across all namespaces
- `logs <pod> [container]` - Read pod logs (`-c`, `--since`, `--timestamps`)
- `set <deployment> <replicas>` - Quick scale to `2`, `4`, `6`, or `8` (same flags as `scale`)
- `scale <deployment> <replicas>` - Scale to any replica count (`--dry-run`, `--yes`)
- `cluster` - Show cluster version, nodes, and namespaces

## Web console

`k8scli serve` starts a web console on your machine. It reads the same kubeconfig as the CLI,
so there is nothing to configure: no database, no account and no cluster-side install. The UI is
embedded in the binary and is English only. A page-by-page guide, with troubleshooting, is in
[docs/web-console.md](docs/web-console.md).

```bash
k8scli ui                         # start the console and open your browser (also: k8scli-ui)
k8scli serve                      # http://127.0.0.1:8484, prints the link only
k8scli serve --read-only          # nothing can change the cluster or the kubeconfig
k8scli serve --addr 127.0.0.1:9000
```

It prints a link with a session token. Open that link; the token is exchanged for a cookie
and removed from the address bar. Press Ctrl+C to stop.

| Flag | Default | Meaning |
|------|---------|---------|
| `--addr` | `127.0.0.1:8484` | Address to listen on (`host:port`). With `k8scli ui`, a free port is used when the default one is taken; an address you pass is never replaced |
| `--read-only` | off | Disable every real change; previews (dry runs) still work |
| `--allow-remote` | off | Allow a non-loopback address (see the security notes) |
| `--no-open` | off | `k8scli ui` only: print the link without opening the browser |
| `--catalog-only` | off | Install Helm apps only from the reviewed catalog: no Artifact Hub search or install |
| `--dr-dir` | `~/.k8scli/dr` | Where the Resilience page keeps plans, run ledgers, the audit log and reports (see [docs/dr.md](docs/dr.md)) |
| `--allow-dr-runs` | off | Allow real DR runs, drills and resume from the console. Dry runs, checks, exports and reports are always on (see [docs/dr.md](docs/dr.md)) |
| `--allow-dr-power` | off | Also allow DR runs that power machines or the cluster off and on, for plans that are not marked `safety.labOnly` (see [docs/dr.md](docs/dr.md)) |
| `--kubeconfig` | `$KUBECONFIG` or `~/.kube/config` | Same flag as the CLI |

`k8scli-ui` is the same binary under another name (a link to `k8scli`, installed by Homebrew, the
`.deb`/`.rpm` packages, `make install` and `make install-local`) and behaves like `k8scli ui`. On
Windows, the zip and the MSI carry a `k8scli-ui.cmd` shim that runs `k8scli.exe ui`; the portable
`.exe` does not, so run `k8scli ui` there.

### What it does

**See**

- **Dashboard**: the home page. A one-line answer to "is my cluster OK?", nodes, pods that need attention, pods with the most restarts, recent warning events, every namespace at a glance, and CPU and memory of the busiest pods when metrics-server is installed.
- **Pods**: status, readiness, restarts and age per namespace (or all of them), a "Needs attention" filter, optional IP and node columns, search, and automatic refresh every 10 seconds (paused while a menu, dialog or form is in use).
- **Logs**: a side drawer per pod with container, line count, `--since`, timestamps, previous container and follow, mirroring `k8scli logs`.
- **Autoscaling** (HorizontalPodAutoscalers), **Namespaces**, **Contexts** and **Configuration** (ConfigMaps) pages.
- **Apps (Helm)**: installed releases and a catalog of apps to install, see [Apps (Helm)](#apps-helm) below.
- **Resilience**: the disaster-recovery page (readiness score, backups, plans, runs, drills and reports). It is read-only unless you start the console with `--allow-dr-runs`. See [Disaster recovery](#disaster-recovery) and [docs/dr.md](docs/dr.md).

**Change**, with guided forms

| Action | Where |
|--------|-------|
| Deploy something new: image, replicas, port, environment, resources, optional Service | Deployments > New deployment |
| Increase or decrease replicas (stepper or 2/4/6/8) | Deployments > Scale |
| Create, edit or remove an autoscaler (HPA on CPU and memory) | Autoscaling, or Deployments > Autoscale |
| Roll out a new image, restart, edit plain environment variables | Deployments > row menu |
| Add, change or remove a ConfigMap key, create a ConfigMap | Configuration |
| Create a namespace | Namespaces > New namespace |

Every change goes through the same **Review changes** step: the API server validates it as a dry
run, you see what would change and the equivalent `kubectl` command, and only then you apply it.
In a context whose name contains `prod` you must type the context name first. `--read-only`
keeps the preview and blocks the apply. Secrets are never listed or read.

- **Contexts**: "Use here" changes only the console; "Make it the CLI default" writes the kubeconfig, like `k8scli use`.
- **Setup wizards**: *Connect a cluster* (Docker Desktop, minikube, kind, EKS, GKE, AKS or any kubeconfig, with a connection check that explains common failures), *Install the CLI* and *Shell completion*.
- Light and dark themes (follows your system until you choose), and a CLI or `kubectl` equivalent shown under every table and dialog.

### Apps (Helm)

The **Apps (Helm)** page lists the Helm releases of the cluster and installs apps in a few clicks. It uses
the Helm library built into k8scli, so no `helm` executable is needed, and the releases are stored where
Helm keeps them (Secrets in the release namespace), so `helm list` shows the same ones. Every install goes
through the same **Review changes** step as the other changes: a real Helm dry run lists every object that
would be created, with warnings for cluster-wide objects, Secrets and privileged pods, before **Apply**.

**The catalog** has 24 reviewed apps. Each entry pins the chart repository (HTTPS), an exact chart version
and the sha256 of the chart archive; an archive that does not match is refused. You fill in a few fields
(name, namespace, replicas, service type...), never a values file.

| Category | Apps |
|----------|------|
| Demo | Podinfo |
| Observability | Metrics Server, Grafana, Prometheus stack, Grafana Tempo, Grafana Alloy, kube-state-metrics |
| Security | cert-manager, External Secrets, Kyverno, Trivy Operator |
| Networking | Traefik, ExternalDNS, MetalLB |
| Delivery | Argo CD, Argo Rollouts |
| Autoscaling | KEDA, Descheduler, Vertical Pod Autoscaler |
| Databases | CloudNativePG |
| Backup | Velero |
| Messaging | Strimzi Kafka Operator |
| Storage | Longhorn |
| Configuration | Reloader |

**Search Artifact Hub** finds charts that are not in the catalog. The browser never talks to Artifact Hub:
the server does, on the fixed host `artifacthub.io` over HTTPS. A search result can be installed only when:

- the chart is a Helm application chart (not a library), not deprecated and not a pre-release;
- its repository is **official or has a verified publisher** (the badge proves who controls the repository,
  not that the chart is safe), and its archive is served over HTTPS;
- it is not one of the charts that build arbitrary objects out of values (`raw`, `app-template`, `common`...).

Anything else is listed with the reason it cannot be installed here. For an installable result:

- The server asks Artifact Hub for the package again by id and downloads the archive itself. It never uses a
  repository or archive address sent by the browser.
- The preview shows the **sha256 of the archive**. **Apply** sends that digest back, and if the archive is not
  the one you reviewed it is refused (`digest_changed`).
- The only settings are up to 20 `path=value` pairs. Each path must already exist in the chart's default
  values, with a simple value of the same type.
- The preview lists every object and every image, and warns about cluster-wide objects, webhooks, CRDs, Secrets,
  `cluster-admin` bindings, privileged or host-level pods (`privileged`, `hostNetwork`, `hostPID`, `hostPath`) and
  images that are not pinned (`:latest`).
- **Apply** needs you to **type the chart name**, on top of the production confirmation (the context name).

Releases installed from the console carry the label `k8scli.io/managed=true`. **Upgrade** and **Uninstall**
work only on those; releases installed with the Helm CLI are listed as "Elsewhere" and cannot be changed here.
`--read-only` keeps the preview and blocks the apply, and `--catalog-only` removes Artifact Hub search and
install, leaving the catalog. Details, limits and troubleshooting are in
[docs/web-console.md](docs/web-console.md#apps-helm) and [ADR 0004](docs/adr/0004-helm-apps.md).

### Security model

The console can change your cluster, so it is locked down by default:

- It listens on loopback only. Any other address is refused unless you pass `--allow-remote`.
- Every request needs the session token that `serve` or `ui` prints. The link sets an `HttpOnly`, `SameSite=Strict` cookie (`Secure` only when TLS is in front), and scripts may send the token as a bearer token instead.
- The `Host` header must be the listen address, `localhost`, `127.0.0.1` or `[::1]` on the same port, which blocks DNS rebinding.
- Under `/api/`, requests that the browser labels `Sec-Fetch-Site: cross-site` or `same-site` are refused. `same-site` matters: a page served from another port of `localhost` is "same site" and would otherwise be able to send the cookie. Any `Origin` that is not the console's own is refused on every method, and requests other than `GET` need the `X-K8scli-Request` header.
- A strict Content-Security-Policy (`default-src 'none'`, scripts and styles from the console only, no inline code) and no third-party requests: no CDN, no web fonts, no analytics.
- Each request carries the context shown in the UI, so the console never changes the terminal's current context. Only "Make it the CLI default" writes the kubeconfig, and `--read-only` disables it.
- The changes it can make are the guided forms listed above, Helm install, upgrade and uninstall of the catalog or of an Artifact Hub chart you reviewed (see [ADR 0004](docs/adr/0004-helm-apps.md)), setting the default context, and, only with `--allow-dr-runs`, DR runs (see [docs/dr.md](docs/dr.md)). Each is built from a fixed, typed request (see [ADR 0003](docs/adr/0003-guided-write-actions.md)). There is no endpoint that accepts YAML or an arbitrary patch, and no exec, no Secret access and no delete of namespaces or workloads. Uninstalling a Helm release removes only what that release created, and only for releases the console installed.
- Outbound requests: the console talks to your cluster and, for Helm, to chart repositories and `artifacthub.io`. The hosts are fixed or come from Artifact Hub's own record, never from the browser. `--catalog-only` stops the Artifact Hub requests.
- Edits to an existing deployment (image, environment) use the version of the object that was read. If the API server reports a conflict, which is common while pods roll out, the console reads the object again and applies the same edit, up to 4 retries, before it reports a 409.

Known limit: browsers do not isolate cookies by port, so another local web app on `localhost` receives
the console's cookie on its own requests. The checks above stop a browser from using it against the
console's API, but a non-browser process on your machine that gets hold of the token can. That
process could also read your kubeconfig directly, so the console adds no exposure beyond it.

With `--allow-remote` the Host check is skipped (the other checks stay) and anyone holding the link can use your kubeconfig
credentials. Put it behind something you trust, and prefer `--read-only`.

### Docker

```bash
docker run --rm -p 127.0.0.1:8484:8484 -v ~/.kube:/home/nonroot/.kube:ro \
  ghcr.io/1985epma/k8scli:latest serve --addr 0.0.0.0:8484 --allow-remote --read-only
```

Publish the port on `127.0.0.1` only. The token is in the container logs. The image has no `aws`,
`gcloud` or `kubelogin`, so kubeconfigs that rely on those credential plugins do not work in it.
The kubeconfig is mounted read-only, so use `--read-only` to avoid errors when setting a default.

## Disaster recovery

The DR module stops and starts applications, databases, nodes and whole clusters in a safe order,
and measures how long recovery takes (RTO) and how much data would be lost (RPO). You describe the
system once in a plan file (`k8scli-dr.yaml`): the apps and databases, their tiers, the health checks
and the provider that powers nodes. The engine runs it as an ordered runbook with a dry run first, a
resumable record, an audit log and a typed confirmation for production.

```bash
scripts/dr-lab.sh up      # a three-node kind lab with a sample system, to try everything safely
```

k8scli is a client without a resident process: it does not take backups, switch traffic or fail over
by itself. It checks, exports, stops, starts and rehearses.

```bash
k8scli dr plan validate k8scli-dr.yaml          # check a plan file
k8scli dr check -n shop                         # readiness score and what to fix (read-only)
k8scli dr export -n shop                        # clean manifests, Secrets never written
k8scli dr down --plan k8scli-dr.yaml            # dry run: every step, risk and equivalent command
k8scli dr down --plan k8scli-dr.yaml --execute  # run it (a typed phrase in production)
k8scli dr up --plan k8scli-dr.yaml --execute    # start it again, tier by tier, with health gates
k8scli dr drill --plan lab.yaml --execute       # stop, start and measure RTO and RPO
k8scli dr report                                # the latest drill report
```

Anything that changes something needs `--execute`; powering machines off needs `stop <context>` typed.
A drill proves ordered stop/start and timing; it does not restore data from a backup (guided backup and restore are not in this version, see the [user guide](docs/dr.md#not-in-this-version)).
See the [user guide](docs/dr.md), the example plans in [docs/examples](docs/examples) and
[ADR 0005](docs/adr/0005-disaster-recovery.md).

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

# Open the web console
k8scli serve
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
