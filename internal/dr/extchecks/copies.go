package extchecks

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/1985epma/k8scli/internal/dr/backups"
	"github.com/1985epma/k8scli/internal/dr/checks"
)

var backupLocations = schema.GroupVersionResource{Group: "velero.io", Version: "v1", Resource: "backupstoragelocations"}

// Copy is one copy of the data.
type Copy struct {
	// Kind is "live" (the volumes in use), "object-storage" (a Velero backup location) or
	// "volume-snapshot" (CSI snapshots, which live on the same storage system as the volumes).
	Kind      string `json:"kind"`
	Name      string `json:"name,omitempty"`
	Provider  string `json:"provider,omitempty"`
	Bucket    string `json:"bucket,omitempty"`
	Prefix    string `json:"prefix,omitempty"`
	Region    string `json:"region,omitempty"`
	Endpoint  string `json:"endpoint,omitempty"`
	Available bool   `json:"available"`
	// InCluster is true for a location served from inside the cluster it protects.
	InCluster bool `json:"inCluster,omitempty"`
	// OffSite is nil when it cannot be told from the cluster.
	OffSite *bool  `json:"offSite,omitempty"`
	Note    string `json:"note,omitempty"`
}

// CopiesReport is the 3-2-1 assessment.
type CopiesReport struct {
	Copies []Copy `json:"copies"`
	// Total counts the live volumes plus every independent copy that is available.
	Total int `json:"total"`
	// Media lists the independent kinds of storage found ("volumes", "object-storage").
	Media         []string `json:"media"`
	ObjectTargets int      `json:"objectTargets"`
	Snapshots     int      `json:"snapshots"`
	// OffSite is "yes", "no" or "unknown".
	OffSite         string `json:"offSite"`
	ClusterRegion   string `json:"clusterRegion,omitempty"`
	VeleroInstalled bool   `json:"veleroInstalled"`
	Schedules       int    `json:"schedules"`
	// Met is true when there are at least three copies, on two kinds of storage, with one off-site.
	Met bool `json:"met"`
	// Immutability is always a note: object lock is a bucket setting the Kubernetes API does not show.
	Immutability string `json:"immutability"`
}

func (r *runner) checkCopies(ctx context.Context, namespaces []string) *CopiesReport {
	env := r.opts.Primary
	report := &CopiesReport{OffSite: "unknown", Immutability: "cannot be verified from the Kubernetes API"}
	report.Copies = append(report.Copies, Copy{Kind: "live", Name: "volumes in use", Available: true})

	listing, err := backups.List(ctx, env, backups.ListOptions{})
	if err != nil {
		r.notice("could not read the backups: %v", err)
		listing = &backups.Listing{}
	}
	r.notices = append(r.notices, listing.Notices...)
	report.VeleroInstalled = listing.VeleroInstalled

	covers := func(item backups.Item) bool {
		if len(item.Namespaces) == 0 {
			return true
		}
		for _, have := range item.Namespaces {
			if have == "*" {
				return true
			}
			for _, want := range namespaces {
				if have == want {
					return true
				}
			}
		}
		return false
	}
	for _, item := range listing.Items {
		switch item.Source {
		case backups.SourceVeleroSchedule:
			if covers(item) {
				report.Schedules++
			}
		case backups.SourceVolumeSnapshot:
			if item.OK {
				for _, ns := range namespaces {
					if item.Namespace == ns {
						report.Snapshots++
						break
					}
				}
			}
		}
	}

	report.ClusterRegion = r.clusterRegion(ctx)
	locations := r.readLocations(ctx)
	seen := map[string]bool{}
	for _, loc := range locations {
		key := loc.Provider + "|" + loc.Bucket + "|" + loc.Prefix + "|" + loc.Endpoint
		if seen[key] {
			continue
		}
		seen[key] = true
		loc.OffSite = offSite(loc, report.ClusterRegion)
		report.Copies = append(report.Copies, loc)
	}
	if report.Snapshots > 0 {
		report.Copies = append(report.Copies, Copy{Kind: "volume-snapshot", Name: fmt.Sprintf("%d snapshot(s)", report.Snapshots), Available: true,
			Note: "on the same storage system as the volumes, so not an independent medium"})
	}

	independent := 0
	var offTrue, offFalse, offUnknown int
	for i := range report.Copies {
		c := &report.Copies[i]
		if c.Kind != "object-storage" {
			continue
		}
		report.ObjectTargets++
		if !c.Available || c.InCluster {
			continue
		}
		independent++
		switch {
		case c.OffSite == nil:
			offUnknown++
		case *c.OffSite:
			offTrue++
		default:
			offFalse++
		}
	}
	report.Total = 1 + independent
	if report.Snapshots > 0 {
		report.Total++
	}
	report.Media = []string{"volumes"}
	if independent > 0 {
		report.Media = append(report.Media, "object-storage")
	}
	switch {
	case offTrue > 0:
		report.OffSite = "yes"
	case offUnknown > 0:
		report.OffSite = "unknown"
	case offFalse > 0 || (report.ObjectTargets > 0 && independent == 0):
		report.OffSite = "no"
	}
	report.Met = report.Total >= 3 && len(report.Media) >= 2 && report.OffSite == "yes"

	r.copyFindings(report)
	r.count("backupLocations", report.ObjectTargets)
	return report
}

