# Chaos experiments and external-dependency checks

Two commands of the disaster-recovery module, both under `k8scli dr`:

| Command | What it does | Changes the cluster? |
|---------|--------------|----------------------|
| `dr chaos kill-pods`, `drain-node`, `cordon-node` | Injects one small, bounded failure and measures how the apps of your plan react | Yes, only with `--execute` |
| `dr extcheck` | Looks at what a restore depends on outside the workloads: registries, certificates, referenced Secrets and ConfigMaps, classes, and the 3-2-1 rule for backups | Never |

Read [dr.md](dr.md) first for the plan file (`k8scli-dr.yaml`), `dr check`, drills and RTO/RPO.

## `dr chaos`

### What it is, and what it is not

It answers one question: *when this specific thing fails, does my system notice, and how long until it is healthy
again?* The answer is a **measured** time to detect and time to recover, in the same format as a drill report.

It is not a chaos platform. There are no network faults, no CPU or memory stress, no clock skew, and **no node power
loss**: switching machines off is the provider's job and is rehearsed with `dr drill --full`. It also does not run on
a schedule.

### The safety model

Every rule below is enforced in the code that injects the failure, not only in the command line, and is checked again
right before anything changes (so removing a label while an experiment is planned stops it).

| Rule | What it prevents |
|------|------------------|
| The plan must say `safety.labOnly: true` | Running it against a cluster nobody declared disposable |
| Pods are killed only in namespaces labelled `k8scli.io/chaos-allowed=true` (value exactly `true`) | Hitting a workload nobody opted in. System namespaces are refused even when labelled |
| At most `--max-percent` (default 50%) of a workload's replicas, and never more than 20 pods | A "small" failure that is not small |
| At least one replica always stays ready | Turning a chaos experiment into an outage |
| Pods are removed through the Eviction API, and a PodDisruptionBudget with no room refuses the experiment | Bypassing the protection the team already declared |
| Control-plane nodes are never touched (label or taint) | Breaking the cluster itself |
| A node action needs another ready, schedulable worker | Leaving pods with nowhere to go |
| A drain refuses to evict pods of namespaces that are not marked (`--allow-other-namespaces` accepts it) | A node-wide action hiding behind a namespace-wide label |
| Nothing is injected when the apps are not healthy before the injection | Blaming an experiment for an existing problem |
| What was cordoned is uncordoned afterwards, on a context that survives a Ctrl-C | Leaving the cluster degraded |
| A context that looks like production also needs the typed phrase `chaos <context>` | Pointing it at the wrong cluster |
| `--execute` is required; without it every command is a dry run | Surprises |
| The result and an audit line are stored under `~/.k8scli/dr/chaos/` (0600) | No trace of who did what |

Two things are deliberately **not** protected: a context that does not have "prod" in its name is not detected as
production (the same heuristic as the rest of k8scli), and a PodDisruptionBudget that is too generous allows what it
allows.

### Opting a namespace in

```bash
kubectl create namespace chaos-test
kubectl label namespace chaos-test k8scli.io/chaos-allowed=true
```

Remove the label to stop chaos there at once:

```bash
kubectl label namespace chaos-test k8scli.io/chaos-allowed-
```

### Commands

All of them read the plan (`--plan`, default `./k8scli-dr.yaml`) and observe its apps: an app with the default health
check counts as healthy when its workloads have all replicas ready, and an app with `health: {kind: http}` when the URL
answers the expected status.

```bash
# Dry run: validates every rule, shows what would be evicted and the equivalent kubectl command
k8scli dr chaos kill-pods -n chaos-test --workload api --plan k8scli-dr.yaml

# Really do it
k8scli dr chaos kill-pods -n chaos-test --workload api --plan k8scli-dr.yaml --execute

# Evict 2 of 4 pods of a StatefulSet
k8scli dr chaos kill-pods -n data --kind StatefulSet --workload redis --count 2 --plan k8scli-dr.yaml --execute

# Cordon a worker (nothing is evicted), observe, then uncordon it
k8scli dr chaos cordon-node --node worker-1 --plan k8scli-dr.yaml --execute

# Cordon and drain a worker, observe, then uncordon it
k8scli dr chaos drain-node --node worker-1 --plan k8scli-dr.yaml --execute
```

| Flag | Applies to | Meaning |
|------|-----------|---------|
| `--plan FILE` | all | Plan file. Must be a lab plan |
| `-n NAMESPACE` | `kill-pods` | Namespace of the workload (must be labelled) |
| `--workload NAME`, `--kind`, `--count`, `--max-percent` | `kill-pods` | What to evict and how much (defaults: Deployment, 1, 50) |
| `--node NAME` | node commands | The worker to act on |
| `--drain-timeout` | `drain-node` | How long the drain may take (default 5m) |
| `--allow-other-namespaces` | `drain-node` | Accept evicting pods of namespaces that are not marked |
| `--watch app,app` | all | Observe only these apps (default: every app in scope) |
| `--window` | all | How long to look for a degradation before calling the failure absorbed (default 30s) |
| `--poll` | all | How often the apps are checked (default 2s) |
| `--max-duration` | all | Stop observing after this long (default 5m, at most 30m) |
| `--execute` | all | Really inject. Without it: dry run |
| `--confirm PHRASE` | all | The typed phrase for production-looking contexts; asked interactively when omitted |
| `-o json` | all | The full result as JSON |

### Reading the result

