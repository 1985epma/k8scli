package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/1985epma/k8scli/internal/dr"
)

// eksTarget is one managed node group. Its control plane keeps running (and billing); scaling the
// group to zero stops the compute.
type eksTarget struct {
	cluster, nodegroup, region, profile string
}

// withAccount appends the region and, when set, the profile.
func (t eksTarget) withAccount(args ...string) []string {
	args = append(args, "--region", t.region)
	if t.profile != "" {
		args = append(args, "--profile", t.profile)
	}
	return args
}

func (t eksTarget) identity() string {
	return strings.Join([]string{t.cluster, t.nodegroup, t.region, t.profile}, "/")
}

func (t eksTarget) describeArgs() []string {
	return t.withAccount("eks", "describe-nodegroup", "--cluster-name", t.cluster, "--nodegroup-name", t.nodegroup,
		"--query", "nodegroup.{status:status,scalingConfig:scalingConfig}", "--output", "json")
}

func (t eksTarget) updateArgs(minSize, maxSize, desired string) []string {
	return t.withAccount("eks", "update-nodegroup-config", "--cluster-name", t.cluster, "--nodegroup-name", t.nodegroup,
		"--scaling-config", "minSize="+minSize+",maxSize="+maxSize+",desiredSize="+desired)
}

func (t eksTarget) waitArgs() []string {
	return t.withAccount("eks", "wait", "nodegroup-active", "--cluster-name", t.cluster, "--nodegroup-name", t.nodegroup)
}

type eksNodegroup struct {
	Status        string `json:"status"`
	ScalingConfig struct {
		MinSize     int `json:"minSize"`
		MaxSize     int `json:"maxSize"`
		DesiredSize int `json:"desiredSize"`
	} `json:"scalingConfig"`
}

func (n eksNodegroup) zero() bool {
	return n.ScalingConfig.DesiredSize == 0 && n.ScalingConfig.MinSize == 0
}

func eksSteps(env *dr.Env, plan *dr.Plan, dir dr.Direction) ([]dr.Step, error) {
	p := newParams(KindEKSNodegroup, plan.Provider.Params)
	p.rejectUnknown("cluster", "nodegroup", "region", "profile")
	target := eksTarget{cluster: p.required("cluster"), nodegroup: p.required("nodegroup"), region: p.required("region"), profile: p.optional("profile")}
	if p.err != nil {
		return nil, p.err
	}
	state, err := newStateRef(env, plan, KindEKSNodegroup, target.identity())
	if err != nil {
		return nil, err
	}
	return []dr.Step{powerStep(&eksAction{env: env, target: target, state: state, down: dir == dr.DirDown})}, nil
}

// eksAction scales a node group to zero (saving its size first) or back to the saved size.
type eksAction struct {
	env    *dr.Env
	target eksTarget
	state  *stateRef
	down   bool
}

func (a *eksAction) ID() string {
	verb := "scale-up"
	if a.down {
		verb = "scale-down"
	}
	return fmt.Sprintf("power/eks/%s/%s:%s", a.target.cluster, a.target.nodegroup, verb)
}

func (a *eksAction) Title() string {
	if a.down {
		return fmt.Sprintf("Scale EKS node group %s to 0", a.target.nodegroup)
	}
	return fmt.Sprintf("Scale EKS node group %s back up", a.target.nodegroup)
}

func (a *eksAction) Describe() string {
	if a.down {
		return fmt.Sprintf("Saves the size of node group %s, scales it to 0 and waits until the update finishes. The control plane of %s keeps running.", a.target.nodegroup, a.target.cluster)
	}
	return fmt.Sprintf("Restores the size saved when node group %s was scaled to 0 and waits until the update finishes.", a.target.nodegroup)
}

func (a *eksAction) Risk() dr.Risk {
	if a.down {
		return dr.RiskDestructive
	}
	return dr.RiskLow
}