func (r *runner) copyFindings(report *CopiesReport) {
	cluster := func(msg, fix string, rule string, sev checks.Severity) {
		r.add(checks.Finding{Rule: rule, Severity: sev, Kind: "Backup", Name: "3-2-1", Message: msg, Fix: fix})
	}
	for _, c := range report.Copies {
		if c.Kind != "object-storage" {
			continue
		}
		if c.InCluster {
			cluster(fmt.Sprintf("backup location %s is served from inside the cluster (%s): losing the cluster loses these backups", c.Name, c.Endpoint),
				"point it at storage outside the cluster, in another place", "DR147", checks.SeverityHigh)
		} else if !c.Available {
			cluster(fmt.Sprintf("backup location %s is not available", c.Name),
				"kubectl -n velero describe backupstoragelocation "+c.Name, "DR145", checks.SeverityMedium)
		}
	}

	if report.ObjectTargets == 0 && report.Snapshots == 0 {
		cluster("no backup location and no volume snapshot was found: the only copy of the data is the live volumes",
			"install Velero from Apps (Helm) and add a backup location in another place", "DR140", checks.SeverityHigh)
		return
	}
	if report.Total < 3 {
		cluster(fmt.Sprintf("%d copy(ies) of the data (the live volumes count as one); the 3-2-1 rule asks for three", report.Total),
			"add a second independent backup location", "DR141", checks.SeverityMedium)
	}
	if len(report.Media) < 2 {
		cluster("the copies are on one kind of storage only (snapshots on the same storage system as the volumes are not independent)",
			"keep one copy in object storage outside the cluster", "DR142", checks.SeverityMedium)
	}
	switch report.OffSite {
	case "no":
		msg := "no usable backup location is off-site"
		if report.ClusterRegion != "" {
			msg += ": every available one is in the cluster's region (" + report.ClusterRegion + ") or inside the cluster"
		}
		cluster(msg, "add a location in another region or provider", "DR143", checks.SeverityMedium)
	case "unknown":
		if report.ObjectTargets > 0 {
			cluster("it cannot be verified from the cluster that any copy is off-site (the region of the backup locations or of the cluster is not known)",
				"check the bucket's region by hand", "DR143", checks.SeverityLow)
		}
	}
	if report.ObjectTargets > 0 {
		cluster("immutability (object lock or versioning) of the backup buckets cannot be verified from the Kubernetes API",
			"check object lock and versioning in the storage provider, against ransomware", "DR144", checks.SeverityLow)
	}
	if report.VeleroInstalled && report.Schedules == 0 {
		cluster("no Velero Schedule covers these namespaces, so backups only happen when somebody starts one",
			"velero schedule create daily --schedule=\"0 3 * * *\"", "DR146", checks.SeverityMedium)
	}
}

// readLocations reads the Velero BackupStorageLocations.
func (r *runner) readLocations(ctx context.Context) []Copy {
	env := r.opts.Primary
	if env.Dynamic == nil {
		return nil
	}
	list, err := env.Dynamic.Resource(backupLocations).Namespace(metav1.NamespaceAll).List(ctx, metav1.ListOptions{Limit: pageSize})
	if err != nil {
		if !isNotFound(err) {
			r.notice("could not read the Velero backup locations: %v", err)
		}
		return nil
	}
	var out []Copy
	for i := range list.Items {
		u := &list.Items[i]
		provider, _, _ := unstructured.NestedString(u.Object, "spec", "provider")
		bucket, _, _ := unstructured.NestedString(u.Object, "spec", "objectStorage", "bucket")
		prefix, _, _ := unstructured.NestedString(u.Object, "spec", "objectStorage", "prefix")
		config, _, _ := unstructured.NestedStringMap(u.Object, "spec", "config")
		phase, _, _ := unstructured.NestedString(u.Object, "status", "phase")
		c := Copy{Kind: "object-storage", Name: u.GetName(), Provider: provider, Bucket: bucket, Prefix: prefix,
			Region: config["region"], Endpoint: config["s3Url"], Available: phase == "Available"}
		c.InCluster = inCluster(c.Endpoint)
		if c.InCluster {
			c.Note = "served from inside the cluster"
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// inCluster tells an S3 endpoint served by the cluster itself (MinIO as a Service) from an external one.
func inCluster(endpoint string) bool {
	if endpoint == "" {
		return false
	}
	raw := endpoint
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return false
	}
	if host == "localhost" || strings.HasSuffix(host, ".svc") || strings.HasSuffix(host, ".svc.cluster.local") || strings.HasSuffix(host, ".cluster.local") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	// A bare Service name such as "minio:9000".
	return !strings.Contains(host, ".")
}

// clusterRegion reads the region label of the first node that has one.
func (r *runner) clusterRegion(ctx context.Context) string {
	nodes, err := r.opts.Primary.Client.CoreV1().Nodes().List(ctx, metav1.ListOptions{Limit: 100})
	if err != nil {
		return ""
	}
	for i := range nodes.Items {
		if region := nodes.Items[i].Labels["topology.kubernetes.io/region"]; region != "" {
			return region
		}
	}
	return ""
}

// offSite compares the region of a location with the cluster's. It is nil when either is not known.
func offSite(c Copy, clusterRegion string) *bool {
	if c.InCluster {
		no := false
		return &no
	}
	if c.Region == "" || clusterRegion == "" {
		return nil
	}
	different := !strings.EqualFold(c.Region, clusterRegion)
	return &different
}
