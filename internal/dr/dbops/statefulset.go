package dbops

import (
	"context"
	"fmt"
	"strconv"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/1985epma/k8scli/internal/dr"
)

const (
	// replicasAnnotation keeps the replica count a StatefulSet had before the shutdown, on the
	// object itself, so it survives a cluster restart and does not depend on one machine's disk.
	replicasAnnotation = "k8scli.io/dr-replicas-before"
	stateKeySuffix     = "/databases"
)

// savedDatabase is what "down" remembers for the matching "up", in dr.Env.State.
type savedDatabase struct {
	Kind     string    `json:"kind"`
	Replicas int32     `json:"replicas"`
	Saved    time.Time `json:"saved"`
}

// savedDocument is stored under "<context>/databases", keyed by "namespace/ref".
type savedDocument map[string]savedDatabase

// statefulSetTarget is one plain StatefulSet database of the plan.
type statefulSetTarget struct {
	p    *Planner
	env  *dr.Env
	plan *dr.Plan
	db   dr.Database
}

func (s *statefulSetTarget) name() string { return s.db.Namespace + "/" + s.db.Ref }

func (s *statefulSetTarget) stateKey() string { return s.env.Context + stateKeySuffix }

func (s *statefulSetTarget) actionID(verb string) string {
	return "databases/" + s.db.Namespace + "/" + s.db.Ref + ":" + verb
}

func (s *statefulSetTarget) get(ctx context.Context) (*appsv1.StatefulSet, error) {
	if s.env.Client == nil {
		return nil, fmt.Errorf("%w: no Kubernetes client to read StatefulSets", dr.ErrUnreachable)
	}
	sts, err := s.env.Client.AppsV1().StatefulSets(s.db.Namespace).Get(ctx, s.db.Ref, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("StatefulSet %s was not found: check database %q in the plan", s.name(), s.db.Name)
	}
	return sts, err
}

// desired is spec.replicas, which the API defaults to 1 when it is unset.
func desired(sts *appsv1.StatefulSet) int32 {
	if sts.Spec.Replicas == nil {
		return 1
	}
	return *sts.Spec.Replicas
}

func (s *statefulSetTarget) steps(ctx context.Context, dir dr.Direction) ([]dr.Step, error) {
	if dir == dr.DirDown {
		return s.downSteps(ctx)
	}
	return s.upSteps(ctx), nil
}

func (s *statefulSetTarget) downSteps(ctx context.Context) ([]dr.Step, error) {
	if s.env.Client == nil {
		return nil, fmt.Errorf("%w: no Kubernetes client to read StatefulSets", dr.ErrUnreachable)
	}

	current := "its current number of"
	preflightNote := ""
	if sts, err := s.get(ctx); err == nil {
		current = strconv.Itoa(int(desired(sts)))
		if summary, blocked := s.preflight(ctx, sts); blocked != nil {
			preflightNote = " BLOCKED: " + blocked.Error()
		} else {
			preflightNote = " Currently OK: " + summary + "."
		}
	} else {
		preflightNote = " It could not be evaluated now: " + err.Error()
	}

	preflight := &funcAction{
		id:       s.actionID("preflight"),
		title:    "Check health and backups before stopping " + s.name(),
		describe: "Check that every replica of StatefulSet " + s.name() + " is Ready and that the newest backup is recent enough." + preflightNote,
		risk:     dr.RiskLow,
		done: func(ctx context.Context) (bool, error) {
			sts, err := s.get(ctx)
			if err != nil {
				return false, err
			}
			return desired(sts) == 0, nil
		},
		apply: func(ctx context.Context) error {
			sts, err := s.get(ctx)
			if err != nil {
				return err
			}
			summary, err := s.preflight(ctx, sts)
			if err != nil {
				return err
			}
			logf(s.env, "%s: %s", s.name(), summary)
			return nil
		},
	}

	stop := &funcAction{
		id:    s.actionID("stop"),
		title: "Stop StatefulSet " + s.name(),
		describe: "Scale StatefulSet " + s.name() + " from " + current + " replicas to 0 (Kubernetes stops them in reverse ordinal order). " +
			"WARNING: a plain StatefulSet is stopped without application-level checkpointing or a clean database shutdown, so a " +
			"logical backup (pg_dump, mysqldump...) is recommended first. The previous replica count is saved in the annotation " +
			replicasAnnotation + " and in the run state.",
		risk:       dr.RiskHigh,
		equivalent: fmt.Sprintf("kubectl scale statefulset %s --replicas=0 -n %s", s.db.Ref, s.db.Namespace),
		done: func(ctx context.Context) (bool, error) {
			sts, err := s.get(ctx)
			if err != nil {
				return false, err
			}
			return desired(sts) == 0, nil
		},
		apply: s.applyStop,
	}

	return []dr.Step{
		{Phase: dr.PhaseDatabases, Action: preflight},
		{Phase: dr.PhaseDatabases, Action: stop, Gate: s.stoppedGate()},
	}, nil
}

