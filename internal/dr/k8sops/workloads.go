package k8sops

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

// Workload kinds handled by the apps planner.
const (
	KindDeployment  = "Deployment"
	KindStatefulSet = "StatefulSet"
)

// WorkloadRef names a Deployment or a StatefulSet.
type WorkloadRef struct {
	Kind      string
	Namespace string
	Name      string
}

func (w WorkloadRef) String() string { return w.Namespace + "/" + w.Name }

// key is the identity used in the DR state document.
func (w WorkloadRef) key() string { return w.Kind + "/" + w.Namespace + "/" + w.Name }

// kubectlKind is the resource name kubectl uses.
func (w WorkloadRef) kubectlKind() string { return strings.ToLower(w.Kind) }

// label is how titles name the object: the kind only when it is not a Deployment.
func (w WorkloadRef) label() string {
	if w.Kind == KindStatefulSet {
		return "StatefulSet " + w.String()
	}
	return w.String()
}

func parseKey(key string) (WorkloadRef, bool) {
	parts := strings.SplitN(key, "/", 3)
	if len(parts) != 3 || (parts[0] != KindDeployment && parts[0] != KindStatefulSet) {
		return WorkloadRef{}, false
	}
	return WorkloadRef{Kind: parts[0], Namespace: parts[1], Name: parts[2]}, true
}

// ops wraps the API calls on workloads. sleep paces the retries after a conflict (nil means real time).
type ops struct {
	client kubernetes.Interface
	sleep  func(ctx context.Context, d time.Duration) error
}

// replicas reads the desired replica count through the scale subresource.
func (o ops) replicas(ctx context.Context, ref WorkloadRef) (int32, error) {
	switch ref.Kind {
	case KindDeployment:
		scale, err := o.client.AppsV1().Deployments(ref.Namespace).GetScale(ctx, ref.Name, metav1.GetOptions{})
		if err != nil {
			return 0, err
		}
		return scale.Spec.Replicas, nil
	case KindStatefulSet:
		scale, err := o.client.AppsV1().StatefulSets(ref.Namespace).GetScale(ctx, ref.Name, metav1.GetOptions{})
		if err != nil {
			return 0, err
		}
		return scale.Spec.Replicas, nil
	}
	return 0, fmt.Errorf("unsupported workload kind %q", ref.Kind)
}

// setReplicas goes through the scale subresource so it never overwrites unrelated changes. Each attempt
// reads the scale again; a workload that already has the wanted count needs no write, so a retry after a
// write that did land is harmless.
func (o ops) setReplicas(ctx context.Context, ref WorkloadRef, replicas int32) error {
	what := fmt.Sprintf("scale %s to %d", ref, replicas)
	switch ref.Kind {
	case KindDeployment:
		api := o.client.AppsV1().Deployments(ref.Namespace)
		return retryOnConflict(ctx, o.sleep, what, func() error {
			scale, err := api.GetScale(ctx, ref.Name, metav1.GetOptions{})
			if err != nil {
				return err
			}
			if scale.Spec.Replicas == replicas {
				return nil
			}
			updated := scale.DeepCopy()
			updated.Spec.Replicas = replicas
			_, err = api.UpdateScale(ctx, ref.Name, updated, metav1.UpdateOptions{})
			return err
		})
	case KindStatefulSet:
		api := o.client.AppsV1().StatefulSets(ref.Namespace)
		return retryOnConflict(ctx, o.sleep, what, func() error {
			scale, err := api.GetScale(ctx, ref.Name, metav1.GetOptions{})
			if err != nil {
				return err
			}
			if scale.Spec.Replicas == replicas {
				return nil
			}
			updated := scale.DeepCopy()
			updated.Spec.Replicas = replicas
			_, err = api.UpdateScale(ctx, ref.Name, updated, metav1.UpdateOptions{})
			return err
		})
	}
	return fmt.Errorf("unsupported workload kind %q", ref.Kind)
}

