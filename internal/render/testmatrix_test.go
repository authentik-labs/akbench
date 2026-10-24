package render

import (
	"testing"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
)

func TestTestMatrixBuildsOneTestCaseAndDeploymentPairPerTemplate(t *testing.T) {
	m := &benchv1alpha1.TestMatrix{
		Name: "pr-1", Namespace: "bench",
		Labels: map[string]string{"bench.goauthentik.io/pull-request": "1"},
		Spec: benchv1alpha1.TestMatrixSpec{
			Variants: []benchv1alpha1.TestMatrixVariant{
				{Name: "pr", Tag: "gh-abc"},
				{Name: "2026-6-0", Image: "ghcr.io/goauthentik/server", Tag: "2026.6.0"},
			},
			Baseline:    "2026-6-0",
			Templates:   []string{"login", "oauth-oidc"},
			Deployment:  benchv1alpha1.AuthentikDeploymentSpec{Authentik: benchv1alpha1.AuthentikSpec{Image: "ghcr.io/goauthentik/dev-server"}},
			ExtraValues: "authentik:\n  log_level: debug\n",
			TestCase:    benchv1alpha1.TestCaseSpec{Profile: benchv1alpha1.ProfileBurn, VUs: 4, Template: "ignored"},
		},
	}

	out, err := TestMatrix(m)
	if err != nil {
		t.Fatal(err)
	}

	if len(out.TestCases) != 2 || len(out.Deployments) != 4 {
		t.Fatalf("got %d test cases and %d deployments, want 2 and 4", len(out.TestCases), len(out.Deployments))
	}
	if out.ExtraValues == nil {
		t.Fatal("want an extra-values ConfigMap")
	}

	tc := out.TestCases[1]
	if tc.Name != "pr-1-oauth-oidc" || tc.Spec.Template != "oauth-oidc" || tc.Spec.VUs != 4 {
		t.Errorf("test case = %s template=%s vus=%d", tc.Name, tc.Spec.Template, tc.Spec.VUs)
	}
	if got := tc.Spec.Deployments(); len(got) != 2 || got[0].Name != "pr-1-pr-oauth-oidc" || got[1].Name != "pr-1-2026-6-0-oauth-oidc" {
		t.Errorf("deploymentRefs = %v", got)
	}
	if tc.Spec.BaselineRef.Name != "pr-1-2026-6-0-oauth-oidc" {
		t.Errorf("baselineRef = %s", tc.Spec.BaselineRef.Name)
	}
	if tc.Labels[benchv1alpha1.LabelTestMatrix] != "pr-1" || tc.Labels["bench.goauthentik.io/pull-request"] != "1" {
		t.Errorf("labels = %v", tc.Labels)
	}

	pr, release := out.Deployments[0], out.Deployments[1]
	if pr.Spec.Namespace != pr.Name || pr.Spec.Authentik.Image != "ghcr.io/goauthentik/dev-server" || pr.Spec.Authentik.Tag != "gh-abc" {
		t.Errorf("pr deployment = %s ns=%s %s:%s", pr.Name, pr.Spec.Namespace, pr.Spec.Authentik.Image, pr.Spec.Authentik.Tag)
	}
	if release.Spec.Authentik.Image != "ghcr.io/goauthentik/server" || release.Spec.Authentik.Tag != "2026.6.0" {
		t.Errorf("release deployment runs %s:%s", release.Spec.Authentik.Image, release.Spec.Authentik.Tag)
	}
	if ref := pr.Spec.Authentik.ExtraValuesRef; ref == nil || ref.Name != out.ExtraValues.Name {
		t.Errorf("extraValuesRef = %v, want %s", ref, out.ExtraValues.Name)
	}

	m.Spec.ExtraValues = "authentik:\n  log_level: info\n"
	if changed, _ := TestMatrix(m); changed.ExtraValues.Name == out.ExtraValues.Name {
		t.Error("extra-values ConfigMap name didn't change with its content")
	}

	m.Name = "a-matrix-name-long-enough-to-overflow"
	if _, err := TestMatrix(m); err == nil {
		t.Error("want an error for a deployment name past Helm's release name limit")
	}
}
