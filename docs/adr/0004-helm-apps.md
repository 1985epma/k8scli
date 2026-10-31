# ADR 0004: Install apps with Helm from the web console

- Status: accepted (widened by the amendment at the end)
- Date: 2026-10-31
- Amends: ADR 0003

## Context

After the guided actions of ADR 0003, people asked to install programs (an ingress controller,
metrics-server, cert-manager, a monitoring stack) from the console. These are Helm charts. A
chart is a bundle of manifests, so "install any chart" is exactly the generic write surface that
ADR 0001 and 0003 rule out: it can create RBAC, CRDs, webhooks and privileged workloads.

## Decision

The console installs apps from a **curated catalog** with the **Helm SDK embedded in the binary**.

1. **Curated catalog.** The catalog is Go code in this repository. Each entry pins the chart
   repository URL (HTTPS only), the chart name, an exact chart version and the sha256 digest of the
   chart archive. The server refuses to install a chart whose downloaded archive does not match
   the digest. Users cannot type a repository URL, a chart name or a raw values file. (The amendment
   adds a second, review-based path: charts found by searching Artifact Hub.)
2. **Fixed form.** Each entry declares the few fields people may set (name, namespace, replicas,
   a service type...). The server maps them to chart values. Fields marked secret are never
   returned by the API or written to logs.
3. **Same rules as ADR 0003.** Preview first (`dryRun`, a real Helm dry run that renders the
   manifests and lists what would be created, flagging cluster-scoped kinds such as
   ClusterRole, CRD and webhooks), the production gate (`confirm` equal to the context name),
   read-only mode, and the equivalent `helm` command.
4. **Scope of upgrade and uninstall.** Every release is listed. Install, upgrade and uninstall
   only act on releases that carry the label `k8scli.io/managed=true`, which the console sets
   when it installs. A release installed with the Helm CLI can be seen but not changed here.
5. **Bounded, not waited for.** A change runs with a 5 minute Helm timeout inside a 6 minute request
   (a 3 minute one for a dry run), and keeps going if the browser tab is closed. The install does not
   wait for pods to become ready and does not use `--atomic`: a failed install is reported and left for
   the person to inspect or uninstall. Helm hooks of the chart run as usual. Only one change runs at a
   time for a given release.
6. **Embedded SDK.** `helm.sh/helm/v3` is a library dependency. It makes the binary about 15 MiB
   larger (43.7 to 58.9 MiB stripped when it was added), needs no `helm` executable, and works the same in the container image. Release state is
   kept where Helm keeps it (Secrets in the release namespace), so `helm list` shows the same.
7. **Maintenance.** Catalog versions and digests are updated by a maintainer with a script
   (`scripts/update-helm-catalog.sh`, which prints new versions and digests for review), never
   automatically at run time.

### Still out of scope

Adding repositories, installing local archives or OCI references typed by the user, arbitrary
values, rollback UI, Helm hooks debugging, uninstall of releases not created by the console.

## Consequences

(Partly superseded by the amendment at the end: search results add a second, review-based path.)

- The console can now create cluster-wide resources. For the catalog, only those of reviewed, pinned
  charts; for a search result, whatever the reviewed chart contains (see the amendment). Every install
  shows what it will create before it asks to apply.
- For the catalog, supply chain risk is limited to the entries: a compromised upstream repository
  cannot change what is installed, because the digest would not match. For search results the digest
  only proves that the archive did not change between the review and the apply; the person's review is
  the control.
- The catalog is a maintenance cost. An entry must be updated when upstream publishes a security
  fix, and a stale catalog is a risk to watch in releases.
- `govulncheck` now covers Helm's dependency tree; the dependency count in `go.sum` grows.

## Amendment: a bigger catalog and Artifact Hub search

After the first version, the decision was widened: the catalog grew to 24 reviewed apps, and people can
**search Artifact Hub by name** and install a chart that is not in the catalog. That moves part of the
trust from "pinned by a maintainer" to "reviewed by the person at install time", so it comes with
stronger rules. `--catalog-only` on `serve` and `ui` switches the whole second path off.

1. **Search is read-only and goes through the server.** The browser never talks to Artifact Hub. The
   server queries the fixed host `artifacthub.io` over HTTPS: no redirect to another host (and only a few
   redirects on the same one), a 10 second timeout, a 1 MiB cap on the answer, 20 results per page (at most 50 pages), a
   query of 2 to 80 characters, and an in-memory cache of 5 minutes bounded to 100 searches. The request
   stops when the browser cancels it. It returns only what the UI shows: name, version, description
   (cut to 200 characters), repository and publisher flags, stars, signed, deprecated, a security report
   summary, and whether the result is already in the catalog. A rate limit from Artifact Hub is
   `429 hub_rate_limited`, other failures are `502 hub_unavailable`; an unknown package is
   `404 unknown_package`.
