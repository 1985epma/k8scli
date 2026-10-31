package checks

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	schedulingv1 "k8s.io/api/scheduling/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/1985epma/k8scli/internal/dr"
)

func requests(cpu, memory string) corev1.ResourceList {
	return corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu), corev1.ResourceMemory: resource.MustParse(memory)}
}

func sized(ns, name string, replicas int32, cpu, memory string, mutate ...func(*appsv1.Deployment)) *appsv1.Deployment {
	return deployment(ns, name, replicas, append([]func(*appsv1.Deployment){func(d *appsv1.Deployment) {
		d.Spec.Template.Spec.Containers[0].Resources.Requests = requests(cpu, memory)
	}}, mutate...)...)
}

func readyNode(name, cpu, memory string, mutate ...func(*corev1.Node)) *corev1.Node {
	n := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{
			Allocatable: requests(cpu, memory),
			Conditions:  []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
		},
	}
	for _, m := range mutate {
		m(n)
	}
	return n
}

func podOn(ns, name, node, cpu, memory string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       corev1.PodSpec{NodeName: node, Containers: []corev1.Container{{Name: "c", Resources: corev1.ResourceRequirements{Requests: requests(cpu, memory)}}}},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

func envOf(context string, objects ...runtime.Object) *dr.Env {
	return &dr.Env{Context: context, Client: fake.NewSimpleClientset(objects...), Now: func() time.Time { return fixedNow }}
}

func TestCapacityArithmetic(t *testing.T) {
	primary := envOf("prod", namespace("shop"),
		sized("shop", "api", 3, "500m", "1Gi"),   // 1500m, 3Gi
		sized("shop", "web", 2, "250m", "512Mi"), // 500m, 1Gi
	)
	standby := envOf("dr",
		namespace("shop"),
		readyNode("n1", "4", "8Gi"), readyNode("n2", "4", "8Gi"),
		podOn("other", "busy", "n1", "1", "2Gi"), // already requested: 1 CPU, 2Gi
	)

	report, err := CapacityFit(context.Background(), primary, standby, CapacityOptions{Namespaces: []string{"shop"}})
	if err != nil {
		t.Fatal(err)
	}
	if report.CPU.Demand != 2000 || report.Memory.Demand != 4<<30 {
		t.Errorf("demand = %d m, %d bytes; want 2000 m and 4Gi", report.CPU.Demand, report.Memory.Demand)
	}
	if report.CPU.Allocatable != 8000 || report.CPU.Requested != 1000 || report.CPU.Free != 7000 {
		t.Errorf("cpu supply = %+v", report.CPU)
	}
	if report.Memory.Free != 14<<30 || report.Workloads != 2 || report.Pods != 5 || report.StandbyNodes != 2 {
		t.Errorf("memory free = %d workloads=%d pods=%d nodes=%d", report.Memory.Free, report.Workloads, report.Pods, report.StandbyNodes)
	}
	if !report.Fits || !report.OK || report.CPU.DemandText != "2 cores" || report.Memory.DemandText != "4.0 GiB" {
		t.Errorf("report = %+v", report)
	}
}

func TestCapacityDoesNotFitWhenMemoryIsShort(t *testing.T) {
	primary := envOf("prod", namespace("shop"), sized("shop", "big", 4, "100m", "4Gi")) // 16Gi
	standby := envOf("dr", namespace("shop"), readyNode("n1", "8", "8Gi"), readyNode("n2", "8", "4Gi"))

	report, _ := CapacityFit(context.Background(), primary, standby, CapacityOptions{Namespaces: []string{"shop"}})
	if report.Memory.Fits || !report.CPU.Fits || report.Fits || report.OK {
		t.Errorf("report = %+v", report)
	}
}

func TestCapacityLargestPodMustFitOnOneNode(t *testing.T) {
	// Plenty of memory in total (6Gi across two nodes) but a 5Gi pod fits on neither.
	primary := envOf("prod", namespace("shop"), sized("shop", "db", 1, "100m", "5Gi"))
	standby := envOf("dr", namespace("shop"), readyNode("n1", "4", "3Gi"), readyNode("n2", "4", "3Gi"))

	report, _ := CapacityFit(context.Background(), primary, standby, CapacityOptions{Namespaces: []string{"shop"}})
	if !report.CPU.Fits || !report.Memory.Fits {
		t.Fatalf("the sums fit: %+v %+v", report.CPU, report.Memory)
	}
	if report.Largest == nil || report.Largest.Fits || report.Largest.Name != "shop/db" || report.Fits {
		t.Errorf("largest = %+v fits=%v; a 5Gi pod must not fit on 3Gi nodes", report.Largest, report.Fits)
	}
}

func TestCapacityUsesTheRecordedReplicasOfStoppedWorkloads(t *testing.T) {
	stopped := sized("shop", "api", 0, "1", "1Gi", func(d *appsv1.Deployment) {
		d.Annotations = map[string]string{replicasBeforeAnnotation: "3"}
	})
	primary := envOf("prod", namespace("shop"), stopped)
	standby := envOf("dr", namespace("shop"), readyNode("n1", "8", "8Gi"))

	report, _ := CapacityFit(context.Background(), primary, standby, CapacityOptions{Namespaces: []string{"shop"}})
	if report.CPU.Demand != 3000 || report.Pods != 3 {
		t.Errorf("demand = %d, pods = %d; a stopped workload counts at its recorded size", report.CPU.Demand, report.Pods)
	}
	if got := desiredReplicas(workload{Replicas: 0, Annotations: map[string]string{replicasBeforeAnnotation: "oops"}}); got != 0 {
		t.Errorf("a garbled annotation must be ignored, got %d", got)
	}
}

func TestCapacityInitContainersFollowTheSchedulerRule(t *testing.T) {
	spec := &corev1.PodSpec{
		Containers:     []corev1.Container{{Resources: corev1.ResourceRequirements{Requests: requests("100m", "100Mi")}}, {Resources: corev1.ResourceRequirements{Requests: requests("100m", "100Mi")}}},
		InitContainers: []corev1.Container{{Resources: corev1.ResourceRequirements{Requests: requests("1", "50Mi")}}},
	}
	cpu, memory := podRequests(spec)
	if cpu != 1000 || memory != 200<<20 {
		t.Errorf("effective request = %d m, %d bytes; want 1000 m (init wins) and 200Mi (sum wins)", cpu, memory)
	}
}

func TestCapacityIgnoresUnusableNodes(t *testing.T) {
	primary := envOf("prod", namespace("shop"), sized("shop", "api", 1, "1", "1Gi"))
	cordoned := readyNode("cordoned", "16", "64Gi", func(n *corev1.Node) { n.Spec.Unschedulable = true })
	notReady := readyNode("down", "16", "64Gi", func(n *corev1.Node) { n.Status.Conditions[0].Status = corev1.ConditionFalse })
	controlPlane := readyNode("cp", "16", "64Gi", func(n *corev1.Node) {
		n.Spec.Taints = []corev1.Taint{{Key: "node-role.kubernetes.io/control-plane", Effect: corev1.TaintEffectNoSchedule}}
	})
	standby := envOf("dr", namespace("shop"), cordoned, notReady, controlPlane, readyNode("worker", "2", "4Gi"))

	report, _ := CapacityFit(context.Background(), primary, standby, CapacityOptions{Namespaces: []string{"shop"}})
	if report.StandbyNodes != 1 || report.CPU.Allocatable != 2000 {
		t.Errorf("usable nodes = %d, cpu = %d; only the worker counts", report.StandbyNodes, report.CPU.Allocatable)
	}

	none := envOf("dr", namespace("shop"), cordoned)
	empty, _ := CapacityFit(context.Background(), primary, none, CapacityOptions{Namespaces: []string{"shop"}})
	if empty.Fits || len(empty.Notices) == 0 {
		t.Errorf("a standby without usable nodes cannot fit anything: %+v", empty)
	}
}

func TestCapacityIgnoresFinishedPods(t *testing.T) {
	primary := envOf("prod", namespace("shop"), sized("shop", "api", 1, "1", "1Gi"))
	done := podOn("other", "job", "n1", "3", "3Gi")
	done.Status.Phase = corev1.PodSucceeded
	standby := envOf("dr", namespace("shop"), readyNode("n1", "4", "8Gi"), done)

	report, _ := CapacityFit(context.Background(), primary, standby, CapacityOptions{Namespaces: []string{"shop"}})
	if report.CPU.Requested != 0 {
		t.Errorf("requested = %d; a finished pod holds nothing", report.CPU.Requested)
	}
}

func TestCapacityReportsWhatTheStandbyLacks(t *testing.T) {
	class := "fast"
	ingressClass := "nginx"
	primary := envOf("prod",
		namespace("shop"), namespace("billing"),
		sized("shop", "api", 1, "100m", "100Mi", func(d *appsv1.Deployment) { d.Spec.Template.Spec.PriorityClassName = "critical" }),
		sized("shop", "sys", 1, "100m", "100Mi", func(d *appsv1.Deployment) { d.Spec.Template.Spec.PriorityClassName = "system-cluster-critical" }),
		sized("billing", "inv", 1, "100m", "100Mi"),
		pvc("shop", "data", &class),
		pvc("shop", "plain", nil), // default class "standard"
		storageClass("standard", "x", true),
		&networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "shop"}, Spec: networkingv1.IngressSpec{IngressClassName: &ingressClass}},
		&networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: "legacy", Namespace: "shop", Annotations: map[string]string{"kubernetes.io/ingress.class": "traefik"}}},
	)
	standby := envOf("dr",
		namespace("shop"), // billing is missing
		readyNode("n1", "8", "16Gi"),
		storageClass("standard", "x", true), // fast is missing
		&schedulingv1.PriorityClass{ObjectMeta: metav1.ObjectMeta{Name: "unrelated"}},
		&networkingv1.IngressClass{ObjectMeta: metav1.ObjectMeta{Name: "traefik"}}, // nginx is missing
	)

	report, err := CapacityFit(context.Background(), primary, standby, CapacityOptions{Namespaces: []string{"billing", "shop"}})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, m := range report.Missing {
		got = append(got, m.Kind+"/"+m.Name)
	}
	want := []string{"IngressClass/nginx", "Namespace/billing", "PriorityClass/critical", "StorageClass/fast"}
	if len(got) != len(want) {
		t.Fatalf("missing = %v; want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("missing = %v; want %v", got, want)
		}
	}
	if !report.Fits || report.OK {
		t.Errorf("fits=%v ok=%v; the resources fit but things are missing", report.Fits, report.OK)
	}
	if report.Missing[2].UsedBy != "shop/api" {
		t.Errorf("usedBy = %q", report.Missing[2].UsedBy)
	}
}

