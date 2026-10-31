package checks

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/1985epma/k8scli/internal/dr"
)

// Disclaimer is the honest reading of every score in this package.
const Disclaimer = "The score counts risk SIGNALS found in the cluster objects. It does not prove that anything can be recovered: only a restore drill does."

const (
	defaultMaxWorkloads = 2000
	defaultBackupAge    = 24 * time.Hour
	maxPodsPerNamespace = 20000

	// Per-severity deduction caps for one workload (60 + 30 + 10 = 100), so one noisy class of finding
	// cannot zero a score alone.
	capHigh   = 60
	capMedium = 30
	capLow    = 10
	penHigh   = 15
	penMedium = 6
	penLow    = 2
)

// Rule describes one readiness rule.
type Rule struct {
	ID       string   `json:"id"`
	Severity Severity `json:"severity"`
	Title    string   `json:"title"`
	Why      string   `json:"why"`
}

// Rules lists every rule with its default severity (a few findings are raised or lowered by context,
// for example DR004 is low when only the liveness probe is missing).
func Rules() []Rule {
	return []Rule{
		{"DR001", SeverityHigh, "Single replica", "One replica means any pod, node or rollout problem is an outage."},
		{"DR002", SeverityMedium, "No PodDisruptionBudget", "Node drains and upgrades can evict every replica at once."},
		{"DR003", SeverityMedium, "No spread across nodes or zones", "Replicas may be placed together and fail together."},
		{"DR004", SeverityMedium, "Missing readiness or liveness probe", "Traffic reaches pods that are not ready, and hung pods are not restarted."},
		{"DR005", SeverityMedium, "No resource requests", "The scheduler cannot place the workload predictably, and capacity planning for a standby cluster is impossible."},
		{"DR006", SeverityMedium, "Backup or snapshot is stale", "The newest backup covering this namespace is older than the allowed age."},
		{"DR007", SeverityMedium, "All replicas run on one node", "Losing that node takes the whole workload down."},
		{"DR008", SeverityHigh, "No backup at all for stateful data", "Persistent data has no backup or volume snapshot covering its namespace."},
		{"DR009", SeverityMedium, "Single-node cluster", "Any node failure is a full outage."},
		{"DR010", SeverityMedium, "No snapshot class for a StorageClass", "CSI volume snapshots cannot be taken for volumes of this StorageClass."},
		{"DR011", SeverityLow, "Image tag is latest or missing", "A restore may pull a different image than the one that was running."},
	}
}

// Finding is one problem found.
type Finding struct {
	Rule      string   `json:"rule"`
	Severity  Severity `json:"severity"`
	Kind      string   `json:"kind"`
	Name      string   `json:"name,omitempty"`
	Namespace string   `json:"namespace,omitempty"`
	Message   string   `json:"message"`
	// Fix is a concrete command or a hint.
	Fix string `json:"fix,omitempty"`
}

// WorkloadResult is the outcome for one Deployment or StatefulSet.
type WorkloadResult struct {
	Kind     string    `json:"kind"`
	Name     string    `json:"name"`
	Replicas int32     `json:"replicas"`
	Score    int       `json:"score"`
	Stopped  bool      `json:"stopped,omitempty"`
	Findings []Finding `json:"findings"`
}

// NamespaceResult is the outcome for one namespace.
type NamespaceResult struct {
	Name      string           `json:"name"`
	Score     int              `json:"score"`
	Workloads []WorkloadResult `json:"workloads"`
	// Findings are the namespace-level ones (PVCs, backups, storage classes).
	Findings []Finding `json:"findings"`
}

// Summary counts what was found, over ALL findings regardless of the severity filter.
type Summary struct {
	Namespaces int `json:"namespaces"`
	Workloads  int `json:"workloads"`
	Stopped    int `json:"stopped"`
	High       int `json:"high"`
	Medium     int `json:"medium"`
	Low        int `json:"low"`
}

// Report is the readiness result.
type Report struct {
	Context     string            `json:"context"`
	Generated   time.Time         `json:"generated"`
	Disclaimer  string            `json:"disclaimer"`
	Score       int               `json:"score"`
	MinSeverity Severity          `json:"minSeverity"`
	Summary     Summary           `json:"summary"`
	Cluster     []Finding         `json:"cluster"`
	Namespaces  []NamespaceResult `json:"namespaces"`
	Notices     []string          `json:"notices,omitempty"`
	Truncated   bool              `json:"truncated,omitempty"`
}

