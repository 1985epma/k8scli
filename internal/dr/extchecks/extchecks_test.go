package extchecks

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	fakeclient "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/1985epma/k8scli/internal/dr"
	"github.com/1985epma/k8scli/internal/dr/checks"
)

// ---- registries -----------------------------------------------------------------------------------------------

func TestImageHost(t *testing.T) {
	for image, want := range map[string]string{
		"nginx": "docker.io", "nginx:1.27": "docker.io", "library/nginx": "docker.io", "grafana/grafana:11": "docker.io",
		"ghcr.io/org/app:1": "ghcr.io", "registry.example.com/team/app@sha256:abc": "registry.example.com",
		"localhost:5000/app": "localhost:5000", "123456789012.dkr.ecr.eu-west-1.amazonaws.com/app:1": "123456789012.dkr.ecr.eu-west-1.amazonaws.com",
		"Quay.IO/org/app": "quay.io",
	} {
		if got := imageHost(image); got != want {
			t.Errorf("imageHost(%q) = %q; want %q", image, got, want)
		}
	}
}

func TestRegistryRegion(t *testing.T) {
	for host, want := range map[string]struct {
		region   string
		regional bool
	}{
		"123456789012.dkr.ecr.eu-west-1.amazonaws.com":      {"eu-west-1", true},
		"123456789012.dkr.ecr-fips.us-east-1.amazonaws.com": {"us-east-1", true},
		"europe-west4-docker.pkg.dev":                       {"europe-west4", true},
		"us-docker.pkg.dev":                                 {"", false}, // a multi-region location
		"europe-docker.pkg.dev":                             {"", false},
		"gcr.io":                                            {"", false},
		"public.ecr.aws":                                    {"", false},
		"registry.example.com":                              {"", false},
	} {
		region, regional := registryRegion(host)
		if region != want.region || regional != want.regional {
			t.Errorf("registryRegion(%q) = %q, %v; want %q, %v", host, region, regional, want.region, want.regional)
		}
	}
}

func TestRegionBoundRegistriesAreFlagged(t *testing.T) {
	primary := newEnv("primary", []runtime.Object{
		deploy("shop", "ecr", image("123456789012.dkr.ecr.eu-west-1.amazonaws.com/app:1")),
		deploy("shop", "ar", image("europe-west4-docker.pkg.dev/proj/repo/app:1")),
		deploy("shop", "multi", image("us-docker.pkg.dev/proj/repo/app:1")),
	})
	report := run(t, Options{Primary: primary, Namespaces: []string{"shop"}})
	wantRule(t, report, "DR101", "ecr")
	wantRule(t, report, "DR101", "ar")
	for _, f := range findingsOf(report, "DR101") {
		if f.Name == "multi" {
			t.Errorf("a multi-region Artifact Registry location is not region bound: %+v", f)
		}
	}
	if f := wantRule(t, report, "DR101", "ecr"); f.Severity != checks.SeverityMedium || !strings.Contains(f.Message, "eu-west-1") {
		t.Errorf("finding = %+v", f)
	}
}

func TestDockerHubAndPrivateRegistries(t *testing.T) {
	primary := newEnv("primary", []runtime.Object{
		deploy("shop", "hub", image("nginx:1.27")),
		deploy("shop", "hub-authed", func(s *corev1.PodSpec) {
			s.Containers[0].Image = "nginx:1.27"
			s.ImagePullSecrets = []corev1.LocalObjectReference{{Name: "dockerhub"}}
		}),
		deploy("shop", "private", image("registry.example.com/app:1")),
		deploy("shop", "private-sa", func(s *corev1.PodSpec) { s.ServiceAccountName = "robot" }),
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "robot", Namespace: "shop"}, ImagePullSecrets: []corev1.LocalObjectReference{{Name: "regcred"}}},
		deploy("shop", "public", image("ghcr.io/org/app:1")),
	})
	report := run(t, Options{Primary: primary, Namespaces: []string{"shop"}})

	wantRule(t, report, "DR103", "hub")
	wantRule(t, report, "DR104", "private")
	for _, name := range []string{"hub-authed", "private-sa", "public"} {
		for _, rule := range []string{"DR103", "DR104"} {
			for _, f := range findingsOf(report, rule) {
				if f.Name == name {
					t.Errorf("%s must not get %s: %+v", name, rule, f)
				}
			}
		}
	}
}