```
Experiment kill-pods on kind-k8scli-lab (plan lab) [executed]  id chaos-20261031-142210-a1b2c3

Injection:  Kill 1 pod(s) of chaos-test/api (risk high)
Recovery:   nothing to undo; the controller recreates what was removed
Watching:   app api
Healthy before: yes

Result:     the system degraded and recovered
Time to detect:   4s (measured)
Time to recover:  10s (measured), target 30s: pass
```

| Outcome | Meaning |
|---------|---------|
| **absorbed** | No probe was unhealthy during the window: the system took the failure without an outage. Time to recover is 0 |
| **degraded and recovered** | A probe became unhealthy (time to detect) and the apps stayed healthy for a full second, over at least two checks (time to recover) |
| **did not recover** | Still unhealthy when `--max-duration` ran out. The command exits with an error, and the recovery step still ran |

Times are measured from the start of the injection. When the apps declare a target (`rto` on the app or in `targets`)
and you pass exactly one app with `--watch APP`, the result is compared with it (pass or fail).

Limits to keep in mind:

- The apps are checked while the injection runs (every 500ms at most), so a degradation inside a long drain is still
  seen. Right after the injection they are checked every 250ms for 10s, then every `--poll`. A degradation shorter than
  that sampling can still go unseen and show up as "absorbed"; the result's notes state the resolution that was used.
- Time to recover is the first healthy check of the final healthy stretch, and the system must stay healthy for a full
  second, so a pod that flaps ready is not counted as recovered.
- "Healthy" means what the plan's health check says. A pod that is ready but returns errors is healthy unless you give
  the app an `http` check.

### Where results go

`~/.k8scli/dr/chaos/<id>.json` and one line per experiment in `~/.k8scli/dr/chaos/audit.jsonl` (time, actor, context,
plan, injection, outcome). A dry run stores nothing. The `timeToDetect`, `timeToRecover` and `stages` fields use the same
types as a drill report, so they can be pasted into one.

## `dr extcheck`

```bash
k8scli dr extcheck --plan k8scli-dr.yaml                  # against the plan's drContext, or against itself
k8scli dr extcheck -n shop --to dr-cluster --expiry-days 14
k8scli dr extcheck --plan k8scli-dr.yaml --fail-on high   # exit 1 when a high finding exists (CI)
k8scli dr extcheck --plan k8scli-dr.yaml -o json
```

With a DR context (`--to`, or `drContext` in the plan), references are checked against **that** cluster: this is the
useful mode, because it tells you what the restore will miss. Without one they are checked against the cluster
itself, which finds references that are already broken.

### Secrets and ConfigMaps are checked by name only

The check needs to know whether a Secret *exists*, never what is in it. The names are listed with a metadata-only
request (`PartialObjectMetadataList`), so the values do not leave the API server. A client that cannot do that falls
back to a normal list and copies the names; no value is kept or printed. Both paths are covered by tests that fail if a
value reaches a result. `--fail-on`, `-o json` and the audit trail never contain a value.

### Rules

| Id | Severity | Finding |
|----|----------|---------|
| DR101 | medium | Image in a registry bound to one region (Amazon ECR, regional Artifact Registry) |
| DR102 | high | An `imagePullSecret` is missing in the target cluster |
| DR103 | low | Anonymous Docker Hub pulls (rate limits during a mass restore) |
| DR104 | low | Private-looking registry and no pull credentials on the pod or its ServiceAccount |
| DR105 | low | Every image comes from one registry |
| DR110 | high | cert-manager Certificate is not Ready |
| DR111 | medium / high | Certificate expires within `--expiry-days` (high under 7 days or expired) |
| DR112 | high | Ingress TLS secret is missing in the target cluster |
| DR113 | medium | cert-manager is not installed in the DR cluster |
| DR120 | high | A Secret the workload cannot start without is missing in the target cluster |
| DR121 | high | A ConfigMap the workload cannot start without is missing in the target cluster |
| DR125 | high | The namespace does not exist in the DR cluster |
| DR130 | high / medium | StorageClass missing in the target cluster, or the default one is needed and there is none |
| DR131 | medium | IngressClass missing in the target cluster |
| DR132 | medium | PriorityClass missing in the target cluster |
| DR140 | high | No backup location and no volume snapshot found |
| DR141 | medium | Fewer than three copies of the data |
| DR142 | medium | Only one kind of storage |
| DR143 | medium / low | No copy is off-site (low when it cannot be verified) |
| DR144 | low | Immutability (object lock, versioning) cannot be verified from the Kubernetes API |
| DR145 | medium | A Velero backup location is unavailable |
| DR146 | medium | No Velero Schedule covers the namespaces |
| DR147 | high | A backup location is inside the cluster it protects |

Optional references (`optional: true`) are not reported. Findings are sorted by severity and are the same shape as
`dr check` findings (`rule`, `severity`, `kind`, `name`, `namespace`, `message`, `fix`).

### The 3-2-1 check

It counts the live volumes as one copy and adds each **distinct, available, off-cluster** Velero backup location, plus
one for CSI volume snapshots. Snapshots live on the same storage system as the volumes, so they do not count as a
second medium. A location counts as off-site only when its region is known and differs from the cluster's region
(from the nodes' `topology.kubernetes.io/region` label); when either is unknown it says "cannot be verified" instead of
guessing. A location served from inside the cluster (an in-cluster MinIO) is reported and not counted.

### What it cannot see

- Whether a bucket has object lock or versioning, who can delete from it, or whether an image registry is replicated:
  these are not in the Kubernetes API. The report says so.
- Region-bound registries are recognised only for Amazon ECR and regional Artifact Registry host names.
- Credentials that nodes get from the cloud account (IAM) are invisible: DR104 is a low finding for that reason.
- Whether the restored pods will actually start. Only `dr drill` proves that.
