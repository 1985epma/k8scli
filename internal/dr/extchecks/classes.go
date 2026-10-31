package extchecks

import (
	"context"
	"fmt"

	"github.com/1985epma/k8scli/internal/dr"
	"github.com/1985epma/k8scli/internal/dr/checks"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// classSets are the cluster-scoped classes that exist in the target cluster. An OK flag false means the
// list could not be read, so nothing is reported about that kind.
type classSets struct {
	storage        map[string]bool
	storageDefault bool
	storageOK      bool
	ingress        map[string]bool
	ingressOK      bool
	priority       map[string]bool
	priorityOK     bool
}

const defaultStorageClassAnnotation = "storageclass.kubernetes.io/is-default-class"

var builtinPriorityClasses = map[string]bool{"system-cluster-critical": true, "system-node-critical": true}

func (r *runner) readClasses(ctx context.Context, env *dr.Env) *classSets {
	sets := &classSets{storage: map[string]bool{}, ingress: map[string]bool{}, priority: map[string]bool{}}
	client := env.Client

	if list, err := client.StorageV1().StorageClasses().List(ctx, metav1.ListOptions{}); err != nil {
		r.notice("could not list StorageClasses: %v", err)
	} else {
		sets.storageOK = true
		for i := range list.Items {
			sets.storage[list.Items[i].Name] = true
			if list.Items[i].Annotations[defaultStorageClassAnnotation] == "true" {
				sets.storageDefault = true
			}
		}
	}
	if list, err := client.NetworkingV1().IngressClasses().List(ctx, metav1.ListOptions{}); err != nil {
		r.notice("could not list IngressClasses: %v", err)
	} else {
		sets.ingressOK = true
		for i := range list.Items {
			sets.ingress[list.Items[i].Name] = true
		}
	}
	if list, err := client.SchedulingV1().PriorityClasses().List(ctx, metav1.ListOptions{}); err != nil {
		r.notice("could not list PriorityClasses: %v", err)
	} else {
		sets.priorityOK = true
		for i := range list.Items {
			sets.priority[list.Items[i].Name] = true
		}
	}
	return sets
}

// checkReferences reports Secrets and ConfigMaps a workload needs that the target cluster lacks.
func (r *runner) checkReferences(w *workload, names *nameSets, targetName string) {
	refs := required(&w.Template.Spec)
	if names.secrets != nil {
		for _, name := range sortedKeys(refs.secrets) {
			if !names.secrets[name] {
				r.add(checks.Finding{Rule: "DR120", Severity: checks.SeverityHigh, Kind: w.Kind, Name: w.Name, Namespace: w.Namespace,
					Message: fmt.Sprintf("Secret %q is referenced but is not in %s", name, targetName),
					Fix:     "restore it from your secret store (the value is never read by k8scli)"})
			}
		}
	}
	if names.configMaps != nil {
		for _, name := range sortedKeys(refs.configMaps) {
			if !names.configMaps[name] {
				r.add(checks.Finding{Rule: "DR121", Severity: checks.SeverityHigh, Kind: w.Kind, Name: w.Name, Namespace: w.Namespace,
					Message: fmt.Sprintf("ConfigMap %q is referenced but is not in %s", name, targetName),
					Fix:     "apply it there: k8scli dr export includes ConfigMaps"})
			}
		}
	}
}

// checkClasses reports the PriorityClass a workload needs.
func (r *runner) checkClasses(w *workload, classes *classSets, targetName string) {
	name := w.Template.Spec.PriorityClassName
	if name == "" || builtinPriorityClasses[name] || !classes.priorityOK || classes.priority[name] {
		return
	}
	r.add(checks.Finding{Rule: "DR132", Severity: checks.SeverityMedium, Kind: w.Kind, Name: w.Name, Namespace: w.Namespace,
		Message: fmt.Sprintf("PriorityClass %q is not in %s", name, targetName),
		Fix:     "create the PriorityClass there before the workloads"})
}

// checkVolumes reports StorageClasses used by PVCs and by StatefulSet claim templates.
func (r *runner) checkVolumes(set *nsData, classes *classSets, targetName string) {
	if !classes.storageOK {
		return
	}
	seen := map[string]bool{}
	report := func(kind, name, class string) {
		key := kind + "/" + name + "/" + class
		if seen[key] {
			return
		}
		seen[key] = true
		switch {
		case class == "" && !classes.storageDefault:
			r.add(checks.Finding{Rule: "DR130", Severity: checks.SeverityMedium, Kind: kind, Name: name, Namespace: set.namespace,
				Message: "it uses the default StorageClass, and " + targetName + " has no default one",
				Fix:     "mark a StorageClass as default there, or set storageClassName explicitly"})
		case class != "" && !classes.storage[class]:
			r.add(checks.Finding{Rule: "DR130", Severity: checks.SeverityHigh, Kind: kind, Name: name, Namespace: set.namespace,
				Message: fmt.Sprintf("StorageClass %q is not in %s", class, targetName),
				Fix:     "create the StorageClass there (same name) before restoring volumes"})
		}
	}
	for i := range set.pvcs {
		pvc := &set.pvcs[i]
		if pvc.Spec.StorageClassName != nil && *pvc.Spec.StorageClassName == "" {
			continue // a static volume on purpose
		}
		class := ""
		if pvc.Spec.StorageClassName != nil {
			class = *pvc.Spec.StorageClassName
		}
		report("PersistentVolumeClaim", pvc.Name, class)
	}
	for i := range set.workloads {
		w := &set.workloads[i]
		for j := range w.Claims {
			claim := &w.Claims[j]
			if claim.Spec.StorageClassName != nil && *claim.Spec.StorageClassName == "" {
				continue
			}
			class := ""
			if claim.Spec.StorageClassName != nil {
				class = *claim.Spec.StorageClassName
			}
			report(w.Kind, w.Name, class)
		}
	}
}

// checkIngresses reports IngressClasses and TLS secrets the target cluster lacks.
func (r *runner) checkIngresses(set *nsData, classes *classSets, names *nameSets, targetName string) {
	for i := range set.ingresses {
		ing := &set.ingresses[i]
		class := ""
		if ing.Spec.IngressClassName != nil {
			class = *ing.Spec.IngressClassName
		} else if v, ok := ing.Annotations["kubernetes.io/ingress.class"]; ok {
			class = v
		}
		if class != "" && classes.ingressOK && !classes.ingress[class] {
			r.add(checks.Finding{Rule: "DR131", Severity: checks.SeverityMedium, Kind: "Ingress", Name: ing.Name, Namespace: set.namespace,
				Message: fmt.Sprintf("IngressClass %q is not in %s", class, targetName),
				Fix:     "install the ingress controller there (see Apps (Helm))"})
		}
		if names == nil || names.secrets == nil {
			continue
		}
		reported := map[string]bool{}
		for _, tls := range ing.Spec.TLS {
			if tls.SecretName == "" || reported[tls.SecretName] || names.secrets[tls.SecretName] {
				continue
			}
			reported[tls.SecretName] = true
			r.add(checks.Finding{Rule: "DR112", Severity: checks.SeverityHigh, Kind: "Ingress", Name: ing.Name, Namespace: set.namespace,
				Message: fmt.Sprintf("TLS secret %q is not in %s", tls.SecretName, targetName),
				Fix:     "let cert-manager issue it there, or restore it from your secret store"})
		}
	}
}