// ReadinessOptions tune Readiness.
type ReadinessOptions struct {
	// Namespaces limits the check; empty means every namespace except the system ones.
	Namespaces    []string
	IncludeSystem bool
	// MinSeverity hides findings below it (the score and the summary still count them).
	MinSeverity Severity
	// MaxWorkloads bounds memory and time (default 2000).
	MaxWorkloads int
	// Backups finds the newest backup of a namespace (default backups.Newest).
	Backups BackupFinder
	// BackupMaxAge is how old a backup may be before DR006 fires (default 24h).
	BackupMaxAge time.Duration
}

// workload is a Deployment or a StatefulSet, normalised.
type workload struct {
	Kind        string
	Name        string
	Namespace   string
	Replicas    int32
	Selector    *metav1.LabelSelector
	Template    corev1.PodTemplateSpec
	Annotations map[string]string
}

func (w workload) ref() string { return strings.ToLower(w.Kind) + "/" + w.Name }

type podInfo struct {
	Labels labels.Set
	Node   string
	Phase  corev1.PodPhase
}

type pvcInfo struct {
	Name         string
	StorageClass *string
}

// Readiness scores how well the workloads of a cluster are set up to survive failures. It reads only.
func Readiness(ctx context.Context, env *dr.Env, options ReadinessOptions) (*Report, error) {
	if err := requireClient(env); err != nil {
		return nil, err
	}
	if options.MinSeverity == "" {
		options.MinSeverity = SeverityLow
	}
	if options.MaxWorkloads <= 0 {
		options.MaxWorkloads = defaultMaxWorkloads
	}
	if options.BackupMaxAge <= 0 {
		options.BackupMaxAge = defaultBackupAge
	}
	find := defaultFinder(options.Backups)

	report := &Report{
		Context: env.Context, Generated: now(env), Disclaimer: Disclaimer, MinSeverity: options.MinSeverity,
		Cluster: []Finding{}, Namespaces: []NamespaceResult{},
	}

	namespaces := append([]string(nil), options.Namespaces...)
	if len(namespaces) == 0 {
		var err error
		if namespaces, err = namespaceNames(ctx, env.Client, options.IncludeSystem); err != nil {
			return nil, err
		}
	}
	sort.Strings(namespaces)

	analysis := &analysis{env: env, options: options, find: find, report: report, nodeCount: -1}
	analysis.loadNodes(ctx)
	remaining := options.MaxWorkloads
	for _, namespace := range namespaces {
		if remaining <= 0 {
			report.Truncated = true
			report.Notices = append(report.Notices, fmt.Sprintf("Stopped after %d workloads; narrow the namespaces or raise the limit.", options.MaxWorkloads))
			break
		}
		result, used, err := analysis.namespace(ctx, namespace, remaining)
		if err != nil {
			return nil, err
		}
		remaining -= used
		report.Namespaces = append(report.Namespaces, *result)
	}

	analysis.summarize()
	return report, nil
}

type analysis struct {
	env     *dr.Env
	options ReadinessOptions
	find    BackupFinder
	report  *Report

	// caches shared by namespaces
	storageClasses     map[string]storagev1.StorageClass
	snapshotDrivers    map[string]bool
	snapshotsKnown     bool
	snapshotCRDsAbsent bool

	// nodeCount is the number of nodes, or -1 when it could not be read. DR007 only makes sense with
	// two or more nodes: on a single node DR009 already says it.
	nodeCount int
}

func (a *analysis) notice(format string, args ...any) {
	a.report.Notices = append(a.report.Notices, fmt.Sprintf(format, args...))
}

