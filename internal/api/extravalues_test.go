package api

import (
	"net/http"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
)

func TestGetExtraValuesWithoutAnySet(t *testing.T) {
	s, _ := newServer(t, Config{}, sampleDeployment("topo-a"))
	rec := do(t, s, http.MethodGet, "/api/v1/deployments/topo-a/extra-values", nil)
	requireStatus(t, rec, http.StatusOK)

	body := decodeBody[extraValuesBody](t, rec)
	if body.ValuesYAML != "" {
		t.Fatalf("got valuesYAML %q, want empty", body.ValuesYAML)
	}
}

func TestPutAndGetExtraValues(t *testing.T) {
	s, k8s := newServer(t, Config{}, sampleDeployment("topo-a"))

	yamlBody := "authentik:\n  log_level: debug\n"
	rec := do(t, s, http.MethodPut, "/api/v1/deployments/topo-a/extra-values", extraValuesBody{ValuesYAML: yamlBody})
	requireStatus(t, rec, http.StatusOK)

	got := decodeBody[extraValuesBody](t, rec)
	if got.ValuesYAML != yamlBody {
		t.Fatalf("response valuesYAML = %q, want %q", got.ValuesYAML, yamlBody)
	}

	// The ConfigMap exists and the deployment now references it.
	var cm corev1.ConfigMap
	if err := k8s.Get(t.Context(), types.NamespacedName{Name: "topo-a-extra-values", Namespace: testNamespace}, &cm); err != nil {
		t.Fatalf("expected extra-values ConfigMap to exist: %v", err)
	}
	if cm.Data["values.yaml"] != yamlBody {
		t.Fatalf("configmap values.yaml = %q, want %q", cm.Data["values.yaml"], yamlBody)
	}

	var topo benchv1alpha1.AuthentikDeployment
	if err := k8s.Get(t.Context(), types.NamespacedName{Name: "topo-a", Namespace: testNamespace}, &topo); err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	if topo.Spec.Authentik.ExtraValuesRef == nil || topo.Spec.Authentik.ExtraValuesRef.Name != "topo-a-extra-values" {
		t.Fatalf("deployment extraValuesRef = %+v, want topo-a-extra-values", topo.Spec.Authentik.ExtraValuesRef)
	}

	// GET now returns what was saved.
	rec = do(t, s, http.MethodGet, "/api/v1/deployments/topo-a/extra-values", nil)
	requireStatus(t, rec, http.StatusOK)
	got = decodeBody[extraValuesBody](t, rec)
	if got.ValuesYAML != yamlBody {
		t.Fatalf("GET valuesYAML = %q, want %q", got.ValuesYAML, yamlBody)
	}

	// Saving again with different content updates the same ConfigMap.
	updated := "authentik:\n  log_level: warning\n"
	rec = do(t, s, http.MethodPut, "/api/v1/deployments/topo-a/extra-values", extraValuesBody{ValuesYAML: updated})
	requireStatus(t, rec, http.StatusOK)
	if err := k8s.Get(t.Context(), types.NamespacedName{Name: "topo-a-extra-values", Namespace: testNamespace}, &cm); err != nil {
		t.Fatalf("get configmap after update: %v", err)
	}
	if cm.Data["values.yaml"] != updated {
		t.Fatalf("configmap values.yaml after update = %q, want %q", cm.Data["values.yaml"], updated)
	}
}

func TestPutExtraValuesRejectsInvalidYAML(t *testing.T) {
	s, _ := newServer(t, Config{}, sampleDeployment("topo-a"))
	rec := do(t, s, http.MethodPut, "/api/v1/deployments/topo-a/extra-values",
		extraValuesBody{ValuesYAML: "not: valid: yaml: [1,2"})
	requireStatus(t, rec, http.StatusBadRequest)
}

func TestPutExtraValuesUnknownDeployment(t *testing.T) {
	s, _ := newServer(t, Config{})
	rec := do(t, s, http.MethodPut, "/api/v1/deployments/does-not-exist/extra-values",
		extraValuesBody{ValuesYAML: ""})
	requireStatus(t, rec, http.StatusNotFound)
}

func TestGetHelmValues(t *testing.T) {
	s, _ := newServer(t, Config{})
	rec := do(t, s, http.MethodGet, "/api/v1/deployments/topo-a/helm-values", nil)
	requireStatus(t, rec, http.StatusOK)
	if got := decodeBody[extraValuesBody](t, rec).ValuesYAML; got != "" {
		t.Fatalf("valuesYAML before HelmRelease exists = %q, want empty", got)
	}

	hr := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "helm.toolkit.fluxcd.io/v2",
		"kind":       "HelmRelease",
		"metadata":   map[string]any{"name": "topo-a", "namespace": testNamespace},
		"spec":       map[string]any{"values": map[string]any{"authentik": map[string]any{"log_level": "debug"}}},
	}}
	s, _ = newServer(t, Config{}, hr)
	rec = do(t, s, http.MethodGet, "/api/v1/deployments/topo-a/helm-values", nil)
	requireStatus(t, rec, http.StatusOK)
	if got, want := decodeBody[extraValuesBody](t, rec).ValuesYAML, "authentik:\n  log_level: debug\n"; got != want {
		t.Fatalf("valuesYAML = %q, want %q", got, want)
	}
}
