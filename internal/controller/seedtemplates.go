package controller

import (
	"context"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
	k6scripts "github.com/authentik-labs/akbench/k6/scripts"
)

// DefaultTemplateName is the built-in template TestCaseSpec.Template's own
// CRD default resolves to (see testcase_controller.go's resolveTemplate).
const DefaultTemplateName = "login"

// mustEmbeddedScript reads one of this binary's own embedded k6 scripts
// (k6/scripts/*.ts). A miss here means a built-in template name and its
// ScriptName have drifted out of sync within this same package - a build-time
// programming error, not a runtime condition any caller could recover from.
func mustEmbeddedScript(name string) string {
	c, ok := k6scripts.Get(name)
	if !ok {
		panic(fmt.Sprintf("embedded k6 script %q is missing", name))
	}
	return c
}

// defaultTestCaseTemplates seeds the operator's own built-in k6 test
// templates from the exact same embedded scripts (k6/scripts/*.ts) this
// binary already carries, so every install runs the scripts this operator
// version shipped with (see SeedDefaultTestCaseTemplates).
func defaultTestCaseTemplates(namespace string) []benchv1alpha1.TestCaseTemplate {
	return []benchv1alpha1.TestCaseTemplate{
		{
			Name: DefaultTemplateName, Namespace: namespace,
			Spec: benchv1alpha1.TestCaseTemplateSpec{
				Description: "Walks the authentik flow executor through a standard username/password (+ optional MFA) login.",
				Script:      mustEmbeddedScript("login.ts"),
			},
		},
		{
			Name: "oauth-oidc", Namespace: namespace,
			Spec: benchv1alpha1.TestCaseTemplateSpec{
				Description:  "Drives an OAuth2/OIDC authorization-code flow through an oidc-test-client relying party, reusing the same flow-executor walk as login.",
				Script:       mustEmbeddedScript("oauth.ts"),
				Dependencies: []benchv1alpha1.TestCaseDependency{benchv1alpha1.DependencyOIDCClient},
			},
		},
		{
			Name: "oauth-client-credentials", Namespace: namespace,
			Spec: benchv1alpha1.TestCaseTemplateSpec{
				Description:  "Requests client_credentials tokens straight from authentik's OAuth2 token endpoint, no flow or relying party involved.",
				Script:       mustEmbeddedScript("oauth-token.ts"),
				Dependencies: []benchv1alpha1.TestCaseDependency{benchv1alpha1.DependencyOAuthToken},
			},
		},
		{
			Name: "oauth-token-exchange", Namespace: namespace,
			Spec: benchv1alpha1.TestCaseTemplateSpec{
				Description: "Exchanges a client_credentials access token for another provider's access token (RFC 8693 token exchange) " +
					"at authentik's OAuth2 token endpoint. Each VU reuses its subject token until it expires.",
				Script:       mustEmbeddedScript("oauth-token-exchange.ts"),
				Dependencies: []benchv1alpha1.TestCaseDependency{benchv1alpha1.DependencyOAuthToken},
			},
		},
		{
			Name: "saml", Namespace: namespace,
			Spec: benchv1alpha1.TestCaseTemplateSpec{
				Description:  "Drives an SP-initiated SAML login (HTTP-POST response binding) through a saml-test-sp service provider, reusing the same flow-executor walk as login.",
				Script:       mustEmbeddedScript("saml.ts"),
				Dependencies: []benchv1alpha1.TestCaseDependency{benchv1alpha1.DependencySAMLClient},
			},
		},
		{
			Name: "oidc-source", Namespace: namespace,
			Spec: benchv1alpha1.TestCaseTemplateSpec{
				Description: "Logs in through an authentik OAuth source federating to dex: dex's password form, the callback, " +
					"then the source authentication flow. Every VU is the same dex user, enrolled once in setup().",
				Script:       mustEmbeddedScript("oidc-source.ts"),
				Dependencies: []benchv1alpha1.TestCaseDependency{benchv1alpha1.DependencyOIDCIdP},
			},
		},
		{
			Name: "saml-source", Namespace: namespace,
			Spec: benchv1alpha1.TestCaseTemplateSpec{
				Description: "Logs in through an authentik SAML source federating to saml-test-idp: the IdP's login form, the POSTed " +
					"SAMLResponse, then the source authentication flow. Every VU is the same IdP user, enrolled once in setup().",
				Script:       mustEmbeddedScript("saml-source.ts"),
				Dependencies: []benchv1alpha1.TestCaseDependency{benchv1alpha1.DependencySAMLIdP},
			},
		},
		{
			Name: "mixed-usage", Namespace: namespace,
			Spec: benchv1alpha1.TestCaseTemplateSpec{
				Description: "Mixes logins, logins followed by API browsing, failed logins and flow page loads, " +
					"picked from a PRNG seeded per TestCase (spec.seed) so every run replays the same sequence.",
				Script: mustEmbeddedScript("mixed.ts"),
			},
		},
		{
			Name: "breakpoint", Namespace: namespace,
			Spec: benchv1alpha1.TestCaseTemplateSpec{
				Description: "Test until failure: logs in like login, adding spec.vus VUs every 2 minutes " +
					"until error rate or p95 latency thresholds trip, then aborts. Run as a Quick TestRun.",
				Script: mustEmbeddedScript("breakpoint.ts"),
			},
		},
	}
}

// SeedDefaultTestCaseTemplates creates or updates the operator's built-in
// templates, resetting their spec to the embedded one on every start - an
// operator upgrade always ships its own scripts, and local edits to a
// built-in template are discarded (copy it under another name to keep
// changes). Called once at manager startup, before the manager's own cache
// starts, with a plain uncached client - so any failure here (e.g. the CRD
// not installed yet) surfaces immediately as a clear startup error instead of
// racing the manager's own lifecycle.
func SeedDefaultTestCaseTemplates(ctx context.Context, c client.Client, namespace string) error {
	for _, want := range defaultTestCaseTemplates(namespace) {
		tmpl := &benchv1alpha1.TestCaseTemplate{ObjectMeta: want.ObjectMeta}
		if _, err := controllerutil.CreateOrUpdate(ctx, c, tmpl, func() error {
			tmpl.Spec = want.Spec
			return nil
		}); err != nil {
			return fmt.Errorf("seed TestCaseTemplate %q: %w", want.Name, err)
		}
	}
	return nil
}