// namespace analyses one namespace and returns the number of workloads it used from the budget.
func (a *analysis) namespace(ctx context.Context, namespace string, budget int) (*NamespaceResult, int, error) {
	client := a.env.Client
	result := &NamespaceResult{Name: namespace, Workloads: []WorkloadResult{}, Findings: []Finding{}}

	workloads, err := listWorkloads(ctx, client, namespace)
	if err != nil {
		return nil, 0, err
	}
	if len(workloads) > budget {
		workloads = workloads[:budget]
		a.report.Truncated = true
		a.notice("Namespace %s was cut at the workload limit.", namespace)
	}

	pdbs, _, err := pageAll(maxObjects, func(o metav1.ListOptions) ([]policyv1.PodDisruptionBudget, string, error) {
		page, err := client.PolicyV1().PodDisruptionBudgets(namespace).List(ctx, o)
		if err != nil {
			return nil, "", err
		}
		return page.Items, page.Continue, nil
	})
	if err != nil {
		a.notice("Could not read PodDisruptionBudgets in %s: %v", namespace, err)
	}
	pods, podsTruncated, err := pageAll(maxPodsPerNamespace, func(o metav1.ListOptions) ([]podInfo, string, error) {
		page, err := client.CoreV1().Pods(namespace).List(ctx, o)
		if err != nil {
			return nil, "", err
		}
		slim := make([]podInfo, 0, len(page.Items))
		for i := range page.Items {
			p := &page.Items[i]
			slim = append(slim, podInfo{Labels: labels.Set(p.Labels), Node: p.Spec.NodeName, Phase: p.Status.Phase})
		}
		return slim, page.Continue, nil
	})
	if err != nil {
		a.notice("Could not read pods in %s: %v", namespace, err)
	}
	if podsTruncated {
		a.notice("Namespace %s has more than %d pods; node placement checks may be incomplete.", namespace, maxPodsPerNamespace)
	}
	pvcs, _, err := pageAll(maxObjects, func(o metav1.ListOptions) ([]pvcInfo, string, error) {
		page, err := client.CoreV1().PersistentVolumeClaims(namespace).List(ctx, o)
		if err != nil {
			return nil, "", err
		}
		slim := make([]pvcInfo, 0, len(page.Items))
		for i := range page.Items {
			slim = append(slim, pvcInfo{Name: page.Items[i].Name, StorageClass: page.Items[i].Spec.StorageClassName})
		}
		return slim, page.Continue, nil
	})
	if err != nil {
		a.notice("Could not read PVCs in %s: %v", namespace, err)
	}

	hasStateful := len(pvcs) > 0
	for _, w := range workloads {
		if w.Kind == "StatefulSet" {
			hasStateful = true
		}
	}

	// Backups are read once per namespace.
	var backupAt time.Time
	backupFound, backupKnown := false, false
	if hasStateful {
		at, found, err := a.find(ctx, a.env, namespace)
		if err != nil {
			a.notice("Could not check backups for %s: %v", namespace, err)
		} else {
			backupAt, backupFound, backupKnown = at, found, true
		}
	}
	noBackup := backupKnown && !backupFound

	for _, w := range workloads {
		wr := WorkloadResult{Kind: w.Kind, Name: w.Name, Replicas: w.Replicas, Findings: []Finding{}}
		if w.Replicas == 0 {
			wr.Stopped = true
			wr.Score = 100
			result.Workloads = append(result.Workloads, wr)
			continue
		}
		wr.Findings = analyzeWorkload(w, pdbs, pods, a.nodeCount != 1)
		if w.Kind == "StatefulSet" && noBackup {
			wr.Findings = append(wr.Findings, Finding{
				Rule: "DR008", Severity: SeverityHigh, Kind: w.Kind, Name: w.Name, Namespace: namespace,
				Message: "No backup or volume snapshot covers this namespace, and this StatefulSet holds persistent data.",
				Fix:     backupFix(namespace),
			})
		}
		sortFindings(wr.Findings)
		wr.Score = workloadScore(wr.Findings)
		result.Workloads = append(result.Workloads, wr)
	}

	// Namespace-level findings: PVCs, backup age, storage classes.
	if len(pvcs) > 0 {
		switch {
		case noBackup:
			result.Findings = append(result.Findings, Finding{
				Rule: "DR008", Severity: SeverityHigh, Kind: "Namespace", Name: namespace, Namespace: namespace,
				Message: fmt.Sprintf("%d PVC(s) (%s) have no backup or volume snapshot covering the namespace.", len(pvcs), listNames(pvcs)),
				Fix:     backupFix(namespace),
			})
		case backupKnown && a.report.Generated.Sub(backupAt) > a.options.BackupMaxAge:
			age := a.report.Generated.Sub(backupAt)
			result.Findings = append(result.Findings, Finding{
				Rule: "DR006", Severity: SeverityMedium, Kind: "Namespace", Name: namespace, Namespace: namespace,
				Message: fmt.Sprintf("The newest backup covering this namespace is %s old (limit %s); %d PVC(s) depend on it.", humanAge(age), humanAge(a.options.BackupMaxAge), len(pvcs)),
				Fix:     "Check the backup schedule and its last runs: velero schedule get; velero backup get",
			})
		}
		result.Findings = append(result.Findings, a.storageClassFindings(ctx, namespace, pvcs)...)
	}
	sortFindings(result.Findings)

	result.Score = namespaceScore(result)
	return result, len(workloads), nil
}

