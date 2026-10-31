package measure

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestHTTPCheckerReturnsTheStatusWithoutJudgingIt(t *testing.T) {
	var agent atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		agent.Store(r.Header.Get("User-Agent"))
		switch r.URL.Path {
		case "/down":
			w.WriteHeader(http.StatusServiceUnavailable)
		default:
			_, _ = w.Write([]byte("ok"))
		}
	}))
	defer server.Close()

	checker := NewHTTPChecker()
	status, err := checker.Check(context.Background(), server.URL+"/healthz")
	if err != nil || status != http.StatusOK {
		t.Fatalf("status %d, err %v", status, err)
	}
	if agent.Load() != "k8scli-dr" {
		t.Errorf("User-Agent = %v", agent.Load())
	}
	// A 503 is an answer, not an error: the caller decides what "healthy" means.
	if status, err := checker.Check(context.Background(), server.URL+"/down"); err != nil || status != http.StatusServiceUnavailable {
		t.Errorf("503: status %d, err %v", status, err)
	}
}

func TestHTTPCheckerAllowsLoopbackAndNamesThatResolveToIt(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer server.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))

	if status, err := NewHTTPChecker().Check(context.Background(), "http://localhost:"+port+"/"); err != nil || status != 200 {
		t.Errorf("localhost: status %d, err %v", status, err)
	}
}

func TestHTTPCheckerRefusesDangerousTargets(t *testing.T) {
	checker := &HTTPChecker{Lookup: func(_ context.Context, host string) ([]net.IP, error) {
		switch host {
		case "metadata.internal":
			return []net.IP{net.ParseIP("169.254.169.254")}, nil
		case "mixed.internal":
			return []net.IP{net.ParseIP("10.0.0.1"), net.ParseIP("fe80::1")}, nil
		}
		return nil, &net.DNSError{Err: "no such host", Name: host}
	}}

	for name, target := range map[string]string{
		"metadata service":      "http://169.254.169.254/latest/meta-data/",
		"ipv6 link-local":       "http://[fe80::1]/",
		"unspecified v4":        "http://0.0.0.0:8080/",
		"unspecified v6":        "http://[::]:8080/",
		"name -> link-local":    "http://metadata.internal/",
		"one bad answer of two": "http://mixed.internal/",
		"unresolvable":          "http://nope.invalid/",
		"ftp":                   "ftp://example.com/",
		"file":                  "file:///etc/passwd",
		"no scheme":             "example.com/health",
		"no host":               "http:///path",
		"javascript":            "javascript:alert(1)",
	} {
		status, err := checker.Check(context.Background(), target)
		if err == nil || status != 0 {
			t.Errorf("%s: status %d, err %v; want a refusal", name, status, err)
		}
	}
}

func TestHTTPCheckerFollowsRedirectsOnlyOnTheSameHost(t *testing.T) {
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("elsewhere")) }))
	defer other.Close()
	_, otherPort, _ := net.SplitHostPort(strings.TrimPrefix(other.URL, "http://"))

	mux := http.NewServeMux()
	mux.HandleFunc("/same", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/final", http.StatusFound) })
	mux.HandleFunc("/final", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("/away", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://localhost:"+otherPort+"/", http.StatusFound)
	})
	mux.HandleFunc("/metadata", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/", http.StatusFound)
	})
	mux.HandleFunc("/loop", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/loop", http.StatusFound) })
	server := httptest.NewServer(mux)
	defer server.Close()

	checker := NewHTTPChecker()
	if status, err := checker.Check(context.Background(), server.URL+"/same"); err != nil || status != http.StatusNoContent {
		t.Errorf("same-host redirect: status %d, err %v", status, err)
	}
	for _, path := range []string{"/away", "/metadata", "/loop"} {
		if status, err := checker.Check(context.Background(), server.URL+path); err == nil {
			t.Errorf("%s: status %d; want the redirect refused", path, status)
		}
	}
}

func TestHTTPCheckerTimesOutAndCapsTheBody(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
		}
	}))
	defer slow.Close()
	checker := &HTTPChecker{Timeout: 50 * time.Millisecond}
	started := time.Now()
	if _, err := checker.Check(context.Background(), slow.URL); err == nil {
		t.Error("expected a timeout")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Errorf("the timeout took %s", elapsed)
	}

	huge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		chunk := make([]byte, 64<<10)
		for i := 0; i < 160; i++ { // 10 MiB
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	}))
	defer huge.Close()
	if status, err := NewHTTPChecker().Check(context.Background(), huge.URL); err != nil || status != 200 {
		t.Errorf("a large body must not fail the check: status %d, err %v", status, err)
	}
}

func TestBlockedAddressRules(t *testing.T) {
	for address, blocked := range map[string]bool{
		"169.254.1.1":   true,
		"fe80::abcd":    true,
		"224.0.0.251":   true, // link-local multicast (mDNS)
		"239.1.1.1":     false,
		"0.0.0.0":       true,
		"::":            true,
		"10.1.2.3":      false,
		"192.168.0.10":  false,
		"127.0.0.1":     false,
		"::1":           false,
		"93.184.216.34": false,
	} {
		err := blockedAddress(address, net.ParseIP(address))
		if (err != nil) != blocked {
			t.Errorf("blockedAddress(%s) = %v; blocked should be %v", address, err, blocked)
		}
	}
}
