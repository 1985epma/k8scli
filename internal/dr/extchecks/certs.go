package extchecks

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/1985epma/k8scli/internal/dr"
	"github.com/1985epma/k8scli/internal/dr/checks"
)

var certificates = schema.GroupVersionResource{Group: "cert-manager.io", Version: "v1", Resource: "certificates"}

// checkCertificates looks at the cert-manager Certificates of a namespace of the primary cluster: not
// ready, expiring soon, and (once per run) whether the DR cluster can reissue them at all.
func (r *runner) checkCertificates(ctx context.Context, set *nsData, opts Options, target *dr.Env) {
	if opts.Primary.Dynamic == nil {
		return
	}
	list, err := opts.Primary.Dynamic.Resource(certificates).Namespace(set.namespace).List(ctx, metav1.ListOptions{Limit: pageSize})
	if err != nil {
		if !isNotFound(err) {
			r.notice("could not list certificates in %s: %v", set.namespace, err)
		}
		return
	}
	r.count("certificates", len(list.Items))
	limit := time.Duration(opts.ExpiryDays) * 24 * time.Hour

	for i := range list.Items {
		cert := &list.Items[i]
		name := cert.GetName()
		if ready, known := certReady(cert); known && !ready {
			r.add(checks.Finding{Rule: "DR110", Severity: checks.SeverityHigh, Kind: "Certificate", Name: name, Namespace: set.namespace,
				Message: "the certificate is not Ready" + certReason(cert),
				Fix:     fmt.Sprintf("kubectl describe certificate %s -n %s", name, set.namespace)})
		}
		notAfter, found, _ := unstructured.NestedString(cert.Object, "status", "notAfter")
		if !found {
			continue
		}
		expires, err := time.Parse(time.RFC3339, notAfter)
		if err != nil {
			continue
		}
		left := expires.Sub(r.now)
		switch {
		case left <= 0:
			r.add(checks.Finding{Rule: "DR111", Severity: checks.SeverityHigh, Kind: "Certificate", Name: name, Namespace: set.namespace,
				Message: "the certificate has expired on " + expires.Format("2006-01-02"),
				Fix:     "check why cert-manager is not renewing it"})
		case left <= limit:
			severity := checks.SeverityMedium
			if left < 7*24*time.Hour {
				severity = checks.SeverityHigh
			}
			r.add(checks.Finding{Rule: "DR111", Severity: severity, Kind: "Certificate", Name: name, Namespace: set.namespace,
				Message: fmt.Sprintf("the certificate expires in %d day(s), on %s", int(left.Hours()/24), expires.Format("2006-01-02")),
				Fix:     "check that cert-manager renews it"})
		}
	}

	if len(list.Items) > 0 && opts.Standby != nil && !r.checkedCertManager {
		r.checkedCertManager = true
		r.checkCertManagerInStandby(ctx, target)
	}
}

func (r *runner) checkCertManagerInStandby(ctx context.Context, standby *dr.Env) {
	if standby.Dynamic == nil {
		return
	}
	_, err := standby.Dynamic.Resource(certificates).Namespace(metav1.NamespaceAll).List(ctx, metav1.ListOptions{Limit: 1})
	switch {
	case err == nil:
	case isNotFound(err) || apierrors.IsNotFound(err):
		r.add(checks.Finding{Rule: "DR113", Severity: checks.SeverityMedium, Kind: "Cluster", Name: standby.Context,
			Message: "cert-manager is not installed in the DR cluster, so the certificates cannot be reissued there",
			Fix:     "install cert-manager there first (see Apps (Helm))"})
	default:
		r.notice("could not check cert-manager in the DR cluster: %v", err)
	}
}

func certReady(cert *unstructured.Unstructured) (ready, known bool) {
	conditions, found, _ := unstructured.NestedSlice(cert.Object, "status", "conditions")
	if !found {
		return false, false
	}
	for _, c := range conditions {
		m, ok := c.(map[string]any)
		if !ok || m["type"] != "Ready" {
			continue
		}
		return m["status"] == "True", true
	}
	return false, false
}

func certReason(cert *unstructured.Unstructured) string {
	conditions, _, _ := unstructured.NestedSlice(cert.Object, "status", "conditions")
	for _, c := range conditions {
		if m, ok := c.(map[string]any); ok && m["type"] == "Ready" {
			if msg, _ := m["message"].(string); msg != "" {
				return ": " + msg
			}
		}
	}
	return ""
}