func listNames(pvcs []pvcInfo) string {
	names := make([]string, 0, len(pvcs))
	for _, p := range pvcs {
		names = append(names, p.Name)
	}
	sort.Strings(names)
	if len(names) > 5 {
		return strings.Join(names[:5], ", ") + fmt.Sprintf(", and %d more", len(names)-5)
	}
	return strings.Join(names, ", ")
}

func backupFix(namespace string) string {
	return fmt.Sprintf("Install Velero and schedule a backup: velero schedule create %s-daily --schedule='0 2 * * *' --include-namespaces %s", namespace, namespace)
}

// analyzeWorkload applies the workload-level rules.
// checkPlacement is false on a single-node cluster, where DR007 would only repeat DR009.
func analyzeWorkload(w workload, pdbs []policyv1.PodDisruptionBudget, pods []podInfo, checkPlacement bool) []Finding {
	finding := func(rule string, severity Severity, message, fix string) Finding {
		return Finding{Rule: rule, Severity: severity, Kind: w.Kind, Name: w.Name, Namespace: w.Namespace, Message: message, Fix: fix}
	}
	var out []Finding
	spec := w.Template.Spec

	// DR001
	if w.Replicas == 1 {
		fix := fmt.Sprintf("kubectl scale %s -n %s --replicas=2", w.ref(), w.Namespace)
		if w.Kind == "StatefulSet" {
			fix = fmt.Sprintf("Run at least 2 replicas and make sure the application replicates its data: kubectl scale %s -n %s --replicas=2", w.ref(), w.Namespace)
		}
		out = append(out, finding("DR001", SeverityHigh, "Runs a single replica: any pod, node or rollout problem is an outage.", fix))
	}

	if w.Replicas > 1 {
		// DR002
		if !coveredByPDB(w, pdbs) {
			out = append(out, finding("DR002", SeverityMedium, "No PodDisruptionBudget covers this workload: a node drain can evict every replica at once.", pdbFix(w)))
		}
		// DR003
		if !hasSpread(spec) {
			out = append(out, finding("DR003", SeverityMedium, "No topologySpreadConstraints or pod anti-affinity: replicas may be scheduled together.",
				"Add topologySpreadConstraints with topologyKey kubernetes.io/hostname (and topology.kubernetes.io/zone) to the pod template."))
		}
		// DR007
		if node, together := allOnOneNode(w, pods); checkPlacement && together {
			out = append(out, finding("DR007", SeverityMedium, fmt.Sprintf("All running replicas are on node %s: losing it takes the workload down.", node),
				"Add topologySpreadConstraints or pod anti-affinity, then restart the rollout: kubectl rollout restart "+w.ref()+" -n "+w.Namespace))
		}
	}

	// DR004
	var noReadiness, noLiveness []string
	for _, c := range spec.Containers {
		if c.ReadinessProbe == nil {
			noReadiness = append(noReadiness, c.Name)
		}
		if c.LivenessProbe == nil {
			noLiveness = append(noLiveness, c.Name)
		}
	}
	if len(noReadiness) > 0 {
		out = append(out, finding("DR004", SeverityMedium, "No readiness probe on container(s): "+strings.Join(noReadiness, ", ")+". Traffic can reach pods that are not ready.",
			"Add a readinessProbe to "+strings.Join(noReadiness, ", ")+" in the pod template."))
	}
	if len(noLiveness) > 0 {
		out = append(out, finding("DR004", SeverityLow, "No liveness probe on container(s): "+strings.Join(noLiveness, ", ")+". Hung processes are not restarted.",
			"Add a livenessProbe to "+strings.Join(noLiveness, ", ")+" in the pod template."))
	}

	// DR005
	var noRequests, partialRequests []string
	for _, c := range spec.Containers {
		_, cpu := c.Resources.Requests[corev1.ResourceCPU]
		_, memory := c.Resources.Requests[corev1.ResourceMemory]
		switch {
		case !cpu && !memory:
			noRequests = append(noRequests, c.Name)
		case !cpu || !memory:
			partialRequests = append(partialRequests, c.Name)
		}
	}
	fixRequests := fmt.Sprintf("kubectl set resources %s -n %s --requests=cpu=100m,memory=128Mi", w.ref(), w.Namespace)
	if len(noRequests) > 0 {
		out = append(out, finding("DR005", SeverityMedium, "No resource requests on container(s): "+strings.Join(noRequests, ", ")+".", fixRequests))
	} else if len(partialRequests) > 0 {
		out = append(out, finding("DR005", SeverityLow, "Only one of cpu and memory is requested on container(s): "+strings.Join(partialRequests, ", ")+".", fixRequests))
	}

	// DR011
	var floating []string
	for _, c := range spec.Containers {
		if floatingImage(c.Image) {
			floating = append(floating, c.Name+" ("+c.Image+")")
		}
	}
	if len(floating) > 0 {
		out = append(out, finding("DR011", SeverityLow, "Image uses the latest tag or no tag: "+strings.Join(floating, ", ")+".",
			fmt.Sprintf("Pin a version or a digest: kubectl set image %s -n %s <container>=<image>:<version>", w.ref(), w.Namespace)))
	}
	return out
}

