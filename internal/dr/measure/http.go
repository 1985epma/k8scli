// Package measure turns a disaster-recovery run into numbers: RTO (how long until the applications
// are healthy again), RPO (how much data a restore would lose), a drill that produces a report, and
// the optional PVC heartbeat that measures real data loss for one volume.
//
// Every figure in a report is labelled "measured" (it was observed during the drill) or "estimated"
// (derived from backup metadata, nothing was restored to check it).
package measure

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"
)

const (
	userAgent           = "k8scli-dr"
	defaultHTTPTimeout  = 10 * time.Second
	maxDiscardedBody    = 1 << 20
	maxRedirects        = 5
	dialTimeout         = 5 * time.Second
	errBlockedTargetFmt = "refusing to probe %s: link-local and unspecified addresses are not allowed"
)

// HTTPChecker implements dr.HTTPChecker. It is meant for health URLs of the applications of a plan,
// so private and loopback addresses are fine; link-local addresses (cloud metadata services) and the
// unspecified address are refused, checked again on the address that is really dialled so a DNS answer
// that changes between the check and the connection cannot get around it.
type HTTPChecker struct {
	// Timeout bounds one request; zero means 10 seconds. A shorter context deadline wins.
	Timeout time.Duration
	// Lookup resolves host names; nil means the system resolver. Tests replace it.
	Lookup func(ctx context.Context, host string) ([]net.IP, error)
}

// NewHTTPChecker returns a checker with the default timeout.
func NewHTTPChecker() *HTTPChecker { return &HTTPChecker{} }

// Check issues a GET and returns the status code. A non-2xx answer is not an error: the caller
// decides what status means healthy.
func (c *HTTPChecker) Check(ctx context.Context, rawURL string) (int, error) {
	target, err := parseProbeURL(rawURL)
	if err != nil {
		return 0, err
	}
	if err := c.checkHost(ctx, target.Hostname()); err != nil {
		return 0, err
	}

	timeout := c.Timeout
	if timeout <= 0 {
		timeout = defaultHTTPTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return 0, fmt.Errorf("invalid health URL: %w", err)
	}
	req.Header.Set("User-Agent", userAgent)

	response, err := c.client(target.Host).Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = response.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxDiscardedBody))
	return response.StatusCode, nil
}

func parseProbeURL(rawURL string) (*url.URL, error) {
	target, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("invalid health URL: %w", err)
	}
	if target.Scheme != "http" && target.Scheme != "https" {
		return nil, fmt.Errorf("invalid health URL %q: only http and https are allowed", rawURL)
	}
	if target.Hostname() == "" {
		return nil, fmt.Errorf("invalid health URL %q: it has no host", rawURL)
	}
	return target, nil
}

// checkHost resolves the host and refuses blocked addresses. IP literals skip the resolver.
func (c *HTTPChecker) checkHost(ctx context.Context, host string) error {
	if ip := net.ParseIP(host); ip != nil {
		return blockedAddress(host, ip)
	}
	lookup := c.Lookup
	if lookup == nil {
		lookup = func(ctx context.Context, name string) ([]net.IP, error) {
			addrs, err := net.DefaultResolver.LookupIPAddr(ctx, name)
			ips := make([]net.IP, 0, len(addrs))
			for _, addr := range addrs {
				ips = append(ips, addr.IP)
			}
			return ips, err
		}
	}
	ips, err := lookup(ctx, host)
	if err != nil {
		return fmt.Errorf("cannot resolve %s: %w", host, err)
	}
	for _, ip := range ips {
		if err := blockedAddress(host, ip); err != nil {
			return err
		}
	}
	return nil
}

func blockedAddress(host string, ip net.IP) error {
	if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
		return fmt.Errorf(errBlockedTargetFmt, host)
	}
	return nil
}

// client builds a client whose redirects stay on the original host and whose dialler validates the
// address that is really connected to.
func (c *HTTPChecker) client(originalHost string) *http.Client {
	dialer := &net.Dialer{
		Timeout: dialTimeout,
		Control: func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			if ip := net.ParseIP(host); ip != nil {
				return blockedAddress(address, ip)
			}
			return nil
		},
	}
	return &http.Client{
		Transport: &http.Transport{
			Proxy:             nil, // a proxy would bypass the address checks
			DialContext:       dialer.DialContext,
			DisableKeepAlives: true,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return errors.New("stopped after too many redirects")
			}
			if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
				return fmt.Errorf("redirect to %q refused: only http and https are allowed", req.URL.Scheme)
			}
			if !strings.EqualFold(req.URL.Host, originalHost) {
				return fmt.Errorf("redirect to another host (%s) refused", req.URL.Host)
			}
			return nil
		},
	}
}