// preflight refuses to stop a database that is not fully healthy or whose backups are too old.
func (s *statefulSetTarget) preflight(ctx context.Context, sts *appsv1.StatefulSet) (string, error) {
	want := desired(sts)
	if want > 0 && sts.Status.ReadyReplicas < want {
		return "", fmt.Errorf("%w: StatefulSet %s has %d of %d replicas Ready. "+
			"Refusing to stop a database that is already unhealthy: fix it first, or stop it by hand",
			dr.ErrPreflight, s.name(), sts.Status.ReadyReplicas, want)
	}

	summary, err := s.p.backupAgeCheck(ctx, s.env, s.plan, s.db, nil)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("all %d replicas are Ready; %s", want, summary), nil
}

// applyStop records the replica count and scales to zero in ONE patch, so there is no moment in which
// the database is stopped and its previous size is lost.
func (s *statefulSetTarget) applyStop(ctx context.Context) error {
	sts, err := s.get(ctx)
	if err != nil {
		return err
	}
	current := desired(sts)
	if current == 0 {
		return nil // already stopped; keep whatever size was recorded earlier
	}

	if s.env.State != nil {
		doc := savedDocument{}
		if _, err := s.env.State.Load(ctx, s.stateKey(), &doc); err != nil {
			return fmt.Errorf("reading the saved state: %w", err)
		}
		doc[s.name()] = savedDatabase{Kind: KindStatefulSet, Replicas: current, Saved: nowOf(s.env)}
		// Saved before the patch: if the patch fails nothing is lost, and if it succeeds the size is known.
		if err := s.env.State.Save(ctx, s.stateKey(), doc); err != nil {
			return fmt.Errorf("saving the replica count before stopping %s (nothing was changed): %w", s.name(), err)
		}
	}

	patch := fmt.Sprintf(`{"metadata":{"annotations":{%q:%q}},"spec":{"replicas":0}}`, replicasAnnotation, strconv.Itoa(int(current)))
	_, err = s.env.Client.AppsV1().StatefulSets(s.db.Namespace).
		Patch(ctx, s.db.Ref, types.MergePatchType, []byte(patch), metav1.PatchOptions{FieldManager: fieldManager})
	return err
}

func (s *statefulSetTarget) stoppedGate() dr.Gate {
	return &pollGate{
		name:    "StatefulSet " + s.name() + " stopped",
		env:     s.env,
		timeout: s.p.opts.GateTimeout,
		poll:    s.p.opts.GatePoll,
		check: func(ctx context.Context) (bool, string, error) {
			sts, err := s.get(ctx)
			if err != nil {
				return false, "", err
			}
			if sts.Status.Replicas == 0 && sts.Status.ReadyReplicas == 0 {
				return true, "", nil
			}
			return false, fmt.Sprintf("%d replica(s) are still running", sts.Status.Replicas), nil
		},
	}
}

