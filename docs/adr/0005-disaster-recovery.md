# ADR 0005: Disaster recovery module

- Status: accepted
- Date: 2026-10-31
- Amends: ADR 0001, ADR 0003

## Context

People who run clusters with k8scli asked for help with the day nobody wants: stopping and starting
applications, databases, nodes and whole clusters in a safe order, and knowing how long recovery takes
(RTO) and how much data is lost (RPO). Today that is a wiki page and a lot of `kubectl`.

Three facts shape the design:

1. k8scli is a client. It has no resident process, so it cannot schedule backups or watch for failures.
   It can observe, check, export and orchestrate what the cluster already has (operators, snapshots, the
   cloud provider).
2. A cluster cannot be started through its own API. Powering on is a provider operation (a container,
   a VM, a node group, `az aks start`); only afterwards does the Kubernetes API exist again.
3. A shutdown that stops halfway is worse than no shutdown. Long operations must be resumable and
   leave a record.

ADR 0001 treats node operations and anything like `delete` as out of scope; ADR 0003 limits writes to a
short list of guided actions. This module goes beyond both, so it needs its own rules.

## Decision

Disaster recovery is a **plan file plus an engine that runs it as an ordered, resumable, audited
runbook**. It is not a generic automation tool.

### 1. One declarative source: the plan

`k8scli-dr.yaml` (schema version 1) is owned and versioned by the user. It names the kubeconfig
`context`, an optional standby `drContext`, default `targets` (RTO and RPO), the `apps` (namespace,
optional workload names, `tier`, `dependsOn`, a `health` definition, per-app RTO and RPO), the
`databases` (`cnpg` or `statefulset`, `ref`, `tier`, `maxBackupAge`), the `provider` that powers nodes
or the cluster, and `safety` (`labOnly`, `requireBackupMaxAge`). It is parsed strictly: unknown fields
and invalid values are errors, provider parameters are checked against an allowlist, and `dependsOn`
cycles are rejected. The plan never contains credentials or Secret values.

### 2. Engine rules

1. **Actions are idempotent.** Every operation is an `Action` with `Done` (is the desired state already
   true?) and `Apply`. Finished work is never repeated, which is also what makes resume safe.
2. **Dry run first.** Every action describes itself and gives the equivalent command. A dry run lists the
   ordered steps and what each would do, and changes nothing.
3. **Risk classes.** `low` (suspend, scale up), `high` (scale down, drain, hibernate) and `destructive`
   (power machines or the cluster off).
4. **Resumable ledger.** The ledger of a run is written after every step (atomic replace). `resume <id>`
   re-plans and continues from the first step that is not done. Cancelling the context aborts the run
   and leaves the ledger consistent.
5. **Audit log.** Every run and step event is appended to an append-only JSONL log with the actor, the
   plan, the context and the outcome.
6. **Confirmation phrase.** A real run in a production-looking context (name contains `prod`, as in the
   CLI and the console) needs the context name typed. A shutdown that includes the power phase needs
   `stop <context>`. Dry runs never need it.
7. **Read-only.** `--read-only` refuses every run that is not a dry run.
8. **One run per context.** A lock file with the process id and time prevents two runs on the same
   context; a stale lock is detected.

### 3. Phases and order

Shutdown (`down`) goes `prepare` (suspend CronJobs), `apps`, `databases`, `nodes` (cordon and drain),
`power`. Startup (`up`) is the exact reverse: `power`, `nodes`, `databases`, `apps`, `prepare`.
Inside `apps` and `databases`, tier 0 starts first and stops last, and `dependsOn` adds ordering
constraints. After each tier a health gate (pods ready, or an HTTP check) must pass before the next
tier starts. A scope can restrict a run to some apps, databases or phases.

### 4. Powering on cannot use the Kubernetes API

The `power` phase of `up` runs before any client could work, so providers must not depend on the
cluster client. Planners are called phase by phase while the run progresses, not all at once. A planner
that cannot read the cluster yet reports `ErrUnreachable`: in a dry run its phase is shown as
"computed when the cluster is reachable", in a real run the engine retries until a timeout.
What `down` learns (replica counts, nodes it cordoned, CronJobs it suspended, node group sizes) is
saved twice, as annotations on the objects and in a local state store, so `up` still has it when the
cluster cannot be read. `up` refuses to scale a workload when the previous replica count is unknown: it
never guesses.

### 5. Providers

| Provider | Power off | Power on | Notes |
|----------|-----------|----------|-------|
| `kind` | `docker stop` workers, then control plane | `docker start` control plane, then workers | The lab. Nodes are containers |
| `minikube` | `minikube stop -p <profile>` | `minikube start -p <profile>` | |
| `aks` | `az aks stop` | `az aks start` | Needs the Azure CLI signed in |
| `eks-nodegroup` | Scale the managed node group to 0 | Restore the saved size | The EKS control plane keeps running and billing |
| `gke-nodepool` | Resize the node pool to 0 | Restore the saved size | Standard clusters only |
| `manual` | Prints what to do | Prints what to do | Done is decided by API reachability; `resume` continues after a person acts |
| `docker-desktop` | Not supported | Not supported | No CLI or API; a manual step explains how to use the Docker Desktop settings |

Providers run the vendor CLI through a fixed allowlist of program names, never a shell, with every
parameter validated and passed as a separate argument. Cloud providers are late additions; the local
ones come first.

