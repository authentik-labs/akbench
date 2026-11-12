package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
)

func TestAuthentication(t *testing.T) {
	s, _ := newServer(t, Config{})

	t.Run("rejects a request with no token", func(t *testing.T) {
		rec := httptest.NewRecorder()
		s.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/deployments", nil))
		requireStatus(t, rec, http.StatusUnauthorized)
	})

	t.Run("rejects a wrong token", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/deployments", nil)
		req.Header.Set("Authorization", "Bearer nope")
		rec := httptest.NewRecorder()
		s.Router().ServeHTTP(rec, req)
		requireStatus(t, rec, http.StatusUnauthorized)
	})

	t.Run("leaves probes unauthenticated", func(t *testing.T) {
		rec := httptest.NewRecorder()
		s.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		requireStatus(t, rec, http.StatusOK)
	})
}

func TestDeploymentCRUD(t *testing.T) {
	s, c := newServer(t, Config{})

	created := decodeBody[map[string]any](t, func() *httptest.ResponseRecorder {
		rec := do(t, s, http.MethodPost, "/api/v1/deployments", map[string]any{
			"metadata": map[string]any{"name": "single-node"},
			"spec": map[string]any{
				"namespace": "ak-single-node",
				"server":    map[string]any{"replicas": 2},
				"worker":    map[string]any{"replicas": 1},
				"postgres":  map[string]any{"mode": "Embedded"},
			},
		})
		requireStatus(t, rec, http.StatusCreated)
		return rec
	}())
	if created["kind"] != "AuthentikDeployment" {
		t.Fatalf("created object has kind %v", created["kind"])
	}

	var stored benchv1alpha1.AuthentikDeployment
	key := types.NamespacedName{Name: "single-node", Namespace: testNamespace}
	if err := c.Get(context.Background(), key, &stored); err != nil {
		t.Fatalf("deployment was not created: %v", err)
	}
	if stored.Spec.Server.Replicas != 2 {
		t.Fatalf("got %d server replicas, want 2", stored.Spec.Server.Replicas)
	}

	t.Run("list returns it", func(t *testing.T) {
		rec := do(t, s, http.MethodGet, "/api/v1/deployments", nil)
		requireStatus(t, rec, http.StatusOK)
		body := decodeBody[struct {
			Items []map[string]any `json:"items"`
		}](t, rec)
		if len(body.Items) != 1 {
			t.Fatalf("got %d items, want 1", len(body.Items))
		}
	})

	t.Run("put replaces the spec", func(t *testing.T) {
		rec := do(t, s, http.MethodPut, "/api/v1/deployments/single-node", map[string]any{
			"spec": map[string]any{
				"namespace": "ak-single-node",
				"server":    map[string]any{"replicas": 5},
				"worker":    map[string]any{"replicas": 1},
				"postgres":  map[string]any{"mode": "CNPG", "cnpg": map[string]any{"instances": 3}},
			},
		})
		requireStatus(t, rec, http.StatusOK)

		var updated benchv1alpha1.AuthentikDeployment
		if err := c.Get(context.Background(), key, &updated); err != nil {
			t.Fatalf("get after update: %v", err)
		}
		if updated.Spec.Server.Replicas != 5 {
			t.Fatalf("got %d server replicas, want 5", updated.Spec.Server.Replicas)
		}
		if updated.Spec.Postgres.Mode != benchv1alpha1.PostgresCNPG {
			t.Fatalf("got postgres mode %q, want CNPG", updated.Spec.Postgres.Mode)
		}
	})

	t.Run("delete removes it", func(t *testing.T) {
		rec := do(t, s, http.MethodDelete, "/api/v1/deployments/single-node", nil)
		requireStatus(t, rec, http.StatusNoContent)

		var gone benchv1alpha1.AuthentikDeployment
		err := c.Get(context.Background(), key, &gone)
		if !apierrors.IsNotFound(err) {
			t.Fatalf("got error %v, want NotFound", err)
		}
	})
}

