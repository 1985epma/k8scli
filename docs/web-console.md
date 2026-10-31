# Web console guide

The web console is a local browser interface for `k8scli`. It reads your kubeconfig, talks to your
cluster from your machine, and needs no install in the cluster. The interface is in English only.

For the design and the threat model, see [ADR 0002](adr/0002-local-web-console.md),
[ADR 0003](adr/0003-guided-write-actions.md) (guided changes) and [ADR 0004](adr/0004-helm-apps.md)
(Helm apps). The disaster-recovery page is covered in [dr.md](dr.md).

## Start it

```bash
k8scli ui            # starts the console and opens your browser (also: k8scli-ui)
k8scli serve         # starts it and only prints the link
```

Both print a link such as `http://127.0.0.1:8484/?token=...`. Open it: the token is traded for a
session cookie and removed from the address bar. Press Ctrl+C in the terminal to stop.

| Flag | Meaning |
|------|---------|
| `--addr host:port` | Where to listen. Default `127.0.0.1:8484` |
| `--read-only` | Nothing can be changed. Previews still work |
| `--allow-remote` | Allow a non-loopback address. Anyone with the link can use your credentials |
| `--no-open` | `k8scli ui` only: do not open the browser |
| `--catalog-only` | Install Helm apps only from the reviewed catalog. Artifact Hub search and install are off (they answer `403 catalog_only`) |
| `--dr-dir path` | Where the Resilience page keeps plans, run ledgers, the audit log and reports. Default `~/.k8scli/dr` |
| `--allow-dr-runs` | Allow real DR runs, drills and resume from the console. Checks, exports, dry runs and reports are always available |
| `--allow-dr-power` | Also allow DR runs that power machines or the cluster off and on, for plans not marked `safety.labOnly` |
| `--kubeconfig path` | Use this kubeconfig instead of `$KUBECONFIG` or `~/.kube/config` |

The three DR flags are explained in [dr.md](dr.md).

`k8scli-ui` is the same command under another name (a link to `k8scli`; on Windows, a `k8scli-ui.cmd`
shim in the zip and the MSI). `k8scli ui` picks a free port when 8484 is taken. An `--addr` you type is never replaced: if it is
busy, the command stops with an error.

## The screen

- **Sidebar**: Dashboard, Pods, Deployments, Autoscaling, Apps (Helm), Resilience, Namespaces,
  Configuration (ConfigMaps), Contexts, and the three setup wizards. It collapses on desktop and
  becomes a drawer on a phone.
- **Top bar**: breadcrumbs, a search box (press `/` to focus it) that filters the table you are on,
  the **context switcher**, a Read-only badge when `--read-only` is on, and the theme button (light or
  dark; it follows your system until you pick one).
- **Context switcher**: chooses the context this browser tab looks at. It does **not** change your
  terminal's current context and does not write the kubeconfig. A context whose name contains `prod`
  shows a red dot and a red banner.
- **Namespace**: pages that list namespaced objects have a namespace selector (or "All namespaces").
  The first namespace is the one set on the context, or `default`.
- Tables refresh by themselves every 10 seconds. The refresh pauses while a menu, a dialog or a form
  field is in use, and while the tab is in the background.
- Under every table and dialog, **CLI equivalent** shows the command that does the same. The CLI has
  no `--context` flag, so when you are looking at a context that is not the CLI's default the command
  starts with `k8scli use <context> &&`. Actions the CLI does not have show the `kubectl` command.

## Pages

