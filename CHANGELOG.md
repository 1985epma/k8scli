# Changelog

All notable changes are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/). GitHub release notes are generated
from Conventional Commits by GoReleaser.

## [Unreleased]

### Added

- `version` command (and `--version`), with commit and build date injected at build time
- `-o/--output table|wide|json|yaml` on `pods`, `pods-all`, `cluster` and `contexts`
- `pods`: RESTARTS and AGE columns, kubectl-style STATUS (`CrashLoopBackOff`, `Init:0/1`, ...), pagination
- `logs`: `-c/--container`, `--since`, `--timestamps`; warns when a pod has several containers
- `scale` / `set`: `--dry-run`, `--yes`, context banner and confirmation for production-looking contexts
- `--request-timeout` flag and Ctrl-C handling that cancels in-flight requests
- Container image on GHCR, standalone binaries for every OS/arch, Windows MSI attached to releases
- Cosign-signed `checksums.txt`, SPDX SBOMs and build provenance attestations
- CI workflow (build, vet, tests on Linux/macOS/Windows, golangci-lint, ShellCheck, Docker, MSI), Dependabot, CODEOWNERS, SECURITY.md, CONTRIBUTING.md

**Web console** (see [docs/web-console.md](docs/web-console.md))

- `serve` command: a local web console embedded in the binary, with `--addr`, `--read-only` and `--allow-remote`
- `ui` command and the `k8scli-ui` launcher: start the web console and open the browser, using a free port when 8484 is taken (Homebrew, `.deb`/`.rpm`, `make install` and `make install-local` install `k8scli-ui` as a link to `k8scli`; the Windows zip and MSI carry a `k8scli-ui.cmd` shim, and the portable `.exe` needs `k8scli ui`)
- Dashboard (cluster health, namespaces, pods needing attention, top restarts, warning events, CPU and memory with metrics-server), pods, logs, deployments, namespaces, contexts and ConfigMaps pages, light and dark themes
- Guided changes: create a deployment (with an optional Service), scale, update an image, restart, edit plain environment variables, create/edit/remove a HorizontalPodAutoscaler, edit ConfigMap keys, create namespaces and ConfigMaps. Each one has a server-side dry-run preview, the equivalent `kubectl` command, the production confirmation and `--read-only` support
- Setup wizards: Connect a cluster, Install the CLI and Shell completion
- Security: loopback-only listener, session token exchanged for an `HttpOnly`, `SameSite=Strict` cookie, `Host` allow-list, `Sec-Fetch-Site` and `Origin` checks, CSRF header on non-`GET` requests, strict Content-Security-Policy, no third-party requests
- Edits to a deployment retry up to 4 times when a rollout makes the API server report a conflict
- ADR 0002 (the web console and its threat model) and ADR 0003 (guided write actions), and a user guide in `docs/web-console.md`

