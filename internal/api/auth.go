package api

import (
	"context"
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-logr/logr"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// staticTokenPrincipal is who the audit log names for a request
// authenticated with the shared static token - all such requests look alike.
const staticTokenPrincipal = "static-token"

// MinStaticTokenLength is the shortest static token cmd/apiserver accepts.
// Nothing rate-limits failed logins, so the token's length is what makes
// guessing it infeasible.
const MinStaticTokenLength = 32

type principalKey struct{}

// principal returns who authenticated ctx's request, or "" outside
// requireToken (and the tickets it mints).
func principal(ctx context.Context) string {
	p, _ := ctx.Value(principalKey{}).(string)
	return p
}

// requireToken enforces the caller's credentials: either the static bearer
// token the UI can be configured with, or an OIDC-issued JWT (oidc.go) when
// an issuer is configured. This is the only thing between the public
// internet and the CRDs (and, via shell.go, a cluster-admin shell), so the
// static comparison runs in constant time and everything fails closed.
//
// Requests that change something are audit-logged with who made them (see
// auditWrites); rejected ones are logged too, so repeated failures show up.
func (s *Server) requireToken(next http.Handler) http.Handler {
	return s.requireTokenWithChallenge(func(*http.Request) string { return `Bearer realm="authentik-bench"` })(next)
}

// requireTokenWithChallenge is requireToken answering a rejected request
// with challenge's WWW-Authenticate header instead (mcp_oauth.go).
func (s *Server) requireTokenWithChallenge(challenge func(*http.Request) string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler { return s.authenticated(next, challenge) }
}

func (s *Server) authenticated(next http.Handler, challenge func(*http.Request) string) http.Handler {
	next = auditWrites(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.Token == "" && s.cfg.OIDC == nil {
			// Only reachable in tests - cmd/apiserver refuses to start
			// without either.
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, "unauthenticated")))
			return
		}
		header := challenge(r)
		if presented, ok := bearerToken(r); ok {
			if who, ok := s.authenticate(r, presented); ok {
				next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, who)))
				return
			}
			// RFC 6750 3.1: tells an OAuth client its (likely expired)
			// token was rejected, so it refreshes instead of signing in
			// from scratch.
			header += `, error="invalid_token"`
		}
		auditLog(r).Info("Rejected unauthenticated request", requestKeysAndValues(r)...)
		w.Header().Set("WWW-Authenticate", header)
		writeError(w, r, http.StatusUnauthorized, "missing or invalid bearer token")
	})
}

// authenticate returns who presented is, for the audit log.
func (s *Server) authenticate(r *http.Request, presented string) (string, bool) {
	if s.cfg.Token != "" && subtle.ConstantTimeCompare([]byte(presented), []byte(s.cfg.Token)) == 1 {
		return staticTokenPrincipal, true
	}
	if s.cfg.OIDC == nil {
		return "", false
	}
	who, err := s.cfg.OIDC.Verify(r.Context(), presented)
	if err != nil {
		logf.FromContext(r.Context()).V(1).Info("Rejected bearer token", "reason", err.Error())
		return "", false
	}
	return who, true
}

func bearerToken(r *http.Request) (string, bool) {
	const prefix = "Bearer "
	header := r.Header.Get("Authorization")
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", false
	}
	return header[len(prefix):], true
}

// unauditedRoutes are POSTs that only mint a short-lived credential for
// reading (the Grafana iframe, the live-events feed). The UI calls both on
// every page load, so logging them would bury the entries that matter.
// The shell's ticket mint is audited: it hands out a shell. MCP is
// JSON-RPC over POST, but every tool it serves is read-only (mcp.go).
var unauditedRoutes = map[string]bool{
	"/api/v1/grafana/session": true,
	"/api/v1/events/sessions": true,
	"/api/v1/mcp":             true,
}

// auditWrites logs every request that can change something - anything but
// GET/HEAD/OPTIONS - after it's handled, with who made it and the outcome.
// That covers every create/update/delete, SQL, resets, test runs and shell
// sessions; reads aren't logged.
func auditWrites(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}
		if unauditedRoutes[r.URL.Path] {
			next.ServeHTTP(w, r)
			return
		}
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)
		status := ww.Status()
		if status == 0 {
			status = http.StatusOK
		}
		kv := append([]any{"user", principal(r.Context()), "status", status}, requestKeysAndValues(r)...)
		auditLog(r).Info("Handled write request", kv...)
	})
}

func auditLog(r *http.Request) logr.Logger {
	return logf.FromContext(r.Context()).WithName("audit")
}

// requestKeysAndValues identifies a request in the audit log. forwardedFor
// is whatever the client (or a proxy in front of this one) sent - recorded,
// not trusted.
func requestKeysAndValues(r *http.Request) []any {
	kv := []any{"method", r.Method, "path", r.URL.Path, "remote", r.RemoteAddr, "requestID", middleware.GetReqID(r.Context())}
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		kv = append(kv, "forwardedFor", fwd)
	}
	return kv
}
