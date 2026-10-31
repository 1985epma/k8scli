package k8sops

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	"github.com/1985epma/k8scli/internal/dr"
)

// PreparePlanner suspends CronJobs before the apps stop and resumes them after the apps are back.
//
// Down: every CronJob of the namespaces of the plan's apps that is not already suspended is suspended, and
// exactly those are written to the DR state. A CronJob annotated k8scli.io/dr-keep-running=true, or listed
// in Options.ExcludeCronJobs, is left alone (a backup job, for example). Up: exactly the CronJobs in the
// state are resumed, whatever else is suspended in the cluster stays suspended.
type PreparePlanner struct {
	opts Options
	mem  *memStore
}

// NewPreparePlanner returns the prepare planner.
func NewPreparePlanner(opts Options) *PreparePlanner {
	return &PreparePlanner{opts: opts.withDefaults(), mem: newMemStore()}
}

// Phase implements dr.Planner.
func (p *PreparePlanner) Phase() dr.Phase { return dr.PhasePrepare }

// cronJobRef identifies a CronJob in the DR state.
type cronJobRef struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

func (r cronJobRef) String() string { return r.Namespace + "/" + r.Name }

// Steps implements dr.Planner.
func (p *PreparePlanner) Steps(ctx context.Context, env *dr.Env, plan *dr.Plan, dir dr.Direction, scope dr.Scope) ([]dr.Step, error) {
	if env == nil {
		return nil, fmt.Errorf("the prepare planner needs an Env")
	}
	namespaces, err := scopedNamespaces(plan, scope)
	if err != nil || len(namespaces) == 0 {
		return nil, err
	}
	state := newLedger(env, p.mem, "cronjobs")
	c := clockOf(env)

	if dir == dr.DirUp {
		recorded, _, err := readState[[]cronJobRef](ctx, state)
		if err != nil {
			return nil, fmt.Errorf("read the DR state: %w", err)
		}
		var steps []dr.Step
		for _, ref := range recorded {
			if !contains(namespaces, ref.Namespace) {
				continue
			}
			steps = append(steps, dr.Step{Phase: dr.PhasePrepare, Action: &resumeCronJob{cronJobAction{client: env.Client, ref: ref, state: state, c: c}}})
		}
		return steps, nil
	}

	if env.Client == nil {
		return nil, fmt.Errorf("%w: no Kubernetes client is configured", dr.ErrUnreachable)
	}
	var steps []dr.Step
	for _, namespace := range namespaces {
		list, err := env.Client.BatchV1().CronJobs(namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			if isUnreachable(err) {
				return nil, fmt.Errorf("%w: list CronJobs in %s: %w", dr.ErrUnreachable, namespace, err)
			}
			return nil, fmt.Errorf("list CronJobs in namespace %s: %w", namespace, err)
		}
		for i := range list.Items {
			cj := &list.Items[i]
			ref := cronJobRef{Namespace: namespace, Name: cj.Name}
			if p.keepsRunning(cj, ref) {
				c.logf("leaving CronJob %s running (kept by annotation or option)", ref)
				continue
			}
			if isSuspended(cj) {
				continue // someone else suspended it: not ours to resume later
			}
			steps = append(steps, dr.Step{Phase: dr.PhasePrepare, Action: &suspendCronJob{cronJobAction{client: env.Client, ref: ref, state: state, c: c}}})
		}
	}
	return steps, nil
}

func (p *PreparePlanner) keepsRunning(cj *batchv1.CronJob, ref cronJobRef) bool {
	if cj.Annotations[AnnotationKeepRunning] == "true" {
		return true
	}
	for _, excluded := range p.opts.ExcludeCronJobs {
		if excluded == ref.String() {
			return true
		}
	}
	return false
}

func isSuspended(cj *batchv1.CronJob) bool { return cj.Spec.Suspend != nil && *cj.Spec.Suspend }

// scopedNamespaces returns the sorted, unique namespaces of the apps the run covers.
func scopedNamespaces(plan *dr.Plan, scope dr.Scope) ([]string, error) {
	want := map[string]bool{}
	for _, name := range scope.Apps {
		want[name] = true
	}
	known := map[string]bool{}
	seen := map[string]bool{}
	var out []string
	for _, app := range plan.Apps {
		known[app.Name] = true
		if len(want) > 0 && !want[app.Name] {
			continue
		}
		if !seen[app.Namespace] {
			seen[app.Namespace] = true
			out = append(out, app.Namespace)
		}
	}
	for name := range want {
		if !known[name] {
			return nil, fmt.Errorf("scope names the app %q, which is not in the plan", name)
		}
	}
	sort.Strings(out)
	return out, nil
}