// floatingImage is true for "name", "name:latest" and false for tags and digests.
func floatingImage(image string) bool {
	if strings.Contains(image, "@") {
		return false
	}
	last := image
	if i := strings.LastIndex(image, "/"); i >= 0 {
		last = image[i+1:]
	}
	i := strings.LastIndex(last, ":")
	return i < 0 || last[i+1:] == "latest"
}

func coveredByPDB(w workload, pdbs []policyv1.PodDisruptionBudget) bool {
	set := labels.Set(w.Template.Labels)
	for i := range pdbs {
		if pdbs[i].Spec.Selector == nil {
			continue
		}
		selector, err := metav1.LabelSelectorAsSelector(pdbs[i].Spec.Selector)
		if err == nil && selector.Matches(set) {
			return true
		}
	}
	return false
}

func pdbFix(w workload) string {
	selector := ""
	if w.Selector != nil && len(w.Selector.MatchLabels) > 0 {
		keys := make([]string, 0, len(w.Selector.MatchLabels))
		for k, v := range w.Selector.MatchLabels {
			keys = append(keys, k+"="+v)
		}
		sort.Strings(keys)
		selector = strings.Join(keys, ",")
	}
	if selector == "" {
		return "Create a PodDisruptionBudget with minAvailable: 1 that selects the pods of " + w.ref()
	}
	return fmt.Sprintf("kubectl create poddisruptionbudget %s -n %s --selector=%s --min-available=1", w.Name, w.Namespace, selector)
}

func hasSpread(spec corev1.PodSpec) bool {
	if len(spec.TopologySpreadConstraints) > 0 {
		return true
	}
	anti := (*corev1.PodAntiAffinity)(nil)
	if spec.Affinity != nil {
		anti = spec.Affinity.PodAntiAffinity
	}
	return anti != nil && (len(anti.RequiredDuringSchedulingIgnoredDuringExecution) > 0 || len(anti.PreferredDuringSchedulingIgnoredDuringExecution) > 0)
}

// allOnOneNode reports the node when two or more running pods of the workload share a single node.
func allOnOneNode(w workload, pods []podInfo) (string, bool) {
	if w.Selector == nil {
		return "", false
	}
	selector, err := metav1.LabelSelectorAsSelector(w.Selector)
	if err != nil || selector.Empty() {
		return "", false
	}
	nodes := map[string]bool{}
	count := 0
	for _, p := range pods {
		if p.Phase == corev1.PodRunning && p.Node != "" && selector.Matches(p.Labels) {
			nodes[p.Node] = true
			count++
		}
	}
	if count < 2 || len(nodes) != 1 {
		return "", false
	}
	for node := range nodes {
		return node, true
	}
	return "", false
}