2. **Who can be installed from search.** Only Helm charts of type application (not library), not
   deprecated, not a pre-release, served over HTTPS, from a repository that is **official or has a
   verified publisher**, and not on a deny list of charts that build arbitrary objects from values
   (`raw`, `app-template`, `common`, `generic`, `manifests`...). Everything else is listed with the
   reason (`not_installable`, `400`); library and pre-release status is only known at review time.
   The verified-publisher badge proves who controls the repository, not that the chart is safe: one
   verified repository publishes a chart under another project's name. The review is the control.
3. **By id, never by address.** The server asks Artifact Hub for the package again by id at review and
   at apply time. It keeps, from each search, the (repository, chart) pair Artifact Hub returned for an
   id, for 1 hour and at most 2,000 entries, because Artifact Hub has no lookup by id alone; an id it has
   not seen is `404 unknown_package` ("search again"). Release labels carry the pair, so an upgrade does
   not depend on that cache. A repository or archive address sent by the browser is never used.
4. **Digest on first use.** The server downloads the archive (HTTPS, at most 16 MiB, 2 minutes, redirects
   only to the same host or GitHub hosts), computes its sha256 and checks it against the digest Artifact
   Hub reports when there is one (`digest_mismatch` otherwise). The preview returns the digest. The apply
   must send back the digest that was reviewed; if it differs, `409 digest_changed` and nothing is applied.
5. **Settings are limited.** No values file. Up to 20 `path=value` pairs; a path is names joined by dots
   (at most 128 characters and 8 levels), must already exist in the chart's default values, must be a
   simple value (not a list or a group) of the same type, a value is at most 256 characters, and a path may
   not pass through keys that take whole objects (`extraObjects`, `extraManifests`, `manifests`, `objects`,
   `rawResources`...).
6. **The review is stricter for charts outside the catalog.** The preview lists every object and every
   image and warns about cluster-scoped kinds, admission webhooks, CRDs, Secrets, bindings to
   `cluster-admin`, roles that grant every verb on every resource, privileged or host-level pods
   (`privileged`, `hostNetwork`, `hostPID`, `hostPath`) and images that are not pinned (`:latest` or no
   tag). A setting can change an image, and the review shows the changed one. The apply also needs the
   **typed chart name** (`409 phrase_required` otherwise), on top of the production gate (the context
   name).
7. **Ownership and labels.** A release installed from the catalog carries `k8scli.io/managed=true` and
   `k8scli.io/catalog=<id>`. One installed from search carries `k8scli.io/managed=true`,
   `k8scli.io/source=hub`, `k8scli.io/package=<package id>`, `k8scli.io/hub-repo` and
   `k8scli.io/hub-chart`. Upgrade and uninstall work on managed releases and on nothing else
   (`403 not_managed`); the API reports each release's `source` as `catalog`, `hub` or `other`.
8. **Upgrade of a search release is implemented through the same flow:** a new version means a new digest
   and a new review with the typed name. An upgrade that would change nothing answers "unchanged" and
   needs neither. Settings that look like secrets are hidden when a release is described.
9. **Dry run falls back offline when it must.** Helm's client and server dry runs validate against the
   API, which fails while an app's CRDs do not exist yet (for example kube-prometheus-stack). The preview
   then renders offline, like `helm template`, with a warning. A real install never falls back. An
   upgrade whose CRDs have disappeared is `400 missing_crds`.
10. **Read-only and production** behave as before: the preview works, the apply is blocked
    (`403 read_only`), and the production gate needs the context name.

### Consequences of the amendment

- A reviewed chart from a verified publisher can still be unsafe for a given cluster; the review is
  the control, which is why the preview is detailed and the typed name is required.
- The server now makes outbound requests to `artifacthub.io` and to chart hosts. Hosts are fixed or
  taken from Artifact Hub's own record, never from the browser, and `--catalog-only` removes all of it.
- The catalog is a maintenance cost that grows with its size. `scripts/update-helm-catalog.sh` checks every
  entry against upstream, and keeping it current is tracked in
  [issue #18](https://github.com/1985epma/k8scli/issues/18). Not yet verified from the UI against a real
  cluster: see [issue #19](https://github.com/1985epma/k8scli/issues/19).