func TestPullSecretsMustExistInTheTargetCluster(t *testing.T) {
	withSecret := func(name string) *corev1.PodSpec { return nil }
	_ = withSecret
	workload := deploy("shop", "api", func(s *corev1.PodSpec) {
		s.ImagePullSecrets = []corev1.LocalObjectReference{{Name: "regcred"}, {Name: "other"}}
	})
	primary := newEnv("primary", []runtime.Object{ns("shop"), workload, secret("shop", "regcred", nil), secret("shop", "other", nil)})

	// The DR cluster has one of the two: only the other is reported.
	standby := newEnv("dr", []runtime.Object{ns("shop"), secret("shop", "regcred", map[string]string{"k": "v"})})
	report := run(t, Options{Primary: primary, Standby: standby, Namespaces: []string{"shop"}})
	f := wantRule(t, report, "DR102", "api")
	if f.Severity != checks.SeverityHigh || !strings.Contains(f.Message, `"other"`) || !strings.Contains(f.Message, "the DR cluster") {
		t.Errorf("finding = %+v", f)
	}
	if len(findingsOf(report, "DR102")) != 1 {
		t.Errorf("only the missing one must be reported: %s", dump(report))
	}

	// Without a standby the same check runs against the primary itself.
	broken := newEnv("primary", []runtime.Object{ns("shop"), workload, secret("shop", "regcred", nil)})
	report = run(t, Options{Primary: broken, Namespaces: []string{"shop"}})
	if f := wantRule(t, report, "DR102", "api"); !strings.Contains(f.Message, "this cluster") {
		t.Errorf("finding = %+v", f)
	}
}

func TestASingleRegistryIsANote(t *testing.T) {
	one := newEnv("primary", []runtime.Object{
		deploy("shop", "a", image("registry.example.com/a:1")), deploy("shop", "b", image("registry.example.com/b:1")),
	})
	report := run(t, Options{Primary: one, Namespaces: []string{"shop"}})
	if f := wantRule(t, report, "DR105", "registry.example.com"); f.Severity != checks.SeverityLow {
		t.Errorf("finding = %+v", f)
	}
	if len(report.Registries) != 1 || report.Registries[0].Images != 2 || !report.Registries[0].Private {
		t.Errorf("registries = %+v", report.Registries)
	}

	two := newEnv("primary", []runtime.Object{
		deploy("shop", "a", image("registry.example.com/a:1")), deploy("shop", "b", image("ghcr.io/b:1")),
	})
	noRule(t, run(t, Options{Primary: two, Namespaces: []string{"shop"}}), "DR105")
	alone := newEnv("primary", []runtime.Object{deploy("shop", "a", image("registry.example.com/a:1"))})
	noRule(t, run(t, Options{Primary: alone, Namespaces: []string{"shop"}}), "DR105")
}

// ---- references ------------------------------------------------------------------------------------------------

func referencingWorkload() *appsv1.Deployment {
	optional := true
	return deploy("shop", "api", func(s *corev1.PodSpec) {
		s.Containers[0].Env = []corev1.EnvVar{
			{Name: "A", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "db-pass"}, Key: "p"}}},
			{Name: "B", ValueFrom: &corev1.EnvVarSource{ConfigMapKeyRef: &corev1.ConfigMapKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "app-cfg"}, Key: "k"}}},
			{Name: "C", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "maybe"}, Key: "p", Optional: &optional}}},
			{Name: "D", Value: "plain"},
		}
		s.Containers[0].EnvFrom = []corev1.EnvFromSource{
			{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "from-secret"}}},
			{ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "from-cm"}}},
		}
		s.InitContainers = []corev1.Container{{Name: "init", Image: "busybox", Env: []corev1.EnvVar{
			{Name: "E", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "init-secret"}, Key: "p"}}},
		}}}
		s.Volumes = []corev1.Volume{
			{Name: "v1", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "vol-secret"}}},
			{Name: "v2", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "vol-cm"}}}},
			{Name: "v3", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "vol-optional", Optional: &optional}}},
			{Name: "v4", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{Sources: []corev1.VolumeProjection{
				{Secret: &corev1.SecretProjection{LocalObjectReference: corev1.LocalObjectReference{Name: "proj-secret"}}},
				{ConfigMap: &corev1.ConfigMapProjection{LocalObjectReference: corev1.LocalObjectReference{Name: "proj-cm"}}},
			}}}},
		}
	})
}

