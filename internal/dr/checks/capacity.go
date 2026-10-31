package checks

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/1985epma/k8scli/internal/dr"
)

// replicasBeforeAnnotation is the annotation the apps planner writes when it scales a workload to zero;
// the capacity check uses it so a stopped workload still counts at its real size.
const replicasBeforeAnnotation = "k8scli.io/dr-replicas-before"

// CapacityOptions select what has to fit on the standby cluster.
type CapacityOptions struct {
	// Namespaces to cover; empty means every namespace except the system ones.
	Namespaces []string
	// Workloads limits a namespace to named Deployments or StatefulSets; a namespace that is not a key
	// (or whose list is empty) is taken whole.
	Workloads     map[string][]string
	IncludeSystem bool
}

// CapacityOptionsFromPlan covers the apps and statefulset databases of a plan.
func CapacityOptionsFromPlan(plan *dr.Plan) CapacityOptions {
	options := CapacityOptions{Workloads: map[string][]string{}}
	whole := map[string]bool{}
	seen := map[string]bool{}
	add := func(namespace string, names []string) {
		if !seen[namespace] {
			seen[namespace] = true
			options.Namespaces = append(options.Namespaces, namespace)
		}
		if len(names) == 0 {
			whole[namespace] = true
			return
		}
		options.Workloads[namespace] = append(options.Workloads[namespace], names...)
	}
	for _, app := range plan.Apps {
		add(app.Namespace, app.Workloads)
	}
	for _, db := range plan.Databases {
		if db.Kind == "statefulset" {
			add(db.Namespace, []string{db.Ref})
		} else {
			// A CNPG cluster's pods are not Deployments or StatefulSets: take the namespace whole.
			add(db.Namespace, nil)
		}
	}
	for namespace := range whole {
		delete(options.Workloads, namespace)
	}
	sort.Strings(options.Namespaces)
	return options
}

// ResourceFit compares what the apps ask for with what the standby has free. CPU is in millicores,
// memory in bytes.
type ResourceFit struct {
	Demand      int64  `json:"demand"`
	Free        int64  `json:"free"`
	Allocatable int64  `json:"allocatable"`
	Requested   int64  `json:"requested"`
	Fits        bool   `json:"fits"`
	DemandText  string `json:"demandText"`
	FreeText    string `json:"freeText"`
}

// LargestPod checks the biggest single pod against the node with the most room.
type LargestPod struct {
	Name      string `json:"name"`
	CPU       int64  `json:"cpu"`
	Memory    int64  `json:"memory"`
	Fits      bool   `json:"fits"`
	BestNode  string `json:"bestNode,omitempty"`
	NodeFreeC int64  `json:"nodeFreeCpu"`
	NodeFreeM int64  `json:"nodeFreeMemory"`
}

// Missing is something the apps use that the standby does not have.
type Missing struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
	UsedBy    string `json:"usedBy,omitempty"`
}

// CapacityReport is the answer of CapacityFit.
type CapacityReport struct {
	Primary      string      `json:"primary"`
	Standby      string      `json:"standby"`
	Workloads    int         `json:"workloads"`
	Pods         int         `json:"pods"`
	StandbyNodes int         `json:"standbyNodes"`
	CPU          ResourceFit `json:"cpu"`
	Memory       ResourceFit `json:"memory"`
	Largest      *LargestPod `json:"largestPod,omitempty"`
	// StorageDemand is the sum of the PVC storage requests, for information (bytes).
	StorageDemand int64     `json:"storageDemand"`
	Missing       []Missing `json:"missing"`
	// Fits is true when CPU, memory and the largest pod fit; OK also requires nothing to be missing.
	Fits    bool     `json:"fits"`
	OK      bool     `json:"ok"`
	Notices []string `json:"notices,omitempty"`
}

// podRequests is the effective request of one pod: the sum of its containers, or the biggest init
// container when that is larger (the scheduler's rule).
func podRequests(spec *corev1.PodSpec) (cpu, memory int64) {
	for _, c := range spec.Containers {
		cpu += c.Resources.Requests.Cpu().MilliValue()
		memory += c.Resources.Requests.Memory().Value()
	}
	for _, c := range spec.InitContainers {
		if v := c.Resources.Requests.Cpu().MilliValue(); v > cpu {
			cpu = v
		}
		if v := c.Resources.Requests.Memory().Value(); v > memory {
			memory = v
		}
	}
	return cpu, memory
}

