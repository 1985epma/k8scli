package extchecks

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"

	"github.com/1985epma/k8scli/internal/dr/checks"
)

func copiesRun(t *testing.T, objects []runtime.Object, dynamicObjects ...runtime.Object) *Report {
	t.Helper()
	primary := newEnv("primary", append([]runtime.Object{ns("shop")}, objects...), dynamicObjects...)
	return run(t, Options{Primary: primary, Namespaces: []string{"shop"}})
}

func TestNoBackupToolingMeansOnlyTheLiveCopy(t *testing.T) {
	report := copiesRun(t, nil)
	if report.Copies.Total != 1 || report.Copies.Met || report.Copies.OffSite != "unknown" {
		t.Errorf("copies = %+v", report.Copies)
	}
	f := wantRule(t, report, "DR140", "")
	if f.Severity != checks.SeverityHigh || !strings.Contains(f.Fix, "Apps (Helm)") {
		t.Errorf("DR140 = %+v", f)
	}
	// With nothing at all, the other 3-2-1 findings would only repeat it.
	for _, rule := range []string{"DR141", "DR142", "DR143", "DR144", "DR146"} {
		noRule(t, report, rule)
	}
}

func TestOneOffSiteLocationIsTwoCopiesNotThree(t *testing.T) {
	report := copiesRun(t, []runtime.Object{nodeInRegion("us-east-1")},
		location("default", "aws", "dr-bucket", "eu-west-1", "", "Available"),
		veleroBackup("b1", fixedNow), veleroSchedule("daily"))

	c := report.Copies
	if c.Total != 2 || c.ObjectTargets != 1 || c.OffSite != "yes" || c.ClusterRegion != "us-east-1" || c.Met {
		t.Errorf("copies = %+v", c)
	}
	if strings.Join(c.Media, ",") != "volumes,object-storage" {
		t.Errorf("media = %v", c.Media)
	}
	wantRule(t, report, "DR141", "")
	wantRule(t, report, "DR144", "")
	for _, rule := range []string{"DR140", "DR142", "DR143", "DR145", "DR146", "DR147"} {
		noRule(t, report, rule)
	}
	if f := wantRule(t, report, "DR144", ""); f.Severity != checks.SeverityLow || !strings.Contains(f.Message, "cannot be verified") {
		t.Errorf("immutability must be reported as unverifiable: %+v", f)
	}
}

func TestTwoLocationsAndSnapshotsMeetTheRule(t *testing.T) {
	report := copiesRun(t, []runtime.Object{nodeInRegion("us-east-1")},
		location("a", "aws", "bucket-a", "us-east-1", "", "Available"),
		location("b", "aws", "bucket-b", "eu-west-1", "", "Available"),
		volumeSnapshot("shop", "snap"), veleroBackup("b1", fixedNow), veleroSchedule("daily"))

	c := report.Copies
	if c.Total != 4 || c.Snapshots != 1 || !c.Met || c.OffSite != "yes" {
		t.Errorf("copies = %+v", c)
	}
	// Only the note about immutability is left.
	if len(report.Findings) != 1 || report.Findings[0].Rule != "DR144" {
		t.Errorf("findings = %s", dump(report))
	}
}

func TestEverythingInTheClusterRegionIsNotOffSite(t *testing.T) {
	report := copiesRun(t, []runtime.Object{nodeInRegion("us-east-1")},
		location("a", "aws", "bucket-a", "us-east-1", "", "Available"), veleroBackup("b1", fixedNow), veleroSchedule("daily"))
	if report.Copies.OffSite != "no" {
		t.Errorf("off-site = %q", report.Copies.OffSite)
	}
	f := wantRule(t, report, "DR143", "")
	if f.Severity != checks.SeverityMedium || !strings.Contains(f.Message, "us-east-1") {
		t.Errorf("DR143 = %+v", f)
	}
}

func TestAnUnknownRegionIsAQuestionNotAFailure(t *testing.T) {
	report := copiesRun(t, nil, location("a", "aws", "bucket-a", "", "", "Available"), veleroBackup("b1", fixedNow), veleroSchedule("daily"))
	if report.Copies.OffSite != "unknown" {
		t.Errorf("off-site = %q", report.Copies.OffSite)
	}
	if f := wantRule(t, report, "DR143", ""); f.Severity != checks.SeverityLow || !strings.Contains(f.Message, "cannot be verified") {
		t.Errorf("DR143 = %+v", f)
	}
}