func TestReferencedSecretsAndConfigMapsMustExistInTheTarget(t *testing.T) {
	primary := newEnv("primary", []runtime.Object{ns("shop"), referencingWorkload()})
	standby := newEnv("dr", []runtime.Object{ns("shop"), secret("shop", "db-pass", nil), configMap("shop", "app-cfg", nil)})
	report := run(t, Options{Primary: primary, Standby: standby, Namespaces: []string{"shop"}})

	var secrets, configMaps []string
	for _, f := range findingsOf(report, "DR120") {
		secrets = append(secrets, f.Message)
	}
	for _, f := range findingsOf(report, "DR121") {
		configMaps = append(configMaps, f.Message)
	}
	gotSecrets, gotCMs := strings.Join(secrets, "|"), strings.Join(configMaps, "|")
	for _, want := range []string{"from-secret", "init-secret", "vol-secret", "proj-secret"} {
		if !strings.Contains(gotSecrets, want) {
			t.Errorf("DR120 should report %q: %s", want, gotSecrets)
		}
	}
	for _, want := range []string{"from-cm", "vol-cm", "proj-cm"} {
		if !strings.Contains(gotCMs, want) {
			t.Errorf("DR121 should report %q: %s", want, gotCMs)
		}
	}
	// Present in the standby, or optional: never reported.
	for _, unwanted := range []string{"db-pass", "maybe", "vol-optional", "app-cfg"} {
		if strings.Contains(gotSecrets+gotCMs, unwanted) {
			t.Errorf("%q must not be reported: %s %s", unwanted, gotSecrets, gotCMs)
		}
	}
	if f := findingsOf(report, "DR120")[0]; f.Severity != checks.SeverityHigh || f.Kind != "Deployment" || f.Namespace != "shop" {
		t.Errorf("finding = %+v", f)
	}
}

// The core promise of this package: values are never read into a result.
func TestNoSecretOrConfigMapValueEverReachesTheReport(t *testing.T) {
	const secretValue, configValue = "SUPER-SECRET-VALUE-9f3a", "CONFIG-VALUE-77ab"
	primary := newEnv("primary", []runtime.Object{
		ns("shop"), referencingWorkload(),
		secret("shop", "db-pass", map[string]string{"password": secretValue}),
		configMap("shop", "app-cfg", map[string]string{"setting": configValue}),
	})
	standby := newEnv("dr", []runtime.Object{
		ns("shop"),
		secret("shop", "db-pass", map[string]string{"password": secretValue}),
		secret("shop", "regcred", map[string]string{".dockerconfigjson": secretValue}),
		configMap("shop", "app-cfg", map[string]string{"setting": configValue}),
	})
	report := run(t, Options{Primary: primary, Standby: standby, Namespaces: []string{"shop"}})
	out := marshal(t, report)
	for _, value := range []string{secretValue, configValue} {
		if strings.Contains(out, value) {
			t.Fatalf("a value leaked into the report: %s", out)
		}
	}

	// And nothing but list calls was made on Secrets and ConfigMaps: never a get of one object.
	client := standby.Client.(*fakeclient.Clientset)
	for _, action := range client.Actions() {
		res := action.GetResource().Resource
		if (res == "secrets" || res == "configmaps") && action.GetVerb() != "list" {
			t.Errorf("unexpected %s on %s", action.GetVerb(), res)
		}
	}
}

func TestReferencesAreCheckedAgainstThePrimaryWithoutAStandby(t *testing.T) {
	primary := newEnv("primary", []runtime.Object{ns("shop"), referencingWorkload(), secret("shop", "db-pass", nil)})
	report := run(t, Options{Primary: primary, Namespaces: []string{"shop"}})
	f := wantRule(t, report, "DR121", "api")
	if !strings.Contains(f.Message, "this cluster") {
		t.Errorf("finding = %+v", f)
	}
	if report.Standby != "" {
		t.Errorf("standby = %q", report.Standby)
	}
}

