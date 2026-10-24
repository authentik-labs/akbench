package api

import (
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
)

// MCP OAuth through the web UI's own OIDC provider, for when OIDC login is
// on and Enterprise-Managed Authorization (mcp_auth.go) isn't.
//
// The MCP endpoint keeps accepting exactly what the rest of the API does -
// the static token or an OIDC access token for the web UI's client - so an
// MCP client gets no more and no less than the UI. What this adds is the
// discovery MCP's authorization spec expects: a 401 challenge naming RFC
// 9728 protected resource metadata, which lists the OIDC issuer as the
// authorization server. The client then runs authorization code + PKCE
// against that issuer itself, as the web UI's public client (it's
// pre-registered, there's no Dynamic Client Registration), and presents
// the access token here.
//
// The provider must allow the MCP client's loopback redirect URI (e.g.
// http://localhost:<port>/callback) on top of the web UI's own.

const (
	// mcpPath is the MCP endpoint's path under this server.
	mcpPath = "/api/v1/mcp"
	// mcpResourceName is the MCP server's human-readable name, in its
	// protected resource metadata and challenges.
	mcpResourceName = "authentik-bench"
)

// mcpOAuthEnabled reports whether the MCP endpoint advertises the OIDC
// issuer for OAuth.
func (s *Server) mcpOAuthEnabled() bool {
	return s.mcpAuth == nil && s.cfg.OIDC != nil && s.cfg.OIDCConfig.Issuer != ""
}

// mcpOAuthMetadataPath is where the protected resource metadata is served:
// RFC 9728's well-known prefix plus the resource's own path.
func (s *Server) mcpOAuthMetadataPath() string {
	path := mcpPath
	if u, err := url.Parse(s.cfg.MCPResource); err == nil && s.cfg.MCPResource != "" {
		path = strings.TrimSuffix(u.Path, "/")
	}
	return wellKnownProtectedResource + path
}

// mcpResource is the MCP endpoint's public URL, its OAuth resource
// identifier: Config.MCPResource if set, else derived from the request (the
// metadata is public, so a forged Host only misleads whoever forged it).
func (s *Server) mcpResource(r *http.Request) string {
	if s.cfg.MCPResource != "" {
		return s.cfg.MCPResource
	}
	return requestOrigin(r) + mcpPath
}

// requestOrigin is the scheme and host the client used to reach this
// server, honoring a reverse proxy's X-Forwarded-Proto/-Host.
func requestOrigin(r *http.Request) string {
	u := url.URL{Scheme: "http"}
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		u.Scheme = "https"
	}
	host := r.Host
	if fwd, _, _ := strings.Cut(r.Header.Get("X-Forwarded-Host"), ","); strings.TrimSpace(fwd) != "" {
		host = strings.TrimSpace(fwd)
	}
	u.Host = host
	return u.String()
}

// mcpOAuthScopes are the web UI's scopes plus offline_access, so the
// provider issues the MCP client a refresh token. The client refreshes at
// the provider directly; the refreshed access token has the same audience,
// so it's accepted like the first.
func (s *Server) mcpOAuthScopes() []string {
	scopes := s.cfg.OIDCConfig.scopes()
	if slices.Contains(scopes, oidc.ScopeOfflineAccess) {
		return scopes
	}
	return append(slices.Clone(scopes), oidc.ScopeOfflineAccess)
}

// mcpOAuthChallenge points an unauthenticated MCP client at the metadata.
func (s *Server) mcpOAuthChallenge(r *http.Request) string {
	resource, _ := url.Parse(s.mcpResource(r))
	metadataURL := resource.Scheme + "://" + resource.Host + s.mcpOAuthMetadataPath()
	return fmt.Sprintf(`Bearer realm=%q, resource_metadata=%q, scope=%q`,
		mcpResourceName, metadataURL, strings.Join(s.mcpOAuthScopes(), " "))
}

// mcpOAuthMetadata serves RFC 9728 protected resource metadata naming the
// OIDC issuer as the MCP endpoint's authorization server.
func (s *Server) mcpOAuthMetadata(w http.ResponseWriter, r *http.Request) {
	auth.ProtectedResourceMetadataHandler(&oauthex.ProtectedResourceMetadata{
		Resource:               s.mcpResource(r),
		AuthorizationServers:   []string{s.cfg.OIDCConfig.Issuer},
		ScopesSupported:        s.mcpOAuthScopes(),
		BearerMethodsSupported: []string{"header"},
		ResourceName:           mcpResourceName,
	}).ServeHTTP(w, r)
}
