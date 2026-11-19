package contract

import (
	"testing"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
	"github.com/authentik-labs/akbench/internal/render"
)

// wantTenantEnvNames are the env vars a script using the seeded-tenant
// dependency reads to work out the tenant's users and groups and log in as
// them. Rendered only for such a template, on top of wantEnvNames.
var wantTenantEnvNames = []string{
	"TENANT_SEED",
	"TENANT_USERS",
	"TENANT_GROUPS",
	"TENANT_GROUP_DEPTH",
	"TENANT_GROUPS_PER_USER",
	"TENANT_PASSWORD",
}

func TestK6TenantEnvVarNames(t *testing.T) {
	p := params(exampleTestCase(benchv1alpha1.ProfileQuick))
	p.Tenant = &benchv1alpha1.TenantParams{Seed: 1, Users: 50, Groups: 20, GroupDepth: 2, GroupsPerUser: 3}
	vars := p.EnvVars()
	got := make([]string, 0, len(vars))
	for _, e := range vars {
		got = append(got, e.Name)
	}
	want := append(append([]string(nil), wantEnvNames...), wantTenantEnvNames...)
	assertSameSet(t, "env var names with a tenant", want, got)

	values := envValueMap(vars)
	if users, perUser := values["TENANT_USERS"], values["TENANT_GROUPS_PER_USER"]; users != "50" || perUser != "3" {
		t.Errorf("TENANT_USERS/TENANT_GROUPS_PER_USER = %q/%q, want 50/3", users, perUser)
	}
	for _, e := range vars {
		if e.Name != "TENANT_PASSWORD" {
			continue
		}
		if e.ValueFrom == nil || e.ValueFrom.SecretKeyRef == nil || e.ValueFrom.SecretKeyRef.Name == "" ||
			e.ValueFrom.SecretKeyRef.Key != render.TenantPasswordKey {
			t.Errorf("TENANT_PASSWORD = %+v, want it read from the %q key of a named Secret", e, render.TenantPasswordKey)
		}
	}
}
