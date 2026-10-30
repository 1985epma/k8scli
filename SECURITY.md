# Security policy

## Supported versions

Only the latest release receives security fixes.

## Reporting a vulnerability

Please **do not** open a public issue. Report it privately through
[GitHub Security Advisories](https://github.com/1985epma/k8scli/security/advisories/new).

Include the affected version (`k8scli version`), how to reproduce the problem and
its impact. You can expect an acknowledgement within a few days.

## What the project does to stay safe

- `govulncheck`, `gosec` and Semgrep (OWASP Top 10, Go, secrets) run on every pull request and weekly
- Dependabot keeps Go modules, GitHub Actions and Docker base images up to date
- GitHub Actions are pinned by commit SHA
- Releases ship a cosign signature of `checksums.txt`, SBOMs and a build provenance attestation
  (see "Verify a release" in the [README](README.md))

## Scope notes

`k8scli` never sends data anywhere other than the Kubernetes API server selected by your
kubeconfig. Commands that change the cluster (`scale`, `set`) print the context they act on and ask for
confirmation when the context name contains `prod`.
