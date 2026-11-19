package render

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
)

func TestEnvVarsWithoutTenant(t *testing.T) {
	p := K6WorkloadParams{TestCase: &benchv1alpha1.TestCase{Name: "login"}}
	for _, v := range p.EnvVars() {
		if strings.HasPrefix(v.Name, "TENANT_") {
			t.Errorf("%s is rendered without a tenant", v.Name)
		}
	}
}

func TestEnvVarsWithTenant(t *testing.T) {
	p := K6WorkloadParams{
		TestCase: &benchv1alpha1.TestCase{Name: "login"},
		Tenant:   &benchv1alpha1.TenantParams{Seed: 7, Users: 5000, Groups: 100, GroupDepth: 3, GroupsPerUser: 5},
	}
	env := map[string]corev1.EnvVar{}
	for _, v := range p.EnvVars() {
		env[v.Name] = v
	}
	for name, want := range map[string]string{
		"TENANT_SEED":            "7",
		"TENANT_USERS":           "5000",
		"TENANT_GROUPS":          "100",
		"TENANT_GROUP_DEPTH":     "3",
		"TENANT_GROUPS_PER_USER": "5",
	} {
		if got := env[name].Value; got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	password := env["TENANT_PASSWORD"]
	if password.Value != "" || password.ValueFrom == nil || password.ValueFrom.SecretKeyRef == nil {
		t.Fatalf("TENANT_PASSWORD = %+v, want it read from a Secret", password)
	}
	if ref := password.ValueFrom.SecretKeyRef; ref.Name != "bench-tenant-password" || ref.Key != TenantPasswordKey {
		t.Errorf("TENANT_PASSWORD reads %s/%s, want bench-tenant-password/%s", ref.Name, ref.Key, TenantPasswordKey)
	}
	if env["USERS_FILE"].Value != UsersFilePath {
		t.Error("a tenant must not replace the fixture users' env")
	}
}
