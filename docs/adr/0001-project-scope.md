# ADR 0001: Project scope and roadmap

- Status: accepted
- Date: 2026-10-30

## Context

`k8scli` is a small CLI for everyday Kubernetes work. It started with a handful of
commands (contexts, pods, logs, scale, cluster info). `kubectl` already covers the whole API,
so without a clear scope the project would slowly turn into a worse `kubectl`.

## Decision

`k8scli` is a **convenience layer for frequent, mostly read-only tasks**. It complements
`kubectl`; it does not replace it.

A command belongs in k8scli when it meets all of these:

1. It is something people do many times a day (switching context, finding a broken pod, reading logs, scaling).
2. The default output is better than `kubectl`'s for that task (clear status, safe defaults, current context shown).
3. It can be implemented on `kubernetes.Interface` and tested with the fake clientset.
4. Anything that changes the cluster shows the context and namespace first, and supports `--dry-run`.

### Non-goals

- Generic `apply`, `delete`, `edit`, `patch` or CRD management
- Plugin system or cluster provisioning
- Reimplementing kubectl's full flag surface

### Technical decisions that follow

- Commands are thin; shared state lives in `cmd.App`; logic that can be tested without cobra lives in `internal/`.
- Commands depend on `kubernetes.Interface`, never on the concrete clientset.
- Machine-readable output (`-o json|yaml`) goes to stdout; hints, banners and warnings go to stderr.
- Write operations use the narrowest API available (e.g. the `scale` subresource).

## Roadmap

Candidates, in priority order. Each needs its own issue that shows it satisfies the criteria above.

| Candidate | Why it fits | Notes |
|-----------|-------------|-------|
| `events` | Quick view of warnings for a namespace/pod | Read-only, sorted by time |
| `rollout restart <deployment>` | Common follow-up to `scale` | Write operation: context banner, `--dry-run`, production confirmation |
| `top pods` | Resource usage next to status | Needs metrics-server; degrade gracefully |
| `describe pod` (summary) | Shows why a pod is not ready | Curated summary, not a full `kubectl describe` |
| Dynamic shell completion | Context, namespace and deployment names | Uses cobra's `ValidArgsFunction` |

Explicitly deferred: `exec` (interactive streaming adds a lot of terminal handling for little gain over `kubectl exec`).

## Consequences

- Pull requests that add generic API verbs will be declined by pointing to this ADR.
- The scope can change, but through a new ADR that supersedes this one.
