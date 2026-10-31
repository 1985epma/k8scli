# ADR 0003: Guided write actions in the web console

- Status: accepted (extended by ADR 0004 for Helm apps and ADR 0005 for disaster recovery)
- Date: 2026-10-31
- Amends: ADR 0001, ADR 0002

## Context

The web console started with one write action, scaling a deployment. People asked for the
everyday tasks around it: create an autoscaler, deploy something, change replicas or
configuration, and see the state of the cluster at a glance. ADR 0001 and 0002 left
`apply`, `edit`, `delete` and `patch` out of scope, because a generic write surface turns
the project into a worse `kubectl` and a bigger attack surface.

## Decision

The console gets **guided actions**: fixed forms for a short list of common tasks. It does not
get a generic write surface.

| Area | Actions | Notes |
|------|---------|-------|
| Dashboard | Read only | Cluster summary, namespaces, pods that need attention, top restarts, recent warnings, CPU and memory when metrics-server is installed |
| Namespaces | List, create | No delete; names starting with `kube-` are refused, at most 20 labels |
| Deployments | Create, scale, update image, restart, edit plain environment variables | Create can add a Service (0 to 100 replicas, at most 50 variables). The container is created with `allowPrivilegeEscalation: false`. If creating the Service fails, the new Deployment is removed |
| Autoscaling | Create or update a HorizontalPodAutoscaler (autoscaling/v2), remove one | CPU and memory utilization targets (1 to 200 %), 1 to 500 replicas, one HPA per Deployment |
| Configuration | List ConfigMaps, create one, add, change or remove one key | Secrets are never listed or read. At most 100 keys, 256 KiB per value, under 1 MiB in total. Keys in `binaryData` are not editable |

### Rules every write action follows

1. **Fixed shape.** The server builds the object from a small typed request. There is no
   endpoint that accepts YAML, JSON manifests or arbitrary patches.
2. **Preview first.** Every action accepts `dryRun`. The server sends the real request with
   `DryRun: All`, so the API server validates it, and returns what would change. The UI shows the
   preview before it offers "Apply".
3. **Production gate.** A context whose name contains `prod` needs `confirm` equal to the
   context name for any real (non dry-run) change. The server enforces it, not only the UI.
4. **Read-only mode.** `--read-only` rejects every real change; dry runs still work.
5. **Narrow calls.** Prefer the narrowest API: the `scale` subresource, JSON merge patch on a
   single ConfigMap key, optimistic concurrency (`resourceVersion`) when a read-modify-write is
   unavoidable. While pods roll out the controller rewrites the Deployment status constantly, so a
   conflict is usually not another person: the console re-reads the object and applies the same edit
   again, up to 4 times, and only then answers 409 `conflict`. Edits touch one field at a time
   (an image, some variables), so the rest of the object is kept as it is on the fresh read.
6. **Traceable.** Objects created by the console carry `app.kubernetes.io/managed-by: k8scli`.
7. **Validated input.** Names are DNS-1123, images and quantities are parsed, counts have upper
   bounds, ConfigMap values are limited in size.
8. **No new secrets exposure.** Secrets are not listed. Environment variables that come from a
   Secret or ConfigMap reference are shown read-only and left untouched.

### Equivalent commands

The CLI has no commands for these actions (except `scale`), and adding them is not part of this ADR. Every form
shows the equivalent `kubectl` command (for example `kubectl autoscale deployment api --min=2
--max=10 --cpu-percent=80`) so people can reproduce or script it.

Two later ADRs add actions under the same rules (fixed shape, preview first, production gate,
read-only mode): Helm apps in [ADR 0004](0004-helm-apps.md) and disaster-recovery runs in
[ADR 0005](0005-disaster-recovery.md).

### Still out of scope

Delete of namespaces or workloads, `exec`, port-forward, editing arbitrary resources or CRDs,
Secret values, RBAC changes, node operations. The exceptions are the ones those ADRs define: the
uninstall of a Helm release that the console installed (which removes only what the release created),
and, only when the console is started with `--allow-dr-runs`, the drain and power steps of a DR plan.

## Consequences

- The write surface grows from one endpoint to a handful (and later Helm and DR), all behind the same
  guard (read-only, production confirmation, dry run, validation) implemented once. `/api/scale` still
  predates that guard and is tracked in [issue #15](https://github.com/1985epma/k8scli/issues/15).
- Each new action needs a test for: dry run changes nothing, read-only blocks it, production
  confirmation is enforced, invalid input is rejected.
- Requests for more write actions are judged against the rules above. A request that needs a
  free-form manifest is declined.
- The README and `docs/web-console.md` must say what the console can change.