**Apps (Helm)** (see [docs/web-console.md](docs/web-console.md#apps-helm) and [ADR 0004](docs/adr/0004-helm-apps.md))

- Apps (Helm) page: lists Helm releases and installs, upgrades and uninstalls apps with the Helm library built into the binary (no `helm` executable needed; releases are stored where Helm keeps them, so `helm list` shows the same)
- A catalog of 24 reviewed apps (Podinfo, Metrics Server, Grafana, Prometheus stack, Tempo, Alloy, kube-state-metrics, cert-manager, External Secrets, Kyverno, Trivy Operator, Traefik, ExternalDNS, MetalLB, Argo CD, Argo Rollouts, KEDA, Descheduler, Vertical Pod Autoscaler, CloudNativePG, Velero, Strimzi, Longhorn, Reloader). Each entry pins an HTTPS repository, an exact chart version and the sha256 of the archive; a mismatch is refused. A few typed fields per app, no values file, secret fields never returned
- Search Artifact Hub by name from the console (the server queries `artifacthub.io`; the browser never does) and install a chart that is not in the catalog, only from an official or verified-publisher repository, never a library, deprecated or pre-release chart or one on a deny list of charts that build arbitrary objects. The preview shows the archive digest and the apply must send it back (`digest_changed` otherwise); only up to 20 `path=value` settings that already exist in the chart are accepted; the review lists every object and image and warns about cluster-wide objects, webhooks, CRDs, Secrets, `cluster-admin` bindings and privileged, host-level or unpinned workloads; applying needs the typed chart name on top of the production confirmation
- `--catalog-only` on `serve` and `ui` turns Artifact Hub search and install off
- A real Helm dry run for every preview (server side, with an offline render when an app's CRDs do not exist yet), the equivalent `helm` command, the production confirmation and `--read-only` support
- Upgrade and uninstall only for releases the console installed (label `k8scli.io/managed=true`); releases installed elsewhere are listed but not changed. The uninstall preview warns about CRDs and volumes that Helm keeps
- `scripts/update-helm-catalog.sh` checks every catalog entry against upstream and prints the newest eligible version for review; it never edits the catalog
- ADR 0004 (Helm apps, with an amendment for the bigger catalog and Artifact Hub search)

**Disaster recovery** (see [docs/dr.md](docs/dr.md))

- Disaster-recovery module: a declarative plan (`k8scli-dr.yaml`), an engine that runs it as an ordered runbook (dry run, resumable ledger, audit log, typed confirmation, read-only), and adapters to stop and start apps, databases (CloudNativePG, StatefulSets), nodes and whole clusters (kind, minikube, AKS, EKS node groups, GKE node pools).
- `k8scli dr` commands: `plan validate|show`, `check`, `export`, `diff`, `backups`, `down`, `up`, `resume`, `status`, `runs`, `drill`, `report`, `nodes drain|uncordon` and `cluster stop|start`. Everything that changes something needs `--execute` (a dry run is the default) and shows the confirmation phrase it needs
- Not in this version (tracked as issues, see [docs/dr.md](docs/dr.md#not-in-this-version)): guided Velero backup and restore, failover and failback, evidence pack, CNPG switchover. Without them a drill proves ordered stop/start and timing, not data restoration
- `dr check --fail-on SEVERITY` exits non-zero when findings reach that severity, to use as a CI gate
- RTO measured in drills and RPO estimated or measured with a heartbeat, with reports
- A three-node kind lab for trying it all safely: `scripts/dr-lab.sh up`
- ADR 0005 (the DR module and its safety rules), a user guide in `docs/dr.md` and example plans in `docs/examples/`

### Changed

- Code split into `cmd/` and `internal/` packages and decoupled from the concrete clientset, so commands are unit tested with a fake
- Default namespace is now the one configured in the current kubeconfig context (falls back to `default`)
- `scale` / `set` use the `scale` subresource instead of rewriting the whole Deployment, and `scale` rejects non-numeric or negative replica counts
- `contexts` output is sorted and shows cluster, user and namespace; `use` writes the kubeconfig through client-go's locking
- Table output no longer prints a banner line, so it is safe to pipe; `pods-all` puts NAMESPACE first
- `help` is now the standard cobra help, with usage examples per command
- Go module renamed from `k8s-cli` to `github.com/1985epma/k8scli`
- GitHub Actions pinned by SHA; SAST tools and Semgrep pinned to explicit versions; least-privilege permissions per job
- Release is gated on CI and SAST, fails early when the Homebrew token is missing, and no longer needs QEMU or the manual binary upload step
- `golang.org/x/net` and `golang.org/x/text` bumped to versions with security fixes
- `internal/kubeconfig` gained `RESTConfigFor` (a client config for any context without rewriting the file) and `Paths`
- ADR 0001 is amended by ADR 0002, ADR 0003 and ADR 0004, which add the local web console, its guided write actions and Helm apps to the project scope (ADR 0005 adds disaster recovery)

### Fixed

- `logs <pod> <container>` ignored the container argument and always read the first container
- `contexts` listed contexts in random order
- `scripts/build-msi.ps1`: PowerShell line continuation and the output file name (`$Version_windows` was an undefined variable)
- The MSI now adds `k8scli` to the `PATH`

### Removed

- The stale in-repo Homebrew formula (hard-coded sha256 and version); the tap formula is generated by GoReleaser
