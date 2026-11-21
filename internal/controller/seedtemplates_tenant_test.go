package controller

import (
	"strings"
	"testing"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
	"github.com/authentik-labs/akbench/internal/render"
)

// A built-in script that imports the tenant library only works with the
// TENANT_* env vars, which k6 gets only for a template listing seeded-tenant;
// and a template that lists it without using the library would hold a test
// case back for a tenant its script never touches.
func TestBuiltinTemplatesUsingTenantLibraryNeedSeededTenant(t *testing.T) {
	uses := 0
	for _, tmpl := range defaultTestCaseTemplates("default") {
		imports := strings.Contains(tmpl.Spec.Script, `"./`+render.TenantLibFileName+`"`)
		needs := tmpl.Spec.HasDependency(benchv1alpha1.DependencySeededTenant)
		if imports != needs {
			t.Errorf("template %s: imports %s = %v, lists seeded-tenant = %v", tmpl.Name, render.TenantLibFileName, imports, needs)
		}
		if imports {
			uses++
		}
	}
	if uses == 0 {
		t.Errorf("no built-in template imports %s", render.TenantLibFileName)
	}
}