func TestCapacityWorkloadFilter(t *testing.T) {
	primary := envOf("prod", namespace("shop"), sized("shop", "api", 2, "1", "1Gi"), sized("shop", "batch", 5, "1", "1Gi"))
	standby := envOf("dr", namespace("shop"), readyNode("n1", "16", "16Gi"))

	report, _ := CapacityFit(context.Background(), primary, standby, CapacityOptions{
		Namespaces: []string{"shop"}, Workloads: map[string][]string{"shop": {"api"}},
	})
	if report.Workloads != 1 || report.CPU.Demand != 2000 {
		t.Errorf("workloads = %d demand = %d; only api was selected", report.Workloads, report.CPU.Demand)
	}
}

func TestCapacityOptionsFromPlan(t *testing.T) {
	plan := &dr.Plan{
		Apps: []dr.App{
			{Name: "api", Namespace: "shop", Workloads: []string{"api"}},
			{Name: "web", Namespace: "shop", Workloads: []string{"web"}},
			{Name: "all", Namespace: "billing"},
		},
		Databases: []dr.Database{
			{Name: "pg", Namespace: "shop", Kind: "statefulset", Ref: "pg"},
			{Name: "cnpg", Namespace: "orders", Kind: "cnpg", Ref: "main"},
		},
	}
	options := CapacityOptionsFromPlan(plan)
	if len(options.Namespaces) != 3 || options.Namespaces[0] != "billing" || options.Namespaces[1] != "orders" || options.Namespaces[2] != "shop" {
		t.Errorf("namespaces = %v", options.Namespaces)
	}
	if _, ok := options.Workloads["billing"]; ok {
		t.Error("a namespace taken whole must not be filtered")
	}
	if _, ok := options.Workloads["orders"]; ok {
		t.Error("a CNPG database takes its namespace whole")
	}
	if got := options.Workloads["shop"]; len(got) != 3 {
		t.Errorf("shop workloads = %v; want api, web and pg", got)
	}
}

func TestCapacityNeedsBothClients(t *testing.T) {
	ok := envOf("x")
	if _, err := CapacityFit(context.Background(), &dr.Env{}, ok, CapacityOptions{}); err == nil {
		t.Error("expected an error without a primary client")
	}
	if _, err := CapacityFit(context.Background(), ok, &dr.Env{}, CapacityOptions{}); err == nil {
		t.Error("expected an error without a standby client")
	}
}

func TestFormatting(t *testing.T) {
	if formatCPU(2000) != "2 cores" || formatCPU(1500) != "1.50 cores" {
		t.Errorf("cpu: %q %q", formatCPU(2000), formatCPU(1500))
	}
	if formatBytes(512<<20) != "512 MiB" || formatBytes(3<<30) != "3.0 GiB" {
		t.Errorf("bytes: %q %q", formatBytes(512<<20), formatBytes(3<<30))
	}
}
