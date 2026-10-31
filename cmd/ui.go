package cmd

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/1985epma/k8scli/internal/web"
)

// launcherName is the extra command that opens the web console. It is the same
// binary under another name (a symlink), so it needs no second build.
const launcherName = "k8scli-ui"

// openBrowser is replaced in tests.
var openBrowser = defaultOpenBrowser

// LauncherArgs rewrites the process arguments when the binary was started as
// k8scli-ui, so `k8scli-ui --read-only` behaves like `k8scli ui --read-only`.
func LauncherArgs(argv []string) []string {
	if len(argv) == 0 {
		return argv
	}
	name := argv[0]
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	name = strings.TrimSuffix(strings.ToLower(name), ".exe")
	if name != launcherName {
		return argv
	}
	return append([]string{argv[0], "ui"}, argv[1:]...)
}

func newUICmd(app *App) *cobra.Command {
	options := web.Options{Addr: web.DefaultAddr}
	var noOpen bool

	cmd := &cobra.Command{
		Use:   "ui",
		Short: "Open the web console in your browser",
		Long: "Start the web console and open it in your default browser. It is the same console as `k8scli serve`, " +
			"but when port 8484 is taken it picks a free one. It is also installed as the k8scli-ui command.",
		Example: "  k8scli ui\n  k8scli ui --read-only\n  k8scli-ui",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			addr, err := resolveListenAddr(options.Addr, cmd.Flags().Changed("addr"))
			if err != nil {
				return err
			}
			options.Addr = addr
			options.Kubeconfig = app.Kubeconfig
			options.RequestTimeout = app.RequestTimeout
			options.Version = shortVersion()

			server, err := web.New(options)
			if err != nil {
				return err
			}
			if !noOpen {
				go app.openWhenReady(cmd.Context(), server, addr)
			}
			return server.Run(cmd.Context(), app.Out)
		},
	}

	cmd.Flags().StringVar(&options.Addr, "addr", options.Addr, "address to listen on (host:port); a free port is used if the default is taken")
	cmd.Flags().BoolVar(&options.ReadOnly, "read-only", false, "disable scaling and every other change")
	cmd.Flags().BoolVar(&options.CatalogOnly, "catalog-only", false, "install Helm apps only from the reviewed catalog: no Artifact Hub search")
	cmd.Flags().BoolVar(&options.AllowRemote, "allow-remote", false, "allow listening beyond loopback (anyone with the link can use your credentials)")
	addDRWebFlags(cmd, &options)
	cmd.Flags().BoolVar(&noOpen, "no-open", false, "print the link without opening the browser")
	return cmd
}

// resolveListenAddr keeps an address the user asked for, and otherwise falls
// back to a free port when the default one is busy.
func resolveListenAddr(addr string, explicit bool) (string, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("invalid listen address %q: %w", addr, err)
	}

	if port != "0" {
		probe, err := net.Listen("tcp", addr)
		if err == nil {
			_ = probe.Close()
			return addr, nil
		}
		if explicit {
			return "", fmt.Errorf("cannot listen on %s: %w", addr, err)
		}
	}

	free, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		return "", fmt.Errorf("cannot find a free port on %s: %w", host, err)
	}
	defer func() { _ = free.Close() }()
	return free.Addr().String(), nil
}

// openWhenReady waits for the server to accept connections, then opens the link.
func (a *App) openWhenReady(ctx context.Context, server *web.Server, addr string) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && ctx.Err() == nil {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	ip := net.ParseIP(host)
	if ip == nil || ip.IsUnspecified() {
		ip = net.IPv4(127, 0, 0, 1)
	}
	var portNumber int
	if _, err := fmt.Sscanf(port, "%d", &portNumber); err != nil {
		return
	}
	link := server.URL(&net.TCPAddr{IP: ip, Port: portNumber})

	if err := openBrowser(link); err != nil {
		fmt.Fprintf(a.ErrOut, "Could not open a browser (%v). Open the link above yourself.\n", err)
	}
}

func defaultOpenBrowser(link string) error {
	var command *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		command = exec.Command("open", link) // #nosec G204 -- the link is built here from the loopback address and a random token
	case "windows":
		command = exec.Command("rundll32", "url.dll,FileProtocolHandler", link) // #nosec G204 -- same link as above
	default:
		command = exec.Command("xdg-open", link) // #nosec G204 -- same link as above
	}
	if err := command.Start(); err != nil {
		return err
	}
	// Reap the helper so it does not linger as a zombie; its result is irrelevant.
	go func() { _ = command.Wait() }()
	return nil
}