// storageClassFindings raises DR010 per StorageClass whose provisioner has no VolumeSnapshotClass.
func (a *analysis) storageClassFindings(ctx context.Context, namespace string, pvcs []pvcInfo) []Finding {
	if a.env.Dynamic == nil {
		return nil
	}
	if err := a.loadStorage(ctx); err != nil {
		a.notice("Could not check storage classes: %v", err)
		return nil
	}

	used := map[string][]string{}
	defaultClass := ""
	for name, sc := range a.storageClasses {
		if sc.Annotations["storageclass.kubernetes.io/is-default-class"] == "true" || sc.Annotations["storageclass.beta.kubernetes.io/is-default-class"] == "true" {
			if defaultClass == "" || name < defaultClass {
				defaultClass = name
			}
		}
	}
	for _, p := range pvcs {
		class := defaultClass
		if p.StorageClass != nil {
			class = *p.StorageClass
		}
		if class != "" {
			used[class] = append(used[class], p.Name)
		}
	}

	var out []Finding
	names := make([]string, 0, len(used))
	for name := range used {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		sc, ok := a.storageClasses[name]
		if !ok || a.snapshotDrivers[sc.Provisioner] {
			continue
		}
		message := fmt.Sprintf("StorageClass %s (provisioner %s) has no matching VolumeSnapshotClass, so CSI snapshots of its volumes are not possible.", name, sc.Provisioner)
		fix := "Install the external-snapshotter CRDs and create a VolumeSnapshotClass with driver " + sc.Provisioner
		if a.snapshotCRDsAbsent {
			message = fmt.Sprintf("StorageClass %s (provisioner %s): the VolumeSnapshot CRDs are not installed, so CSI snapshots are not possible.", name, sc.Provisioner)
		}
		out = append(out, Finding{Rule: "DR010", Severity: SeverityMedium, Kind: "StorageClass", Name: name, Namespace: namespace, Message: message, Fix: fix})
	}
	return out
}

var snapshotClasses = schema.GroupVersionResource{Group: "snapshot.storage.k8s.io", Version: "v1", Resource: "volumesnapshotclasses"}

// loadStorage reads StorageClasses and VolumeSnapshotClass drivers once.
func (a *analysis) loadStorage(ctx context.Context) error {
	if a.snapshotsKnown {
		return nil
	}
	list, _, err := pageAll(maxObjects, func(o metav1.ListOptions) ([]storagev1.StorageClass, string, error) {
		page, err := a.env.Client.StorageV1().StorageClasses().List(ctx, o)
		if err != nil {
			return nil, "", err
		}
		return page.Items, page.Continue, nil
	})
	if err != nil {
		return err
	}
	a.storageClasses = map[string]storagev1.StorageClass{}
	for _, sc := range list {
		a.storageClasses[sc.Name] = sc
	}

	a.snapshotDrivers = map[string]bool{}
	classes, err := a.env.Dynamic.Resource(snapshotClasses).List(ctx, metav1.ListOptions{})
	switch {
	case err == nil:
		for i := range classes.Items {
			if driver, _, _ := unstructured.NestedString(classes.Items[i].Object, "driver"); driver != "" {
				a.snapshotDrivers[driver] = true
			}
		}
	case apierrors.IsNotFound(err) || meta.IsNoMatchError(err):
		a.snapshotCRDsAbsent = true
	default:
		return err
	}
	a.snapshotsKnown = true
	return nil
}

// loadNodes counts the nodes once; the count feeds DR007 and DR009.
func (a *analysis) loadNodes(ctx context.Context) {
	names, _, err := pageAll(maxObjects, func(o metav1.ListOptions) ([]string, string, error) {
		page, err := a.env.Client.CoreV1().Nodes().List(ctx, o)
		if err != nil {
			return nil, "", err
		}
		names := make([]string, 0, len(page.Items))
		for i := range page.Items {
			names = append(names, page.Items[i].Name)
		}
		return names, page.Continue, nil
	})
	if err != nil {
		a.notice("Could not list nodes: %v", err)
		return
	}
	a.nodeCount = len(names)
	if len(names) == 1 {
		a.report.Cluster = append(a.report.Cluster, Finding{
			Rule: "DR009", Severity: SeverityMedium, Kind: "Cluster", Name: a.env.Context,
			Message: fmt.Sprintf("The cluster has a single node (%s): any node failure is a full outage.", names[0]),
			Fix:     "Add worker nodes (a node group, node pool or machine) so workloads can be rescheduled.",
		})
	}
}