// Equivalent shows the commands; the sizes are only known at run time, except that "up" can already
// show the ones saved by an earlier power-off.
func (a *eksAction) Equivalent() string {
	update := command{"aws", a.target.updateArgs("0", "<current max>", "0")}
	if !a.down {
		sizes := [3]string{"<saved min>", "<saved max>", "<saved desired>"}
		if saved, found, err := a.state.load(context.Background()); err == nil && found {
			sizes = [3]string{fmt.Sprint(saved.Min), fmt.Sprint(saved.Max), fmt.Sprint(saved.Desired)}
		}
		update = command{"aws", a.target.updateArgs(sizes[0], sizes[1], sizes[2])}
	}
	return update.line() + " && " + command{"aws", a.target.waitArgs()}.line()
}

func (a *eksAction) describe(ctx context.Context) (eksNodegroup, error) {
	out, err := a.env.Exec.Run(ctx, "aws", a.target.describeArgs()...)
	if err != nil {
		return eksNodegroup{}, fmt.Errorf("reading node group %s: %w", a.target.nodegroup, err)
	}
	var group eksNodegroup
	if err := json.Unmarshal([]byte(out), &group); err != nil {
		return eksNodegroup{}, fmt.Errorf("reading node group %s: unexpected answer from aws: %w", a.target.nodegroup, err)
	}
	return group, nil
}

// Done: scaled to zero and no update in flight (down), or running with nodes and no update in flight (up).
func (a *eksAction) Done(ctx context.Context) (bool, error) {
	group, err := a.describe(ctx)
	if err != nil {
		return false, err
	}
	if group.Status != "ACTIVE" {
		return false, nil
	}
	if a.down {
		return group.zero(), nil
	}
	return group.ScalingConfig.DesiredSize > 0, nil
}

func (a *eksAction) Apply(ctx context.Context) error {
	group, err := a.describe(ctx)
	if err != nil {
		return err
	}
	if a.down {
		err = a.scaleDown(ctx, group)
	} else {
		err = a.scaleUp(ctx, group)
	}
	if err != nil {
		return err
	}
	// Also reached on resume, when the update was already requested: wait for it to finish.
	if _, err := a.env.Exec.Run(ctx, "aws", a.target.waitArgs()...); err != nil {
		return err
	}
	if !a.down {
		return a.state.delete(ctx)
	}
	return nil
}

func (a *eksAction) scaleDown(ctx context.Context, group eksNodegroup) error {
	if group.zero() {
		return nil // already requested; only the wait is left
	}
	// A fresh reading always wins over an older saved state: it is what is running now.
	if err := a.state.save(ctx, savedState{Min: group.ScalingConfig.MinSize, Max: group.ScalingConfig.MaxSize, Desired: group.ScalingConfig.DesiredSize}); err != nil {
		return err
	}
	keep := max(group.ScalingConfig.MaxSize, 1) // EKS does not accept a maximum below 1
	_, err := a.env.Exec.Run(ctx, "aws", a.target.updateArgs("0", fmt.Sprint(keep), "0")...)
	return err
}

func (a *eksAction) scaleUp(ctx context.Context, group eksNodegroup) error {
	saved, found, err := a.state.load(ctx)
	if err != nil {
		return err
	}
	if group.ScalingConfig.DesiredSize > 0 {
		return nil // nodes are already there; only the wait is left
	}
	if !found {
		return fmt.Errorf("%w: %w for node group %s; set its size by hand with `aws eks update-nodegroup-config` and resume", dr.ErrPreflight, errNoState, a.target.nodegroup)
	}
	if saved.Max < 1 || saved.Min < 0 || saved.Desired < saved.Min || saved.Desired > saved.Max {
		return fmt.Errorf("%w: the saved size of node group %s looks wrong (min %d, max %d, desired %d)", dr.ErrPreflight, a.target.nodegroup, saved.Min, saved.Max, saved.Desired)
	}
	_, err = a.env.Exec.Run(ctx, "aws", a.target.updateArgs(fmt.Sprint(saved.Min), fmt.Sprint(saved.Max), fmt.Sprint(saved.Desired))...)
	return err
}