func TestAMissingNamespaceInTheStandbyIsOneFindingNotAFlood(t *testing.T) {
	primary := newEnv("primary", []runtime.Object{ns("shop"), referencingWorkload()})
	standby := newEnv("dr", []runtime.Object{ns("other")})
	report := run(t, Options{Primary: primary, Standby: standby, Namespaces: []string{"shop"}})
	f := wantRule(t, report, "DR125", "shop")
	if f.Severity != checks.SeverityHigh || !strings.Contains(f.Fix, "kubectl create namespace shop") {
		t.Errorf("finding = %+v", f)
	}
	noRule(t, report, "DR120")
	noRule(t, report, "DR121")
}

func TestAListThatFailsBecomesANoticeNotAFalseFinding(t *testing.T) {
	primary := newEnv("primary", []runtime.Object{ns("shop"), referencingWorkload()})
	standby := newEnv("dr", []runtime.Object{ns("shop")})
	standby.Client.(*fakeclient.Clientset).PrependReactor("list", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("forbidden")
	})
	report := run(t, Options{Primary: primary, Standby: standby, Namespaces: []string{"shop"}})
	noRule(t, report, "DR120")
	wantRule(t, report, "DR121", "api") // ConfigMaps could still be listed
	if !strings.Contains(strings.Join(report.Notices, "|"), "Secret names in shop") {
		t.Errorf("notices = %v", report.Notices)
	}
}

// ---- classes, volumes, ingresses ---------------------------------------------------------------------------------

func TestStorageClassesMustExistInTheTarget(t *testing.T) {
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "shop"},
		Spec: appsv1.StatefulSetSpec{VolumeClaimTemplates: []corev1.PersistentVolumeClaim{
			{ObjectMeta: metav1.ObjectMeta{Name: "data"}, Spec: corev1.PersistentVolumeClaimSpec{StorageClassName: strp("fast")}},
		}},
	}
	primary := newEnv("primary", []runtime.Object{ns("shop"), sts, pvc("shop", "cache", strp("gp3")), pvc("shop", "static", strp("")), pvc("shop", "dflt", nil)})
	standby := newEnv("dr", []runtime.Object{ns("shop"), storageClass("gp3", false)})
	report := run(t, Options{Primary: primary, Standby: standby, Namespaces: []string{"shop"}})

	if f := wantRule(t, report, "DR130", "db"); f.Severity != checks.SeverityHigh || f.Kind != "StatefulSet" || !strings.Contains(f.Message, `"fast"`) {
		t.Errorf("claim template finding = %+v", f)
	}
	if f := wantRule(t, report, "DR130", "dflt"); f.Severity != checks.SeverityMedium || !strings.Contains(f.Message, "default StorageClass") {
		t.Errorf("default class finding = %+v", f)
	}
	for _, f := range findingsOf(report, "DR130") {
		if f.Name == "cache" || f.Name == "static" {
			t.Errorf("%s must not be reported: %+v", f.Name, f)
		}
	}

	withDefault := newEnv("dr", []runtime.Object{ns("shop"), storageClass("gp3", false), storageClass("fast", true)})
	report = run(t, Options{Primary: primary, Standby: withDefault, Namespaces: []string{"shop"}})
	noRule(t, report, "DR130")
}

func TestIngressClassPriorityClassAndTLSSecrets(t *testing.T) {
	ing := ingress("shop", "web", strp("nginx"), "web-tls")
	legacy := ingress("shop", "old", nil, "old-tls")
	legacy.Annotations = map[string]string{"kubernetes.io/ingress.class": "traefik"}
	prio := deploy("shop", "api", func(s *corev1.PodSpec) { s.PriorityClassName = "important" })
	builtin := deploy("shop", "dns", func(s *corev1.PodSpec) { s.PriorityClassName = "system-cluster-critical" })
	primary := newEnv("primary", []runtime.Object{ns("shop"), ing, legacy, prio, builtin})
	standby := newEnv("dr", []runtime.Object{ns("shop"), ingressClass("traefik"), secret("shop", "old-tls", nil)})
	report := run(t, Options{Primary: primary, Standby: standby, Namespaces: []string{"shop"}})

	wantRule(t, report, "DR131", "web")
	wantRule(t, report, "DR112", "web")
	wantRule(t, report, "DR132", "api")
	for _, f := range report.Findings {
		if (f.Rule == "DR131" || f.Rule == "DR112") && f.Name == "old" {
			t.Errorf("the legacy ingress is fine in the DR cluster: %+v", f)
		}
		if f.Rule == "DR132" && f.Name == "dns" {
			t.Errorf("built-in priority classes always exist: %+v", f)
		}
	}
	if f := wantRule(t, report, "DR112", "web"); f.Severity != checks.SeverityHigh || f.Kind != "Ingress" {
		t.Errorf("finding = %+v", f)
	}
}

