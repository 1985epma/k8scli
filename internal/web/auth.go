package web

import (
	"crypto/subtle"
	"net/http"
	"net/url"
	"strings"
)

const (
	sessionCookie = "k8scli_session"
	// csrfHeader must be present on every request that is not GET/HEAD. Browsers
	// do not add custom headers to cross-origin form posts, and a cross-origin
	// fetch carrying one needs a CORS preflight that the server never grants.
	csrfHeader = "X-K8scli-Request"

	contentSecurityPolicy = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; " +
		"connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"
)

// secure wraps next with the console's request checks, in this order: security
// headers, Host allow-list (DNS rebinding), token exchange, session check and
// the CSRF checks for state-changing methods.
func (s *Server) secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := w.Header()
		header.Set("Content-Security-Policy", contentSecurityPolicy)
		header.Set("X-Content-Type-Options", "nosniff")
		header.Set("X-Frame-Options", "DENY")
		header.Set("Referrer-Policy", "no-referrer")
		header.Set("Cache-Control", "no-store")

		if !s.opts.AllowRemote && !s.hostAllowed(r.Host) {
			http.Error(w, "Forbidden host", http.StatusForbidden)
			return
		}

		// Opening the printed link trades the token for a cookie and then
		// redirects to a clean URL, so the token does not stay in the history.
		if r.Method == http.MethodGet && r.URL.Path == "/" {
			if token := r.URL.Query().Get("token"); token != "" {
				if !s.validToken(token) {
					writeUnauthorized(w, r)
					return
				}
				// #nosec G124 -- a fixed Secure=true makes Safari drop the cookie on http://localhost; Secure is set when TLS is in front.
				http.SetCookie(w, &http.Cookie{
					Name:     sessionCookie,
					Value:    token,
					Path:     "/",
					HttpOnly: true,
					SameSite: http.SameSiteStrictMode,
					// The console speaks plain HTTP on loopback, where browsers differ on
					// Secure cookies; behind a TLS-terminating proxy it applies.
					Secure: r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https"),
				})
				http.Redirect(w, r, "/", http.StatusSeeOther)
				return
			}
		}

		if !s.authenticated(r) {
			writeUnauthorized(w, r)
			return
		}

		// SameSite=Strict does not help against another web app on a different
		// port of the same host (ports do not make a different "site"), so a
		// page on localhost:3000 could send this cookie along with a fetch.
		// Browsers label such requests; the API only serves its own page.
		if strings.HasPrefix(r.URL.Path, "/api/") {
			if site := r.Header.Get("Sec-Fetch-Site"); site == "cross-site" || site == "same-site" {
				writeError(w, http.StatusForbidden, "cross_site", "cross-site requests are not allowed")
				return
			}
		}
		if origin := r.Header.Get("Origin"); origin != "" && !sameOrigin(origin, r.Host) {
			writeError(w, http.StatusForbidden, "csrf", "cross-origin request rejected")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get(csrfHeader) == "" {
			writeError(w, http.StatusForbidden, "csrf", "missing "+csrfHeader+" header")
			return
		}

		next.ServeHTTP(w, r)
	})
}

func (s *Server) hostAllowed(host string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.allowedHosts[strings.ToLower(host)]
}

func (s *Server) validToken(candidate string) bool {
	return subtle.ConstantTimeCompare([]byte(candidate), []byte(s.token)) == 1
}

// authenticated accepts the session cookie, or a bearer token for scripts.
func (s *Server) authenticated(r *http.Request) bool {
	if cookie, err := r.Cookie(sessionCookie); err == nil && s.validToken(cookie.Value) {
		return true
	}
	if bearer, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		return s.validToken(bearer)
	}
	return false
}

func sameOrigin(origin, host string) bool {
	parsed, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return strings.EqualFold(parsed.Host, host)
}

func writeUnauthorized(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		writeError(w, http.StatusUnauthorized, "unauthorized", "open the link printed by `k8scli serve` or `k8scli ui` to start a session")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte("Unauthorized.\n\nOpen the link printed by `k8scli serve` or `k8scli ui`. It carries the session token.\n"))
}
