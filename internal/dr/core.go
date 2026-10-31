// Package dr is the disaster-recovery module: a declarative plan (k8scli-dr.yaml), an engine that
// runs it as an ordered, resumable, auditable runbook, and the adapters that stop and start
// applications, databases, nodes and whole clusters.
//
// This file is the contract shared by every sub-package. Sub-packages:
//
//	dr/engine    runs a Runbook: dry run, state ledger, resume, abort, audit log, safety gates
//	dr/k8sops    applications, CronJobs and nodes (cordon/drain/uncordon), plus pod-readiness gates
//	dr/dbops     databases (CloudNativePG, generic StatefulSets) and their RPO estimates
//	dr/provider  powering nodes or clusters on and off (kind, minikube, az, aws, gcloud) through their CLIs
//	dr/measure   RTO and RPO: timelines, probes, heartbeat, drills and reports
//	dr/checks    read-only: readiness score, capacity fit, backup freshness, dependency discovery, export, diff
//	dr/wire      builds an Env and the planners for a plan (used by the CLI and the web console)
//
// Design rules (see docs/adr/0005-disaster-recovery.md):
//   - Every operation is an Action: idempotent (Done reports an already-satisfied state), describable
//     (dry run prints what would happen without touching anything) and risk-classified.
//   - Nothing destructive runs without a dry-run plan the caller has seen, the production gate,
//     and, for destructive steps, a typed confirmation phrase.
//   - Shutdown and startup are not symmetric: powering a cluster ON happens before the Kubernetes API
//     exists, so the provider phase must not need Env.Client, and "up" planners fall back to the state
//     saved by the matching "down" run (Env.State) when the cluster cannot be read yet.
package dr

import (
	"context"
	"errors"
	"time"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

// ---- Plan (k8scli-dr.yaml) -----------------------------------------------------------------------------

// PlanVersion is the only schema version understood today.
const PlanVersion = 1

// Plan describes what to protect and in which order things stop and start. It is a file the user
// owns and versions; durations are Go duration strings ("30m", "4h").
type Plan struct {
	Version int    `json:"version"`
	Name    string `json:"name"`
	// Context is the kubeconfig context the plan acts on.
	Context string `json:"context"`
	// DRContext is the standby cluster (optional): used by diff, capacity and failover.
	DRContext string     `json:"drContext,omitempty"`
	Targets   Targets    `json:"targets,omitempty"`
	Apps      []App      `json:"apps,omitempty"`
	Databases []Database `json:"databases,omitempty"`
	// Provider says how nodes or the whole cluster are powered (optional).
	Provider *Provider `json:"provider,omitempty"`
	Safety   Safety    `json:"safety,omitempty"`
}

// Targets are the default objectives, used when an app does not set its own.
type Targets struct {
	RTO string `json:"rto,omitempty"`
	RPO string `json:"rpo,omitempty"`
}

// App is a group of workloads that stops and starts together.
type App struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	// Workloads limits the app to some Deployments or StatefulSets; empty means every workload in the
	// namespace that is not a database of this plan.
	Workloads []string `json:"workloads,omitempty"`
	// Tier orders startup: tier 0 starts first and stops last. Databases usually sit in tier 0.
	Tier      int      `json:"tier,omitempty"`
	DependsOn []string `json:"dependsOn,omitempty"`
	Health    *Health  `json:"health,omitempty"`
	RTO       string   `json:"rto,omitempty"`
	RPO       string   `json:"rpo,omitempty"`
}

// Health is what "the app is back" means.
type Health struct {
	// Kind is "pods-ready" (default) or "http".
	Kind string `json:"kind,omitempty"`
	// URL and Status are for kind "http"; only http and https are accepted.
	URL     string `json:"url,omitempty"`
	Status  int    `json:"status,omitempty"`
	Timeout string `json:"timeout,omitempty"`
}

// Database is a stateful dependency with its own stop and start procedure.
type Database struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	// Kind is "cnpg" (CloudNativePG Cluster) or "statefulset".
	Kind string `json:"kind"`
	// Ref is the name of the CNPG Cluster or of the StatefulSet.
	Ref  string `json:"ref"`
	Tier int    `json:"tier,omitempty"`
	// MaxBackupAge blocks a stop when the newest backup is older ("24h"). Empty means do not check.
	MaxBackupAge string `json:"maxBackupAge,omitempty"`
	RTO          string `json:"rto,omitempty"`
	RPO          string `json:"rpo,omitempty"`
}

// Provider is the adapter that powers nodes or the whole cluster.
type Provider struct {
	// Kind is one of: kind, minikube, docker-desktop, aks, eks-nodegroup, gke-nodepool, manual.
	Kind   string            `json:"kind"`
	Params map[string]string `json:"params,omitempty"`
}

