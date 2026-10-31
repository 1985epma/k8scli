// Package web serves the k8scli web console: a small JSON API on top of the same kubeconfig
// handling the CLI uses, plus the embedded single-page UI.
//
// The console reads the cluster (dashboard, pods, logs, deployments, namespaces, ConfigMaps,
// autoscalers, Helm releases) and makes a short list of guided changes (ADR 0003 and 0004). It can
// change a cluster, so it is locked down by default: it only listens on loopback, every request
// needs the session token printed at start up, state-changing requests carry a custom header that
// browsers will not send cross-origin, and every change has a server-side preview, a production
// confirmation and a read-only mode.
package web

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"k8s.io/client-go/kubernetes"

	"github.com/1985epma/k8scli/internal/kubeconfig"
)

//go:embed static
var staticFiles embed.FS

// DefaultAddr is where the console listens unless told otherwise.
const DefaultAddr = "127.0.0.1:8484"

const (
	clientTTL      = 2 * time.Minute
	defaultTimeout = 30 * time.Second
)

// Options configures a Server.
type Options struct {
	// Kubeconfig is an explicit kubeconfig path; empty means the kubectl rules.
	Kubeconfig string
	// Addr is the listen address, host:port.
	Addr string
	// ReadOnly rejects every request that would change the cluster or the kubeconfig.
	ReadOnly bool
	// CatalogOnly keeps Helm installs to the reviewed catalog: no Artifact Hub search, no install from search.
	CatalogOnly bool
	// AllowRemote permits a non-loopback listen address.
	AllowRemote bool
	// RequestTimeout bounds the calls to the cluster (log streams are exempt).
	RequestTimeout time.Duration
	// Version is shown in the UI.
	Version string

	// DRDir holds disaster-recovery data: plans (plans/), ledgers, audit log and reports. Empty means
	// ~/.k8scli/dr (ADR 0005).
	DRDir string
	// AllowDRRuns enables real DR runs, drills and resume. Checks, exports, dry runs and reports are always on.
	AllowDRRuns bool
	// AllowDRPower lets a run reach the power phase (machines or cluster on/off) for plans that are not
	// marked safety.labOnly.
	AllowDRPower bool
}

// Server is the web console.
type Server struct {
	opts   Options
	loader kubeconfig.Loader
	token  string

	// newClient builds a clientset for a context. timeout 0 means no client
	// side timeout, which log streaming needs. Tests replace it.
	newClient func(contextName string, timeout time.Duration) (kubernetes.Interface, error)
	now       func() time.Time

	// helm installs the apps of the catalog (ADR 0004).
	helm *helmEngine

	// dr serves the disaster-recovery endpoints (ADR 0005).
	dr *drService

	mu           sync.Mutex
	clients      map[string]cachedClient
	allowedHosts map[string]bool
}

type cachedClient struct {
	client  kubernetes.Interface
	expires time.Time
}

// New validates the options and returns a Server with a fresh session token.
func New(opts Options) (*Server, error) {
	if opts.Addr == "" {
		opts.Addr = DefaultAddr
	}
	if opts.RequestTimeout <= 0 {
		opts.RequestTimeout = defaultTimeout
	}

	host, _, err := net.SplitHostPort(opts.Addr)
	if err != nil {
		return nil, fmt.Errorf("invalid listen address %q: %w", opts.Addr, err)
	}
	if !isLoopback(host) && !opts.AllowRemote {
		return nil, fmt.Errorf("refusing to listen on %q: the console can change your cluster, so it only listens on loopback (use --allow-remote to override)", opts.Addr)
	}

	token, err := newToken()
	if err != nil {
		return nil, err
	}

	s := &Server{
		opts:    opts,
		loader:  kubeconfig.New(opts.Kubeconfig),
		token:   token,
		now:     time.Now,
		clients: map[string]cachedClient{},
	}
	s.newClient = s.buildClient
	s.helm = newHelmEngine(s)
	s.dr = newDRService(s)
	s.setAllowedHosts(opts.Addr)
	return s, nil
}

// Token is the secret that opens a browser session.
func (s *Server) Token() string { return s.token }

// URL returns the address to open, including the token.
func (s *Server) URL(addr net.Addr) string {
	u := url.URL{Scheme: "http", Host: displayHost(addr), RawQuery: url.Values{"token": {s.token}}.Encode(), Path: "/"}
	return u.String()
}

// Run listens on the configured address and serves until ctx is cancelled.
func (s *Server) Run(ctx context.Context, out io.Writer) error {
	listener, err := net.Listen("tcp", s.opts.Addr)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", s.opts.Addr, err)
	}
	defer func() { _ = listener.Close() }()
	s.setAllowedHosts(listener.Addr().String())
	// DR runs live as long as the server: stopping it aborts them and leaves their ledgers resumable.
	s.dr.setBase(ctx)

	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		// Requests inherit ctx so log streams end when the process is stopped.
		BaseContext: func(net.Listener) context.Context { return ctx },
	}

	mode := "read-write"
	if s.opts.ReadOnly {
		mode = "read-only"
	}
	fmt.Fprintf(out, "k8scli web console (%s)\n\n  Open: %s\n\nThe link carries the session token. Press Ctrl+C to stop.\n", mode, s.URL(listener.Addr()))
	if s.opts.AllowRemote && !isLoopback(hostOf(s.opts.Addr)) {
		fmt.Fprintln(out, "\nWarning: listening beyond loopback. Anyone who gets the link can use your kubeconfig credentials.")
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(listener) }()

	select {
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("failed to stop the web console: %w", err)
		}
		// Give aborted DR runs a moment to write their last ledger entry.
		s.dr.shutdown(5 * time.Second)
		return nil
	}
}