// annotation reads AnnotationReplicasBefore.
func (o ops) annotation(ctx context.Context, ref WorkloadRef) (string, bool, error) {
	var annotations map[string]string
	switch ref.Kind {
	case KindDeployment:
		d, err := o.client.AppsV1().Deployments(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
		if err != nil {
			return "", false, err
		}
		annotations = d.Annotations
	case KindStatefulSet:
		s, err := o.client.AppsV1().StatefulSets(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
		if err != nil {
			return "", false, err
		}
		annotations = s.Annotations
	default:
		return "", false, fmt.Errorf("unsupported workload kind %q", ref.Kind)
	}
	value, ok := annotations[AnnotationReplicasBefore]
	return value, ok, nil
}

// setAnnotation writes AnnotationReplicasBefore with a merge patch; a nil value removes it.
func (o ops) setAnnotation(ctx context.Context, ref WorkloadRef, value *string) error {
	var annotation any
	if value != nil {
		annotation = *value
	}
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": map[string]any{AnnotationReplicasBefore: annotation}}})
	if err != nil {
		return err
	}
	what := "record the replica count on " + ref.String()
	switch ref.Kind {
	case KindDeployment:
		return retryOnConflict(ctx, o.sleep, what, func() error {
			_, err := o.client.AppsV1().Deployments(ref.Namespace).Patch(ctx, ref.Name, types.MergePatchType, patch, metav1.PatchOptions{})
			return err
		})
	case KindStatefulSet:
		return retryOnConflict(ctx, o.sleep, what, func() error {
			_, err := o.client.AppsV1().StatefulSets(ref.Namespace).Patch(ctx, ref.Name, types.MergePatchType, patch, metav1.PatchOptions{})
			return err
		})
	}
	return fmt.Errorf("unsupported workload kind %q", ref.Kind)
}

// workloadStatus is what the gates need to know about a workload.
type workloadStatus struct {
	desired  int32
	ready    int32
	current  int32 // status.replicas
	observed bool  // the controller has seen the latest spec
	selector labels.Selector
}

func selectorOf(sel *metav1.LabelSelector) labels.Selector {
	if sel == nil || (len(sel.MatchLabels) == 0 && len(sel.MatchExpressions) == 0) {
		return nil
	}
	parsed, err := metav1.LabelSelectorAsSelector(sel)
	if err != nil {
		return nil
	}
	return parsed
}

func (o ops) status(ctx context.Context, ref WorkloadRef) (workloadStatus, error) {
	switch ref.Kind {
	case KindDeployment:
		d, err := o.client.AppsV1().Deployments(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
		if err != nil {
			return workloadStatus{}, err
		}
		desired := int32(1) // the API default when spec.replicas is unset
		if d.Spec.Replicas != nil {
			desired = *d.Spec.Replicas
		}
		return workloadStatus{desired: desired, ready: d.Status.ReadyReplicas, current: d.Status.Replicas,
			observed: d.Status.ObservedGeneration >= d.Generation, selector: selectorOf(d.Spec.Selector)}, nil
	case KindStatefulSet:
		s, err := o.client.AppsV1().StatefulSets(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
		if err != nil {
			return workloadStatus{}, err
		}
		desired := int32(1)
		if s.Spec.Replicas != nil {
			desired = *s.Spec.Replicas
		}
		return workloadStatus{desired: desired, ready: s.Status.ReadyReplicas, current: s.Status.Replicas,
			observed: s.Status.ObservedGeneration >= s.Generation, selector: selectorOf(s.Spec.Selector)}, nil
	}
	return workloadStatus{}, fmt.Errorf("unsupported workload kind %q", ref.Kind)
}

// candidate is a workload found in a namespace, with what the planner needs to describe it.
type candidate struct {
	ref WorkloadRef
	// live is false when the candidate was rebuilt from the DR state because the cluster is unreachable.
	live          bool
	replicas      int32
	annotation    string
	hasAnnotation bool
}

// list returns every Deployment and StatefulSet of a namespace.
func (o ops) list(ctx context.Context, namespace string) ([]candidate, error) {
	deployments, err := o.client.AppsV1().Deployments(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	statefulSets, err := o.client.AppsV1().StatefulSets(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}

	var out []candidate
	for i := range deployments.Items {
		d := &deployments.Items[i]
		replicas := int32(1)
		if d.Spec.Replicas != nil {
			replicas = *d.Spec.Replicas
		}
		note, has := d.Annotations[AnnotationReplicasBefore]
		out = append(out, candidate{ref: WorkloadRef{KindDeployment, namespace, d.Name}, live: true, replicas: replicas, annotation: note, hasAnnotation: has})
	}
	for i := range statefulSets.Items {
		s := &statefulSets.Items[i]
		replicas := int32(1)
		if s.Spec.Replicas != nil {
			replicas = *s.Spec.Replicas
		}
		note, has := s.Annotations[AnnotationReplicasBefore]
		out = append(out, candidate{ref: WorkloadRef{KindStatefulSet, namespace, s.Name}, live: true, replicas: replicas, annotation: note, hasAnnotation: has})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ref.Kind != out[j].ref.Kind {
			return out[i].ref.Kind < out[j].ref.Kind
		}
		return out[i].ref.Name < out[j].ref.Name
	})
	return out, nil
}