func TestALocationInsideTheClusterIsNotACopy(t *testing.T) {
	report := copiesRun(t, []runtime.Object{nodeInRegion("us-east-1")},
		location("minio", "aws", "velero", "minio", "http://minio.velero.svc:9000", "Available"), veleroBackup("b1", fixedNow), veleroSchedule("daily"))
	c := report.Copies
	if c.Total != 1 || c.OffSite != "no" || len(c.Media) != 1 {
		t.Errorf("copies = %+v", c)
	}
	if f := wantRule(t, report, "DR147", "3-2-1"); f.Severity != checks.SeverityHigh || !strings.Contains(f.Message, "minio.velero.svc") {
		t.Errorf("DR147 = %+v", f)
	}
	wantRule(t, report, "DR141", "")
	wantRule(t, report, "DR142", "")
	wantRule(t, report, "DR143", "")
}

func TestAnUnavailableLocationIsReportedAndNotCounted(t *testing.T) {
	report := copiesRun(t, nil,
		location("down", "aws", "bucket-a", "eu-west-1", "", "Unavailable"),
		location("up", "aws", "bucket-b", "eu-west-1", "", "Available"),
		veleroBackup("b1", fixedNow), veleroSchedule("daily"))
	if report.Copies.Total != 2 || report.Copies.ObjectTargets != 2 {
		t.Errorf("copies = %+v", report.Copies)
	}
	if f := wantRule(t, report, "DR145", "3-2-1"); !strings.Contains(f.Message, "down") || f.Severity != checks.SeverityMedium {
		t.Errorf("DR145 = %+v", f)
	}
}

func TestSnapshotsAloneAreNotAnIndependentMedium(t *testing.T) {
	report := copiesRun(t, nil, volumeSnapshot("shop", "snap"))
	c := report.Copies
	if c.Total != 2 || c.ObjectTargets != 0 || len(c.Media) != 1 || c.Met {
		t.Errorf("copies = %+v", c)
	}
	wantRule(t, report, "DR141", "")
	wantRule(t, report, "DR142", "")
	noRule(t, report, "DR140")
	noRule(t, report, "DR144") // no bucket, nothing to lock
	// A snapshot of another namespace does not count for this one.
	other := copiesRun(t, nil, volumeSnapshot("elsewhere", "snap"))
	if other.Copies.Snapshots != 0 {
		t.Errorf("snapshots of another namespace counted: %+v", other.Copies)
	}
}

func TestVeleroWithoutAScheduleIsFlagged(t *testing.T) {
	report := copiesRun(t, nil, location("a", "aws", "bucket-a", "eu-west-1", "", "Available"), veleroBackup("b1", fixedNow))
	wantRule(t, report, "DR146", "3-2-1")
	if !report.Copies.VeleroInstalled || report.Copies.Schedules != 0 {
		t.Errorf("copies = %+v", report.Copies)
	}
}

func TestTheSameBucketTwiceIsOneCopy(t *testing.T) {
	report := copiesRun(t, nil,
		location("a", "aws", "same", "eu-west-1", "", "Available"),
		location("b", "aws", "same", "eu-west-1", "", "Available"),
		veleroBackup("b1", fixedNow), veleroSchedule("daily"))
	if report.Copies.ObjectTargets != 1 || report.Copies.Total != 2 {
		t.Errorf("two locations that point at the same bucket are one copy: %+v", report.Copies)
	}
}

func TestInCluster(t *testing.T) {
	for endpoint, want := range map[string]bool{
		"": false, "http://minio.velero.svc:9000": true, "https://minio.velero.svc.cluster.local": true, "minio:9000": true,
		"http://localhost:9000": true, "http://127.0.0.1:9000": true, "https://s3.eu-west-1.amazonaws.com": false,
		"https://storage.example.com:9000": false, "http://10.0.0.5:9000": false, "::::": false,
	} {
		if got := inCluster(endpoint); got != want {
			t.Errorf("inCluster(%q) = %v; want %v", endpoint, got, want)
		}
	}
}

func TestOffSite(t *testing.T) {
	yes, no := true, false
	for _, tc := range []struct {
		name    string
		copy    Copy
		cluster string
		want    *bool
	}{
		{"different region", Copy{Region: "eu-west-1"}, "us-east-1", &yes},
		{"same region", Copy{Region: "US-EAST-1"}, "us-east-1", &no},
		{"copy region unknown", Copy{}, "us-east-1", nil},
		{"cluster region unknown", Copy{Region: "eu-west-1"}, "", nil},
		{"inside the cluster", Copy{InCluster: true, Region: "eu-west-1"}, "us-east-1", &no},
	} {
		got := offSite(tc.copy, tc.cluster)
		switch {
		case tc.want == nil && got != nil, tc.want != nil && (got == nil || *got != *tc.want):
			t.Errorf("%s: offSite = %v; want %v", tc.name, got, tc.want)
		}
	}
}

func TestWithoutADynamicClientNothingBreaks(t *testing.T) {
	primary := newEnv("primary", nil)
	primary.Dynamic = nil
	report := run(t, Options{Primary: primary, Namespaces: []string{"shop"}})
	wantRule(t, report, "DR140", "")
}
