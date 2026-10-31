package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/1985epma/k8scli/internal/dr"
)

const gkeNodeLabel = "cloud.google.com/gke-nodepool"

// gkeTarget is one node pool of a GKE cluster. The managed control plane keeps running; resizing the
// pool to zero stops the compute. For a regional cluster the node count is per zone.
type gkeTarget struct {
	cluster, nodepool, location, project string
}

func (t gkeTarget) identity() string {
	return strings.Join([]string{t.cluster, t.nodepool, t.location, t.project}, "/")
}

func (t gkeTarget) describeArgs() []string {
	return []string{"container", "node-pools", "describe", t.nodepool, "--cluster", t.cluster, "--location", t.location, "--project", t.project, "--format", "json"}
}

func (t gkeTarget) resizeArgs(nodes string) []string {
	return []string{"container", "clusters", "resize", t.cluster, "--node-pool", t.nodepool, "--num-nodes", nodes,
		"--location", t.location, "--project", t.project, "--quiet"}
}

type gkeNodePool struct {
	InitialNodeCount int      `json:"initialNodeCount"`
	Locations        []string `json:"locations"`
	Autoscaling      struct {
		Enabled bool `json:"enabled"`
	} `json:"autoscaling"`
}

func gkeSteps(env *dr.Env, plan *dr.Plan, dir dr.Direction) ([]dr.Step, error) {
	p := newParams(KindGKENodepool, plan.Provider.Params)
	p.rejectUnknown("cluster", "nodepool", "location", "project")
	target := gkeTarget{cluster: p.required("cluster"), nodepool: p.required("nodepool"), location: p.required("location"), project: p.required("project")}
	if p.err != nil {
		return nil, p.err
	}
	state, err := newStateRef(env, plan, KindGKENodepool, target.identity())
	if err != nil {
		return nil, err
	}
	return []dr.Step{powerStep(&gkeAction{env: env, target: target, state: state, down: dir == dr.DirDown})}, nil
}

// gkeAction resizes a node pool to zero (saving its size first) or back to the saved size.
type gkeAction struct {
	env    *dr.Env
	target gkeTarget
	state  *stateRef
	down   bool
}

func (a *gkeAction) ID() string {
	verb := "scale-up"
	if a.down {
		verb = "scale-down"
	}
	return fmt.Sprintf("power/gke/%s/%s:%s", a.target.cluster, a.target.nodepool, verb)
}

func (a *gkeAction) Title() string {
	if a.down {
		return fmt.Sprintf("Resize GKE node pool %s to 0", a.target.nodepool)
	}
	return fmt.Sprintf("Resize GKE node pool %s back up", a.target.nodepool)
}

func (a *gkeAction) Describe() string {
	if a.down {
		return fmt.Sprintf("Saves the node count of pool %s, resizes it to 0 (the control plane of %s keeps running). It refuses when the autoscaler is on.", a.target.nodepool, a.target.cluster)
	}
	return fmt.Sprintf("Resizes pool %s back to the node count saved before it was scaled to 0.", a.target.nodepool)
}

func (a *gkeAction) Risk() dr.Risk {
	if a.down {
		return dr.RiskDestructive
	}
	return dr.RiskLow
}

func (a *gkeAction) Equivalent() string {
	nodes := "0"
	if !a.down {
		nodes = "<saved nodes per zone>"
		if saved, found, err := a.state.load(context.Background()); err == nil && found {
			nodes = fmt.Sprint(saved.Nodes)
		}
	}
	return command{"gcloud", a.target.resizeArgs(nodes)}.line()
}

func (a *gkeAction) describe(ctx context.Context) (gkeNodePool, error) {
	out, err := a.env.Exec.Run(ctx, "gcloud", a.target.describeArgs()...)
	if err != nil {
		return gkeNodePool{}, fmt.Errorf("reading node pool %s: %w", a.target.nodepool, err)
	}
	var pool gkeNodePool
	if err := json.Unmarshal([]byte(out), &pool); err != nil {
		return gkeNodePool{}, fmt.Errorf("reading node pool %s: unexpected answer from gcloud: %w", a.target.nodepool, err)
	}
	return pool, nil
}

// nodeCount counts the pool's nodes through the Kubernetes API. known is false while the API is
// unreachable, in which case nothing can be concluded from the count.
func (a *gkeAction) nodeCount(ctx context.Context) (count int, known bool) {
	if !APIReachable(ctx, a.env.Client) {
		return 0, false
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	nodes, err := a.env.Client.CoreV1().Nodes().List(ctx, metav1.ListOptions{LabelSelector: gkeNodeLabel + "=" + a.target.nodepool})
	if err != nil {
		return 0, false
	}
	return len(nodes.Items), true
}

// Done: no nodes left in the pool (down) or some nodes back (up). Without API access neither can be
// proved, so the step is simply run again; resizing to the same size is harmless.
func (a *gkeAction) Done(ctx context.Context) (bool, error) {
	count, known := a.nodeCount(ctx)
	if !known {
		return false, nil
	}
	if a.down {
		return count == 0, nil
	}
	return count > 0, nil
}

func (a *gkeAction) Apply(ctx context.Context) error {
	if a.down {
		return a.scaleDown(ctx)
	}
	return a.scaleUp(ctx)
}

func (a *gkeAction) scaleDown(ctx context.Context) error {
	pool, err := a.describe(ctx)
	if err != nil {
		return err
	}
	if pool.Autoscaling.Enabled {
		return fmt.Errorf("%w: the autoscaler is enabled on node pool %s and would bring the nodes back; disable it first (`gcloud container clusters update %s --node-pool %s --no-enable-autoscaling`)",
			dr.ErrPreflight, a.target.nodepool, a.target.cluster, a.target.nodepool)
	}

	zones := max(len(pool.Locations), 1)
	count, known := a.nodeCount(ctx)
	perZone, save := 0, true
	switch {
	case known && count == 0:
		save = false // already empty: keep whatever an earlier power-off saved
	case known:
		perZone = (count + zones - 1) / zones
	default:
		// Without API access fall back to what GKE reports for the pool, unless a power-off already
		// saved the real size (a resumed run).
		if _, found, err := a.state.load(ctx); err != nil {
			return err
		} else if found {
			save = false
		}
		perZone = pool.InitialNodeCount
	}
	if save {
		if perZone < 1 {
			return fmt.Errorf("%w: could not tell how many nodes pool %s has per zone, so it could not be restored; refusing to resize it to 0", dr.ErrPreflight, a.target.nodepool)
		}
		if err := a.state.save(ctx, savedState{Nodes: perZone}); err != nil {
			return err
		}
	}
	_, err = a.env.Exec.Run(ctx, "gcloud", a.target.resizeArgs("0")...)
	return err
}

func (a *gkeAction) scaleUp(ctx context.Context) error {
	saved, found, err := a.state.load(ctx)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("%w: %w for node pool %s; resize it by hand with `gcloud container clusters resize` and resume", dr.ErrPreflight, errNoState, a.target.nodepool)
	}
	if saved.Nodes < 1 {
		return fmt.Errorf("%w: the saved node count of pool %s looks wrong (%d)", dr.ErrPreflight, a.target.nodepool, saved.Nodes)
	}
	if _, err := a.env.Exec.Run(ctx, "gcloud", a.target.resizeArgs(fmt.Sprint(saved.Nodes))...); err != nil {
		return err
	}
	return a.state.delete(ctx)
}
