package checks

import (
	"encoding/base64"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// An operator such as CloudNativePG owns Secrets (app credentials, the superuser, certificates). They are
// not exported because the operator creates them again, but the inventory used to skip them silently and
// say "0 listed" although they exist.

const operatorSecretValue = "OPERATOR-OWNED-PASSWORD-777"

func withOperatorSecrets() []runtime.Object {
	controller := true
	owner := []metav1.OwnerReference{{APIVersion: "postgresql.cnpg.io/v1", Kind: "Cluster", Name: "shop-db", Controller: &controller}}
	notController := []metav1.OwnerReference{{APIVersion: "v1", Kind: "ConfigMap", Name: "just-referenced"}}
	return append(exportFixture(),
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "shop-db-app", Namespace: "shop", OwnerReferences: owner}, Type: corev1.SecretTypeBasicAuth,
			Data: map[string][]byte{"username": []byte("app"), "password": []byte(operatorSecretValue), "uri": []byte("postgres://x")},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "shop-db-ca", Namespace: "shop", OwnerReferences: owner}, Type: corev1.SecretTypeOpaque,
			Data: map[string][]byte{"ca.crt": []byte("CA-CERT-DATA")},
		},
		// A non-controller owner reference does not make a Secret generated: it is a normal Secret.
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "referenced", Namespace: "shop", OwnerReferences: notController}, Type: corev1.SecretTypeOpaque,
			Data: map[string][]byte{"k": []byte("v")},
		},
	)
}

func TestInventoryListsControllerOwnedSecretsButNeverExportsThem(t *testing.T) {
	files, result := exported(t, withOperatorSecrets(), ExportOptions{})
	inventory := files["INVENTORY.md"]

	for _, want := range []string{
		"| shop-db-app | kubernetes.io/basic-auth | 3 | Cluster/shop-db |",
		"| shop-db-ca | Opaque | 1 | Cluster/shop-db |",
		"| referenced | Opaque | 1 | - |",
		"| db-credentials | Opaque | 2 | - |",
		"2 of these are owned by a controller",
	} {
		if !strings.Contains(inventory, want) {
			t.Errorf("inventory is missing %q:\n%s", want, inventory)
		}
	}

	byName := map[string]SecretInfo{}
	for _, s := range result.Secrets {
		byName[s.Name] = s
	}
	if len(result.Secrets) != 5 || byName["shop-db-app"].OwnedBy != "Cluster/shop-db" || byName["shop-db-app"].Keys != 3 || byName["referenced"].OwnedBy != "" {
		t.Errorf("secrets = %+v", result.Secrets)
	}
	if result.Skipped < 2 {
		t.Errorf("skipped = %d; the owned Secrets were not exported and must count as skipped", result.Skipped)
	}

	// Never a value, never a key name, never a file.
	for name, content := range files {
		if strings.HasPrefix(name, "secret/") {
			t.Errorf("unexpected secret file %s", name)
		}
		for _, forbidden := range []string{
			operatorSecretValue, base64.StdEncoding.EncodeToString([]byte(operatorSecretValue)), "CA-CERT-DATA", "postgres://x", "ca.crt",
		} {
			if strings.Contains(content, forbidden) {
				t.Errorf("%s contains %q", name, forbidden)
			}
		}
	}
}

func TestOwnedSecretsStayOutEvenWhenSecretsAreIncluded(t *testing.T) {
	files, result := exported(t, withOperatorSecrets(), ExportOptions{IncludeSecrets: true})

	for name := range files {
		if strings.Contains(name, "shop-db-app") || strings.Contains(name, "shop-db-ca") {
			t.Errorf("a controller-owned Secret was exported as %s", name)
		}
	}
	for name, content := range files {
		if strings.Contains(content, "OPERATOR-OWNED-PASSWORD-777") || strings.Contains(content, base64.StdEncoding.EncodeToString([]byte(operatorSecretValue))) {
			t.Errorf("%s contains an operator-owned value", name)
		}
	}
	// Only the namespace's own Secrets (db-credentials, tls-cert, referenced) are written.
	if result.Counts["Secret"] != 3 {
		t.Errorf("exported Secrets = %d; want 3", result.Counts["Secret"])
	}
	if !strings.Contains(files["INVENTORY.md"], "except the ones a controller owns") || !strings.Contains(files["INVENTORY.md"], "shop-db-app") {
		t.Errorf("the inventory must explain the exception and still list the owned Secrets:\n%s", files["INVENTORY.md"])
	}
}

func TestInventoryWithoutOwnedSecretsKeepsItsOldLook(t *testing.T) {
	files, _ := exported(t, exportFixture(), ExportOptions{})
	if strings.Contains(files["INVENTORY.md"], "owned by a controller") {
		t.Error("the note about owned Secrets must only appear when there are some")
	}
}