### 6. Database policy

- **Operators over raw StatefulSets.** CloudNativePG is the first supported operator: stopping a cluster
  is its declarative hibernation (`cnpg.io/hibernation=on`, back with `off`), and its status gives backup
  and recoverability times without any database credentials.
- **A plain StatefulSet** is supported but called out: it is scaled down in reverse ordinal order with no
  application-level checkpoint, so a logical backup is recommended.
- **Never a blind scale-to-zero.** A database stop has a pre-flight: all replicas Ready and, when
  `maxBackupAge` (or `safety.requireBackupMaxAge`) is set, a backup younger than that. If it fails, the run
  stops before touching anything.
- Switchover and promotion are not offered until there is a declarative, documented way to do them
  safely. The module does not patch status subresources.

### 7. Measuring RTO and RPO

- **RTO is measured**, in a drill: from the moment the disaster is declared (T0) until every app's health
  gate has passed. The report has the timeline of each step and gate, the RTO per app and overall, and
  compares them with the targets.
- **RPO is estimated or measured, and the report says which.** Estimated from the newest backup or
  snapshot, from the operator status (backup, WAL archive) or from replication lag. Measured with a
  heartbeat: a small writer records a timestamp on the data volume, and after recovery the newest
  timestamp found gives the real loss, `T0 - heartbeat`. A passing score never claims more than what
  was observed.
- Reports are local files (JSON and Markdown) and can be exported as evidence. Scheduled drills run in
  CI, not in a console process.

### 8. The web console

The console runs the same engine with the same rules. It only offers plans from the user's DR
directory, requires a dry run before a run, allows one active run per context, shows progress live and
has an abort button, and `--read-only` leaves only dry runs. The terminal's current context is never
changed.

### Still out of scope

- Automatic failover or failure detection (no resident process).
- Switching DNS, load balancers or any traffic.
- etcd snapshots and anything that needs access to the control-plane machines.
- Restoring into a production context without a drill in a lab first. The module will not offer a
  one-click production restore.
- Reading Secret values. The plan, the reports and the export never include them; an export lists Secret
  names only.
- Deleting namespaces, volumes or backups.

## Threat model

| Risk | Control |
|------|---------|
| A destructive run by mistake | Dry run lists every step; a typed phrase for production and for power off; one run per context |
| A run that stops halfway | Ledger after every step, `resume`, idempotent actions, abort leaves a consistent state |
| Stopping a database without a usable backup | Pre-flight on backup age; refusal rather than a warning |
| Guessing replica counts on startup | `up` refuses when the saved count is missing |
| Command injection through the plan | Strict schema, allowlisted provider parameters, validated again in the provider, no shell, program allowlist |
| A plan or a request aimed at the wrong cluster | The context is part of the plan and of every confirmation phrase |
| Secrets leaking into plans, reports or exports | No Secret values are read; Secret fields never appear in output |
| Making a stopped cluster look healthy | Reports separate measured from estimated and show failures as failures |
| Tampering with the history | Append-only audit log with fsync; local files with `0600` permissions |

## Consequences

- The blast radius of a mistake is large: this module can stop a cluster. The rules above are the
  product, not extras, and every new adapter must follow them and ship with unit tests against fakes.
- Drills belong in a lab. The repository ships a three-node kind lab (`scripts/dr-lab.sh`) so node, database
  and shutdown behavior can be tested for real. Docker Desktop's single node cannot.
- Provider and operator integrations are a maintenance cost: CRDs and CLIs change. Adapters detect
  what is installed and fail with a clear message instead of guessing.
- A plan file is another artifact to keep in step with the cluster. `dr check` and `dr diff` exist to
  show the drift.
- Requests for failover automation, traffic switching or one-click production restore are declined by
  pointing to this ADR.

## Cut from the first version

The first version covers the plan file, the engine, stopping and starting applications, databases, nodes
and clusters, RTO and RPO measurement (drills), read-only checks, the CLI, the console API and the
Resilience page. These were designed, partly drafted, and deliberately left out; each has an issue:

| Item | Issue |
|------|-------|
| Guided Velero backup and staged restore (`recovery:` section, `recover` phase) | [#4](https://github.com/1985epma/k8scli/issues/4) |
| Failover and failback to a standby cluster | [#5](https://github.com/1985epma/k8scli/issues/5) |
| Audit evidence pack and scheduled automation templates | [#6](https://github.com/1985epma/k8scli/issues/6) |
| CloudNativePG switchover and replica-cluster promotion | [#7](https://github.com/1985epma/k8scli/issues/7) |
| Live validation of AKS, EKS node groups, GKE node pools and minikube | [#8](https://github.com/1985epma/k8scli/issues/8) |
| Drills under partial failure (crash mid-run, node loss, resume) | [#9](https://github.com/1985epma/k8scli/issues/9) |
| Four-eyes approval for production runs | [#10](https://github.com/1985epma/k8scli/issues/10) |
| Webhook notifications with an allow-list | [#11](https://github.com/1985epma/k8scli/issues/11) |
| Continuous restore tests | [#12](https://github.com/1985epma/k8scli/issues/12) |
| SQL heartbeat for database RPO | [#13](https://github.com/1985epma/k8scli/issues/13) |

Until backup and restore exist, a drill proves that workloads and databases stop and start in the right
order and measures how long that takes. It does not prove that data can be restored from a backup.

