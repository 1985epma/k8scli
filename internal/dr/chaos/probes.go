package chaos

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/1985epma/k8scli/internal/dr"
	"github.com/1985epma/k8scli/internal/dr/k8sops"
)

// Probe answers one question during an experiment: is this part of the system healthy right now? It is
// polled, never blocking, so the experiment can time when the answer changes.
type Probe interface {
	Name() string
	Check(ctx context.Context) (healthy bool, detail string, err error)
}

// PodsReadyProbe is healthy while every workload has as many ready replicas as it wants.
func PodsReadyProbe(env *dr.Env, name string, refs []k8sops.WorkloadRef) Probe {
	return &podsReady{env: env, name: name, refs: refs}
}

type podsReady struct {
	env  *dr.Env
	name string
	refs []k8sops.WorkloadRef
}

func (p *podsReady) Name() string { return p.name }

func (p *podsReady) Check(ctx context.Context) (bool, string, error) {
	if p.env == nil || p.env.Client == nil {
		return false, "", fmt.Errorf("%w: no Kubernetes client is configured", dr.ErrUnreachable)
	}
	var waiting []string
	for _, ref := range p.refs {
		desired, ready, err := replicasOf(ctx, p.env, ref)
		if err != nil {
			return false, "", fmt.Errorf("%s: %w", ref, err)
		}
		if ready < desired {
			waiting = append(waiting, fmt.Sprintf("%s %d/%d ready", ref, ready, desired))
		}
	}
	if len(waiting) > 0 {
		return false, strings.Join(waiting, ", "), nil
	}
	return true, "", nil
}

func replicasOf(ctx context.Context, env *dr.Env, ref k8sops.WorkloadRef) (desired, ready int32, err error) {
	if ref.Kind == "StatefulSet" {
		s, err := env.Client.AppsV1().StatefulSets(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
		if err != nil {
			return 0, 0, err
		}
		return int32Or(s.Spec.Replicas, 1), s.Status.ReadyReplicas, nil
	}
	d, err := env.Client.AppsV1().Deployments(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
	if err != nil {
		return 0, 0, err
	}
	return int32Or(d.Spec.Replicas, 1), d.Status.ReadyReplicas, nil
}

func int32Or(p *int32, fallback int32) int32 {
	if p == nil {
		return fallback
	}
	return *p
}

// HTTPProbe is healthy while the URL answers with the wanted status (200 when zero).
func HTTPProbe(env *dr.Env, name, url string, want int) Probe {
	if want == 0 {
		want = http.StatusOK
	}
	return &httpProbe{env: env, name: name, url: url, want: want}
}

type httpProbe struct {
	env  *dr.Env
	name string
	url  string
	want int
}

func (p *httpProbe) Name() string { return p.name }

func (p *httpProbe) Check(ctx context.Context) (bool, string, error) {
	if p.env == nil || p.env.HTTP == nil {
		return false, "", fmt.Errorf("%w: no HTTP checker is configured", dr.ErrPreflight)
	}
	checkCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	status, err := p.env.HTTP.Check(checkCtx, p.url)
	if err != nil {
		return false, "request failed: " + err.Error(), nil
	}
	if status != p.want {
		return false, fmt.Sprintf("%s answered %d, want %d", p.url, status, p.want), nil
	}
	return true, "", nil
}

// ProbesFromPlan builds one probe per app of the plan, following the app's own health definition. An
// empty namespace means every app; names restrict to those apps.
func ProbesFromPlan(ctx context.Context, env *dr.Env, plan *dr.Plan, namespace string, names []string) ([]Probe, error) {
	if plan == nil || env == nil {
		return nil, fmt.Errorf("%w: a plan and an environment are required", dr.ErrPreflight)
	}
	wanted := map[string]bool{}
	for _, n := range names {
		wanted[n] = true
	}
	var probes []Probe
	for i := range plan.Apps {
		app := &plan.Apps[i]
		if (namespace != "" && app.Namespace != namespace) || (len(wanted) > 0 && !wanted[app.Name]) {
			continue
		}
		if app.Health != nil && app.Health.Kind == "http" {
			probes = append(probes, HTTPProbe(env, "app "+app.Name, app.Health.URL, app.Health.Status))
			continue
		}
		refs, err := appWorkloads(ctx, env, plan, app)
		if err != nil {
			return nil, err
		}
		if len(refs) == 0 {
			continue
		}
		probes = append(probes, PodsReadyProbe(env, "app "+app.Name, refs))
	}
	return probes, nil
}

// appWorkloads resolves the workloads of an app: the named ones, or every Deployment and StatefulSet of
// its namespace that is not a database of the plan.
func appWorkloads(ctx context.Context, env *dr.Env, plan *dr.Plan, app *dr.App) ([]k8sops.WorkloadRef, error) {
	var refs []k8sops.WorkloadRef
	if len(app.Workloads) > 0 {
		for _, name := range app.Workloads {
			_, err := env.Client.AppsV1().Deployments(app.Namespace).Get(ctx, name, metav1.GetOptions{})
			switch {
			case err == nil:
				refs = append(refs, k8sops.WorkloadRef{Kind: "Deployment", Namespace: app.Namespace, Name: name})
			case apierrors.IsNotFound(err):
				if _, serr := env.Client.AppsV1().StatefulSets(app.Namespace).Get(ctx, name, metav1.GetOptions{}); serr != nil {
					return nil, fmt.Errorf("workload %s/%s of app %s was not found", app.Namespace, name, app.Name)
				}
				refs = append(refs, k8sops.WorkloadRef{Kind: "StatefulSet", Namespace: app.Namespace, Name: name})
			default:
				return nil, fmt.Errorf("read workload %s/%s: %w", app.Namespace, name, err)
			}
		}
		return refs, nil
	}

	isDatabase := map[string]bool{}
	for _, db := range plan.Databases {
		if db.Namespace == app.Namespace {
			isDatabase[db.Ref] = true
		}
	}
	deployments, err := env.Client.AppsV1().Deployments(app.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list deployments in %s: %w", app.Namespace, err)
	}
	for i := range deployments.Items {
		if name := deployments.Items[i].Name; !isDatabase[name] {
			refs = append(refs, k8sops.WorkloadRef{Kind: "Deployment", Namespace: app.Namespace, Name: name})
		}
	}
	sets, err := env.Client.AppsV1().StatefulSets(app.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list statefulsets in %s: %w", app.Namespace, err)
	}
	for i := range sets.Items {
		if name := sets.Items[i].Name; !isDatabase[name] {
			refs = append(refs, k8sops.WorkloadRef{Kind: "StatefulSet", Namespace: app.Namespace, Name: name})
		}
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].String() < refs[j].String() })
	return refs, nil
}
