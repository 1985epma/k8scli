package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
)

// Bounds for ConfigMap edits. The API server refuses objects over 1 MiB; the
// console stays under it so a patch never fails halfway through.
const (
	cmMaxValueBytes = 256 << 10
	cmMaxTotalBytes = 1<<20 - 1
	cmMaxKeys       = 100
	cmMaxListed     = 5000
	cmPreviewRunes  = 60
	cmMaxCommandLen = 1000
	// cmMaxBodyBytes covers a full ConfigMap with JSON escaping (a byte can grow to 6).
	cmMaxBodyBytes = 4 << 20
)

type configMapRow struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Keys      int    `json:"keys"`
	Size      int    `json:"size"`
	Age       string `json:"age"`
	Managed   bool   `json:"managed"`
}

// configMapSize is the number of bytes held in keys and values.
func configMapSize(cm *corev1.ConfigMap) int {
	size := 0
	for key, value := range cm.Data {
		size += len(key) + len(value)
	}
	for key, value := range cm.BinaryData {
		size += len(key) + len(value)
	}
	return size
}

func (s *Server) handleConfigMaps(w http.ResponseWriter, r *http.Request) {
	namespace, err := namespaceParam(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	_, client, err := s.cluster(r.URL.Query().Get("context"), 0)
	if err != nil {
		writeErr(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.opts.RequestTimeout)
	defer cancel()

	rows := []configMapRow{}
	options := metav1.ListOptions{Limit: listPageSize}
	for {
		list, err := client.CoreV1().ConfigMaps(namespace).List(ctx, options)
		if err != nil {
			writeErr(w, err)
			return
		}
		for i := range list.Items {
			cm := &list.Items[i]
			rows = append(rows, configMapRow{
				Namespace: cm.Namespace,
				Name:      cm.Name,
				Keys:      len(cm.Data) + len(cm.BinaryData),
				Size:      configMapSize(cm),
				Age:       s.age(cm.CreationTimestamp),
				Managed:   cm.Labels[managedByLabel] == managedByValue,
			})
		}
		if list.Continue == "" || len(rows) >= cmMaxListed {
			break
		}
		options.Continue = list.Continue
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Namespace != rows[j].Namespace {
			return rows[i].Namespace < rows[j].Namespace
		}
		return rows[i].Name < rows[j].Name
	})
	writeJSON(w, http.StatusOK, rows)
}

type configMapDetail struct {
	Namespace  string            `json:"namespace"`
	Name       string            `json:"name"`
	Age        string            `json:"age"`
	Labels     map[string]string `json:"labels"`
	Data       map[string]string `json:"data"`
	BinaryKeys []string          `json:"binaryKeys"`
}

func (s *Server) handleConfigMapDetail(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	namespace, err := validNamespace(query.Get("namespace"))
	if err != nil {
		writeErr(w, err)
		return
	}
	name, err := validName("configmap", query.Get("name"))
	if err != nil {
		writeErr(w, err)
		return
	}
	_, client, err := s.cluster(query.Get("context"), 0)
	if err != nil {
		writeErr(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.opts.RequestTimeout)
	defer cancel()

	cm, err := client.CoreV1().ConfigMaps(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		writeErr(w, err)
		return
	}

	detail := configMapDetail{
		Namespace:  cm.Namespace,
		Name:       cm.Name,
		Age:        s.age(cm.CreationTimestamp),
		Labels:     map[string]string{},
		Data:       map[string]string{},
		BinaryKeys: []string{},
	}
	for key, value := range cm.Labels {
		detail.Labels[key] = value
	}
	for key, value := range cm.Data {
		detail.Data[key] = value
	}
	for key := range cm.BinaryData {
		detail.BinaryKeys = append(detail.BinaryKeys, key)
	}
	sort.Strings(detail.BinaryKeys)
	writeJSON(w, http.StatusOK, detail)
}

// ---- create ---------------------------------------------------------------

type configMapCreateRequest struct {
	mutation
	Namespace string            `json:"namespace"`
	Name      string            `json:"name"`
	Data      map[string]string `json:"data"`
}

// validateConfigMapData checks keys and sizes of the data of a new ConfigMap.
func validateConfigMapData(data map[string]string) error {
	if len(data) > cmMaxKeys {
		return badRequest("at most %d keys are allowed", cmMaxKeys)
	}
	total := 0
	for key, value := range data {
		if err := validateConfigMapEntry(key, value); err != nil {
			return err
		}
		total += len(key) + len(value)
	}
	if total > cmMaxTotalBytes {
		return badRequest("the data is too large: a ConfigMap holds less than 1 MiB")
	}
	return nil
}

func validateConfigMapEntry(key, value string) error {
	if problems := validation.IsConfigMapKey(key); len(problems) > 0 {
		return badRequest("invalid key %q: use letters, digits, '-', '_' or '.'", key)
	}
	if len(value) > cmMaxValueBytes {
		return badRequest("the value of %q is larger than %d KiB", key, cmMaxValueBytes>>10)
	}
	return nil
}

func (s *Server) handleConfigMapCreate(w http.ResponseWriter, r *http.Request) {
	var req configMapCreateRequest
	if err := decodeBodyLimit(w, r, &req, cmMaxBodyBytes); err != nil {
		writeErr(w, err)
		return
	}
	namespace, err := validNamespace(req.Namespace)
	if err != nil {
		writeErr(w, err)
		return
	}
	name, err := validName("configmap", req.Name)
	if err != nil {
		writeErr(w, err)
		return
	}
	if err := validateConfigMapData(req.Data); err != nil {
		writeErr(w, err)
		return
	}

	contextName, err := s.authorize(req.mutation)
	if err != nil {
		writeErr(w, err)
		return
	}
	_, client, err := s.cluster(contextName, 0)
	if err != nil {
		writeErr(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.opts.RequestTimeout)
	defer cancel()

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: managedLabels(name)},
		Data:       req.Data,
	}
	if _, err := client.CoreV1().ConfigMaps(namespace).Create(ctx, cm, metav1.CreateOptions{DryRun: dryRunOption(req.DryRun)}); err != nil {
		writeErr(w, clusterExistsError("ConfigMap", name, err))
		return
	}

	keys := sortedKeys(req.Data)
	changes := make([]string, 0, len(keys))
	command := fmt.Sprintf("kubectl create configmap %s -n %s", name, namespace)
	for _, key := range keys {
		changes = append(changes, fmt.Sprintf("%s: %s", key, cmPreview(req.Data[key])))
		command += fmt.Sprintf(" --from-literal=%s", cmShellQuote(key+"="+req.Data[key]))
	}
	if len(command) > cmMaxCommandLen {
		command = fmt.Sprintf("kubectl create configmap %s -n %s --from-file=<file>", name, namespace)
	}

	summary := fmt.Sprintf("ConfigMap %s created with %d keys", name, len(keys))
	if req.DryRun {
		summary = fmt.Sprintf("ConfigMap %s would be created with %d keys", name, len(keys))
	}
	writeJSON(w, http.StatusOK, changeResponse{
		Context: contextName, Kind: "ConfigMap", Namespace: namespace, Name: name, Action: "created", DryRun: req.DryRun,
		Summary: summary, Changes: changes, Equivalent: command,
	})
}

// ---- change one key -------------------------------------------------------

type configMapDataRequest struct {
	mutation
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Key       string `json:"key"`
	// Value is a JSON string to set the key, or null to remove it. A missing
	// value is refused, so a client that forgets it cannot delete a key by accident.
	Value json.RawMessage `json:"value"`
}

// parseConfigMapValue returns the new value, or remove=true for JSON null.
func parseConfigMapValue(raw json.RawMessage) (value string, remove bool, err error) {
	if len(raw) == 0 {
		return "", false, badRequest("value is required: send a string to set the key or null to remove it")
	}
	if string(raw) == "null" {
		return "", true, nil
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", false, badRequest("value must be a string or null")
	}
	return value, false, nil
}

func (s *Server) handleConfigMapData(w http.ResponseWriter, r *http.Request) {
	var req configMapDataRequest
	if err := decodeBodyLimit(w, r, &req, cmMaxBodyBytes); err != nil {
		writeErr(w, err)
		return
	}
	namespace, err := validNamespace(req.Namespace)
	if err != nil {
		writeErr(w, err)
		return
	}
	name, err := validName("configmap", req.Name)
	if err != nil {
		writeErr(w, err)
		return
	}
	value, remove, err := parseConfigMapValue(req.Value)
	if err != nil {
		writeErr(w, err)
		return
	}
	if !remove {
		if err := validateConfigMapEntry(req.Key, value); err != nil {
			writeErr(w, err)
			return
		}
	} else if problems := validation.IsConfigMapKey(req.Key); len(problems) > 0 {
		writeErr(w, badRequest("invalid key %q", req.Key))
		return
	}

	contextName, err := s.authorize(req.mutation)
	if err != nil {
		writeErr(w, err)
		return
	}
	_, client, err := s.cluster(contextName, 0)
	if err != nil {
		writeErr(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.opts.RequestTimeout)
	defer cancel()

	configMaps := client.CoreV1().ConfigMaps(namespace)
	current, err := configMaps.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		writeErr(w, err)
		return
	}

	response := changeResponse{Context: contextName, Kind: "ConfigMap", Namespace: namespace, Name: name, DryRun: req.DryRun, Changes: []string{}}
	old, exists := current.Data[req.Key]
	if _, binary := current.BinaryData[req.Key]; binary {
		writeErr(w, badRequest("key %q holds binary data, which the console does not edit", req.Key))
		return
	}

	switch {
	case remove && !exists:
		response.Action = "unchanged"
		response.Summary = fmt.Sprintf("ConfigMap %s has no key %q: nothing to remove", name, req.Key)
	case !remove && exists && old == value:
		response.Action = "unchanged"
		response.Summary = fmt.Sprintf("Key %q of ConfigMap %s already has that value", req.Key, name)
	default:
		newSize := configMapSize(current) - len(old) + len(value)
		if !exists {
			newSize += len(req.Key)
		}
		if remove {
			newSize = configMapSize(current) - len(req.Key) - len(old)
		}
		if newSize > cmMaxTotalBytes {
			writeErr(w, badRequest("the ConfigMap would be larger than 1 MiB"))
			return
		}
		if !remove && !exists && len(current.Data)+len(current.BinaryData) >= cmMaxKeys {
			writeErr(w, badRequest("at most %d keys are allowed", cmMaxKeys))
			return
		}

		patch, err := configMapPatch(req.Key, value, remove)
		if err != nil {
			writeErr(w, err)
			return
		}
		if _, err := configMaps.Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{DryRun: dryRunOption(req.DryRun)}); err != nil {
			writeErr(w, err)
			return
		}

		response.Action = "updated"
		switch {
		case remove:
			response.Changes = append(response.Changes, fmt.Sprintf("%s: %s -> (removed)", req.Key, cmPreview(old)))
			response.Summary = fmt.Sprintf("Key %q removed from ConfigMap %s", req.Key, name)
		case !exists:
			response.Changes = append(response.Changes, fmt.Sprintf("%s: (new) -> %s", req.Key, cmPreview(value)))
			response.Summary = fmt.Sprintf("Key %q added to ConfigMap %s", req.Key, name)
		default:
			response.Changes = append(response.Changes, fmt.Sprintf("%s: %s -> %s", req.Key, cmPreview(old), cmPreview(value)))
			response.Summary = fmt.Sprintf("Key %q of ConfigMap %s changed", req.Key, name)
		}
		if req.DryRun {
			response.Summary = strings.Replace(response.Summary, " removed", " would be removed", 1)
			response.Summary = strings.Replace(response.Summary, " added", " would be added", 1)
			response.Summary = strings.Replace(response.Summary, " changed", " would change", 1)
		}
		response.Equivalent = cmPatchCommand(name, namespace, patch)
	}
	if response.Equivalent == "" {
		response.Equivalent = fmt.Sprintf("kubectl get configmap %s -n %s -o yaml", name, namespace)
	}
	writeJSON(w, http.StatusOK, response)
}

