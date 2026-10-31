package extchecks

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/rest"

	"github.com/1985epma/k8scli/internal/dr"
)

// nameSets are the names of the Secrets and ConfigMaps that exist in one namespace of the target cluster.
// A nil map means "could not be listed": the check that needs it is skipped instead of reporting
// everything as missing.
type nameSets struct {
	secrets    map[string]bool
	configMaps map[string]bool
}

func (r *runner) readNames(ctx context.Context, env *dr.Env, namespace string) *nameSets {
	sets := &nameSets{}
	var err error
	if sets.secrets, err = listNames(ctx, env, "secrets", namespace); err != nil {
		r.notice("could not list the Secret names in %s: %v", namespace, err)
		sets.secrets = nil
	}
	if sets.configMaps, err = listNames(ctx, env, "configmaps", namespace); err != nil {
		r.notice("could not list the ConfigMap names in %s: %v", namespace, err)
		sets.configMaps = nil
	}
	return sets
}

// listNames returns the names of the Secrets or ConfigMaps of a namespace and NOTHING else. When the
// client is a real REST client it asks the API server for metadata only (PartialObjectMetadataList), so
// the values never leave the cluster. Other clients (the fake used in tests) fall back to a typed list
// whose objects are dropped as soon as their names are copied.
func listNames(ctx context.Context, env *dr.Env, resource, namespace string) (map[string]bool, error) {
	// The namespace becomes part of a request path: only a valid namespace name may get that far.
	if problems := validation.IsDNS1123Label(namespace); len(problems) > 0 {
		return nil, fmt.Errorf("invalid namespace name %q", namespace)
	}
	if rc, ok := env.Client.CoreV1().RESTClient().(*rest.RESTClient); ok && rc != nil {
		return listNamesMetadata(ctx, rc, resource, namespace)
	}
	return listNamesTyped(ctx, env, resource, namespace)
}

// listNamesTyped copies names out of a typed list and lets every object go; no value is kept.
func listNamesTyped(ctx context.Context, env *dr.Env, resource, namespace string) (map[string]bool, error) {
	names := map[string]bool{}
	options := metav1.ListOptions{Limit: pageSize}
	for {
		var next string
		switch resource {
		case "secrets":
			list, err := env.Client.CoreV1().Secrets(namespace).List(ctx, options)
			if err != nil {
				return nil, err
			}
			for i := range list.Items {
				names[list.Items[i].Name] = true
			}
			next = list.Continue
		case "configmaps":
			list, err := env.Client.CoreV1().ConfigMaps(namespace).List(ctx, options)
			if err != nil {
				return nil, err
			}
			for i := range list.Items {
				names[list.Items[i].Name] = true
			}
			next = list.Continue
		default:
			return nil, fmt.Errorf("unsupported resource %q", resource)
		}
		if len(names) > maxNames {
			return nil, fmt.Errorf("more than %d %s in %s", maxNames, resource, namespace)
		}
		if next == "" {
			return names, nil
		}
		options.Continue = next
	}
}

// metadataAccept asks for the metadata-only representation, falling back to a plain list for servers
// that do not know it; the decoding below only keeps metadata.name either way.
const metadataAccept = "application/json;as=PartialObjectMetadataList;g=meta.k8s.io;v=v1, application/json"

type metadataPage struct {
	Items []struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
	} `json:"items"`
	Metadata struct {
		Continue string `json:"continue"`
	} `json:"metadata"`
}

func listNamesMetadata(ctx context.Context, rc *rest.RESTClient, resource, namespace string) (map[string]bool, error) {
	names := map[string]bool{}
	next := ""
	for {
		request := rc.Get().
			Namespace(namespace).
			Resource(resource).
			SetHeader("Accept", metadataAccept).
			Param("limit", strconv.Itoa(pageSize))
		if next != "" {
			request = request.Param("continue", next)
		}
		raw, err := request.DoRaw(ctx)
		if err != nil {
			return nil, err
		}
		var page metadataPage
		if err := json.Unmarshal(raw, &page); err != nil {
			return nil, fmt.Errorf("unexpected answer listing %s: %w", resource, err)
		}
		for _, item := range page.Items {
			if item.Metadata.Name != "" {
				names[item.Metadata.Name] = true
			}
		}
		if len(names) > maxNames {
			return nil, fmt.Errorf("more than %d %s in %s", maxNames, resource, namespace)
		}
		if page.Metadata.Continue == "" {
			return names, nil
		}
		next = page.Metadata.Continue
	}
}

// namespaceExists reports whether a namespace exists in a cluster.
func namespaceExists(ctx context.Context, env *dr.Env, namespace string) (bool, error) {
	_, err := env.Client.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	return err == nil, err
}