// ---- certificates ------------------------------------------------------------------------------------------------

func TestCertificates(t *testing.T) {
	primary := newEnv("primary", []runtime.Object{ns("shop")},
		certificate("shop", "broken", false, fixedNow.Add(90*24*time.Hour)),
		certificate("shop", "soon", true, fixedNow.Add(10*24*time.Hour)),
		certificate("shop", "very-soon", true, fixedNow.Add(3*24*time.Hour)),
		certificate("shop", "expired", true, fixedNow.Add(-24*time.Hour)),
		certificate("shop", "fine", true, fixedNow.Add(80*24*time.Hour)),
	)
	report := run(t, Options{Primary: primary, Namespaces: []string{"shop"}})

	if f := wantRule(t, report, "DR110", "broken"); f.Severity != checks.SeverityHigh || !strings.Contains(f.Message, "issuer not ready") {
		t.Errorf("not ready = %+v", f)
	}
	if f := wantRule(t, report, "DR111", "soon"); f.Severity != checks.SeverityMedium || !strings.Contains(f.Message, "10 day") {
		t.Errorf("soon = %+v", f)
	}
	if f := wantRule(t, report, "DR111", "very-soon"); f.Severity != checks.SeverityHigh {
		t.Errorf("under a week is high: %+v", f)
	}
	if f := wantRule(t, report, "DR111", "expired"); f.Severity != checks.SeverityHigh || !strings.Contains(f.Message, "expired") {
		t.Errorf("expired = %+v", f)
	}
	for _, f := range report.Findings {
		if f.Name == "fine" {
			t.Errorf("a healthy certificate must not be reported: %+v", f)
		}
	}

	// The window is configurable.
	report = run(t, Options{Primary: primary, Namespaces: []string{"shop"}, ExpiryDays: 5})
	for _, f := range findingsOf(report, "DR111") {
		if f.Name == "soon" {
			t.Errorf("10 days is outside a 5 day window: %+v", f)
		}
	}
}

func TestCertManagerMustBeInstalledInTheStandby(t *testing.T) {
	primary := newEnv("primary", []runtime.Object{ns("a"), ns("b")},
		certificate("a", "one", true, fixedNow.Add(80*24*time.Hour)), certificate("b", "two", true, fixedNow.Add(80*24*time.Hour)))
	standby := newEnv("dr", []runtime.Object{ns("a"), ns("b")})
	// A cluster without the CRD answers NotFound to the list.
	standby.Dynamic.(interface {
		PrependReactor(verb, resource string, reaction k8stesting.ReactionFunc)
	}).PrependReactor("list", "certificates", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("the server could not find the requested resource")
	})
	report := run(t, Options{Primary: primary, Standby: standby, Namespaces: []string{"a", "b"}})
	if got := len(findingsOf(report, "DR113")); got != 1 {
		t.Errorf("DR113 must be raised once per run, raised %d times: %s", got, dump(report))
	}

	// Installed there: nothing.
	ok := newEnv("dr", []runtime.Object{ns("a"), ns("b")})
	noRule(t, run(t, Options{Primary: primary, Standby: ok, Namespaces: []string{"a", "b"}}), "DR113")
	// No certificates in the primary: nothing to reissue.
	none := newEnv("primary", []runtime.Object{ns("a")})
	noRule(t, run(t, Options{Primary: none, Standby: standby, Namespaces: []string{"a"}}), "DR113")
}

// ---- the report itself -----------------------------------------------------------------------------------------

func TestRunNeedsNamespacesAndClients(t *testing.T) {
	primary := newEnv("primary", nil)
	if _, err := Run(context.Background(), Options{Primary: primary}); err == nil || !strings.Contains(err.Error(), "no namespace") {
		t.Errorf("no namespaces: %v", err)
	}
	if _, err := Run(context.Background(), Options{Namespaces: []string{"shop"}}); !errors.Is(err, dr.ErrUnreachable) {
		t.Errorf("no primary: %v", err)
	}
	standby := newEnv("dr", nil)
	standby.Client = nil
	if _, err := Run(context.Background(), Options{Primary: primary, Standby: standby, Namespaces: []string{"shop"}}); !errors.Is(err, dr.ErrUnreachable) {
		t.Errorf("standby without a client: %v", err)
	}
}