// desiredReplicas is the replica count to plan for: the live one, or the one recorded before the
// workload was stopped.
func desiredReplicas(w workload) int32 {
	if w.Replicas == 0 {
		if value, ok := w.Annotations[replicasBeforeAnnotation]; ok {
			if n, err := strconv.ParseInt(value, 10, 32); err == nil && n > 0 {
				return int32(n)
			}
		}
	}
	return w.Replicas
}

// usableNode reports whether pods may be placed on the node: Ready, not cordoned, not a tainted
// control-plane node.
func usableNode(n *corev1.Node) bool {
	if n.Spec.Unschedulable {
		return false
	}
	ready := false
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			ready = c.Status == corev1.ConditionTrue
		}
	}
	if !ready {
		return false
	}
	for _, t := range n.Spec.Taints {
		if (t.Effect == corev1.TaintEffectNoSchedule || t.Effect == corev1.TaintEffectNoExecute) &&
			(t.Key == "node-role.kubernetes.io/control-plane" || t.Key == "node-role.kubernetes.io/master") {
			return false
		}
	}
	return true
}

// CapacityFit checks whether the workloads selected on the primary cluster would fit on the standby:
// the sum of replicas x requests against the standby's allocatable minus what is already requested
// there, the largest single pod against the roomiest node, and the namespaces, StorageClasses,
// PriorityClasses and IngressClasses the workloads need.
//
// The check is conservative: pods already running on the standby count as used, even when they are the
// same applications scaled down.
func CapacityFit(ctx context.Context, primary, standby *dr.Env, options CapacityOptions) (*CapacityReport, error) {
	if err := requireClient(primary); err != nil {
		return nil, fmt.Errorf("primary: %w", err)
	}
	if err := requireClient(standby); err != nil {
		return nil, fmt.Errorf("standby: %w", err)
	}

	namespaces := append([]string(nil), options.Namespaces...)
	if len(namespaces) == 0 {
		var err error
		if namespaces, err = namespaceNames(ctx, primary.Client, options.IncludeSystem); err != nil {
			return nil, err
		}
	}
	sort.Strings(namespaces)

	report := &CapacityReport{Primary: primary.Context, Standby: standby.Context, Missing: []Missing{}}
	missing := newMissingSet()

	var biggest struct {
		name        string
		cpu, memory int64
	}
	defaultPrimaryClass := ""
	classLoaded := false

	for _, namespace := range namespaces {
		workloads, err := listWorkloads(ctx, primary.Client, namespace)
		if err != nil {
			return nil, err
		}
		wanted := map[string]bool{}
		for _, name := range options.Workloads[namespace] {
			wanted[name] = true
		}

		used := 0
		for _, w := range workloads {
			if len(wanted) > 0 && !wanted[w.Name] {
				continue
			}
			used++
			replicas := int64(desiredReplicas(w))
			cpu, memory := podRequests(&w.Template.Spec)
			report.Workloads++
			report.Pods += int(replicas)
			report.CPU.Demand += cpu * replicas
			report.Memory.Demand += memory * replicas
			if replicas > 0 && (memory > biggest.memory || (memory == biggest.memory && cpu > biggest.cpu)) {
				biggest.name, biggest.cpu, biggest.memory = namespace+"/"+w.Name, cpu, memory
			}
			if pc := w.Template.Spec.PriorityClassName; pc != "" && !strings.HasPrefix(pc, "system-") {
				missing.add("PriorityClass", pc, "", namespace+"/"+w.Name)
			}
		}
		if used == 0 {
			continue
		}

		if _, err := standby.Client.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{}); apierrors.IsNotFound(err) {
			missing.add("Namespace", namespace, "", "the apps selected in this namespace")
		} else if err != nil {
			report.Notices = append(report.Notices, fmt.Sprintf("Could not check namespace %s on the standby: %v", namespace, err))
		}

		// PVCs: storage classes and storage demand.
		pvcs, _, err := pageAll(maxObjects, func(o metav1.ListOptions) ([]corev1.PersistentVolumeClaim, string, error) {
			page, err := primary.Client.CoreV1().PersistentVolumeClaims(namespace).List(ctx, o)
			if err != nil {
				return nil, "", err
			}
			return page.Items, page.Continue, nil
		})
		if err != nil {
			report.Notices = append(report.Notices, fmt.Sprintf("Could not read PVCs in %s: %v", namespace, err))
		}
		for i := range pvcs {
			report.StorageDemand += pvcs[i].Spec.Resources.Requests.Storage().Value()
			class := ""
			if pvcs[i].Spec.StorageClassName != nil {
				class = *pvcs[i].Spec.StorageClassName
			} else {
				if !classLoaded {
					defaultPrimaryClass = defaultStorageClass(ctx, primary)
					classLoaded = true
				}
				class = defaultPrimaryClass
			}
			if class != "" {
				missing.add("StorageClass", class, "", namespace+"/"+pvcs[i].Name)
			}
		}

		// Ingress classes.
		ingresses, err := primary.Client.NetworkingV1().Ingresses(namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			report.Notices = append(report.Notices, fmt.Sprintf("Could not read Ingresses in %s: %v", namespace, err))
		} else {
			for i := range ingresses.Items {
				class := ""
				if ingresses.Items[i].Spec.IngressClassName != nil {
					class = *ingresses.Items[i].Spec.IngressClassName
				} else {
					class = ingresses.Items[i].Annotations["kubernetes.io/ingress.class"]
				}
				if class != "" {
					missing.add("IngressClass", class, "", namespace+"/"+ingresses.Items[i].Name)
				}
			}
		}
	}

	// What the standby offers.
	nodes, _, err := pageAll(maxObjects, func(o metav1.ListOptions) ([]corev1.Node, string, error) {
		page, err := standby.Client.CoreV1().Nodes().List(ctx, o)
		if err != nil {
			return nil, "", err
		}
		return page.Items, page.Continue, nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list nodes of the standby: %w", err)
	}
	type nodeRoom struct {
		cpu, memory int64
	}
	rooms := map[string]*nodeRoom{}
	for i := range nodes {
		if !usableNode(&nodes[i]) {
			continue
		}
		report.StandbyNodes++
		cpu, memory := nodes[i].Status.Allocatable.Cpu().MilliValue(), nodes[i].Status.Allocatable.Memory().Value()
		report.CPU.Allocatable += cpu
		report.Memory.Allocatable += memory
		rooms[nodes[i].Name] = &nodeRoom{cpu: cpu, memory: memory}
	}

	pods, truncated, err := pageAll(maxObjects, func(o metav1.ListOptions) ([]corev1.Pod, string, error) {
		o.FieldSelector = "status.phase!=Succeeded,status.phase!=Failed"
		page, err := standby.Client.CoreV1().Pods("").List(ctx, o)
		if err != nil {
			return nil, "", err
		}
		return page.Items, page.Continue, nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list pods of the standby: %w", err)
	}
	if truncated {
		report.Notices = append(report.Notices, fmt.Sprintf("The standby has more than %d pods; the free capacity may be overstated.", maxObjects))
	}
	for i := range pods {
		p := &pods[i]
		room, ok := rooms[p.Spec.NodeName]
		if !ok || p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		cpu, memory := podRequests(&p.Spec)
		report.CPU.Requested += cpu
		report.Memory.Requested += memory
		room.cpu -= cpu
		room.memory -= memory
	}

	report.CPU.Free = report.CPU.Allocatable - report.CPU.Requested
	report.Memory.Free = report.Memory.Allocatable - report.Memory.Requested
	report.CPU.Fits = report.CPU.Demand <= report.CPU.Free
	report.Memory.Fits = report.Memory.Demand <= report.Memory.Free
	report.CPU.DemandText, report.CPU.FreeText = formatCPU(report.CPU.Demand), formatCPU(report.CPU.Free)
	report.Memory.DemandText, report.Memory.FreeText = formatBytes(report.Memory.Demand), formatBytes(report.Memory.Free)

	report.Fits = report.CPU.Fits && report.Memory.Fits
	if biggest.name != "" {
		largest := &LargestPod{Name: biggest.name, CPU: biggest.cpu, Memory: biggest.memory}
		names := make([]string, 0, len(rooms))
		for name := range rooms {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			r := rooms[name]
			if r.cpu >= largest.CPU && r.memory >= largest.Memory {
				largest.Fits = true
			}
			if largest.BestNode == "" || r.memory > largest.NodeFreeM {
				largest.BestNode, largest.NodeFreeC, largest.NodeFreeM = name, r.cpu, r.memory
			}
		}
		report.Largest = largest
		if !largest.Fits {
			report.Fits = false
		}
	}
	if report.StandbyNodes == 0 {
		report.Fits = false
		report.Notices = append(report.Notices, "The standby has no usable node (Ready, schedulable and not a tainted control-plane node).")
	}

	// Look the missing things up on the standby.
	for _, item := range missing.sorted() {
		if item.Kind == "Namespace" {
			report.Missing = append(report.Missing, item) // already known to be absent
			continue
		}
		exists, err := existsOnStandby(ctx, standby, item)
		if err != nil {
			report.Notices = append(report.Notices, fmt.Sprintf("Could not check %s %s on the standby: %v", item.Kind, item.Name, err))
			continue
		}
		if !exists {
			report.Missing = append(report.Missing, item)
		}
	}
	report.OK = report.Fits && len(report.Missing) == 0
	return report, nil
}

func defaultStorageClass(ctx context.Context, env *dr.Env) string {
	list, err := env.Client.StorageV1().StorageClasses().List(ctx, metav1.ListOptions{})
	if err != nil {
		return ""
	}
	best := ""
	for _, sc := range list.Items {
		if sc.Annotations["storageclass.kubernetes.io/is-default-class"] == "true" || sc.Annotations["storageclass.beta.kubernetes.io/is-default-class"] == "true" {
			if best == "" || sc.Name < best {
				best = sc.Name
			}
		}
	}
	return best
}

func existsOnStandby(ctx context.Context, env *dr.Env, item Missing) (bool, error) {
	var err error
	switch item.Kind {
	case "StorageClass":
		_, err = env.Client.StorageV1().StorageClasses().Get(ctx, item.Name, metav1.GetOptions{})
	case "PriorityClass":
		_, err = env.Client.SchedulingV1().PriorityClasses().Get(ctx, item.Name, metav1.GetOptions{})
	case "IngressClass":
		_, err = env.Client.NetworkingV1().IngressClasses().Get(ctx, item.Name, metav1.GetOptions{})
	default:
		return true, nil
	}
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	return err == nil, err
}

// missingSet collects the things to look up, remembering who uses them (a few names only).
type missingSet struct {
	items map[string]*Missing
	users map[string][]string
}

func newMissingSet() *missingSet {
	return &missingSet{items: map[string]*Missing{}, users: map[string][]string{}}
}

func (m *missingSet) add(kind, name, namespace, usedBy string) {
	key := kind + "/" + namespace + "/" + name
	if _, ok := m.items[key]; !ok {
		m.items[key] = &Missing{Kind: kind, Name: name, Namespace: namespace}
	}
	if len(m.users[key]) < 3 {
		m.users[key] = append(m.users[key], usedBy)
	}
}

func (m *missingSet) sorted() []Missing {
	keys := make([]string, 0, len(m.items))
	for k := range m.items {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]Missing, 0, len(keys))
	for _, k := range keys {
		item := *m.items[k]
		item.UsedBy = strings.Join(m.users[k], ", ")
		out = append(out, item)
	}
	return out
}

func formatCPU(milli int64) string {
	if milli%1000 == 0 {
		return fmt.Sprintf("%d cores", milli/1000)
	}
	return fmt.Sprintf("%.2f cores", float64(milli)/1000)
}

func formatBytes(b int64) string {
	const gib, mib = 1 << 30, 1 << 20
	switch {
	case b >= gib || b <= -gib:
		return fmt.Sprintf("%.1f GiB", float64(b)/gib)
	default:
		return fmt.Sprintf("%.0f MiB", float64(b)/mib)
	}
}
