# Contributing

Thanks for helping! Before adding a command, read [ADR 0001](docs/adr/0001-project-scope.md)
to see what fits in k8scli.

## Layout

```text
main.go                  entry point (signal handling only)
cmd/                     cobra commands; App holds the shared state
internal/kubeconfig/     kubeconfig loading, contexts, namespace resolution
internal/output/         table / json / yaml rendering
internal/podstatus/      kubectl-style pod STATUS, READY, RESTARTS
internal/web/            web console: JSON API and the embedded UI (see below)
internal/web/static/     HTML, CSS and JavaScript of the UI, embedded with go:embed
docs/adr/                architecture decision records
docs/web-console.md      user guide of the web console
```

Commands depend on `kubernetes.Interface`, so tests inject
`k8s.io/client-go/kubernetes/fake` (see `cmd/helpers_test.go`) and never need a cluster.

## Development loop

```bash
make test        # unit tests with -race
make ci          # gofmt, go vet, golangci-lint, tests
make vulncheck   # govulncheck
make gosec       # gosec
make package     # GoReleaser snapshot (needs goreleaser)
```

Install `golangci-lint` v2 locally; CI pins the version in `.github/workflows/ci.yml`.

## Working on the web console

The console (`k8scli serve` / `k8scli ui`, see [docs/web-console.md](docs/web-console.md)) is a Go
JSON API in `internal/web/*.go` plus a static UI in `internal/web/static/`. There is no build step and
no npm: the files are embedded in the binary, so **rebuild after every UI edit** (`make build`).

### Run it against a local cluster, safely

Use a throwaway cluster (kind, minikube or Docker Desktop), never a shared one:

```bash
make build
./k8scli ui --no-open          # prints the link; or: go run . ui
```

- Create a namespace for your experiments from the console (*Namespaces > New namespace*) or with
  `kubectl create namespace k8scli-dev`, and delete it when you are done.
- To try the production confirmation without a production cluster, copy your kubeconfig, rename the
  context (`KUBECONFIG=copy kubectl config rename-context docker-desktop docker-desktop-prod`) and
  start the console with `--kubeconfig copy`. Anything with `prod` in the context name is gated.
- `--read-only` is the quickest way to check that a new write action respects it.
- The UI needs no cluster to render its error and empty states: point `--kubeconfig` at a file whose
  server is unreachable.

### Rules for a new write action

A new action that changes the cluster must follow [ADR 0003](docs/adr/0003-guided-write-actions.md).
In short:

1. A fixed, typed request. No YAML, manifests or free-form patches, and nothing that reads Secret values.
2. Call `s.authorize(mutation)` first (`guard.go`): it applies read-only mode and the production
   confirmation. Do not write your own checks.
3. Support `dryRun` by passing `DryRun: metav1.DryRunAll` (`dryRunOption`) to the real call, and answer
   with a `changeResponse`: summary, change lines, and the equivalent `kubectl` command.
4. Validate every input (DNS-1123 names, quantities, upper bounds) and use the narrowest API call.
5. Put the new form behind the shared review-and-apply dialog (`confirmChange` in `static/forms.js`).

Every new action needs tests (reuse `newEnv`, `do` and `decode` from `internal/web/web_test.go`):

- a dry run changes nothing and still validates (the fake clientset ignores `DryRun`, so use a reactor
  that records it, like `trackWrites` in `deploy_test.go`);
- `--read-only` blocks the real change but not the dry run;
- a `prod` context needs `confirm`, and a wrong `confirm` is refused;
- invalid input gives 400;
- the happy path, and "unchanged" when nothing differs.

Changes to `internal/web/auth.go` also need a test for the control they touch (Host, token, cookie,
`Origin`, `Sec-Fetch-Site`, CSRF header) and a security review.

### Rules for the UI code

The server sends a strict Content-Security-Policy (`default-src 'none'`, scripts and styles from
`'self'`), so:

- **No inline code**: no `style="..."`, no `onclick=` and friends, no inline `<script>` or `<style>`.
  Set sizes with classes, SVG attributes or `<progress>`.
- **No external requests**: no CDN, web fonts, analytics or remote images. Everything is in `static/`.
- **Escape everything that came from the cluster** with `esc()` before it goes into `innerHTML`, or
  use `textContent`. Pod names, labels, ConfigMap values and log lines are untrusted.
- **English only**, in the UI and in error messages.
- Use the color tokens in `app.css` (`var(--surface)` and friends) so both themes work, and check a
  phone width (about 390 px) and the dark theme before sending a change.
- Run `node --input-type=module --check < file.js` on every JavaScript file you touch.

## Maintaining the Helm catalog

The Apps (Helm) catalog is Go code: `internal/web/helm_catalog.go` (the first group) and
`internal/web/helm_catalog_more.go` (the rest). Read [ADR 0004](docs/adr/0004-helm-apps.md) first. A
catalog entry is a trust decision, so it is reviewed like code.