func TestNamespacesComeFromThePlanWhenNotGiven(t *testing.T) {
	plan := &dr.Plan{Apps: []dr.App{{Name: "a", Namespace: "shop"}, {Name: "b", Namespace: "shop"}}, Databases: []dr.Database{{Name: "d", Namespace: "data", Kind: "statefulset", Ref: "x"}}}
	got := namespacesOf(Options{Plan: plan})
	if strings.Join(got, ",") != "data,shop" {
		t.Errorf("namespaces = %v", got)
	}
	if got := namespacesOf(Options{Plan: plan, Namespaces: []string{"only", "only"}}); strings.Join(got, ",") != "only" {
		t.Errorf("explicit namespaces win and are de-duplicated: %v", got)
	}
}

func TestReportIsSortedFilteredAndCountedOverEverything(t *testing.T) {
	primary := newEnv("primary", []runtime.Object{
		ns("shop"), referencingWorkload(),
		deploy("shop", "hub", image("nginx")),
	})
	standby := newEnv("dr", []runtime.Object{ns("shop")})
	all := run(t, Options{Primary: primary, Standby: standby, Namespaces: []string{"shop"}})
	for i := 1; i < len(all.Findings); i++ {
		if rank(all.Findings[i-1].Severity) < rank(all.Findings[i].Severity) {
			t.Fatalf("findings must be sorted by severity: %s", dump(all))
		}
	}
	if all.Summary.High == 0 || all.Summary.Low == 0 || all.Summary.Total != len(all.Findings) {
		t.Errorf("summary = %+v with %d findings", all.Summary, len(all.Findings))
	}

	high := run(t, Options{Primary: primary, Standby: standby, Namespaces: []string{"shop"}, MinSeverity: checks.SeverityHigh})
	for _, f := range high.Findings {
		if f.Severity != checks.SeverityHigh {
			t.Errorf("only high findings should remain, got %+v", f)
		}
	}
	if high.Summary != all.Summary {
		t.Errorf("the summary counts everything regardless of the filter: %+v vs %+v", high.Summary, all.Summary)
	}

	again := run(t, Options{Primary: primary, Standby: standby, Namespaces: []string{"shop"}})
	if marshal(t, again.Findings) != marshal(t, all.Findings) {
		t.Error("two runs must give the same findings in the same order")
	}
	if all.Disclaimer == "" || all.Checked["workloads"] != 2 || all.Standby != "dr" || all.Context != "primary" {
		t.Errorf("report = %+v", all)
	}
}

func TestEveryRuleHasADescriptionAndAUniqueID(t *testing.T) {
	seen := map[string]bool{}
	for _, rule := range Rules() {
		if !strings.HasPrefix(rule.ID, "DR1") || rule.Title == "" || rule.Why == "" || seen[rule.ID] {
			t.Errorf("bad rule %+v", rule)
		}
		seen[rule.ID] = true
	}
	// The ids must not collide with the readiness rules.
	for _, rule := range checks.Rules() {
		if seen[rule.ID] {
			t.Errorf("%s collides with a readiness rule", rule.ID)
		}
	}
}

func TestEveryRaisedFindingUsesAKnownRule(t *testing.T) {
	known := map[string]bool{}
	for _, rule := range Rules() {
		known[rule.ID] = true
	}
	primary := newEnv("primary", []runtime.Object{ns("shop"), referencingWorkload(), deploy("shop", "hub", image("nginx")),
		pvc("shop", "p", strp("gone")), ingress("shop", "i", strp("none"), "tls")},
		certificate("shop", "c", false, fixedNow.Add(time.Hour)))
	standby := newEnv("dr", []runtime.Object{ns("shop")})
	report := run(t, Options{Primary: primary, Standby: standby, Namespaces: []string{"shop"}})
	if len(report.Findings) < 8 {
		t.Fatalf("expected a busy report, got %s", dump(report))
	}
	for _, f := range report.Findings {
		if !known[f.Rule] {
			t.Errorf("%s is not in Rules()", f.Rule)
		}
		if f.Message == "" || f.Severity == "" {
			t.Errorf("incomplete finding %+v", f)
		}
	}
}