// Safety holds the plan-level guard rails.
type Safety struct {
	// LabOnly marks a context where drills and destructive runs are allowed without extra gates.
	LabOnly bool `json:"labOnly,omitempty"`
	// RequireBackupMaxAge is the default MaxBackupAge for databases that do not set one.
	RequireBackupMaxAge string `json:"requireBackupMaxAge,omitempty"`
}

// ---- Runbook ---------------------------------------------------------------------------------------------

// Direction is the way a runbook goes.
type Direction string

// Directions.
const (
	DirDown Direction = "down"
	DirUp   Direction = "up"
)

// Phase groups steps. On "down" the order is prepare, apps, databases, nodes, power; "up" is the
// reverse: power, nodes, databases, apps, prepare.
type Phase string

// Phases.
const (
	PhasePrepare   Phase = "prepare" // suspend or resume CronJobs, pre-flight checks
	PhaseApps      Phase = "apps"
	PhaseDatabases Phase = "databases"
	PhaseNodes     Phase = "nodes" // cordon, drain, uncordon
	PhasePower     Phase = "power" // provider: machines or the cluster on or off
)

// DownOrder and UpOrder are the phase sequences; UpOrder is DownOrder reversed.
var (
	DownOrder = []Phase{PhasePrepare, PhaseApps, PhaseDatabases, PhaseNodes, PhasePower}
	UpOrder   = []Phase{PhasePower, PhaseNodes, PhaseDatabases, PhaseApps, PhasePrepare}
)

// KnownPhase reports whether the phase exists in either direction.
func KnownPhase(phase Phase) bool {
	for _, order := range [][]Phase{DownOrder, UpOrder} {
		for _, p := range order {
			if p == phase {
				return true
			}
		}
	}
	return false
}

// Risk classifies an action so the engine can demand the right confirmation.
type Risk int

// Risks.
const (
	RiskLow         Risk = iota // reads, suspends, scales up
	RiskHigh                    // scales down, drains, hibernates
	RiskDestructive             // powers machines or the cluster off; data may be lost if misused
)

func (r Risk) String() string {
	switch r {
	case RiskLow:
		return "low"
	case RiskHigh:
		return "high"
	default:
		return "destructive"
	}
}

// Action is one idempotent operation.
type Action interface {
	// ID is stable across runs for the same operation on the same object, e.g. "apps/payments/api:scale-down".
	ID() string
	// Title is a short English label: "Scale payments/api to 0".
	Title() string
	// Describe is one sentence for the dry run.
	Describe() string
	Risk() Risk
	// Done reports whether the desired state already holds. The engine calls it before Apply and
	// when resuming, so an interrupted run never repeats a finished step.
	Done(ctx context.Context) (bool, error)
	Apply(ctx context.Context) error
	// Equivalent is the command a person would run for the same effect, or "".
	Equivalent() string
}

// Gate blocks until a condition holds (pods ready, URL answers) or its own timeout passes.
type Gate interface {
	Name() string
	Wait(ctx context.Context) error
}

// Step is an Action plus the optional Gate that must pass before the next step starts.
type Step struct {
	Phase  Phase
	Action Action
	Gate   Gate
	// Optional steps that fail are reported and skipped instead of failing the run.
	Optional bool
}

// Scope narrows a run.
type Scope struct {
	// Apps and Databases restrict the run to the named entries; empty means all of them.
	Apps      []string
	Databases []string
	// Phases restricts the run to some phases; empty means every phase that applies.
	Phases []Phase
}

// ErrUnreachable is returned by a Planner that cannot read the cluster yet (typically "up" while the
// cluster is still off). The engine then shows the phase as "computed when the cluster is reachable".
var ErrUnreachable = errors.New("cluster is not reachable")

// Planner turns a Plan into the steps of one phase. It is called phase by phase while the run
// progresses, not all at once, because later phases may only be computable after earlier ones.
type Planner interface {
	Phase() Phase
	Steps(ctx context.Context, env *Env, plan *Plan, dir Direction, scope Scope) ([]Step, error)
}

// Runbook is the ordered work for one run, built by the engine from the planners.
type Runbook struct {
	Plan      *Plan
	Direction Direction
	Scope     Scope
	Created   time.Time
}

// ---- Environment -----------------------------------------------------------------------------------------

// Env is what adapters may use. Everything external is an interface so tests never touch the network.
type Env struct {
	// Context is the kubeconfig context name the run acts on.
	Context string
	// Client and Dynamic may be nil, or point at an unreachable cluster, during the power phase of "up".
	Client  kubernetes.Interface
	Dynamic dynamic.Interface
	// Exec runs external programs (provider CLIs). It never uses a shell.
	Exec Exec
	// HTTP checks health URLs.
	HTTP HTTPChecker
	// State persists what "down" learned (replica counts, node names...) for the matching "up".
	State StateStore
	Now   func() time.Time
	// Sleep waits or returns ctx.Err(); tests replace it.
	Sleep func(ctx context.Context, d time.Duration) error
	Logf  func(format string, args ...any)
}