An entry has an `ID`, `Name`, `Description` and `Category`, the `Chart` with an exact `Version` and
`AppVersion`, the HTTPS `Repo`, the `archiveURL` and its sha256 `digest`, the default release name and
namespace, `ClusterScoped` when it creates objects outside its namespace, a `Docs` link, and a short list
of typed `Fields` (bool, string, int or select, with limits) that map to chart values. Secret fields are
never returned by the API or shown in a command.

To add or bump an entry:

1. Use an actively maintained chart from the project's own repository, over HTTPS. Take a version that is
   **at least seven days old**, so a bad release has time to be pulled. Read the release notes, the chart's
   `kubeVersion` and any values that changed.
2. Take the digest from the repository's `index.yaml` and set `archiveURL` to the archive itself. Never
   copy a digest from anywhere else: it is the control that makes the pin mean something.
3. Keep the fields few and prove each one: every field must change the rendered chart.
4. Run `scripts/update-helm-catalog.sh`. It runs the network tests (`K8SCLI_HELM_LIVE=1 go test ./internal/web
   -run 'TestHelmCatalogUpstream|TestHelmCatalogFieldsHaveAnEffect'`): the first downloads every pinned
   archive and verifies its digest, the second renders each chart offline with every field changed. It
   also prints the newest eligible version for each entry. It never edits the catalog, so a person reads
   the output and edits the file.
5. Run `go test ./internal/web/`. The regular tests need no network and check ids, HTTPS repositories,
   digest format and field mapping. Preview the app on a disposable cluster before you merge. For a
   cluster-wide chart (CRDs, ClusterRoles) use a throwaway cluster and remove the CRDs afterwards: Helm
   does not delete them.

Charts that are retired or abandoned are not added. Ingress-nginx is retired upstream, and the Bitnami
charts depend on images that are no longer free, so neither is in the catalog. A stale entry is a risk:
when upstream publishes a security fix, bump it. Automating the reminder is tracked in
[issue #18](https://github.com/1985epma/k8scli/issues/18).

Anything that installs a chart from Artifact Hub search follows the rules in the ADR's amendment (verified
or official repositories only, digest shown and sent back, existing settings only, typed chart name). Do not
add a way to type a repository address, an archive address or a values file.

## Working on the disaster-recovery module

The DR module can stop a cluster, so it has the strictest rules in the project: read
[ADR 0005](docs/adr/0005-disaster-recovery.md) before you change it.

- Test it against the lab, never against a real cluster: `scripts/dr-lab.sh up` creates a three-node
  kind cluster (context `kind-k8scli-lab`) with a sample system, and `scripts/dr-lab.sh down` removes it.
  The script never uses or changes your current context. Docker Desktop's single node is not a
  substitute: nodes, databases and shutdown need several nodes.
- Destructive lab tests (drain, node or cluster stop, hibernation) share one lab, so take the lock first:
  `mkdir "${TMPDIR:-/tmp}/k8scli-dr-lab.lock"`, run the test, then `rmdir` it, also on failure.
- Every operation is an idempotent `Action` (`Done` and `Apply`) with a description, an equivalent command
  and a risk class. New adapters need unit tests against fakes (fake clientset, fake dynamic client, a fake
  command runner), including the dry run, the idempotent re-run and the refusal paths.
- Providers run vendor CLIs only through the program allowlist, never a shell, with every parameter
  validated and passed as its own argument. Add injection cases to the tests.
- Anything that powers off, drains or hibernates needs the confirmation rules of the engine; do not add a
  way around them.

## Commits and pull requests

- Use [Conventional Commits](https://www.conventionalcommits.org/) (`feat:`, `fix:`, `docs:`, `ci:`, ...). The release changelog is generated from them; `docs:`, `test:` and `chore:` are left out of it.
- Keep one concern per pull request and add tests for new behaviour.
- Update the README and the command `Example:` text when flags or output change.

## Recommended branch protection for `main`

Configure under *Settings → Branches* (this repository does not manage it for you):

- Require a pull request before merging, with review from code owners
- Require status checks: `Test` (all matrix entries), `Lint`, `Cross-platform build`, `Docker image`, `Windows MSI`, `sast`, `Semgrep OWASP Scan`, `Dependency Review`
- Require branches to be up to date and conversations to be resolved
- Block force pushes and deletions

## Releasing

1. Make sure `main` is green and `CHANGELOG.md` has the notes under *Unreleased*.
2. Tag and push: `git tag v0.2.0 && git push origin v0.2.0`.
3. The `Release` workflow runs CI and SAST, then GoReleaser publishes archives, packages, the container image, SBOMs and the signed checksums; the MSI job attaches the installer afterwards.
4. To redo a failed release, run the `Release` workflow manually and choose the tag in *Use workflow from*.

The workflow needs the `HOMEBREW_TAP_GITHUB_TOKEN` secret (write access to `1985epma/homebrew-k8scli`).
