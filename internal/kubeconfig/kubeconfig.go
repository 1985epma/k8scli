// Package kubeconfig wraps client-go's kubeconfig handling so that every
// command resolves the file, the current context and the namespace the same way.
package kubeconfig

import (
	"fmt"
	"sort"
	"strings"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// DefaultNamespace is used when neither the flag nor the kubeconfig context set one.
const DefaultNamespace = "default"

// Loader resolves kubeconfig files following the kubectl rules: an explicit
// path wins, otherwise $KUBECONFIG (which may list several files) and finally
// ~/.kube/config.
type Loader struct {
	explicitPath string
}

// New returns a Loader. An empty path means "use the default loading rules".
func New(explicitPath string) Loader {
	return Loader{explicitPath: explicitPath}
}

func (l Loader) rules() *clientcmd.ClientConfigLoadingRules {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if l.explicitPath != "" {
		rules.ExplicitPath = l.explicitPath
	}
	return rules
}

func (l Loader) clientConfig() clientcmd.ClientConfig {
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(l.rules(), &clientcmd.ConfigOverrides{})
}

// Load reads the merged kubeconfig.
func (l Loader) Load() (*clientcmdapi.Config, error) {
	config, err := l.rules().Load()
	if err != nil {
		return nil, fmt.Errorf("failed to load kubeconfig: %w", err)
	}
	return config, nil
}

// RESTConfig builds a client configuration for the current context.
func (l Loader) RESTConfig() (*rest.Config, error) {
	config, err := l.clientConfig().ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to build config: %w", err)
	}
	return config, nil
}

// RESTConfigFor builds a client configuration for the named context without
// touching the kubeconfig file, so a long-running process can talk to a
// context other than the current one.
func (l Loader) RESTConfigFor(contextName string) (*rest.Config, error) {
	overrides := &clientcmd.ConfigOverrides{CurrentContext: contextName}
	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(l.rules(), overrides).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to build config for context %q: %w", contextName, err)
	}
	return config, nil
}

// Paths lists the kubeconfig files that are read, in precedence order. The
// files may not exist yet.
func (l Loader) Paths() []string {
	return l.rules().GetLoadingPrecedence()
}

// CurrentContext returns the name of the current context, or "" when unknown.
func (l Loader) CurrentContext() string {
	config, err := l.Load()
	if err != nil {
		return ""
	}
	return config.CurrentContext
}

// Namespace returns the namespace configured for the current context,
// falling back to DefaultNamespace.
func (l Loader) Namespace() string {
	namespace, _, err := l.clientConfig().Namespace()
	if err != nil || namespace == "" {
		return DefaultNamespace
	}
	return namespace
}

// ContextInfo describes one kubeconfig context.
type ContextInfo struct {
	Name      string `json:"name"`
	Current   bool   `json:"current"`
	Cluster   string `json:"cluster"`
	User      string `json:"user"`
	Namespace string `json:"namespace,omitempty"`
}

// Contexts lists the contexts sorted by name so the output is stable.
func (l Loader) Contexts() ([]ContextInfo, error) {
	config, err := l.Load()
	if err != nil {
		return nil, err
	}

	names := make([]string, 0, len(config.Contexts))
	for name := range config.Contexts {
		names = append(names, name)
	}
	sort.Strings(names)

	contexts := make([]ContextInfo, 0, len(names))
	for _, name := range names {
		ctx := config.Contexts[name]
		contexts = append(contexts, ContextInfo{
			Name:      name,
			Current:   name == config.CurrentContext,
			Cluster:   ctx.Cluster,
			User:      ctx.AuthInfo,
			Namespace: ctx.Namespace,
		})
	}
	return contexts, nil
}

// UseContext switches the current context. The write goes through
// clientcmd.ModifyConfig, which locks the file and only touches what changed.
func (l Loader) UseContext(name string) error {
	rules := l.rules()
	config, err := rules.Load()
	if err != nil {
		return fmt.Errorf("failed to load kubeconfig: %w", err)
	}

	if _, ok := config.Contexts[name]; !ok {
		return fmt.Errorf("context %q not found", name)
	}

	config.CurrentContext = name
	if err := clientcmd.ModifyConfig(rules, *config, true); err != nil {
		return fmt.Errorf("failed to save kubeconfig: %w", err)
	}
	return nil
}

// LooksLikeProduction reports whether a context name suggests production. It is a heuristic on the
// name only (anything containing "prod", case-insensitive), shared by the CLI confirmation, the web
// console gate and the DR module so they never disagree. A production context that is not named
// that way is not detected.
func LooksLikeProduction(contextName string) bool {
	return strings.Contains(strings.ToLower(contextName), "prod")
}