// Handler returns the full HTTP handler: API, static UI and the security wrapper.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/state", s.handleState)
	mux.HandleFunc("GET /api/overview", s.handleOverview)
	mux.HandleFunc("GET /api/pods", s.handlePods)
	mux.HandleFunc("GET /api/deployments", s.handleDeployments)
	mux.HandleFunc("GET /api/logs", s.handleLogs)
	mux.HandleFunc("GET /api/check", s.handleCheck)
	mux.HandleFunc("POST /api/scale", s.handleScale)
	mux.HandleFunc("POST /api/contexts/default", s.handleSetDefault)

	// Guided write actions and the read views behind them (ADR 0003).
	mux.HandleFunc("GET /api/dashboard", s.handleDashboard)
	mux.HandleFunc("GET /api/namespaces", s.handleNamespaces)
	mux.HandleFunc("POST /api/namespaces", s.handleNamespaceCreate)
	mux.HandleFunc("GET /api/configmaps", s.handleConfigMaps)
	mux.HandleFunc("GET /api/configmap", s.handleConfigMapDetail)
	mux.HandleFunc("POST /api/configmaps", s.handleConfigMapCreate)
	mux.HandleFunc("POST /api/configmaps/data", s.handleConfigMapData)
	mux.HandleFunc("GET /api/deployment", s.handleDeploymentDetail)
	mux.HandleFunc("POST /api/deployments/create", s.handleDeploymentCreate)
	mux.HandleFunc("POST /api/deployments/image", s.handleDeploymentImage)
	mux.HandleFunc("POST /api/deployments/restart", s.handleDeploymentRestart)
	mux.HandleFunc("POST /api/deployments/env", s.handleDeploymentEnv)
	mux.HandleFunc("GET /api/autoscalers", s.handleAutoscalers)
	mux.HandleFunc("POST /api/autoscalers", s.handleAutoscalerSave)
	mux.HandleFunc("POST /api/autoscalers/delete", s.handleAutoscalerDelete)

	// Disaster recovery (ADR 0005).
	s.registerDRRoutes(mux)

	// Apps installed with Helm from the curated catalog (ADR 0004).
	mux.HandleFunc("GET /api/helm/catalog", s.handleHelmCatalog)
	mux.HandleFunc("GET /api/helm/releases", s.handleHelmReleases)
	mux.HandleFunc("GET /api/helm/release", s.handleHelmRelease)
	mux.HandleFunc("POST /api/helm/install", s.handleHelmInstall)
	mux.HandleFunc("POST /api/helm/upgrade", s.handleHelmUpgrade)
	mux.HandleFunc("POST /api/helm/uninstall", s.handleHelmUninstall)
	mux.HandleFunc("GET /api/helm/search", s.handleHelmSearch)
	mux.HandleFunc("GET /api/helm/capabilities", s.handleHelmCapabilities)
	mux.HandleFunc("/api/", func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "unknown API endpoint")
	})

	static, err := fs.Sub(staticFiles, "static")
	if err != nil {
		// The directory is embedded at build time, so this cannot happen.
		panic(err)
	}
	mux.Handle("/", http.FileServerFS(static))

	return s.secure(mux)
}

func (s *Server) setAllowedHosts(addr string) {
	hosts := map[string]bool{}
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return
	}
	for _, h := range []string{"localhost", "127.0.0.1", "[::1]"} {
		hosts[h+":"+port] = true
	}
	if h := hostOf(addr); h != "" && h != "0.0.0.0" && h != "::" {
		hosts[net.JoinHostPort(h, port)] = true
	}

	s.mu.Lock()
	s.allowedHosts = hosts
	s.mu.Unlock()
}

// buildClient is the production newClient.
func (s *Server) buildClient(contextName string, timeout time.Duration) (kubernetes.Interface, error) {
	config, err := s.loader.RESTConfigFor(contextName)
	if err != nil {
		return nil, err
	}
	config.Timeout = timeout
	config.UserAgent = "k8scli-web/" + s.opts.Version
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create clientset: %w", err)
	}
	return client, nil
}

// clientFor returns a cached clientset so exec credential plugins (aws, gcloud)
// are not started on every request. Clients with a timeout are never cached.
func (s *Server) clientFor(contextName string, timeout time.Duration) (kubernetes.Interface, error) {
	if timeout > 0 {
		return s.newClient(contextName, timeout)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if cached, ok := s.clients[contextName]; ok && s.now().Before(cached.expires) {
		return cached.client, nil
	}
	client, err := s.newClient(contextName, 0)
	if err != nil {
		return nil, err
	}
	s.clients[contextName] = cachedClient{client: client, expires: s.now().Add(clientTTL)}
	return client, nil
}

// dropClients forgets every cached clientset.
func (s *Server) dropClients() {
	s.mu.Lock()
	defer s.mu.Unlock()
	clear(s.clients)
}

func newToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("failed to generate a session token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func hostOf(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return ""
	}
	return host
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// displayHost turns a listen address into something a browser can open.
func displayHost(addr net.Addr) string {
	tcp, ok := addr.(*net.TCPAddr)
	if !ok {
		return addr.String()
	}
	host := tcp.IP.String()
	if tcp.IP.IsUnspecified() {
		host = "localhost"
	}
	return net.JoinHostPort(host, fmt.Sprint(tcp.Port))
}
