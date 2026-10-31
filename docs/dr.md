# Disaster recovery with k8scli

The disaster-recovery (DR) module stops and starts applications, databases, nodes and whole
clusters in a safe order, and measures how long recovery takes (RTO) and how much data is lost
(RPO). Design and rules: [ADR 0005](adr/0005-disaster-recovery.md).

> **Status.** The `k8scli dr ...` commands described here exist and are tested against fake clusters and
> the kind lab. The web console page is documented in [web-console.md](web-console.md). Powering machines
> off and on is verified for kind only; the AKS, EKS and GKE adapters are tested with a fake command
> runner, not against a cloud account.

## What it is, and what it is not

k8scli is a client that runs on your machine. It has no background process, so it **cannot schedule
backups or notice a failure by itself**. What it does:

- **Checks** how ready a namespace is for a disaster (read-only).
- **Exports** and **compares** cluster state (read-only).
- **Stops and starts** apps, databases, nodes and clusters from a plan, with a dry run, a resumable
  record and an audit log.
- **Rehearses** recovery in a drill and **measures** the RTO, and estimates or measures the RPO.

What it does not do: take or store backups (use Velero, your database operator or your cloud),
switch DNS or traffic, fail over automatically, read Secret values, or restore into production without
a drill first.

> **What a drill proves.** Without guided backup and restore (see [Not in this version](#not-in-this-version)),
> a drill proves that the plan stops and starts in order and how long that takes. It does **not** restore
> data from a backup, so it does not prove that a restore works.

## Concepts

| Term | Meaning |
|------|---------|
| **Plan** | `k8scli-dr.yaml`: what to protect, in which order, and the objectives. You own it and version it |
| **Tier** | Order of startup. Tier 0 starts first and stops last (databases are usually tier 0) |
| **Phase** | A stage of a run. `down`: prepare, apps, databases, nodes, power. `up`: the reverse |
| **Action** | One idempotent operation (scale `shop/api` to 0, hibernate a database, drain a node) |
| **Gate** | A condition that must hold before the next step: pods ready, or an HTTP check |
| **Dry run** | Lists every step and the equivalent command. Changes nothing |
| **Run** | One execution. It has an id, a ledger written after every step, and audit events |
| **Resume** | Continue an interrupted run from the first step that is not done |
| **Drill** | A rehearsal that stops and restarts, and records a timeline, the RTO and the RPO |
| **RTO** | Recovery time: from the moment of the disaster until the service is validated healthy |
| **RPO** | Recovery point: how much recent data is lost, in time |

### Stopping and starting are not mirror images

Stopping goes `prepare` (suspend CronJobs), `apps` (highest tier first), `databases`, `nodes` (cordon
and drain), `power`. Starting goes the other way: `power`, `nodes`, `databases`, `apps` (tier 0 first),
`prepare`.

Powering a cluster **on** cannot use the Kubernetes API, because it does not exist yet. The `power`
phase talks to the provider (Docker, `az`, a node group) and then waits for the API. Whatever the
shutdown learned (replica counts, nodes it cordoned, CronJobs it suspended) was saved on the objects
and in a local state store, so startup can use it even when the cluster is still unreachable. If the
replica count of a workload is unknown, startup **refuses** to scale it rather than guess.

## Quick start with the lab

The repository ships a three-node kind cluster with a small sample system, so you can try everything
without touching a real cluster. You need Docker, `kind` and `kubectl`.

```bash
scripts/dr-lab.sh up            # cluster k8scli-lab (context kind-k8scli-lab) and the sample system
scripts/dr-lab.sh status        # nodes, workloads, database state, memory
scripts/dr-lab.sh check-data    # write a heartbeat row to the database and read the newest back
```

The lab is namespace `shop`: tier 0 `shop-db` (CloudNativePG, 2 instances) and `legacy-db` (a plain
StatefulSet, 2 replicas), tier 1 `api`, tier 2 `web` (with an HPA) and a CronJob. The ready-made plan
is [examples/k8scli-dr.lab.yaml](examples/k8scli-dr.lab.yaml). Delete the lab with
`scripts/dr-lab.sh down`.

Docker Desktop's built-in Kubernetes has one node, so draining and powering nodes only make sense in
the lab (or another multi-node cluster).

## The plan file

A complete, commented template is in [examples/k8scli-dr.example.yaml](examples/k8scli-dr.example.yaml).
The file is parsed strictly: unknown fields and invalid values are errors.

### Top level

| Field | Required | Meaning |
|-------|----------|---------|
| `version` | yes | Schema version. Only `1` exists |
| `name` | yes | Name of the plan. Used in runs and reports |
| `context` | yes | The kubeconfig context the plan acts on. A name containing `prod` is production |
| `drContext` | no | The standby cluster, for `dr diff` and the capacity check |
| `targets` | no | Default objectives (`rto`, `rpo`) |
| `apps` | no | The applications (below) |
| `databases` | no | The databases (below) |
| `provider` | no | How nodes or the cluster are powered |
| `safety` | no | Guard rails |

### `targets`, `safety`

| Field | Meaning |
|-------|---------|
| `targets.rto` | Default recovery time objective, for example `1h` |
| `targets.rpo` | Default recovery point objective, for example `15m` |
| `safety.labOnly` | `true` marks a lab context where drills may run without extra gates |
| `safety.requireBackupMaxAge` | Default `maxBackupAge` for databases that do not set one |

### `apps[]`

| Field | Meaning |
|-------|---------|
| `name` | Unique name of the app |
| `namespace` | Namespace of its workloads |
| `workloads` | Names of the Deployments or StatefulSets in the app. Empty means every workload in the namespace that is not a database of this plan |
| `tier` | Startup order. `0` starts first and stops last. Default `0` |
| `dependsOn` | Names of other apps that must be healthy first. No cycles |
| `health` | What "the app is back" means (below) |
| `rto`, `rpo` | The app's own objectives, overriding `targets` |

`health`:

| Field | Meaning |
|-------|---------|
| `kind` | `pods-ready` (default: every workload has all replicas ready) or `http` |
| `url` | For `http`: the address to GET. Only `http` and `https` are accepted |
| `status` | For `http`: the status code that means healthy |
| `timeout` | How long to wait before the gate fails, for example `10m` |

### `databases[]`

| Field | Meaning |
|-------|---------|
| `name` | Unique name |
| `namespace` | Namespace |
| `kind` | `cnpg` (a CloudNativePG `Cluster`) or `statefulset` |
| `ref` | Name of the CNPG `Cluster` or of the StatefulSet |
| `tier` | Startup order, as for apps |
| `maxBackupAge` | A stop is refused when the newest backup is older, for example `24h`. Empty means no check |
| `rto`, `rpo` | The database's own objectives |

### `provider`

| `kind` | `params` | Notes |
|--------|----------|-------|
| `kind` | `name` | Nodes are Docker containers. Used by the lab |
| `minikube` | `profile` | |
| `aks` | `resourceGroup`, `name`, `subscription` (optional) | Uses the Azure CLI |
| `eks-nodegroup` | `cluster`, `nodegroup`, `region`, `profile` (optional) | Scales a managed node group. The EKS control plane keeps running |
| `gke-nodepool` | `cluster`, `nodepool`, `location`, `project` | Resizes a node pool |
| `docker-desktop` | none | Cannot be powered by a command; a manual step explains it |
| `manual` | none | The run stops with instructions; `resume` continues after you act |

Parameter values are validated: plain names only, no leading `-`, no spaces or control characters.

## Commands

Everything that changes something needs `--execute`. Without it a command only shows what it would do and
exits 0. `-n` takes one namespace, or a comma-separated list for the read-only commands. The global
`--kubeconfig` flag is honoured. There is no `--context` flag outside `dr`; inside it, commands that act on
a cluster take `--context` (default: the plan's context, then the kubeconfig's current one).

| Command | What it does | Changes the cluster |
|---------|--------------|---------------------|
| `k8scli dr plan validate [FILE]` | Checks the plan and lists every problem at once | No |
| `k8scli dr plan show [FILE] [-o json]` | Order of stopping and starting, targets, provider and the confirmation phrases | No |
| `k8scli dr check [-n ns] [--plan f] [--capacity-to ctx] [--suggest-tiers] [--min-severity s] [--fail-on s]` | Readiness score per namespace and what to fix | No |
| `k8scli dr export -n ns [-o dir\|file.zip]` | Clean manifests; Secrets are never written | No |
| `k8scli dr diff --to ctx -n ns` | Drift between a primary and a standby | No |
| `k8scli dr backups [-n ns]` | Velero backups, schedules, storage locations and volume snapshots | No |
| `k8scli dr down --plan f [--scope p] [--app a] [--database d] [--execute] [--confirm phrase]` | Stop apps, databases and, with a provider, nodes and machines | With `--execute` |
| `k8scli dr up --plan f [...same flags]` | Start everything again, with health gates | With `--execute` |
| `k8scli dr status [id]` / `dr runs` | A run (the latest by default) / the stored runs | No |
| `k8scli dr resume id --plan f [--execute]` | Show where a run stands; with `--execute` continue it | With `--execute` |
| `k8scli dr drill --plan f [--full] [--execute]` | Rehearse and measure the RTO and RPO | With `--execute` |
| `k8scli dr report [id] [--format md\|json\|prometheus] [--list --trend n]` | A drill report, or the history and the RTO trend | No |
| `k8scli dr nodes drain\|uncordon [--node n] [--execute]` | Cordon and drain, or uncordon, nodes | With `--execute` |
| `k8scli dr cluster stop\|start --plan f [--execute]` | The whole plan, machines included | With `--execute` |

`-o json` (or `yaml`) is available on the read-only commands and on dry runs. Data goes to standard output;
banners, prompts and progress go to standard error, so `-o json` can be piped.

### Reading: check, export, diff, backups

```bash
k8scli dr check --plan k8scli-dr.yaml --capacity-to dr-cluster --suggest-tiers
k8scli dr check -n shop,payments --min-severity high --fail-on high   # a CI gate: non-zero exit on findings
k8scli dr export -n shop                      # shop-<context>-<date>.zip in the current folder
k8scli dr diff --plan k8scli-dr.yaml          # uses the plan's context and drContext
k8scli dr backups -n shop
```

- `check` scores each namespace from 0 to 100 from risk **signals** (single replicas, no PodDisruptionBudget,
  no spread, missing probes, stale backups...). It does not prove anything can be recovered. `--capacity-to`
  adds whether the apps fit on the standby; `--suggest-tiers` infers dependencies from Services, env vars
  and network policies and prints a draft of the plan's `apps:` section to review.
- `export` writes objects without `status`, `uid`, `resourceVersion` or `managedFields` and skips objects a
  controller owns. **Secrets are not exported**; `INVENTORY.md` lists what was left out. It refuses to
  overwrite an existing `.zip`, and the file is `0600`.
- `diff` never reads ConfigMap values: only key names are compared.

### A shutdown and startup, step by step

1. **Dry run.** `k8scli dr down --plan k8scli-dr.yaml` prints every step in order with its risk, a sentence
   about what it does, the equivalent command, the gate it waits for and whether it is already done. Read
   it. The last lines show the exact command that runs it for real, with the confirmation phrase.
2. **Run it.** Add `--execute`. The plan is shown again and, when a phrase is required, you type it at the
   prompt, or pass it with `--confirm` in a script (a script without `--confirm` fails and says what to
   pass). Progress is streamed; the run stops at the first failure.
3. **If it stops,** the run prints the exact command to continue. Fix the cause and run
   `k8scli dr resume <id> --plan k8scli-dr.yaml --execute` (or `dr down ... --resume <id> --execute`). Steps
   that are done are skipped. Pass the same scope the run used.
4. **Abort.** Ctrl-C cancels the run cleanly; the ledger stays consistent and the run can be resumed.
5. **Start again.** `k8scli dr up --plan k8scli-dr.yaml --execute`. The provider powers on first, then
   nodes are uncordoned, databases start and must be healthy, then each app tier starts and its gate must
   pass.

`--scope` limits a run to some phases (`prepare`, `apps`, `databases`, `nodes`, `power`); `--app` and
`--database` limit it to named entries.

**Confirmation phrases.** None for a non-production context when only apps and databases stop. The context
name in a production-looking context (the name contains `prod`). `stop <context>` as soon as a step powers
machines off. `dr plan show` prints all three.

**An unknown replica count.** `dr up` never guesses how many replicas a stopped workload had. If neither the
annotation `k8scli.io/dr-replicas-before` nor the saved state has the number, the run stops while it is
still being planned, before anything changes, and names the workload. Set the count by hand, or pass
`--skip-unknown-replicas` to leave such workloads out (they stay stopped).

**A manual step.** Providers that cannot be driven by a CLI (Docker Desktop, `manual`) stop the run with the
instructions in the step, then `dr resume` continues after you did it.

### Nodes and the whole cluster

```bash
k8scli dr nodes drain --node worker-1                 # dry run
k8scli dr nodes drain --node worker-1 --execute
k8scli dr nodes uncordon --execute                    # uncordons exactly the nodes the drain cordoned
k8scli dr cluster stop --plan k8scli-dr.yaml --execute --confirm 'stop my-lab'
```

`nodes` works without a plan file (`--context`); control-plane nodes are never touched. `--delete-emptydir-data`
and `--force` allow evicting pods with emptyDir data or without a controller. `cluster stop|start` is
`dr down|up` over every phase; starting machines happens from outside the cluster (the provider), so
`cluster start` begins with the provider and waits for the API.

## Drills: measuring RTO and RPO

A drill runs the shutdown and the startup from a plan, records the time of every step and gate, and
writes a report. Run drills in a lab, never as a first test in production.

### RTO is measured

The clock starts when the disaster is declared (the start of the shutdown) and stops when every app's
health gate has passed. The report shows the timeline, the RTO of each app and the overall RTO, and
compares each with its target.

### RPO is estimated, or measured

| Source | What it tells you | Measured or estimated |
|--------|-------------------|-----------------------|
| Age of the newest backup or snapshot | Worst-case loss if you restore it | Estimated |
| Operator status (CloudNativePG backup and WAL archive times) | Recoverable data point | Estimated |
| Replication lag | Loss if the primary is lost now | Estimated |
| **Heartbeat** | A small writer records a timestamp on the data volume. After recovery the newest timestamp found gives the real loss: `T0 - heartbeat` | **Measured** |

In the lab, `scripts/dr-lab.sh check-data` writes a heartbeat row to `shop-db`, and
`scripts/dr-lab.sh check-data read` reads the newest one back after a restore.

### Reading a report

A report lists the plan, the context, when it ran, the timeline and, per app and database, the RTO and
RPO with their sources and a verdict against the target: **pass**, **fail** or **unknown** (when a
number could not be measured). Treat an estimate as an upper bound on what you know, not as proof: only
a drill that restored data proves that restoring works. Reports are local JSON and Markdown files that
you can keep as evidence, and the history shows the trend between drills.

## Databases

- Prefer an operator. CloudNativePG is supported: stopping a cluster is its declarative hibernation,
  starting it is the same annotation set to `off`.
- A plain StatefulSet is supported, but it is scaled down without any application-level checkpoint.
  Take a logical backup first.
- A stop is **never** a blind scale-to-zero: all replicas must be Ready and, when `maxBackupAge` is set,
  the newest backup must be younger than it.
- Managed databases outside the cluster (RDS, Cloud SQL) are not stopped by this module.

## Where files go

Under `~/.k8scli/dr/`: `runs/<id>.json` (the ledger of each run), `locks/` (one run per context at a
time), `audit.jsonl` (append-only), `state/` (what a shutdown learned, for the matching startup) and
`reports/<id>.json` and `.md`. The files are readable only by you (`0600`, directories
`0700`).

## Not in this version

These are known gaps, tracked as issues. Until backup and restore are in, **a drill proves ordered stop and
start and their timing, not data restoration.**

| Not included | Issue |
|--------------|-------|
| Guided Velero backup and staged restore | [#4](https://github.com/1985epma/k8scli/issues/4) |
| Failover and failback between clusters | [#5](https://github.com/1985epma/k8scli/issues/5) |
| Audit evidence pack and automation templates | [#6](https://github.com/1985epma/k8scli/issues/6) |
| CloudNativePG switchover | [#7](https://github.com/1985epma/k8scli/issues/7) |
| Live validation of the cloud providers (AKS, EKS node groups, GKE node pools) | [#8](https://github.com/1985epma/k8scli/issues/8) |
| Partial-failure drills | [#9](https://github.com/1985epma/k8scli/issues/9) |
| Four-eyes approval | [#10](https://github.com/1985epma/k8scli/issues/10) |
| Webhooks | [#11](https://github.com/1985epma/k8scli/issues/11) |
| Continuous restore tests | [#12](https://github.com/1985epma/k8scli/issues/12) |
| RPO measured with an SQL heartbeat | [#13](https://github.com/1985epma/k8scli/issues/13) |

What exists today: `dr backups` and `dr check` **read** backups, schedules and snapshots (Velero and CSI),
and the RPO of a drill is estimated from them, or measured with the PVC heartbeat. Nothing in this module
creates or restores a backup.

## Troubleshooting

| Symptom | Likely cause and fix |
|---------|----------------------|
| `another run is already in progress for this context` | Another run holds the lock. Finish or abort it. A lock left by a crashed process is detected as stale |
| `confirmation required` | Type the context name (production), or `stop <context>` for a shutdown with the power phase |
| `read-only: only dry runs are allowed` | You started with `--read-only`. Dry runs still work |
| Startup refuses to scale a workload | The replica count it saved is missing (a manual change, or state removed). Scale it by hand once, then resume |
| A node drain does not finish | A PodDisruptionBudget blocks eviction, or a pod uses local storage. Fix the PDB or the workload; the drain retries until its timeout |
| The API is not reachable after power on | The provider needs more time, or a credential plugin is missing. The run waits and retries; use `resume` |
| The database will not hibernate | A recent backup is required (`maxBackupAge`), or the CloudNativePG operator is not installed |
| `docker-desktop` provider is not supported | Docker Desktop has no CLI or API for this. Use the settings in the app, or use the lab |
| The lab does not come up | Check Docker memory (8 GB total is tight), then `scripts/dr-lab.sh status` and `scripts/dr-lab.sh reset` |

## FAQ

**Does it back up my data?** No. Use Velero, your database operator or your cloud. This module checks
that backups exist and are recent, and rehearses recovery.

**Can I power off EKS or GKE?** Node groups and pools, yes: they scale to zero and back. The managed
control plane keeps running and billing, and cannot be stopped.

**Does it switch traffic to a standby cluster?** No. DNS and load-balancer changes are outside the
module. `dr diff` tells you whether the standby matches.

**Can it run on a schedule?** Not by itself: k8scli has no resident process. Run drills from CI.

**Can I run a shutdown from the web console?** Yes, with the same rules as the CLI: a dry run first, the
typed confirmation, one run per context and an abort button. `--read-only` leaves only dry runs.

## Safety checklist before a production run

- [ ] The plan names the right `context`, and you have read it twice.
- [ ] You ran the same plan as a drill in a lab, and the RTO fit its target.
- [ ] `dr down` (the dry run) was read to the end, including the databases and the power phase.
- [ ] Every database has a recent, tested backup, and `maxBackupAge` is set.
- [ ] The people who depend on the services know and have approved the window.
- [ ] You know how to start again: the provider command works from your terminal and you have the
      permissions for every step.
- [ ] CronJobs and external schedulers that could restart things mid-shutdown are accounted for.
- [ ] Someone else knows the run id and how to `resume` it (and that Ctrl-C aborts cleanly).