// Exec runs a program with arguments and returns its standard output.
type Exec interface {
	Run(ctx context.Context, name string, args ...string) (stdout string, err error)
}

// HTTPChecker issues a GET and returns the status code.
type HTTPChecker interface {
	Check(ctx context.Context, url string) (status int, err error)
}

// StateStore keeps small JSON documents keyed by a string such as "<context>/apps".
type StateStore interface {
	Load(ctx context.Context, key string, into any) (found bool, err error)
	Save(ctx context.Context, key string, value any) error
	Delete(ctx context.Context, key string) error
}

// ---- Running -----------------------------------------------------------------------------------------------

// RunOptions control one run.
type RunOptions struct {
	// DryRun prints the plan and changes nothing; Done is still evaluated where the cluster is reachable.
	DryRun bool
	// Resume continues the run with this id from the first step that is not done.
	Resume string
	// Confirm must match engine.ConfirmPhrase for runs that need it (production contexts, destructive steps).
	Confirm string
	// ReadOnly refuses any run that is not a dry run.
	ReadOnly bool
	// Actor is recorded in the audit log (user name or "web").
	Actor string
	// OnEvent receives progress; it must not block.
	OnEvent func(Event)
}

// Event is one line of progress and of the audit log.
type Event struct {
	Time  time.Time `json:"time"`
	RunID string    `json:"runId"`
	Seq   int       `json:"seq"`
	Actor string    `json:"actor,omitempty"`
	Step  string    `json:"step,omitempty"`
	Kind  string    `json:"kind"` // run-start, step-start, step-done, step-skip, step-fail, gate-wait, gate-ok, gate-fail, run-done, run-fail, run-abort
	Msg   string    `json:"msg,omitempty"`
	Err   string    `json:"err,omitempty"`
}

// RunStatus is where a run ended.
type RunStatus string

// Run statuses.
const (
	StatusDryRun    RunStatus = "dry-run"
	StatusRunning   RunStatus = "running"
	StatusSucceeded RunStatus = "succeeded"
	StatusFailed    RunStatus = "failed"
	StatusAborted   RunStatus = "aborted"
)

// StepStatus is where one step ended.
type StepStatus string

// Step statuses.
const (
	StepPending  StepStatus = "pending"
	StepDone     StepStatus = "done"    // applied by this run
	StepSkipped  StepStatus = "skipped" // already satisfied (Done returned true) or optional and failed
	StepFailed   StepStatus = "failed"
	StepAborted  StepStatus = "aborted"
	StepDeferred StepStatus = "deferred" // phase not computable yet (ErrUnreachable); shown in dry runs
)

// StepResult is the outcome of one step.
type StepResult struct {
	ID         string     `json:"id"`
	Title      string     `json:"title"`
	Describe   string     `json:"describe"`
	Phase      Phase      `json:"phase"`
	Risk       string     `json:"risk"`
	Equivalent string     `json:"equivalent,omitempty"`
	Status     StepStatus `json:"status"`
	Started    time.Time  `json:"started,omitempty"`
	Finished   time.Time  `json:"finished,omitempty"`
	Gate       string     `json:"gate,omitempty"`
	GateWaited string     `json:"gateWaited,omitempty"`
	Err        string     `json:"err,omitempty"`
}

// RunResult is the record of a run, also the on-disk ledger used to resume.
type RunResult struct {
	ID        string       `json:"id"`
	Plan      string       `json:"plan"`
	Context   string       `json:"context"`
	Direction Direction    `json:"direction"`
	Status    RunStatus    `json:"status"`
	Actor     string       `json:"actor,omitempty"`
	Started   time.Time    `json:"started"`
	Finished  time.Time    `json:"finished,omitempty"`
	Steps     []StepResult `json:"steps"`
}

// Runner executes runbooks. engine.Engine implements it; measure and the CLI depend only on this interface.
type Runner interface {
	// Run builds the runbook from the plan with the given planners and executes it. It blocks until the
	// run ends; cancel ctx to abort (the ledger is left consistent so Resume can continue).
	Run(ctx context.Context, env *Env, plan *Plan, dir Direction, scope Scope, planners []Planner, opts RunOptions) (*RunResult, error)
	// Status returns a stored run, or false if the id is unknown.
	Status(ctx context.Context, id string) (*RunResult, bool, error)
	// List returns stored runs, newest first.
	List(ctx context.Context, limit int) ([]RunResult, error)
}

// ---- Errors ----------------------------------------------------------------------------------------------

// Sentinel errors engines and adapters return so callers can map them to messages and HTTP codes.
var (
	ErrConfirmationRequired = errors.New("confirmation required")
	ErrReadOnly             = errors.New("read-only: only dry runs are allowed")
	ErrRunInProgress        = errors.New("another run is already in progress for this context")
	ErrPreflight            = errors.New("pre-flight check failed")
)