func (s *statefulSetTarget) upSteps(ctx context.Context) []dr.Step {
	// Only the saved run state is consulted here: "up" is planned while the cluster may still be off.
	known, replicasWord := "", "<N>"
	if replicas, ok := s.savedFromState(ctx); ok {
		known = fmt.Sprintf(" (%d, from the saved run state)", replicas)
		replicasWord = strconv.Itoa(int(replicas))
	}

	start := &funcAction{
		id:    s.actionID("start"),
		title: "Start StatefulSet " + s.name(),
		describe: "Scale StatefulSet " + s.name() + " back to the replica count it had before the shutdown" + known + ", read from the annotation " +
			replicasAnnotation + " or the saved run state, and wait until every replica is Ready. It refuses to start if that number is unknown.",
		risk:       dr.RiskLow,
		equivalent: fmt.Sprintf("kubectl scale statefulset %s --replicas=%s -n %s", s.db.Ref, replicasWord, s.db.Namespace),
		done: func(ctx context.Context) (bool, error) {
			sts, err := s.get(ctx)
			if err != nil {
				return false, err
			}
			want := desired(sts)
			return want > 0 && sts.Status.ReadyReplicas >= want, nil
		},
		apply: s.applyStart,
	}
	return []dr.Step{{Phase: dr.PhaseDatabases, Action: start, Gate: s.readyGate()}}
}

func (s *statefulSetTarget) savedFromState(ctx context.Context) (int32, bool) {
	if s.env.State == nil {
		return 0, false
	}
	doc := savedDocument{}
	found, err := s.env.State.Load(ctx, s.stateKey(), &doc)
	if err != nil || !found {
		return 0, false
	}
	saved, ok := doc[s.name()]
	return saved.Replicas, ok && saved.Replicas > 0
}

// savedReplicas finds how many replicas to restore: the annotation on the object first, then the run state.
func (s *statefulSetTarget) savedReplicas(ctx context.Context, sts *appsv1.StatefulSet) (int32, bool) {
	if text, ok := sts.Annotations[replicasAnnotation]; ok {
		if n, err := strconv.ParseInt(text, 10, 32); err == nil && n > 0 {
			return int32(n), true
		}
	}
	return s.savedFromState(ctx)
}

func (s *statefulSetTarget) applyStart(ctx context.Context) error {
	sts, err := s.get(ctx)
	if err != nil {
		return err
	}
	if desired(sts) > 0 {
		return nil // already scaled up; the gate waits for the replicas
	}

	target, ok := s.savedReplicas(ctx, sts)
	if !ok {
		return fmt.Errorf("cannot start StatefulSet %s: the number of replicas before the shutdown is unknown "+
			"(neither the annotation %s nor the saved run state has it). Scale it by hand: "+
			"kubectl scale statefulset %s --replicas=<N> -n %s",
			s.name(), replicasAnnotation, s.db.Ref, s.db.Namespace)
	}

	// The annotation is cleared in the same patch so a later, unrelated scale-to-zero is not undone by accident.
	patch := fmt.Sprintf(`{"metadata":{"annotations":{%q:null}},"spec":{"replicas":%d}}`, replicasAnnotation, target)
	if _, err := s.env.Client.AppsV1().StatefulSets(s.db.Namespace).
		Patch(ctx, s.db.Ref, types.MergePatchType, []byte(patch), metav1.PatchOptions{FieldManager: fieldManager}); err != nil {
		return err
	}
	s.forgetState(ctx)
	return nil
}

func (s *statefulSetTarget) forgetState(ctx context.Context) {
	if s.env.State == nil {
		return
	}
	doc := savedDocument{}
	found, err := s.env.State.Load(ctx, s.stateKey(), &doc)
	if err != nil || !found {
		return
	}
	delete(doc, s.name())
	if len(doc) == 0 {
		_ = s.env.State.Delete(ctx, s.stateKey())
		return
	}
	_ = s.env.State.Save(ctx, s.stateKey(), doc)
}

func (s *statefulSetTarget) readyGate() dr.Gate {
	return &pollGate{
		name:    "StatefulSet " + s.name() + " ready",
		env:     s.env,
		timeout: s.p.opts.GateTimeout,
		poll:    s.p.opts.GatePoll,
		check: func(ctx context.Context) (bool, string, error) {
			sts, err := s.get(ctx)
			if err != nil {
				return false, "", err
			}
			want := desired(sts)
			if want > 0 && sts.Status.ReadyReplicas >= want {
				return true, "", nil
			}
			return false, fmt.Sprintf("%d of %d replicas Ready", sts.Status.ReadyReplicas, want), nil
		},
	}
}
