package api

import (
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/oauthex"
)

const testOIDCIssuer = "https://idp.example/application/o/akbench/"

func newOIDCServer(t *testing.T, cfg Config) *Server {
	t.Helper()
	cfg.OIDC = fakeVerifier{valid: "a.valid.jwt"}
	cfg.OIDCConfig = OIDCConfig{Issuer: testOIDCIssuer, ClientID: "bench"}
	s, _ := newServer(t, cfg)
	return s
}

func TestMCPOAuthChallengesWithResourceMetadata(t *testing.T) {
	s := newOIDCServer(t, Config{})
	req := httptest.NewRequest(http.MethodPost, "http://bench.example/api/v1/mcp", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	requireStatus(t, rec, http.StatusUnauthorized)
	challenge := rec.Header().Get("WWW-Authenticate")
	if !strings.Contains(challenge, `resource_metadata="http://bench.example/.well-known/oauth-protected-resource/api/v1/mcp"`) ||
		!strings.Contains(challenge, `scope="openid profile email offline_access"`) {
		t.Fatalf("challenge = %q", challenge)
	}
	if strings.Contains(challenge, "invalid_token") {
		t.Fatalf("challenge without a token = %q", challenge)
	}

	// A rejected (e.g. expired) token is flagged, so the client refreshes.
	req = httptest.NewRequest(http.MethodPost, "http://bench.example/api/v1/mcp", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer expired.jwt")
	rec = httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	requireStatus(t, rec, http.StatusUnauthorized)
	if got := rec.Header().Get("WWW-Authenticate"); !strings.Contains(got, `, error="invalid_token"`) {
		t.Fatalf("challenge for a rejected token = %q", got)
	}

	// The rest of the API keeps its plain challenge.
	rec = requestWithBearer(t, s, "/api/v1/deployments", "")
	if got := rec.Header().Get("WWW-Authenticate"); got != `Bearer realm="authentik-bench"` {
		t.Fatalf("API challenge = %q", got)
	}
}

func TestMCPOAuthServesMetadata(t *testing.T) {
	for name, tc := range map[string]struct {
		cfg              Config
		path             string
		header           http.Header
		wantResource     string
		wantMetadataPath string
	}{
		"derived from the request": {
			wantResource: "http://bench.example/api/v1/mcp",
		},
		"behind a reverse proxy": {
			header:       http.Header{"X-Forwarded-Proto": {"https"}, "X-Forwarded-Host": {"bench.example.com"}},
			wantResource: "https://bench.example.com/api/v1/mcp",
		},
		"configured": {
			cfg:              Config{MCPResource: "https://bench.example.com/bench/api/v1/mcp"},
			wantResource:     "https://bench.example.com/bench/api/v1/mcp",
			wantMetadataPath: "/.well-known/oauth-protected-resource/bench/api/v1/mcp",
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := newOIDCServer(t, tc.cfg)
			path := tc.wantMetadataPath
			if path == "" {
				path = "/.well-known/oauth-protected-resource/api/v1/mcp"
			}
			req := httptest.NewRequest(http.MethodGet, "http://bench.example"+path, nil)
			maps.Copy(req.Header, tc.header)
			rec := httptest.NewRecorder()
			s.Router().ServeHTTP(rec, req)
			requireStatus(t, rec, http.StatusOK)
			meta := decodeBody[oauthex.ProtectedResourceMetadata](t, rec)
			if meta.Resource != tc.wantResource || !slices.Equal(meta.AuthorizationServers, []string{testOIDCIssuer}) ||
				!slices.Contains(meta.ScopesSupported, "openid") || !slices.Contains(meta.ScopesSupported, "offline_access") {
				t.Fatalf("metadata = %+v", meta)
			}
		})
	}
}

// MCP takes exactly the API's own credentials.
func TestMCPOAuthAcceptsAPITokens(t *testing.T) {
	srv := httptest.NewServer(newOIDCServer(t, Config{}).Router())
	t.Cleanup(srv.Close)
	for token, want := range map[string]int{
		"a.valid.jwt": http.StatusOK,
		testToken:     http.StatusOK,
		"forged.jwt":  http.StatusUnauthorized,
	} {
		if resp := postMCP(t, srv.URL, token); resp.StatusCode != want {
			t.Errorf("token %q: status %d, want %d", token, resp.StatusCode, want)
		}
	}
}

func TestMCPOAuthNeedsOIDC(t *testing.T) {
	s, _ := newServer(t, Config{})
	rec := requestWithBearer(t, s, "/.well-known/oauth-protected-resource/api/v1/mcp", "")
	requireStatus(t, rec, http.StatusNotFound)
}
