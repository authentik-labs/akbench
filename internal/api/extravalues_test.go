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

// A ConfigMap with the conventional name that the deployment does not
// reference is not in effect, so it is not reported as the deployment's
// extra values.
func TestGetExtraValuesIgnoresUnattachedConfigMap(t *testing.T) {
	unattached := &corev1.ConfigMap{
		Name: "topo-a-extra-values", Namespace: testNamespace,
		Data: map[string]string{"values.yaml": "authentik:\n  log_level: debug\n"},
	}
	s, _ := newServer(t, Config{}, sampleDeployment("topo-a"), unattached)
	rec := do(t, s, http.MethodGet, "/api/v1/deployments/topo-a/extra-values", nil)
	requireStatus(t, rec, http.StatusOK)
	if got := decodeBody[extraValuesBody](t, rec).ValuesYAML; got != "" {
		t.Fatalf("got valuesYAML %q for an unattached ConfigMap, want empty", got)
	}
}

// The ConfigMap the deployment references is the one reported, whatever it
// is named.
func TestGetExtraValuesFollowsRef(t *testing.T) {
	yamlBody := "authentik:\n  log_level: debug\n"
	topo := sampleDeployment("topo-a")
	topo.Spec.Authentik.ExtraValuesRef = &corev1.LocalObjectReference{Name: "shared-values"}
	referenced := &corev1.ConfigMap{
		Name: "shared-values", Namespace: testNamespace,
		Data: map[string]string{"values.yaml": yamlBody},
	}
	s, _ := newServer(t, Config{}, topo, referenced)
	rec := do(t, s, http.MethodGet, "/api/v1/deployments/topo-a/extra-values", nil)
	requireStatus(t, rec, http.StatusOK)
	if got := decodeBody[extraValuesBody](t, rec).ValuesYAML; got != yamlBody {
		t.Fatalf("got valuesYAML %q, want %q", got, yamlBody)
	}
}

// A deployment that doesn't exist has no extra values to show: its own
// ConfigMap is garbage-collected with it, and one still around is not read.
func TestGetExtraValuesUnknownDeployment(t *testing.T) {
	leftover := &corev1.ConfigMap{
		Name: "gone-extra-values", Namespace: testNamespace,
		Data: map[string]string{"values.yaml": "authentik:\n  log_level: debug\n"},
	}
	s, _ := newServer(t, Config{}, leftover)
	rec := do(t, s, http.MethodGet, "/api/v1/deployments/gone/extra-values", nil)
	requireStatus(t, rec, http.StatusNotFound)
}