| Page | What it shows | What you can do |
|------|---------------|-----------------|
| **Dashboard** | A headline ("Everything looks healthy" or what needs attention), Kubernetes version and nodes, pods running, restarts, pods that need attention, pods with the most restarts, recent warning events, every namespace at a glance, and CPU and memory of the busiest pods | Click a namespace to open its pods. View logs of a failing pod |
| **Pods** | Status, readiness, restarts and age, per namespace or in all of them. "Needs attention" filter, optional IP and node columns | Row menu: View logs, copy the name, copy the logs command |
| **Deployments** | Ready and desired replicas and status | **New deployment**, Scale, and the row menu: Autoscale, Update image, Environment, Restart |
| **Autoscaling** | The HorizontalPodAutoscalers of a namespace: range, targets, current and desired replicas | New autoscaler, edit, remove |
| **Apps (Helm)** | The Helm releases of the namespace (or all), the catalog of 24 reviewed apps and Artifact Hub search | Install from the catalog or from a search result; upgrade and uninstall releases the console installed. See [Apps (Helm)](#apps-helm) |
| **Resilience** | Readiness score, backups, DR plans, runs, drills and reports, export and diff | Preview a run always; real runs only with `--allow-dr-runs`. See [dr.md](dr.md) |
| **Namespaces** | Status, age, pods, deployments and how many pods need attention | New namespace. Jump to the pods or deployments of a namespace |
| **Configuration** | The ConfigMaps of a namespace | Open one to view its keys, add, change or remove a key, or create a ConfigMap. **Secrets are never listed or read** |
| **Contexts** | The kubeconfig contexts, sorted, with "In use" (this tab) and "CLI default" badges | **Use here** (this tab only) or **Make it the CLI default** (writes `current-context`, like `k8scli use`) |

### Logs

Open them from a pod's row menu. The drawer has container, number of lines (20, 100, 500 or all),
"since" (the start, 5 minutes, 1 hour or 24 hours), timestamps, previous container and follow. Closing
the drawer stops the stream. Log lines are shown as plain text; lines with `error`, `warn` or `info`
are colored.

### Dashboard limits

The dashboard reads at most 20,000 pods, 5,000 deployments and 5,000 events in one pass. If a cluster
is bigger, it says so. CPU and memory need
[metrics-server](https://github.com/kubernetes-sigs/metrics-server); without it the card points to
**Apps (Helm)**, where Metrics Server is in the catalog, and nothing else is affected. Local clusters such
as Docker Desktop do not ship it.

## Changing things: review, then apply

Every change goes through the same two steps.

1. **Fill in a form** (for example *Deployments > New deployment*) and choose **Review changes**.
2. **Review.** The console sends your request to the API server as a dry run, so the cluster itself
   validates it, and shows: a summary, what would change line by line, any `warning:` lines, and the
   equivalent `kubectl` command (with a copy button). Nothing has changed yet. If nothing would
   change, it says so.
3. **Apply.** Only this button changes the cluster. A toast confirms it and the page refreshes.

**Production confirmation.** If the context name contains `prod` (for example `eks-prod-us-east-1`),
**Apply** stays disabled until you type the context name. The server enforces this too, so a script
that skips the screen is refused with `409 confirmation_required`. Dry runs never need it.

**Read-only mode.** With `--read-only` the preview works but **Apply** is disabled, and the server
answers `403 read_only` to any real change, including "Make it the CLI default".

What each form changes:

| Task | Details |
|------|---------|
| New deployment | Name, namespace, image, replicas (0 to 100), port, up to 50 environment variables, optional CPU and memory requests and limits, and an optional Service (ClusterIP or LoadBalancer). "Try with nginx" fills an example. Containers are created with `allowPrivilegeEscalation: false`. Objects carry the label `app.kubernetes.io/managed-by: k8scli`. If the Service cannot be created, the new Deployment is removed |
| Scale | A stepper or the 2/4/6/8 shortcuts. "Only simulate" is a dry run |
| Update image | Pick the container and type the new image. Pods roll out one by one |
| Restart | Same as `kubectl rollout restart` |
| Environment | Add, change or remove plain variables of one container. Variables that come from a Secret, ConfigMap or field reference are shown but cannot be changed here. Changing them rolls out new pods |
| Autoscale | Min and max replicas (1 to 500) and a CPU and/or memory target (1 to 200 %), with presets. One autoscaler per deployment. Editing an existing one shows what differs |
| ConfigMap | Create one, or add, change or remove a single key (value up to 256 KiB, up to 100 keys, under 1 MiB in total). Keys in `binaryData` cannot be edited |
| Namespace | Create one, with optional labels. Names starting with `kube-` are refused |

Edits to an existing deployment (image, environment) use the version of the object the console read.
While pods roll out, Kubernetes rewrites the status all the time, so a conflict is common and usually
not another person. The console reads the object again and repeats the same edit up to 4 times before
it reports `409 conflict`.

## Apps (Helm)

The **Apps (Helm)** page installs and manages apps packaged as Helm charts. It uses the Helm library built
into k8scli: no `helm` executable is needed, it behaves the same in the container image, and the release
state lives where Helm keeps it (Secrets in the release namespace), so `helm list` shows the same releases.

The page has **Installed apps** (a table of releases, with the same namespace selector as other pages),
the **Catalog** (cards, with a filter box and category chips) and **Search Artifact Hub**.

### Installed apps

Each row shows the release, chart and version, app version, status, revision and age. Open a release to see
its notes, settings, resources and history.

| Badge | Meaning |
|-------|---------|
| **Managed** | Installed by the console. Carries the label `k8scli.io/managed=true`. **Upgrade** and **Uninstall** are available |
| **Elsewhere** | Installed with the Helm CLI or another tool. Listed so you can see it, but it cannot be changed here |

Releases installed from the catalog also carry `k8scli.io/catalog=<id>`. Releases installed from a search
result carry `k8scli.io/source=hub`, `k8scli.io/package=<Artifact Hub package id>`, `k8scli.io/hub-repo` and
`k8scli.io/hub-chart`. The API reports `source` as `catalog`, `hub` or `other` for each release.

**Upgrade** reuses the install form, prefilled from the running release, on the same pinned chart version.
For a release installed from a search result it goes through the same review, with the new version and its
digest, and the typed chart name. An upgrade with nothing to change answers "unchanged" and needs neither.
**Uninstall** shows what would be removed. Helm does not delete CustomResourceDefinitions, so the preview
warns when the app installed some and you have to remove them yourself; volumes created by StatefulSets are
kept too. Secret-looking settings (paths containing `password`, `secret`, `token`, `key`...) are hidden in
the release details.

### The catalog

24 reviewed apps, each pinned to a chart repository over HTTPS, an exact chart version and the sha256 of
the archive. If the downloaded archive does not match, the install is refused (`digest_mismatch`). You fill
in a few typed fields with limits (for example replicas, service type); there is no values file.

| Category | Apps |
|----------|------|
| Demo | Podinfo |
| Observability | Metrics Server, Grafana, Prometheus stack (kube-prometheus-stack), Grafana Tempo, Grafana Alloy, kube-state-metrics |
| Security | cert-manager, External Secrets, Kyverno, Trivy Operator |
| Networking | Traefik, ExternalDNS, MetalLB |
| Delivery | Argo CD, Argo Rollouts |
| Autoscaling | KEDA, Descheduler, Vertical Pod Autoscaler |
| Databases | CloudNativePG |
| Backup | Velero |
| Messaging | Strimzi Kafka Operator |
| Storage | Longhorn |
| Configuration | Reloader |

Cards show a **Cluster-wide** badge when the app creates objects outside its namespace (ClusterRoles,
CustomResourceDefinitions, webhooks), an **Installed** badge when a release of that chart exists, and a
**Recommended** badge on Metrics Server when the cluster has none. Podinfo is a tiny web app, the safest way
to try the feature. Local clusters (Docker Desktop, kind, minikube) need Metrics Server's field
"Local cluster: accept the kubelet certificate", which adds `--kubelet-insecure-tls`.

Ingress-nginx (retired upstream) and the Bitnami charts (their images are no longer free) are not in the
catalog on purpose.

### Search Artifact Hub

Type at least 2 characters (up to 80). Results come 20 per page, with **Load more**. The browser does not
talk to Artifact Hub: the server queries the fixed host `artifacthub.io` over HTTPS, with no redirect to
another host, a 10 second timeout, a 1 MiB cap on the answer, and a short cache (5 minutes, up to 100
searches). Each result shows the publisher badges (**Verified publisher**, **Official**), **Signed**,
**Deprecated**, stars, the version, a security summary (critical and high counts are highlighted) and
**In catalog** when the chart is already one of the 24.

A result is **installable** only when all of these hold:

- it is a Helm chart of type application (libraries are refused), not deprecated and not a pre-release;
- its repository is **official or has a verified publisher**. The verified badge only proves who controls
  the repository: it does not say the chart is safe. This is why the review and the typed name matter;
- the chart archive is served over HTTPS;
- it is not on the deny list of charts that build arbitrary objects from values (`raw`, `app-template`,
  `common`, `generic`, `manifests`...).

Otherwise the row is shown with the reason and a disabled **Install**.

**Installing a search result** is stricter than the catalog:

1. The form shows a **Community chart** notice. Release name, namespace and "create namespace" work as in
   the catalog. **Settings (optional)** takes up to 20 lines of `path=value`. Each path must already exist
   in the chart's default values (for example `replicaCount=2`), the value must be simple and of the same
   type, and a path may not go through keys that take whole objects (`extraObjects`, `manifests`...).
2. **Review changes** downloads the archive on the server and shows its **sha256**, every object and every
   image (including the ones in Helm hooks and in the chart's `crds/` folder), plus warnings for cluster-wide objects, admission webhooks, CRDs, hooks, Secrets, `lookup` calls, `cluster-admin`
   bindings, wildcard roles, privileged or host-level pods (`privileged`, `hostNetwork`, `hostPID`,
   `hostPath`) and images without a pinned version (`:latest` or no tag).
3. **Apply** needs you to type the **chart name**, plus the context name in a production context. The digest
   you reviewed is sent back; if the archive is not the same, nothing is applied.

The server asks Artifact Hub for the package again by id when you review and when you apply. Nothing
that decides where the chart comes from is taken from the browser. For a package it has not just listed,
it answers `unknown_package`: search again.

### Limits and timing

| Limit | Value |
|-------|-------|
| Chart archive | HTTPS only, at most 16 MiB, 2 minutes to download; redirects only to HTTPS on the same host or a GitHub host |
| Dry run (preview) | 3 minutes |
| Install, upgrade, uninstall | 6 minutes, and they keep running if the browser tab is closed. The release itself times out at 5 minutes. Installs do not wait for pods to be ready: watch the Pods page |
| Release name | A DNS label, at most 53 characters |
| Concurrency | One change at a time per release (`busy` otherwise) |
| Namespaces | "Create namespace" is refused for names starting with `kube-` |
| Resources listed in a preview | The first 200 |

`--read-only` keeps the preview and blocks **Apply**. In a production context **Apply** needs the context
name. `--catalog-only` removes Artifact Hub search and install; the catalog keeps working and the page says
it is in catalog-only mode.

## Setup wizards

They are step-by-step guides in the sidebar, under **Setup**. Progress is kept while you stay on the
page.

- **Connect a cluster**: pick where your cluster runs (Docker Desktop, minikube, kind, Amazon EKS,
  Google GKE, Azure AKS, or another kubeconfig). It builds the exact commands from the names you
  type, then scans your kubeconfig and runs a **connection check** that asks the API server for its
  version. A failure shows the error and a hint (missing credential plugin, refused connection, bad
  host name, expired credentials, untrusted certificate, no permission). The last step opens the
  cluster in the console, or makes it the CLI default.
- **Install the CLI**: for another machine. macOS (Homebrew), Linux (`.deb`, `.rpm` or a single
  binary, with the checksum step), Windows, Docker, or from source, plus optional signature
  verification with `cosign` and `gh attestation verify`.
- **Shell completion**: zsh, bash, fish or PowerShell.

On a first run with no kubeconfig contexts, the console opens **Connect a cluster** by itself.

## Security in short

The console can change your cluster, so it is locked down by default. The full model is in
[ADR 0002](adr/0002-local-web-console.md).

- It listens on loopback only. Another address is refused unless you pass `--allow-remote`, which skips
  the `Host` check and lets anyone with the link use your kubeconfig credentials.
- Every request needs the session token. The link trades it for an `HttpOnly`, `SameSite=Strict` cookie
  (`Secure` only when TLS is in front); scripts may send it as a bearer token.
- The `Host` header must be the listen address, `localhost`, `127.0.0.1` or `[::1]` on the same port
  (DNS rebinding).
- Under `/api/`, a request the browser labels `Sec-Fetch-Site: cross-site` or `same-site` is refused. A page
  on another port of `localhost` counts as same site, so this is what keeps it from using your cookie.
  Any `Origin` that is not the console's own is refused on every method, and every request that is not a
  `GET` needs the `X-K8scli-Request` header.
- A strict Content-Security-Policy (`default-src 'none'`, own scripts and styles, no inline code) and no
  third-party requests from the browser: no CDN, no web fonts, no analytics.
- Each request carries the context shown in the page, so the console never changes the terminal's
  current context. Only "Make it the CLI default" writes the kubeconfig.
- Changes have a fixed shape (no YAML, no arbitrary patches), a server-side preview, the production
  confirmation and `--read-only`. Secrets are never listed or read.
- The server's own outbound requests are limited to your cluster, chart repositories and, for search,
  `artifacthub.io`. The hosts are fixed or taken from Artifact Hub's record, never from the browser.
- Real DR runs, drills and powering machines off need `--allow-dr-runs` (and `--allow-dr-power`).

Known limit: browsers do not isolate cookies by port, so another local web app on `localhost` receives the
cookie on its own requests. The checks above stop a browser from using it against the API; a non-browser
process that gets the token could use it, but it could also read your kubeconfig directly
([issue #17](https://github.com/1985epma/k8scli/issues/17)).

## Troubleshooting

**"cannot listen on 127.0.0.1:8484" or "failed to listen on …"**
The port is in use. `k8scli ui` already picks another one unless you passed `--addr`. With
`k8scli serve`, use `--addr 127.0.0.1:0` for any free port, or choose one.

**"Unauthorized" or "Your session ended"**
The token only lives as long as the process. Open the link printed by `k8scli ui` or `k8scli serve`
again. Each start prints a new token. A browser tab that was open before a restart needs the new link.

**"Forbidden host"**
You opened the console through a name or port it does not expect (for example behind a proxy or
another port forward). Use the printed link. Only `localhost`, `127.0.0.1` and `[::1]` on the
listening port are accepted unless `--allow-remote` is set.

**The dashboard says metrics are not available, or an autoscaler shows `<unknown>` CPU**
metrics-server is not installed in the cluster. Install **Metrics Server** from *Apps (Helm)*. On local
clusters such as Docker Desktop, kind or minikube turn on its field "Local cluster: accept the kubelet
certificate" (it adds `--kubelet-insecure-tls`). CPU-based autoscaling also needs a CPU request on the
pods; the autoscale form warns when it is missing. Give the metrics a minute after installing.

**"credential plugin … executable file not found"**
Your kubeconfig uses `aws`, `gcloud`, `kubelogin` or a similar command to get tokens, and it is not on
the `PATH` of the terminal that started the console. Install it, or start the console from a shell
where it works. Containers do not have these tools, so the Docker image cannot use such kubeconfigs.

**"The credentials were rejected" / Unauthorized from the cluster**
The token in your kubeconfig expired. Refresh it with your cloud CLI (for example
`aws eks update-kubeconfig …`, `gcloud container clusters get-credentials …`,
`az aks get-credentials …`), then check the connection again in the wizard. The console drops its
cached client when you run the check or change the default context.

**"Couldn't read the kubeconfig" or "No cluster connected yet"**
No file was found, or it has no contexts. The Contexts page lists the files being read. Run the
*Connect a cluster* wizard, or point the console at a file with `--kubeconfig`.

**"Production context" banner and the type-the-name box**
The context name contains `prod`. This is by name only, so a context such as `product-catalog-dev` also
matches, and a production cluster that is not named that way is not detected. Rename the context in your
kubeconfig if it gets in your way (see [issue #16](https://github.com/1985epma/k8scli/issues/16)).

**Apply is disabled**
Either the console runs with `--read-only`, you are in a production context and have not typed the
context name yet, or nothing would change.

**A change is rejected by the cluster**
The preview step reports what the API server said (for example an invalid image name or a quota).
Fix the form and review again.

**Artifact Hub search says it did not answer, or "answered 429 Too Many Requests"**
Artifact Hub is rate limiting or down. A rate limit is reported as `429 hub_rate_limited` (with the wait
Artifact Hub asks for, when it sends one) and any other failure as `502 hub_unavailable`. The **Catalog**
keeps working either way. Wait a moment and search again; results are cached for 5 minutes. Without
internet access, or with `--catalog-only`, use the catalog.

**"This console runs in catalog-only mode" or `403 catalog_only`**
The console was started with `--catalog-only`. Restart it without that flag to search Artifact Hub.

**Install is refused with `digest_changed`**
The chart archive is not the one you reviewed (the repository replaced the file, or the version moved).
Nothing was applied. Choose **Review changes** again, read what changed, and apply the new review. The
same applies to an upgrade.

**`digest_mismatch`**
The archive does not match the digest in the catalog, or the one Artifact Hub reports. The file was
changed or the download was tampered with, so it was not used. Try again later; if it persists, do not
install it and report it (for a catalog app, the entry needs a new reviewed digest).

**Apply asks for the chart name (`phrase_required`)**
Charts that are not in the catalog need you to type the chart name in the review dialog, on top of the
context name in a production context. It is on purpose: it makes you stop and read the review.

**"This chart cannot be installed here" (`not_installable`)**
The result is a library chart, a pre-release, not served over HTTPS, on the deny list, or the publisher is
not verified and the repository is not official. Install it with the Helm CLI if you trust it. Some
reasons (library, pre-release) only show up when you review, because the search result does not say.

**"Artifact Hub does not know this package" (`unknown_package`)**
The console keeps what a search returned for 1 hour (up to 2,000 packages) and only installs ids it has
seen. Search for the chart again, then install. The same happens to an old browser tab.

**Upgrade fails with `missing_crds`**
The app's CustomResourceDefinitions are gone from the cluster (Helm does not remove them, but someone
may have deleted them), so the API server cannot validate the new objects. Reinstall the app to bring them
back.

**`incompatible_cluster` or `timeout` on a Helm change**
`incompatible_cluster`: the chart needs a newer Kubernetes than the cluster runs (`kubeVersion`). `timeout`
(`504`): Helm did not finish in time and the release may be half installed. Check the release in the list
and uninstall it if needed.

**A release is listed as "Elsewhere" and Upgrade is disabled**
It was installed without the console's label. Change it with the Helm CLI, or uninstall it there and
install it again from the console.

**"already exists" on install, or `busy`**
A release with that name exists in the namespace (pick another name or upgrade it), or another change to
the same release is still running. Wait for it to finish and reload.

**The Prometheus stack preview shows "CRDs are not installed yet"**
The kube-prometheus-stack and a few other charts bring their own CRDs. Before they exist, the API server
cannot validate the objects that use them, so the preview falls back to an offline render (like
`helm template`) and says so in a warning. The list of objects is still accurate. A real install does not
fall back: it applies the chart and reports the cluster's answer.

**I installed an app and `kubectl get pods` shows nothing ready yet**
Installs do not wait for the pods. Watch the Pods page. If a release is stuck, uninstall it (managed
releases only) or use `helm status` and `helm uninstall` in the terminal.

**I rebuilt k8scli but the page looks old**
The interface is embedded in the binary. Rebuild and restart `k8scli`, then reload with the new link.

## Running it in a container

```bash
docker run --rm -p 127.0.0.1:8484:8484 -v ~/.kube:/home/nonroot/.kube:ro \
  ghcr.io/1985epma/k8scli:latest serve --addr 0.0.0.0:8484 --allow-remote --read-only
```

Publish the port on `127.0.0.1` only. The token is in the container logs. The kubeconfig is mounted
read-only, so keep `--read-only` unless you mount it read-write.