// configMapPatch is a JSON merge patch that touches one key of data and
// nothing else: a null value deletes the key.
func configMapPatch(key, value string, remove bool) ([]byte, error) {
	var entry any = value
	if remove {
		entry = nil
	}
	patch, err := json.Marshal(map[string]any{"data": map[string]any{key: entry}})
	if err != nil {
		return nil, fmt.Errorf("failed to build the patch: %w", err)
	}
	return patch, nil
}

func cmPatchCommand(name, namespace string, patch []byte) string {
	command := fmt.Sprintf("kubectl patch configmap %s -n %s --type merge -p %s", name, namespace, cmShellQuote(string(patch)))
	if len(command) > cmMaxCommandLen {
		return fmt.Sprintf("kubectl edit configmap %s -n %s", name, namespace)
	}
	return command
}

// cmPreview shows a value in a change line: quoted (so newlines are escaped) and short.
func cmPreview(value string) string {
	runes := []rune(value)
	if len(runes) > cmPreviewRunes {
		value = string(runes[:cmPreviewRunes]) + "…"
	}
	return fmt.Sprintf("%q", value)
}

// cmShellQuote single-quotes text for a POSIX shell.
func cmShellQuote(text string) string {
	return "'" + strings.ReplaceAll(text, "'", `'\''`) + "'"
}

func sortedKeys(data map[string]string) []string {
	keys := make([]string, 0, len(data))
	for key := range data {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
