package checks

import (
	"context"
	"fmt"
	"sort"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// listWorkloads returns the Deployments and StatefulSets of a namespace, sorted by kind and name.
func listWorkloads(ctx context.Context, client kubernetes.Interface, namespace string) ([]workload, error) {
	deployments, _, err := pageAll(maxObjects, func(o metav1.ListOptions) ([]appsv1.Deployment, string, error) {
		page, err := client.AppsV1().Deployments(namespace).List(ctx, o)
		if err != nil {
			return nil, "", err
		}
		return page.Items, page.Continue, nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list deployments in %s: %w", namespace, err)
	}
	statefulSets, _, err := pageAll(maxObjects, func(o metav1.ListOptions) ([]appsv1.StatefulSet, string, error) {
		page, err := client.AppsV1().StatefulSets(namespace).List(ctx, o)
		if err != nil {
			return nil, "", err
		}
		return page.Items, page.Continue, nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list statefulsets in %s: %w", namespace, err)
	}

	workloads := make([]workload, 0, len(deployments)+len(statefulSets))
	for i := range deployments {
		d := &deployments[i]
		workloads = append(workloads, workload{Kind: "Deployment", Name: d.Name, Namespace: namespace, Replicas: int32Value(d.Spec.Replicas, 1),
			Selector: d.Spec.Selector, Template: d.Spec.Template, Annotations: d.Annotations})
	}
	for i := range statefulSets {
		s := &statefulSets[i]
		workloads = append(workloads, workload{Kind: "StatefulSet", Name: s.Name, Namespace: namespace, Replicas: int32Value(s.Spec.Replicas, 1),
			Selector: s.Spec.Selector, Template: s.Spec.Template, Annotations: s.Annotations})
	}
	sort.Slice(workloads, func(i, j int) bool {
		if workloads[i].Kind != workloads[j].Kind {
			return workloads[i].Kind < workloads[j].Kind
		}
		return workloads[i].Name < workloads[j].Name
	})
	return workloads, nil
}
