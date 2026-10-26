package api

import (
	"net/http"
	"net/url"
	"strings"
)

// Response headers every route gets. Content-Security-Policy is set here to
// the strictest value (right for JSON), and widened by the two routes that
// serve documents: the SPA (spaCSP) and the Grafana proxy (grafanaCSP).
var securityHeaderValues = map[string]string{
	"X-Content-Type-Options": "nosniff",
	"Referrer-Policy":        "same-origin",
	// Legacy twin of frame-ancestors; SAMEORIGIN because the SPA embeds the
	// Grafana proxy in an iframe.
	"X-Frame-Options": "SAMEORIGIN",
	// No includeSubDomains: the backend has no business deciding for sibling
	// hosts. Browsers ignore this over a certificate they had to click
	// through, so the self-signed default isn't pinned by it.
	"Strict-Transport-Security": "max-age=31536000",
	"Content-Security-Policy":   "default-src 'none'; frame-ancestors 'none'",
}

// grafanaCSP only restricts framing: Grafana's own pages need far more than
// the SPA's policy allows, and Grafana sends its own policy if configured to.
const grafanaCSP = "frame-ancestors 'self'"

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for k, v := range securityHeaderValues {
			w.Header().Set(k, v)
		}
		next.ServeHTTP(w, r)
	})
}

// dropUpstreamSecurityHeaders removes the headers securityHeaders already set
// from a proxied response, which httputil.ReverseProxy would otherwise append
// as second values. Content-Security-Policy is kept: two policies are both
// enforced, so Grafana's own can only narrow grafanaCSP further.
func dropUpstreamSecurityHeaders(res *http.Response) error {
	for k := range securityHeaderValues {
		if k != "Content-Security-Policy" {
			res.Header.Del(k)
		}
	}
	return nil
}

// spaCSP is the web UI's Content-Security-Policy. Beyond its own origin, the
// SPA needs:
//   - 'wasm-unsafe-eval' and data: fetches: ghostty-web (the shell's
//     terminal) fetches its inlined WebAssembly module from a data: URL and
//     compiles it at runtime;
//   - inline styles: React style props and Monaco's injected <style> tags;
//   - blob: workers: Monaco's language workers spawn from blob URLs;
//   - api.github.com: the pull-request comparison page and the PR column on
//     the list pages (web/ui/src/github.ts);
//   - the OIDC issuer: discovery and token requests (connect-src) and
//     oidc-client-ts' silent renew iframe (frame-src).
func spaCSP(oidcIssuer string) string {
	connect := []string{"'self'", "data:", "https://api.github.com"}
	frame := []string{"'self'"}
	if origin := urlOrigin(oidcIssuer); origin != "" {
		connect = append(connect, origin)
		frame = append(frame, origin)
	}
	return strings.Join([]string{
		"default-src 'self'",
		"script-src 'self' 'wasm-unsafe-eval'",
		"style-src 'self' 'unsafe-inline'",
		"img-src 'self' data: blob:",
		"font-src 'self' data:",
		"worker-src 'self' blob:",
		"connect-src " + strings.Join(connect, " "),
		"frame-src " + strings.Join(frame, " "),
		"object-src 'none'",
		"base-uri 'self'",
		"form-action 'self'",
		"frame-ancestors 'self'",
	}, "; ")
}

// urlOrigin returns raw's scheme://host[:port], or "" if raw isn't an
// absolute http(s) URL.
func urlOrigin(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return ""
	}
	return u.Scheme + "://" + u.Host
}
