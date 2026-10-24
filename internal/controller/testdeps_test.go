package controller

import (
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
)

// TestOAuthProviderBlueprintIsValidYAML guards the one thing an envtest
// suite can't (there's no real authentik to apply this against): that the
// generated text is at least well-formed YAML with the shape
// ak apply_blueprint expects (version/entries), and that every value this
// function is given actually ends up in the rendered text - a plain string
// mismatch here would silently register the wrong OAuth2 client against a
// live authentik with no compiler or envtest to catch it.
func TestOAuthProviderBlueprintIsValidYAML(t *testing.T) {
	got := blueprintDocument(oauthProviderEntries("oauth-test-my-case", "client-id-123", "sekrit-secret", "http://oidc-test-client-my-case.default.svc.cluster.local:9009/auth/callback"))

	var parsed map[string]any
	if err := yaml.Unmarshal(got, &parsed); err != nil {
		t.Fatalf("blueprint is not valid YAML: %v\n%s", err, got)
	}
	if parsed["version"] != float64(1) {
		t.Errorf("version = %v, want 1", parsed["version"])
	}
	entries, ok := parsed["entries"].([]any)
	if !ok || len(entries) != 2 {
		t.Fatalf("entries = %v, want a 2-item list (provider, application)", parsed["entries"])
	}

	text := string(got)
	for _, want := range []string{
		"model: authentik_providers_oauth2.oauth2provider",
		"model: authentik_core.application",
		"id: oauth-test-my-case",
		"client_id: client-id-123",
		"client_secret: sekrit-secret",
		"url: http://oidc-test-client-my-case.default.svc.cluster.local:9009/auth/callback",
		"!KeyOf oauth-test-my-case",
		"!Find [authentik_flows.flow, [slug, default-provider-authorization-implicit-consent]]",
		"!Find [authentik_flows.flow, [slug, default-provider-invalidation-flow]]",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("blueprint missing %q\n%s", want, text)
		}
	}
}

// TestDependencyBlueprintCombinesAllEntries checks the fragments
// concatenate into one well-formed blueprint with every model in it.
func TestDependencyBlueprintCombinesAllEntries(t *testing.T) {
	got := blueprintDocument(
		oauthProviderEntries("oauth-test-c-d", "oauth-test-c-d", "secret", "http://client/auth/callback"),
		samlProviderEntries("saml-test-c-d", "http://sp/saml/acs", "http://sp/saml/metadata"),
		oidcSourceEntries("oidc-idp-c-d", "oidc-idp-c-d", "secret", "http://dex/dex"),
		samlSourceEntries("saml-idp-c-d", "http://idp/sso"),
		oauthTokenEntries("oauth-cc-c-d", "secret", "oauth-te-c-d", "secret2"),
	)
	var parsed struct {
		Entries []struct {
			Model string `json:"model"`
		} `json:"entries"`
	}
	if err := yaml.Unmarshal(got, &parsed); err != nil {
		t.Fatalf("blueprint is not valid YAML: %v\n%s", err, got)
	}
	models := make([]string, 0, len(parsed.Entries))
	for _, e := range parsed.Entries {
		models = append(models, e.Model)
	}
	want := []string{
		"authentik_providers_oauth2.oauth2provider", "authentik_core.application",
		"authentik_providers_saml.samlprovider", "authentik_core.application",
		"authentik_sources_oauth.oauthsource",
		"authentik_sources_saml.samlsource",
		"authentik_providers_oauth2.oauth2provider", "authentik_core.application",
		"authentik_providers_oauth2.oauth2provider", "authentik_core.application",
	}
	if strings.Join(models, ",") != strings.Join(want, ",") {
		t.Errorf("models = %v, want %v", models, want)
	}
	for _, want := range []string{
		"acs_url: http://sp/saml/acs",
		"audience: http://sp/saml/metadata",
		"sign_assertion: true",
		"authorization_url: http://dex/dex/auth",
		"access_token_url: http://dex/dex/token",
		"profile_url: http://dex/dex/userinfo",
		"sso_url: http://idp/sso",
		"[slug, default-source-pre-authentication]",
		"- client_credentials",
		`- "urn:ietf:params:oauth:grant-type:token-exchange"`,
		"jwt_federation_providers:\n        - !KeyOf oauth-cc-c-d",
	} {
		if !strings.Contains(string(got), want) {
			t.Errorf("blueprint missing %q\n%s", want, got)
		}
	}
}

func TestDependencyEndpoints(t *testing.T) {
	tc := &benchv1alpha1.TestCase{Name: "c", Namespace: "ns"}
	got := dependencyEndpoints(tc, "d", &benchv1alpha1.TestCaseTemplateSpec{Dependencies: []benchv1alpha1.TestCaseDependency{
		benchv1alpha1.DependencySAMLClient, benchv1alpha1.DependencyOIDCIdP,
	}})
	if got.OIDCClientHost != "" || got.SAMLSourceSlug != "" {
		t.Errorf("unrequested dependencies set: %+v", got)
	}
	if got.SAMLClientHost != "saml-test-sp-c-d.ns.svc.cluster.local" || got.SAMLClientPort != 9009 {
		t.Errorf("saml client = %s:%d", got.SAMLClientHost, got.SAMLClientPort)
	}
	if got.OIDCSourceSlug != "oidc-idp-c-d" {
		t.Errorf("OIDCSourceSlug = %q", got.OIDCSourceSlug)
	}
	got = dependencyEndpoints(tc, "d", &benchv1alpha1.TestCaseTemplateSpec{Dependencies: []benchv1alpha1.TestCaseDependency{
		benchv1alpha1.DependencyOAuthToken,
	}})
	if got.OAuthClientID != "oauth-cc-c-d" || got.OAuthExchangeClientID != "oauth-te-c-d" ||
		got.OAuthSecretName != got.OAuthClientID || got.OAuthExchangeSecretName != got.OAuthExchangeClientID {
		t.Errorf("oauth token clients = %+v", got)
	}
	if empty := dependencyEndpoints(tc, "d", &benchv1alpha1.TestCaseTemplateSpec{}); empty.EnvVars() != nil {
		t.Errorf("no dependencies should render no env vars, got %v", empty.EnvVars())
	}
}