// A client editing a form round-trips a GET, so a submitted status must not
// be able to fake readiness the operator never reported.
func TestCreateIgnoresSubmittedStatus(t *testing.T) {
	s, c := newServer(t, Config{})
	rec := do(t, s, http.MethodPost, "/api/v1/deployments", map[string]any{
		"metadata": map[string]any{"name": "faked", "namespace": "somewhere-else"},
		"spec": map[string]any{
			"namespace": "ak-faked",
			"server":    map[string]any{"replicas": 1},
			"worker":    map[string]any{"replicas": 1},
			"postgres":  map[string]any{"mode": "Embedded"},
		},
		"status": map[string]any{"serviceEndpoint": "attacker.example.com:80"},
	})
	requireStatus(t, rec, http.StatusCreated)

	var stored benchv1alpha1.AuthentikDeployment
	key := types.NamespacedName{Name: "faked", Namespace: testNamespace}
	if err := c.Get(context.Background(), key, &stored); err != nil {
		t.Fatalf("deployment was not created in the server's namespace: %v", err)
	}
	if stored.Status.ServiceEndpoint != "" {
		t.Fatalf("submitted status was persisted: %q", stored.Status.ServiceEndpoint)
	}
}

// The creator comes from authentication, never the body, and survives edits.
func TestCreatedByIsServerSet(t *testing.T) {
	s, c := newServer(t, Config{})
	spec := map[string]any{
		"namespace": "ak-owned",
		"server":    map[string]any{"replicas": 1},
		"worker":    map[string]any{"replicas": 1},
		"postgres":  map[string]any{"mode": "Embedded"},
	}
	forged := map[string]any{benchv1alpha1.AnnotationCreatedBy: "someone-else"}
	rec := do(t, s, http.MethodPost, "/api/v1/deployments", map[string]any{
		"metadata": map[string]any{"name": "owned", "annotations": forged},
		"spec":     spec,
	})
	requireStatus(t, rec, http.StatusCreated)
	rec = do(t, s, http.MethodPut, "/api/v1/deployments/owned", map[string]any{
		"metadata": map[string]any{"annotations": forged},
		"spec":     spec,
	})
	requireStatus(t, rec, http.StatusOK)

	var stored benchv1alpha1.AuthentikDeployment
	key := types.NamespacedName{Name: "owned", Namespace: testNamespace}
	if err := c.Get(context.Background(), key, &stored); err != nil {
		t.Fatal(err)
	}
	if got := stored.Annotations[benchv1alpha1.AnnotationCreatedBy]; got != staticTokenPrincipal {
		t.Fatalf("got created-by %q, want %q", got, staticTokenPrincipal)
	}
}

func TestGetStatusSubresource(t *testing.T) {
	s, _ := newServer(t, Config{}, sampleDeployment("ready-topo"))
	rec := do(t, s, http.MethodGet, "/api/v1/deployments/ready-topo/status", nil)
	requireStatus(t, rec, http.StatusOK)

	status := decodeBody[map[string]any](t, rec)
	if status["serviceEndpoint"] == nil {
		t.Fatalf("status has no serviceEndpoint: %v", status)
	}
}

func TestMissingResourceIs404(t *testing.T) {
	s, _ := newServer(t, Config{})
	rec := do(t, s, http.MethodGet, "/api/v1/testcases/nope", nil)
	requireStatus(t, rec, http.StatusNotFound)
}

// TestKickResourceSetsAnnotation guards the one thing kickResource actually
// does - every controller in this repo reconciles on any Update to its
// watched object regardless of what changed, so setting a fresh annotation
// is the entire mechanism; there's nothing controller-side to also assert.
func TestKickResourceSetsAnnotation(t *testing.T) {
	s, c := newServer(t, Config{}, sampleDeployment("kick-me"))

	rec := do(t, s, http.MethodPost, "/api/v1/deployments/kick-me/reconcile", nil)
	requireStatus(t, rec, http.StatusOK)

	var updated benchv1alpha1.AuthentikDeployment
	key := types.NamespacedName{Name: "kick-me", Namespace: testNamespace}
	if err := c.Get(context.Background(), key, &updated); err != nil {
		t.Fatalf("get after kick: %v", err)
	}
	first := updated.Annotations[kickedAtAnnotation]
	if first == "" {
		t.Fatalf("annotation %q was not set", kickedAtAnnotation)
	}

	// A second kick must change the value, not just be a no-op Update - a
	// repeat with an identical value wouldn't reliably be observed as a new
	// event by a watcher.
	rec = do(t, s, http.MethodPost, "/api/v1/deployments/kick-me/reconcile", nil)
	requireStatus(t, rec, http.StatusOK)
	if err := c.Get(context.Background(), key, &updated); err != nil {
		t.Fatalf("get after second kick: %v", err)
	}
	if updated.Annotations[kickedAtAnnotation] == first {
		t.Fatalf("second kick did not change %q", kickedAtAnnotation)
	}
}