func contains(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

// cronJobAction is what suspend and resume share.
type cronJobAction struct {
	client kubernetes.Interface
	ref    cronJobRef
	state  ledger
	c      clock
}

func (a cronJobAction) setSuspend(ctx context.Context, suspend bool) error {
	patch, err := json.Marshal(map[string]any{"spec": map[string]any{"suspend": suspend}})
	if err != nil {
		return err
	}
	return retryOnConflict(ctx, a.c.sleep, "update CronJob "+a.ref.Namespace+"/"+a.ref.Name, func() error {
		_, err := a.client.BatchV1().CronJobs(a.ref.Namespace).Patch(ctx, a.ref.Name, types.MergePatchType, patch, metav1.PatchOptions{})
		return err
	})
}

func (a cronJobAction) kubectlPatch(suspend bool) string {
	return fmt.Sprintf("kubectl patch cronjob %s -n %s -p '{\"spec\":{\"suspend\":%t}}'", a.ref.Name, a.ref.Namespace, suspend)
}

// suspendCronJob suspends one CronJob and records it.
type suspendCronJob struct{ cronJobAction }

func (a *suspendCronJob) ID() string {
	return fmt.Sprintf("prepare/%s/%s:suspend-cronjob", a.ref.Namespace, a.ref.Name)
}
func (a *suspendCronJob) Title() string      { return "Suspend CronJob " + a.ref.String() }
func (a *suspendCronJob) Risk() dr.Risk      { return dr.RiskLow }
func (a *suspendCronJob) Equivalent() string { return a.kubectlPatch(true) }

func (a *suspendCronJob) Describe() string {
	return fmt.Sprintf("Suspends CronJob %s and records it, so \"up\" resumes this one and only the ones suspended by this run.", a.ref)
}

func (a *suspendCronJob) Done(ctx context.Context) (bool, error) {
	cj, err := a.client.BatchV1().CronJobs(a.ref.Namespace).Get(ctx, a.ref.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return true, nil // it is gone: nothing to suspend
	}
	if err != nil {
		return false, err
	}
	return isSuspended(cj), nil
}

func (a *suspendCronJob) Apply(ctx context.Context) error {
	// Record first: a crash after the patch must still let "up" resume it.
	if err := modifyState(ctx, a.state, func(list *[]cronJobRef) bool {
		for _, ref := range *list {
			if ref == a.ref {
				return true
			}
		}
		*list = append(*list, a.ref)
		return true
	}); err != nil {
		return fmt.Errorf("record CronJob %s in the DR state: %w", a.ref, err)
	}
	if err := a.setSuspend(ctx, true); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("suspend CronJob %s: %w", a.ref, err)
	}
	a.c.logf("suspended CronJob %s", a.ref)
	return nil
}

// resumeCronJob resumes one CronJob that "down" suspended.
type resumeCronJob struct{ cronJobAction }

func (a *resumeCronJob) ID() string {
	return fmt.Sprintf("prepare/%s/%s:resume-cronjob", a.ref.Namespace, a.ref.Name)
}
func (a *resumeCronJob) Title() string      { return "Resume CronJob " + a.ref.String() }
func (a *resumeCronJob) Risk() dr.Risk      { return dr.RiskLow }
func (a *resumeCronJob) Equivalent() string { return a.kubectlPatch(false) }

func (a *resumeCronJob) Describe() string {
	return fmt.Sprintf("Resumes CronJob %s, which the matching \"down\" suspended, and clears it from the DR state.", a.ref)
}

// Done is true once the CronJob is no longer in the DR state: a record left behind still has to be cleared,
// even when someone resumed the CronJob by hand.
func (a *resumeCronJob) Done(ctx context.Context) (bool, error) {
	list, _, err := readState[[]cronJobRef](ctx, a.state)
	if err != nil {
		return false, err
	}
	for _, ref := range list {
		if ref == a.ref {
			return false, nil
		}
	}
	return true, nil
}

func (a *resumeCronJob) Apply(ctx context.Context) error {
	cj, err := a.client.BatchV1().CronJobs(a.ref.Namespace).Get(ctx, a.ref.Name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		a.c.logf("CronJob %s no longer exists; dropping it from the DR state", a.ref)
	case err != nil:
		return err
	case isSuspended(cj):
		if err := a.setSuspend(ctx, false); err != nil {
			return fmt.Errorf("resume CronJob %s: %w", a.ref, err)
		}
		a.c.logf("resumed CronJob %s", a.ref)
	}
	return modifyState(ctx, a.state, func(list *[]cronJobRef) bool {
		kept := (*list)[:0]
		for _, ref := range *list {
			if ref != a.ref {
				kept = append(kept, ref)
			}
		}
		*list = kept
		return len(kept) > 0
	})
}
