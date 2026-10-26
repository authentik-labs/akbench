package registry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newFakeRegistry serves one repository with the given existing tags,
// challenging unauthenticated requests the way ghcr.io does.
func newFakeRegistry(t *testing.T, repository string, existing ...string) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			if got := r.URL.Query().Get("scope"); got != "repository:"+repository+":pull" {
				t.Errorf("unexpected token scope %q", got)
			}
			if got := r.URL.Query().Get("service"); got != "fake" {
				t.Errorf("unexpected token service %q", got)
			}
			_, _ = w.Write([]byte(`{"token":"anon"}`))
			return
		}
		if r.Header.Get("Authorization") != "Bearer anon" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="`+srv.URL+`/token",service="fake",scope="repository:`+repository+`:pull"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		for _, tag := range existing {
			if r.URL.Path == "/v2/"+repository+"/manifests/"+tag {
				w.WriteHeader(http.StatusOK)
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestTagExists(t *testing.T) {
	srv := newFakeRegistry(t, "goauthentik/server", "2026.8.1")
	c := &Checker{Endpoints: map[string]string{"ghcr.io": srv.URL}}
	for tag, want := range map[string]bool{"2026.8.1": true, "2026.8.99": false} {
		got, err := c.TagExists(context.Background(), "ghcr.io/goauthentik/server", tag)
		if err != nil {
			t.Fatalf("tag %s: %v", tag, err)
		}
		if got != want {
			t.Errorf("tag %s: exists = %v, want %v", tag, got, want)
		}
	}
}

func TestTagExistsRejectsInvalidTag(t *testing.T) {
	c := &Checker{}
	for _, tag := range []string{"", "../latest", "-leading-dash", strings.Repeat("a", 129)} {
		if _, err := c.TagExists(context.Background(), "ghcr.io/goauthentik/server", tag); err == nil {
			t.Errorf("tag %q: expected an error", tag)
		}
	}
}

func TestParseImage(t *testing.T) {
	for image, want := range map[string][2]string{
		"ghcr.io/goauthentik/server":   {"ghcr.io", "goauthentik/server"},
		"nginx":                        {"docker.io", "library/nginx"},
		"beryju/authentik":             {"docker.io", "beryju/authentik"},
		"localhost:5000/team/app":      {"localhost:5000", "team/app"},
		"registry.example.com/a/b/c-d": {"registry.example.com", "a/b/c-d"},
	} {
		host, repo, err := ParseImage(image)
		if err != nil {
			t.Fatalf("%s: %v", image, err)
		}
		if host != want[0] || repo != want[1] {
			t.Errorf("%s: got %s %s, want %s %s", image, host, repo, want[0], want[1])
		}
	}
	for _, image := range []string{"ghcr.io/goauthentik/server:2026.8", "ghcr.io/goauthentik/server@sha256:abc", "Upper/Case", ""} {
		if _, _, err := ParseImage(image); err == nil {
			t.Errorf("%q: expected an error", image)
		}
	}
}

func TestParseBearerChallenge(t *testing.T) {
	got, ok := parseBearerChallenge(`Bearer realm="https://ghcr.io/token",service="ghcr.io",scope="repository:a/b:pull"`)
	if !ok || got["realm"] != "https://ghcr.io/token" || got["service"] != "ghcr.io" || got["scope"] != "repository:a/b:pull" {
		t.Errorf("unexpected parse: %v %v", got, ok)
	}
	if _, ok := parseBearerChallenge(`Basic realm="x"`); ok {
		t.Error("Basic challenge must not parse as Bearer")
	}
}

func TestLabelFollowsIndex(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/a/b/manifests/latest":
			_, _ = w.Write([]byte(`{"manifests":[{"digest":"sha256:amd64"},{"digest":"sha256:arm64"}]}`))
		case "/v2/a/b/manifests/sha256:amd64":
			_, _ = w.Write([]byte(`{"config":{"digest":"sha256:cfg"}}`))
		case "/v2/a/b/blobs/sha256:cfg":
			_, _ = w.Write([]byte(`{"config":{"Labels":{"org.opencontainers.image.version":"1.2.3"}}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	c := &Checker{Endpoints: map[string]string{"ghcr.io": srv.URL}}
	got, err := c.Label(context.Background(), "ghcr.io/a/b", "latest", "org.opencontainers.image.version")
	if err != nil || got != "1.2.3" {
		t.Errorf("got %q, %v", got, err)
	}
	if _, err := c.Label(context.Background(), "ghcr.io/a/b", "missing", "x"); err == nil {
		t.Error("expected an error for a missing tag")
	}
}
