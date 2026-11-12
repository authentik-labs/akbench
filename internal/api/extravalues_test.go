package api

import (
	"context"
	"errors"
	"net/http"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

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

func TestPutExtraValuesOnChildDeploymentUpdatesParent(t *testing.T) {
	m := &benchv1alpha1.TestCase{Name: "pr-1", Namespace: testNamespace, UID: "parent-uid"}
	topo := sampleDeployment("pr-1-head-login")
	topo.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: benchv1alpha1.GroupVersion.String(), Kind: "TestCase",
		Name: m.Name, UID: m.UID, Controller: ptr.To(true),
	}}
	s, k8s := newServer(t, Config{}, m, topo)

	yamlBody := "authentik:\n  log_level: debug\n"
	rec := do(t, s, http.MethodPut, "/api/v1/deployments/pr-1-head-login/extra-values", extraValuesBody{ValuesYAML: yamlBody})
	requireStatus(t, rec, http.StatusOK)

	if err := k8s.Get(t.Context(), types.NamespacedName{Name: m.Name, Namespace: testNamespace}, m); err != nil {
		t.Fatalf("get parent: %v", err)
	}
	if m.Spec.ExtraValues != yamlBody {
		t.Fatalf("parent extraValues = %q, want %q", m.Spec.ExtraValues, yamlBody)
	}
	rec = do(t, s, http.MethodGet, "/api/v1/deployments/pr-1-head-login/extra-values", nil)
	requireStatus(t, rec, http.StatusOK)
	if got := decodeBody[extraValuesBody](t, rec).ValuesYAML; got != yamlBody {
		t.Fatalf("GET valuesYAML = %q, want %q", got, yamlBody)
	}
}

// conflictOnce builds a Server whose client answers the first Update of an
// object matching isTarget with a conflict, the way the API server does when
// the operator wrote the object's status since it was read. conflicts counts
// how many were returned.
func conflictOnce(t *testing.T, isTarget func(client.Object) bool, objects ...client.Object) (s *Server, k8s client.Client, conflicts *int) {
	t.Helper()
	conflicts = new(int)
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(objects...).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if isTarget(obj) && *conflicts == 0 {
					*conflicts++
					return apierrors.NewConflict(
						schema.GroupResource{Group: benchv1alpha1.GroupVersion.Group, Resource: "objects"},
						obj.GetName(), errors.New("the object has been modified"))
				}
				return cl.Update(ctx, obj, opts...)
			},
		}).
		Build()
	return New(c, Config{Namespace: testNamespace, Token: testToken}), c, conflicts
}

// A conflict on the deployment update used to fail the request after the
// ConfigMap was written, leaving it with no deployment referencing it.
func TestPutExtraValuesRetriesDeploymentConflict(t *testing.T) {
	s, k8s, conflicts := conflictOnce(t, func(obj client.Object) bool {
		_, ok := obj.(*benchv1alpha1.AuthentikDeployment)
		return ok
	}, sampleDeployment("topo-a"))

	rec := do(t, s, http.MethodPut, "/api/v1/deployments/topo-a/extra-values",
		extraValuesBody{ValuesYAML: "authentik:\n  log_level: debug\n"})
	requireStatus(t, rec, http.StatusOK)
	if *conflicts != 1 {
		t.Fatalf("got %d conflicts, want 1", *conflicts)
	}

	var topo benchv1alpha1.AuthentikDeployment
	if err := k8s.Get(t.Context(), types.NamespacedName{Name: "topo-a", Namespace: testNamespace}, &topo); err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	if topo.Spec.Authentik.ExtraValuesRef == nil || topo.Spec.Authentik.ExtraValuesRef.Name != "topo-a-extra-values" {
		t.Fatalf("deployment extraValuesRef = %+v, want topo-a-extra-values", topo.Spec.Authentik.ExtraValuesRef)
	}
}

func TestPutExtraValuesRetriesParentConflict(t *testing.T) {
	m := &benchv1alpha1.TestCase{Name: "pr-1", Namespace: testNamespace, UID: "parent-uid"}
	topo := sampleDeployment("pr-1-head-login")
	topo.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: benchv1alpha1.GroupVersion.String(), Kind: "TestCase",
		Name: m.Name, UID: m.UID, Controller: ptr.To(true),
	}}
	s, k8s, conflicts := conflictOnce(t, func(obj client.Object) bool {
		_, ok := obj.(*benchv1alpha1.TestCase)
		return ok
	}, m, topo)

	yamlBody := "authentik:\n  log_level: debug\n"
	rec := do(t, s, http.MethodPut, "/api/v1/deployments/pr-1-head-login/extra-values", extraValuesBody{ValuesYAML: yamlBody})
	requireStatus(t, rec, http.StatusOK)
	if *conflicts != 1 {
		t.Fatalf("got %d conflicts, want 1", *conflicts)
	}

	var got benchv1alpha1.TestCase
	if err := k8s.Get(t.Context(), types.NamespacedName{Name: m.Name, Namespace: testNamespace}, &got); err != nil {
		t.Fatalf("get parent: %v", err)
	}
	if got.Spec.ExtraValues != yamlBody {
		t.Fatalf("parent extraValues = %q, want %q", got.Spec.ExtraValues, yamlBody)
	}
}

// Once the deployment references its ConfigMap, saving new content leaves the
// deployment alone: the operator's ConfigMap watch picks the change up, and
// every needless write to the deployment is another chance of a conflict.
func TestPutExtraValuesWritesDeploymentOnlyWhenRefChanges(t *testing.T) {
	updates := 0
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(sampleDeployment("topo-a")).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if _, ok := obj.(*benchv1alpha1.AuthentikDeployment); ok {
					updates++
				}
				return cl.Update(ctx, obj, opts...)
			},
		}).
		Build()
	s := New(c, Config{Namespace: testNamespace, Token: testToken})

	for i, values := range []string{"authentik:\n  log_level: debug\n", "authentik:\n  log_level: info\n"} {
		rec := do(t, s, http.MethodPut, "/api/v1/deployments/topo-a/extra-values", extraValuesBody{ValuesYAML: values})
		requireStatus(t, rec, http.StatusOK)
		if updates != 1 {
			t.Fatalf("after save %d: %d deployment updates, want 1 (setting extraValuesRef)", i+1, updates)
		}
	}
}

// A deployment that keeps conflicting fails the request with the conflict
// rather than retrying forever.
func TestPutExtraValuesGivesUpAfterRepeatedConflicts(t *testing.T) {
	conflicts := 0
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(sampleDeployment("topo-a")).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if _, ok := obj.(*benchv1alpha1.AuthentikDeployment); ok {
					conflicts++
					return apierrors.NewConflict(
						schema.GroupResource{Group: benchv1alpha1.GroupVersion.Group, Resource: "authentikdeployments"},
						obj.GetName(), errors.New("the object has been modified"))
				}
				return cl.Update(ctx, obj, opts...)
			},
		}).
		Build()
	s := New(c, Config{Namespace: testNamespace, Token: testToken})

	rec := do(t, s, http.MethodPut, "/api/v1/deployments/topo-a/extra-values",
		extraValuesBody{ValuesYAML: "authentik:\n  log_level: debug\n"})
	requireStatus(t, rec, http.StatusConflict)
	if conflicts != retry.DefaultBackoff.Steps {
		t.Fatalf("got %d attempts, want %d", conflicts, retry.DefaultBackoff.Steps)
	}
}