// ---- scoring -----------------------------------------------------------------------------------------------

// deduction is the capped penalty of a list of findings.
func deduction(findings []Finding) int {
	var high, medium, low int
	for _, f := range findings {
		switch f.Severity {
		case SeverityHigh:
			high += penHigh
		case SeverityMedium:
			medium += penMedium
		default:
			low += penLow
		}
	}
	return minInt(high, capHigh) + minInt(medium, capMedium) + minInt(low, capLow)
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func clampScore(v int) int {
	switch {
	case v < 0:
		return 0
	case v > 100:
		return 100
	default:
		return v
	}
}

// workloadScore is 100 minus the capped deduction.
func workloadScore(findings []Finding) int { return clampScore(100 - deduction(findings)) }

// namespaceScore is 0.7 x the mean workload score + 0.3 x the worst one (so a single bad workload cannot
// hide behind healthy ones), minus the capped deduction of the namespace-level findings. A namespace with
// no running workloads starts from 100.
func namespaceScore(ns *NamespaceResult) int {
	sum, worst, count := 0, 100, 0
	for _, w := range ns.Workloads {
		if w.Stopped {
			continue
		}
		sum += w.Score
		if w.Score < worst {
			worst = w.Score
		}
		count++
	}
	base := 100
	if count > 0 {
		mean := float64(sum) / float64(count)
		base = int(math.Round(0.7*mean + 0.3*float64(worst)))
	}
	return clampScore(base - deduction(ns.Findings))
}

func (a *analysis) summarize() {
	r := a.report
	r.Summary.Namespaces = len(r.Namespaces)

	weighted, weight := 0, 0
	count := func(findings []Finding) {
		for _, f := range findings {
			switch f.Severity {
			case SeverityHigh:
				r.Summary.High++
			case SeverityMedium:
				r.Summary.Medium++
			default:
				r.Summary.Low++
			}
		}
	}
	count(r.Cluster)
	for i := range r.Namespaces {
		ns := &r.Namespaces[i]
		running := 0
		for j := range ns.Workloads {
			w := &ns.Workloads[j]
			r.Summary.Workloads++
			if w.Stopped {
				r.Summary.Stopped++
			} else {
				running++
			}
			count(w.Findings)
		}
		count(ns.Findings)
		weighted += ns.Score * running
		weight += running
	}

	overall := 100
	if weight > 0 {
		overall = int(math.Round(float64(weighted) / float64(weight)))
	}
	r.Score = clampScore(overall - deduction(r.Cluster))

	// Apply the severity filter to what is shown, after everything is counted and scored.
	minRank := a.options.MinSeverity.rank()
	filter := func(in []Finding) []Finding {
		out := make([]Finding, 0, len(in))
		for _, f := range in {
			if f.Severity.rank() >= minRank {
				out = append(out, f)
			}
		}
		return out
	}
	r.Cluster = filter(r.Cluster)
	for i := range r.Namespaces {
		r.Namespaces[i].Findings = filter(r.Namespaces[i].Findings)
		for j := range r.Namespaces[i].Workloads {
			r.Namespaces[i].Workloads[j].Findings = filter(r.Namespaces[i].Workloads[j].Findings)
		}
	}
	sort.Strings(r.Notices)
}

// sortFindings orders by severity (high first), then rule, kind and name.
func sortFindings(findings []Finding) {
	sort.SliceStable(findings, func(i, j int) bool {
		a, b := findings[i], findings[j]
		switch {
		case a.Severity.rank() != b.Severity.rank():
			return a.Severity.rank() > b.Severity.rank()
		case a.Rule != b.Rule:
			return a.Rule < b.Rule
		case a.Kind != b.Kind:
			return a.Kind < b.Kind
		default:
			return a.Name < b.Name
		}
	})
}