// A reference to a ConfigMap that doesn't exist reports no extra values, so
// the form starts blank and saving repairs the reference.
func TestGetExtraValuesDanglingRef(t *testing.T) {
	topo := sampleDeployment("topo-a")
	topo.Spec.Authentik.ExtraValuesRef = &corev1.LocalObjectReference{Name: "topo-a-extra-values"}
	s, _ := newServer(t, Config{}, topo)
	rec := do(t, s, http.MethodGet, "/api/v1/deployments/topo-a/extra-values", nil)
	requireStatus(t, rec, http.StatusOK)
	if got := decodeBody[extraValuesBody](t, rec).ValuesYAML; got != "" {
		t.Fatalf("got valuesYAML %q for a dangling reference, want empty", got)
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

// The ConfigMap is owned by its deployment, so deleting the deployment
// garbage-collects it instead of leaving it behind - whether this save
// creates it or updates one saved before these had an owner.
func TestPutExtraValuesOwnsConfigMap(t *testing.T) {
	unowned := &corev1.ConfigMap{
		Name: "topo-a-extra-values", Namespace: testNamespace,
		Data: map[string]string{"values.yaml": "authentik:\n  log_level: info\n"},
	}
	for name, exists := range map[string]bool{"created": false, "updated": true} {
		t.Run(name, func(t *testing.T) {
			topo := sampleDeployment("topo-a")
			topo.UID = "topo-uid"
			s, k8s := newServer(t, Config{}, topo)
			if exists {
				s, k8s = newServer(t, Config{}, topo, unowned)
			}

			rec := do(t, s, http.MethodPut, "/api/v1/deployments/topo-a/extra-values",
				extraValuesBody{ValuesYAML: "authentik:\n  log_level: debug\n"})
			requireStatus(t, rec, http.StatusOK)

			var cm corev1.ConfigMap
			if err := k8s.Get(t.Context(), types.NamespacedName{Name: "topo-a-extra-values", Namespace: testNamespace}, &cm); err != nil {
				t.Fatalf("get configmap: %v", err)
			}
			if len(cm.OwnerReferences) != 1 {
				t.Fatalf("ownerReferences = %+v, want exactly the deployment", cm.OwnerReferences)
			}
			owner := cm.OwnerReferences[0]
			if owner.Kind != "AuthentikDeployment" || owner.Name != "topo-a" || owner.UID != "topo-uid" {
				t.Fatalf("owner = %+v, want AuthentikDeployment topo-a (topo-uid)", owner)
			}
		})
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

// A ConfigMap another deployment references as well is written but not
// adopted: owned, it would be deleted with this deployment and leave the
// other one without its extra values.
func TestPutExtraValuesDoesNotAdoptSharedConfigMap(t *testing.T) {
	shared := &corev1.ConfigMap{
		Name: "topo-a-extra-values", Namespace: testNamespace,
		Data: map[string]string{"values.yaml": "authentik:\n  log_level: info\n"},
	}
	topoA := sampleDeployment("topo-a")
	topoA.UID = "topo-a-uid"
	topoB := sampleDeployment("topo-b")
	topoB.Spec.Authentik.ExtraValuesRef = &corev1.LocalObjectReference{Name: shared.Name}
	s, k8s := newServer(t, Config{}, topoA, topoB, shared)

	yamlBody := "authentik:\n  log_level: debug\n"
	rec := do(t, s, http.MethodPut, "/api/v1/deployments/topo-a/extra-values", extraValuesBody{ValuesYAML: yamlBody})
	requireStatus(t, rec, http.StatusOK)

	var cm corev1.ConfigMap
	if err := k8s.Get(t.Context(), types.NamespacedName{Name: shared.Name, Namespace: testNamespace}, &cm); err != nil {
		t.Fatalf("get configmap: %v", err)
	}
	if cm.Data["values.yaml"] != yamlBody {
		t.Fatalf("configmap values.yaml = %q, want %q", cm.Data["values.yaml"], yamlBody)
	}
	if len(cm.OwnerReferences) != 0 {
		t.Fatalf("ownerReferences = %+v, want none while topo-b references it", cm.OwnerReferences)
	}
}

// The garbage collector can delete the ConfigMap between the save reading
// and updating it - one still owned by an earlier deployment of the same
// name. The save then creates it instead of failing.
func TestPutExtraValuesRecreatesConfigMapDeletedDuringSave(t *testing.T) {
	stale := &corev1.ConfigMap{
		Name: "topo-a-extra-values", Namespace: testNamespace,
		Data: map[string]string{"values.yaml": "authentik:\n  log_level: info\n"},
	}
	topo := sampleDeployment("topo-a")
	topo.UID = "topo-uid"
	collected := false
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(topo, stale).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if _, ok := obj.(*corev1.ConfigMap); ok && !collected {
					collected = true
					if err := cl.Delete(ctx, stale.DeepCopy()); err != nil {
						return err
					}
					return apierrors.NewNotFound(corev1.Resource("configmaps"), obj.GetName())
				}
				return cl.Update(ctx, obj, opts...)
			},
		}).
		Build()
	s := New(c, Config{Namespace: testNamespace, Token: testToken})

	yamlBody := "authentik:\n  log_level: debug\n"
	rec := do(t, s, http.MethodPut, "/api/v1/deployments/topo-a/extra-values", extraValuesBody{ValuesYAML: yamlBody})
	requireStatus(t, rec, http.StatusOK)
	if !collected {
		t.Fatal("the ConfigMap was never updated")
	}

	var cm corev1.ConfigMap
	if err := c.Get(t.Context(), types.NamespacedName{Name: stale.Name, Namespace: testNamespace}, &cm); err != nil {
		t.Fatalf("get configmap: %v", err)
	}
	if cm.Data["values.yaml"] != yamlBody {
		t.Fatalf("configmap values.yaml = %q, want %q", cm.Data["values.yaml"], yamlBody)
	}
	if len(cm.OwnerReferences) != 1 || cm.OwnerReferences[0].UID != "topo-uid" {
		t.Fatalf("ownerReferences = %+v, want the deployment", cm.OwnerReferences)
	}
}

// Saving writes the ConfigMap the deployment already references - the one
// GET showed - instead of forking the deployment off it onto its own.
func TestPutExtraValuesWritesReferencedConfigMap(t *testing.T) {
	shared := &corev1.ConfigMap{
		Name: "shared-values", Namespace: testNamespace,
		Data: map[string]string{"values.yaml": "authentik:\n  log_level: info\n", "notes": "kept"},
	}
	topoA := sampleDeployment("topo-a")
	topoA.Spec.Authentik.ExtraValuesRef = &corev1.LocalObjectReference{Name: shared.Name}
	topoB := sampleDeployment("topo-b")
	topoB.Spec.Authentik.ExtraValuesRef = &corev1.LocalObjectReference{Name: shared.Name}
	s, k8s := newServer(t, Config{}, topoA, topoB, shared)

	yamlBody := "authentik:\n  log_level: debug\n"
	rec := do(t, s, http.MethodPut, "/api/v1/deployments/topo-a/extra-values", extraValuesBody{ValuesYAML: yamlBody})
	requireStatus(t, rec, http.StatusOK)

	var cm corev1.ConfigMap
	if err := k8s.Get(t.Context(), types.NamespacedName{Name: shared.Name, Namespace: testNamespace}, &cm); err != nil {
		t.Fatalf("get configmap: %v", err)
	}
	if cm.Data["values.yaml"] != yamlBody || cm.Data["notes"] != "kept" {
		t.Fatalf("configmap data = %v, want values.yaml %q and the other keys kept", cm.Data, yamlBody)
	}
	// Made by hand, so not adopted: deleting topo-a must not delete it.
	if len(cm.OwnerReferences) != 0 {
		t.Fatalf("ownerReferences = %+v, want none", cm.OwnerReferences)
	}
	err := k8s.Get(t.Context(), types.NamespacedName{Name: "topo-a-extra-values", Namespace: testNamespace}, &corev1.ConfigMap{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("get topo-a-extra-values: %v, want NotFound", err)
	}
	var topo benchv1alpha1.AuthentikDeployment
	if err := k8s.Get(t.Context(), types.NamespacedName{Name: "topo-a", Namespace: testNamespace}, &topo); err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	if ref := topo.Spec.Authentik.ExtraValuesRef; ref == nil || ref.Name != shared.Name {
		t.Fatalf("deployment extraValuesRef = %+v, want %s", ref, shared.Name)
	}
}

// A reference to a parent TestCase's content-named ConfigMap (left on a
// deployment no longer controlled by its parent), or to one that doesn't
// exist, is replaced by the deployment's own ConfigMap: the parent's would be
// replaced whenever the parent's extra values change.
func TestPutExtraValuesWritesOwnConfigMapInsteadOfRef(t *testing.T) {
	parentCM := func(mutate func(*corev1.ConfigMap)) *corev1.ConfigMap {
		cm := &corev1.ConfigMap{
			Name: "pr-1-extra-values-0badf00d", Namespace: testNamespace,
			Data: map[string]string{"values.yaml": "authentik:\n  log_level: info\n"},
		}
		mutate(cm)
		return cm
	}
	cases := map[string]*corev1.ConfigMap{
		"labelled by its parent": parentCM(func(cm *corev1.ConfigMap) {
			cm.Labels = map[string]string{benchv1alpha1.LabelParent: "pr-1"}
		}),
		"owned by its parent": parentCM(func(cm *corev1.ConfigMap) {
			cm.OwnerReferences = []metav1.OwnerReference{{
				APIVersion: benchv1alpha1.GroupVersion.String(), Kind: "TestCase",
				Name: "pr-1", UID: "parent-uid", Controller: ptr.To(true),
			}}
		}),
		"missing": nil,
	}
	for name, referenced := range cases {
		t.Run(name, func(t *testing.T) {
			topo := sampleDeployment("topo-a")
			topo.Spec.Authentik.ExtraValuesRef = &corev1.LocalObjectReference{Name: "pr-1-extra-values-0badf00d"}
			objects := []client.Object{topo}
			if referenced != nil {
				objects = append(objects, referenced)
			}
			s, k8s := newServer(t, Config{}, objects...)

			yamlBody := "authentik:\n  log_level: debug\n"
			rec := do(t, s, http.MethodPut, "/api/v1/deployments/topo-a/extra-values", extraValuesBody{ValuesYAML: yamlBody})
			requireStatus(t, rec, http.StatusOK)

			var cm corev1.ConfigMap
			if err := k8s.Get(t.Context(), types.NamespacedName{Name: "topo-a-extra-values", Namespace: testNamespace}, &cm); err != nil {
				t.Fatalf("get topo-a-extra-values: %v", err)
			}
			if cm.Data["values.yaml"] != yamlBody {
				t.Fatalf("configmap values.yaml = %q, want %q", cm.Data["values.yaml"], yamlBody)
			}
			var got benchv1alpha1.AuthentikDeployment
			if err := k8s.Get(t.Context(), types.NamespacedName{Name: "topo-a", Namespace: testNamespace}, &got); err != nil {
				t.Fatalf("get deployment: %v", err)
			}
			if ref := got.Spec.Authentik.ExtraValuesRef; ref == nil || ref.Name != "topo-a-extra-values" {
				t.Fatalf("deployment extraValuesRef = %+v, want topo-a-extra-values", ref)
			}
			if referenced != nil {
				var parent corev1.ConfigMap
				if err := k8s.Get(t.Context(), types.NamespacedName{Name: referenced.Name, Namespace: testNamespace}, &parent); err != nil {
					t.Fatalf("get parent configmap: %v", err)
				}
				if parent.Data["values.yaml"] != referenced.Data["values.yaml"] {
					t.Fatalf("parent configmap values.yaml = %q, want it untouched", parent.Data["values.yaml"])
				}
			}
		})
	}
}
