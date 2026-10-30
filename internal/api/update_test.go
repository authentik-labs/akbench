package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/authentik-labs/akbench/internal/appliance"
)

// newLatestRegistry serves akbench-apiserver:latest labelled with version.
func newLatestRegistry(t *testing.T, version string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/authentik-labs/akbench-apiserver/manifests/latest":
			_, _ = w.Write([]byte(`{"config":{"digest":"sha256:cfg"}}`))
		case "/v2/authentik-labs/akbench-apiserver/blobs/sha256:cfg":
			_, _ = w.Write([]byte(`{"config":{"Labels":{"org.opencontainers.image.version":"` + version + `"}}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestUpdateOutsideAppliance(t *testing.T) {
	s, _ := newServer(t, Config{})
	if view := decodeBody[updateView](t, do(t, s, http.MethodGet, "/api/v1/infrastructure/update", nil)); view.Managed {
		t.Errorf("view = %+v", view)
	}
	requireStatus(t, do(t, s, http.MethodPost, "/api/v1/infrastructure/update", nil), http.StatusConflict)
}

func TestUpdatePinsLatest(t *testing.T) {
	s, k8s := newServer(t, Config{
		Appliance: true, ApplianceImage: "ghcr.io/authentik-labs/akbench-apiserver", ApplianceVersion: "1.0.0",
	})
	s.imageRegistry = newLatestRegistry(t, "0.0.0-main.abc1234").URL
	operator := &appsv1.Deployment{
		Name: "authentik-bench-operator-controller-manager", Namespace: "authentik-bench", Labels: operatorLabels,
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{
			{Name: "manager", Image: "ghcr.io/authentik-labs/akbench:0.0.0-main.1f2a183"},
		}}}},
	}
	if err := k8s.Create(t.Context(), operator); err != nil {
		t.Fatal(err)
	}

	view := decodeBody[updateView](t, do(t, s, http.MethodGet, "/api/v1/infrastructure/update", nil))
	if view.Target != "1.0.0" || view.Latest != "0.0.0-main.abc1234" || view.LatestError != "" ||
		view.Operator != "0.0.0-main.1f2a183" || view.Appliance != "1.0.0" {
		t.Fatalf("view = %+v", view)
	}

	requireStatus(t, do(t, s, http.MethodPost, "/api/v1/infrastructure/update", nil), http.StatusOK)
	var cm corev1.ConfigMap
	if err := k8s.Get(t.Context(), types.NamespacedName{Namespace: appliance.SystemNamespace, Name: appliance.VersionConfigMap}, &cm); err != nil {
		t.Fatal(err)
	}
	if cm.Data[appliance.VersionKeyTarget] != "0.0.0-main.abc1234" || cm.Data[appliance.VersionKeyBase] != "1.0.0" {
		t.Errorf("pin = %v", cm.Data)
	}
	if view := decodeBody[updateView](t, do(t, s, http.MethodGet, "/api/v1/infrastructure/update", nil)); view.Target != "0.0.0-main.abc1234" {
		t.Errorf("target after update = %q", view.Target)
	}
}
