# ADR 0002: Local web console

- Status: accepted (amended by ADR 0003, ADR 0004 and ADR 0005)
- Date: 2026-10-30
- Amends: [ADR 0001](0001-project-scope.md)

## Context

People asked for a browser interface: a place to see pods and their problems at a glance, read logs
without remembering flags, scale safely, and get guided setup (credentials, installing the CLI,
shell completion). Setting up a cluster connection is the part newcomers get stuck on, and a
terminal cannot walk them through it with a live connection check.

ADR 0001 keeps `k8scli` a convenience layer over frequent tasks. A UI must not become a second
product, and it must not make cluster changes easier than the CLI does.

## Decision

Add `k8scli serve`, a web console that runs on the user's machine, and `k8scli ui` (also installed as
`k8scli-ui`), which does the same and opens the browser, picking a free port when the default one is taken.

- **Embedded static UI.** HTML, CSS and JavaScript live in the binary (`go:embed`) and are served by
  the same process. No build step, no framework, no CDN, no web fonts: it works offline and makes no
  third-party requests.
- **JSON API over client-go.** `internal/web` exposes a small API (state, overview, pods,
  deployments, logs, connection check, scale, set default context; ADR 0003, 0004 and 0005 add the
  guided changes, Helm apps and disaster recovery) built on the same
  `internal/kubeconfig` and `internal/podstatus` packages as the CLI and on `kubernetes.Interface`,
  so it is tested with the fake clientset.
- **The context travels with every request.** The UI sends the context it is showing; the server
  validates it against the kubeconfig and builds a client for it. The console never switches the
  terminal's current context.
- **Few mutations.** This ADR allows scaling a deployment (through the `scale` subresource, with
  `dryRun` and a production confirmation) and an explicit "Make it the CLI default" that writes
  `current-context` like `k8scli use`. Everything else stays out: no exec, delete, apply or edit.
  ADR 0003 later adds a short list of guided actions, ADR 0004 installs Helm apps from a reviewed
  catalog or a reviewed Artifact Hub chart, and ADR 0005 adds DR runs behind `--allow-dr-runs`, all
  still without free-form apply or edit.
- **Read-only mode.** `--read-only` rejects every real change and writing the kubeconfig. Dry runs
  still work.
- **Setup wizards** are static guidance plus one server call: a connection check that returns the
  failure and a hint (missing credential plugin, refused connection, expired credentials, ...).
- **English only.** One language keeps the UI and its documentation in step.

### Security decisions

The console holds the user's cluster credentials and can change workloads, so it is a local server
that must not be reachable or drivable by other web pages.

| Control | Defends against |
|---------|-----------------|
| Loopback-only listener; `--allow-remote` is required for anything else | Other machines on the network using the user's credentials |
| Random session token printed at start, exchanged for a `SameSite=Strict`, `HttpOnly` cookie and removed from the URL; constant-time comparison | Other local users or pages calling the API; the token lingering in browser history |
| `Host` allow-list (skipped only with `--allow-remote`) | DNS rebinding, where a hostile site resolves its name to 127.0.0.1 |
| Custom `X-K8scli-Request` header on every non-`GET` request, and an `Origin` check on every method | CSRF from other sites, including form posts |
| Under `/api/`, `Sec-Fetch-Site: cross-site` and `same-site` are refused | Another web app on a different port of `localhost` (same *site*, different origin) using the cookie against the API |
| Content-Security-Policy `default-src 'none'`, scripts and styles from `'self'`, `frame-ancestors 'none'`, `X-Frame-Options: DENY`, `no-store`, `no-referrer` | XSS escalation, clickjacking, leaking URLs |
| Strict input validation (DNS-1123 names, bounded log options, JSON bodies with unknown fields rejected and a size limit) | Path or query tricks reaching the API server |
| Log lines and cluster data rendered with `textContent` or escaped | Stored XSS from pod names, labels or log output |
| Type the context name to change a context whose name contains `prod`, enforced by the server | Changing the wrong cluster by habit or by a scripted call |
| `--read-only` | Accidents when the console is only meant for looking |
| `Secure` on the cookie only when TLS is in front (`X-Forwarded-Proto` or a TLS listener) | Plain HTTP on loopback keeps working in Safari, which drops `Secure` cookies on `http://localhost` |

Not defended:

- A malicious process running as the same user can read the kubeconfig directly, so the console adds
  no protection against that.
- Cookies are not isolated by port, so any web app on `localhost` receives the console's cookie on
  its own requests. Browsers are stopped from using it against the API by the `Sec-Fetch-Site` and
  `Origin` checks, but a non-browser process that obtains the token can. Accepted: such a process
  already has the kubeconfig. Keeping the token only in JavaScript memory would remove the risk and
  was left out to keep the UI simple (tracked in
  [issue #17](https://github.com/1985epma/k8scli/issues/17)).
- With `--allow-remote` the user takes on the network exposure, and the `Host` check is skipped.

## Alternatives considered

- **Desktop app (Electron or similar).** Heavy to build, sign and update for every platform, and it
  would pull a browser engine into a small CLI.
- **Hosted SaaS.** Would need cluster credentials to leave the machine. Rejected.
- **`kubectl proxy` or the Kubernetes Dashboard.** Powerful, but cluster-wide, installed in the
  cluster, and with much more surface than the frequent tasks in ADR 0001.
- **No UI.** Leaves the guided setup and quick triage to documentation.

## Consequences

- New attack surface: a local HTTP server. Changes to `internal/web/auth.go` need security review and
  tests for the controls above.
- The criteria of ADR 0001 still decide what the UI may do. New web features need the same
  justification as new commands, and anything that changes the cluster needs a dry run and a
  confirmation first. Pod exec, delete, apply and edit are out of scope for the UI as well, except for
  the guided actions that ADR 0003, 0004 and 0005 allow.
- The server also makes outbound requests when Helm is used: to chart repositories and `artifacthub.io`.
  Those hosts are fixed or come from Artifact Hub's own record, never from the browser, and
  `--catalog-only` removes the Artifact Hub ones (ADR 0004).
- Every UI action shows its CLI equivalent (or, for the actions the CLI lacks, the `kubectl` one), so people can reproduce or script it.
- The static assets add a few files to maintain without a toolchain, so they are kept small and
  dependency-free.
