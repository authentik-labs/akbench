package controller

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
)

func TestSeedDefaultTenantPresetsResetsEditedPresetAndKeepsOthers(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := benchv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	edited := &benchv1alpha1.TenantPreset{
		Name: "small", Namespace: "default",
		Labels: map[string]string{benchv1alpha1.LabelBuiltIn: "true"},
		Spec:   benchv1alpha1.TenantPresetSpec{Seed: 9, Users: 7, GroupDepth: 1},
	}
	custom := &benchv1alpha1.TenantPreset{
		Name: "ours", Namespace: "default",
		Spec: benchv1alpha1.TenantPresetSpec{Seed: 1, Users: 1234, Groups: 10, GroupDepth: 2, GroupsPerUser: 2},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(edited, custom).Build()

	if err := SeedDefaultTenantPresets(t.Context(), c, "default"); err != nil {
		t.Fatal(err)
	}
	var got benchv1alpha1.TenantPreset
	if err := c.Get(t.Context(), types.NamespacedName{Name: "small", Namespace: "default"}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Spec.Users != 5000 || got.Spec.Seed != 1 {
		t.Errorf("edited built-in preset kept users=%d seed=%d, want the built-in 5000 and 1", got.Spec.Users, got.Spec.Seed)
	}
	if got.Labels[benchv1alpha1.LabelBuiltIn] != "true" {
		t.Errorf("built-in preset has labels %v, want it marked built-in", got.Labels)
	}
	if err := c.Get(t.Context(), types.NamespacedName{Name: "ours", Namespace: "default"}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Spec != custom.Spec || got.Labels[benchv1alpha1.LabelBuiltIn] != "" {
		t.Errorf("custom preset was changed to %+v with labels %v, want it untouched", got.Spec, got.Labels)
	}
	var all benchv1alpha1.TenantPresetList
	if err := c.List(t.Context(), &all); err != nil {
		t.Fatal(err)
	}
	if want := len(defaultTenantPresets("default")) + 1; len(all.Items) != want {
		t.Errorf("found %d presets, want %d", len(all.Items), want)
	}
}

// Whether the built-in presets satisfy the schema is checked against a real
// API server (tenantpreset_schema_test.go). This covers what the schema does
// not: that they can be told apart, and read in order of size.
func TestDefaultTenantPresetsAreDistinctAndOrdered(t *testing.T) {
	seen := map[string]bool{}
	var previous int32
	for _, p := range defaultTenantPresets("default") {
		if seen[p.Name] {
			t.Errorf("preset %q is defined twice", p.Name)
		}
		seen[p.Name] = true
		if p.Spec.Description == "" {
			t.Errorf("preset %q has no description", p.Name)
		}
		if p.Spec.Users <= previous {
			t.Errorf("preset %q has %d users, want more than the %d of the one before it", p.Name, p.Spec.Users, previous)
		}
		previous = p.Spec.Users
	}
}

func TestSeedDefaultTenantPresetsKeepsUserPresetWithBuiltInName(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := benchv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	// As if "medium" became a built-in only in a later release, after
	// someone had already created a preset of that name.
	theirs := &benchv1alpha1.TenantPreset{
		Name: "medium", Namespace: "default",
		Labels: map[string]string{"team": "perf"},
		Spec:   benchv1alpha1.TenantPresetSpec{Description: "Ours", Seed: 3, Users: 777, Groups: 7, GroupDepth: 2, GroupsPerUser: 1},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(theirs).Build()

	if err := SeedDefaultTenantPresets(t.Context(), c, "default"); err != nil {
		t.Fatal(err)
	}
	var got benchv1alpha1.TenantPreset
	if err := c.Get(t.Context(), types.NamespacedName{Name: "medium", Namespace: "default"}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Spec != theirs.Spec || got.Labels[benchv1alpha1.LabelBuiltIn] != "" || got.Labels["team"] != "perf" {
		t.Errorf("preset someone created was changed to %+v with labels %v, want it untouched", got.Spec, got.Labels)
	}
	// The other built-ins are still seeded.
	if err := c.Get(t.Context(), types.NamespacedName{Name: "large", Namespace: "default"}, &got); err != nil {
		t.Fatal(err)
	}
}

func TestSeedDefaultTenantPresetsUnlabelsDroppedBuiltIn(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := benchv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	// Built-ins of an earlier release that this one no longer ships.
	spec := benchv1alpha1.TenantPresetSpec{Seed: 1, Users: 2000, Groups: 60, GroupDepth: 3, GroupsPerUser: 4}
	dropped := &benchv1alpha1.TenantPreset{
		Name: "xl", Namespace: "default",
		Labels: map[string]string{benchv1alpha1.LabelBuiltIn: "true", "team": "perf"},
		Spec:   spec,
	}
	elsewhere := &benchv1alpha1.TenantPreset{
		Name: "xl", Namespace: "other",
		Labels: map[string]string{benchv1alpha1.LabelBuiltIn: "true"},
		Spec:   spec,
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(dropped, elsewhere).Build()

	if err := SeedDefaultTenantPresets(t.Context(), c, "default"); err != nil {
		t.Fatal(err)
	}
	var got benchv1alpha1.TenantPreset
	if err := c.Get(t.Context(), types.NamespacedName{Name: "xl", Namespace: "default"}, &got); err != nil {
		t.Fatalf("dropped built-in preset was deleted, want it kept: %v", err)
	}
	if _, ok := got.Labels[benchv1alpha1.LabelBuiltIn]; ok || got.Labels["team"] != "perf" || got.Spec != spec {
		t.Errorf("dropped built-in preset has labels %v and spec %+v, want only the built-in label gone", got.Labels, got.Spec)
	}
	if err := c.Get(t.Context(), types.NamespacedName{Name: "xl", Namespace: "other"}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Labels[benchv1alpha1.LabelBuiltIn] != "true" {
		t.Errorf("preset in another namespace has labels %v, want it untouched", got.Labels)
	}
	// The current built-ins keep their label.
	if err := c.Get(t.Context(), types.NamespacedName{Name: "tiny", Namespace: "default"}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Labels[benchv1alpha1.LabelBuiltIn] != "true" {
		t.Errorf("built-in preset has labels %v, want it marked built-in", got.Labels)
	}
}

// main logs this error and carries on rather than exiting: an install whose
// CRDs predate TenantPreset must keep its operator running. The fake client
// does not consult its RESTMapper, so the API server's answer is stood in for.
func TestSeedDefaultTenantPresetsWithoutCRD(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := benchv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return &meta.NoKindMatchError{GroupKind: benchv1alpha1.GroupVersion.WithKind("TenantPreset").GroupKind()}
		},
	}).Build()

	err := SeedDefaultTenantPresets(t.Context(), c, "default")
	if !meta.IsNoMatchError(err) {
		t.Errorf("want the no-match error passed on when TenantPreset is not served, got %v", err)
	}
}
