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
docs/adr/                architecture decision records
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
