package extchecks

import (
	"context"
	"fmt"
	"sort"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// workload is a Deployment or StatefulSet reduced to what the checks look at.
type workload struct {
	Kind      string
	Name      string
	Namespace string
	Template  corev1.PodTemplateSpec
	// Claims are the volume claim templates of a StatefulSet.
	Claims []corev1.PersistentVolumeClaim
}

func (w *workload) key() string { return w.Namespace + "/" + w.Kind + "/" + w.Name }

// imageUse is one image of one workload.
type imageUse struct {
	Image     string
	Host      string
	Namespace string
	Workload  string
}

func (w *workload) imageUses() []imageUse {
	var out []imageUse
	add := func(containers []corev1.Container) {
		for _, c := range containers {
			if c.Image == "" {
				continue
			}
			out = append(out, imageUse{Image: c.Image, Host: imageHost(c.Image), Namespace: w.Namespace, Workload: w.Kind + "/" + w.Name})
		}
	}
	add(w.Template.Spec.InitContainers)
	add(w.Template.Spec.Containers)
	return out
}

// nsData is everything read from the primary cluster for one namespace.
type nsData struct {
	namespace string
	workloads []workload
	pvcs      []corev1.PersistentVolumeClaim
	ingresses []networkingv1.Ingress
	// pullSecrets maps a ServiceAccount to the imagePullSecrets it carries.
	pullSecrets map[string][]string
}

func pageAll[T any](max int, list func(metav1.ListOptions) ([]T, string, error)) ([]T, bool, error) {
	var out []T
	options := metav1.ListOptions{Limit: pageSize}
	for {
		items, next, err := list(options)
		if err != nil {
			return nil, false, err
		}
		out = append(out, items...)
		if len(out) >= max {
			return out[:max], true, nil
		}
		if next == "" {
			return out, false, nil
		}
		options.Continue = next
	}
}

func (r *runner) readNamespace(ctx context.Context, namespace string) (*nsData, error) {
	client := r.opts.Primary.Client
	data := &nsData{namespace: namespace, pullSecrets: map[string][]string{}}

	deployments, cut, err := pageAll(maxWorkloads, func(o metav1.ListOptions) ([]appsv1.Deployment, string, error) {
		page, err := client.AppsV1().Deployments(namespace).List(ctx, o)
		if err != nil {
			return nil, "", err
		}
		return page.Items, page.Continue, nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list deployments in %s: %w", namespace, err)
	}
	if cut {
		r.truncated = true
	}
	sets, cut, err := pageAll(maxWorkloads, func(o metav1.ListOptions) ([]appsv1.StatefulSet, string, error) {
		page, err := client.AppsV1().StatefulSets(namespace).List(ctx, o)
		if err != nil {
			return nil, "", err
		}
		return page.Items, page.Continue, nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list statefulsets in %s: %w", namespace, err)
	}
	if cut {
		r.truncated = true
	}
	for i := range deployments {
		d := &deployments[i]
		data.workloads = append(data.workloads, workload{Kind: "Deployment", Name: d.Name, Namespace: namespace, Template: d.Spec.Template})
	}
	for i := range sets {
		s := &sets[i]
		data.workloads = append(data.workloads, workload{Kind: "StatefulSet", Name: s.Name, Namespace: namespace, Template: s.Spec.Template, Claims: s.Spec.VolumeClaimTemplates})
	}
	sort.Slice(data.workloads, func(i, j int) bool { return data.workloads[i].key() < data.workloads[j].key() })

	// The rest is best effort: a permission error on PVCs must not hide the other findings.
	if pvcs, _, err := pageAll(maxWorkloads, func(o metav1.ListOptions) ([]corev1.PersistentVolumeClaim, string, error) {
		page, err := client.CoreV1().PersistentVolumeClaims(namespace).List(ctx, o)
		if err != nil {
			return nil, "", err
		}
		return page.Items, page.Continue, nil
	}); err != nil {
		r.notice("could not list PVCs in %s: %v", namespace, err)
	} else {
		data.pvcs = pvcs
	}
	if ingresses, _, err := pageAll(maxWorkloads, func(o metav1.ListOptions) ([]networkingv1.Ingress, string, error) {
		page, err := client.NetworkingV1().Ingresses(namespace).List(ctx, o)
		if err != nil {
			return nil, "", err
		}
		return page.Items, page.Continue, nil
	}); err != nil {
		r.notice("could not list ingresses in %s: %v", namespace, err)
	} else {
		data.ingresses = ingresses
	}
	if accounts, _, err := pageAll(maxWorkloads, func(o metav1.ListOptions) ([]corev1.ServiceAccount, string, error) {
		page, err := client.CoreV1().ServiceAccounts(namespace).List(ctx, o)
		if err != nil {
			return nil, "", err
		}
		return page.Items, page.Continue, nil
	}); err != nil {
		r.notice("could not list service accounts in %s: %v", namespace, err)
	} else {
		for i := range accounts {
			for _, ref := range accounts[i].ImagePullSecrets {
				data.pullSecrets[accounts[i].Name] = append(data.pullSecrets[accounts[i].Name], ref.Name)
			}
		}
	}
	r.count("namespaces", 1)
	return data, nil
}

// ---- references of a pod template -----------------------------------------------------------------------

type podRefs struct {
	secrets    map[string]bool
	configMaps map[string]bool
}

// required returns the Secrets and ConfigMaps a pod template cannot start without (optional ones are skipped).
func required(spec *corev1.PodSpec) podRefs {
	refs := podRefs{secrets: map[string]bool{}, configMaps: map[string]bool{}}
	notOptional := func(p *bool) bool { return p == nil || !*p }
	scan := func(containers []corev1.Container) {
		for _, c := range containers {
			for _, e := range c.Env {
				if e.ValueFrom == nil {
					continue
				}
				if k := e.ValueFrom.SecretKeyRef; k != nil && notOptional(k.Optional) {
					refs.secrets[k.Name] = true
				}
				if k := e.ValueFrom.ConfigMapKeyRef; k != nil && notOptional(k.Optional) {
					refs.configMaps[k.Name] = true
				}
			}
			for _, from := range c.EnvFrom {
				if from.SecretRef != nil && notOptional(from.SecretRef.Optional) {
					refs.secrets[from.SecretRef.Name] = true
				}
				if from.ConfigMapRef != nil && notOptional(from.ConfigMapRef.Optional) {
					refs.configMaps[from.ConfigMapRef.Name] = true
				}
			}
		}
	}
	scan(spec.InitContainers)
	scan(spec.Containers)
	for _, v := range spec.Volumes {
		if v.Secret != nil && notOptional(v.Secret.Optional) {
			refs.secrets[v.Secret.SecretName] = true
		}
		if v.ConfigMap != nil && notOptional(v.ConfigMap.Optional) {
			refs.configMaps[v.ConfigMap.Name] = true
		}
		if v.Projected != nil {
			for _, src := range v.Projected.Sources {
				if src.Secret != nil && notOptional(src.Secret.Optional) {
					refs.secrets[src.Secret.Name] = true
				}
				if src.ConfigMap != nil && notOptional(src.ConfigMap.Optional) {
					refs.configMaps[src.ConfigMap.Name] = true
				}
			}
		}
	}
	delete(refs.secrets, "")
	delete(refs.configMaps, "")
	return refs
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	if apierrors.IsNotFound(err) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "could not find the requested resource") || strings.Contains(msg, "no matches for kind")
}