func TestKickResourceMissingIs404(t *testing.T) {
	s, _ := newServer(t, Config{})
	rec := do(t, s, http.MethodPost, "/api/v1/deployments/nope/reconcile", nil)
	requireStatus(t, rec, http.StatusNotFound)
}

func TestListRespectsLabelSelector(t *testing.T) {
	run := &benchv1alpha1.TestRun{}
	run.Name = "login-a-abcde"
	run.Namespace = testNamespace
	run.Labels = map[string]string{benchv1alpha1.LabelTestCase: "login-a"}
	run.Spec.TestCaseRef.Name = "login-a"

	other := run.DeepCopy()
	other.Name = "login-b-fghij"
	other.Labels = map[string]string{benchv1alpha1.LabelTestCase: "login-b"}
	other.Spec.TestCaseRef.Name = "login-b"

	s, _ := newServer(t, Config{}, run, other)
	rec := do(t, s, http.MethodGet,
		"/api/v1/testruns?labelSelector=bench.goauthentik.io%2Ftestcase%3Dlogin-a", nil)
	requireStatus(t, rec, http.StatusOK)

	body := decodeBody[struct {
		Items []map[string]any `json:"items"`
	}](t, rec)
	if len(body.Items) != 1 {
		t.Fatalf("got %d items, want only login-a's run", len(body.Items))
	}
}

// Tenant presets are served by the generic resource handlers; this guards the
// path the web UI's preset page uses, and that saving a preset from a form
// (which submits no labels) leaves the operator's built-in mark in place.
func TestTenantPresetCRUD(t *testing.T) {
	builtIn := &benchv1alpha1.TenantPreset{
		Name: "small", Namespace: testNamespace,
		Labels: map[string]string{benchv1alpha1.LabelBuiltIn: "true"},
		Spec:   benchv1alpha1.TenantPresetSpec{Seed: 1, Users: 5000, Groups: 100, GroupDepth: 3, GroupsPerUser: 5},
	}
	s, c := newServer(t, Config{}, builtIn)
	spec := map[string]any{"seed": 1, "users": 2000, "groups": 60, "groupDepth": 3, "groupsPerUser": 4}

	rec := do(t, s, http.MethodPost, "/api/v1/tenantpresets", map[string]any{
		"metadata": map[string]any{"name": "ours"},
		"spec":     spec,
	})
	requireStatus(t, rec, http.StatusCreated)

	rec = do(t, s, http.MethodGet, "/api/v1/tenantpresets", nil)
	requireStatus(t, rec, http.StatusOK)
	list := decodeBody[struct {
		Items []map[string]any `json:"items"`
	}](t, rec)
	if len(list.Items) != 2 {
		t.Fatalf("got %d presets, want the built-in one and the new one", len(list.Items))
	}

	spec["users"] = 3000
	for _, name := range []string{"ours", "small"} {
		rec = do(t, s, http.MethodPut, "/api/v1/tenantpresets/"+name, map[string]any{"spec": spec})
		requireStatus(t, rec, http.StatusOK)
	}
	var stored benchv1alpha1.TenantPreset
	if err := c.Get(context.Background(), types.NamespacedName{Name: "ours", Namespace: testNamespace}, &stored); err != nil {
		t.Fatalf("get after update: %v", err)
	}
	if stored.Spec.Users != 3000 {
		t.Fatalf("got %d users, want 3000", stored.Spec.Users)
	}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "small", Namespace: testNamespace}, &stored); err != nil {
		t.Fatalf("get built-in after update: %v", err)
	}
	if stored.Labels[benchv1alpha1.LabelBuiltIn] != "true" {
		t.Fatalf("saving a built-in preset dropped its mark: labels %v", stored.Labels)
	}

	rec = do(t, s, http.MethodDelete, "/api/v1/tenantpresets/ours", nil)
	requireStatus(t, rec, http.StatusNoContent)
	err := c.Get(context.Background(), types.NamespacedName{Name: "ours", Namespace: testNamespace}, &stored)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("got error %v, want NotFound", err)
	}
}
