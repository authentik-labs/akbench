package render

import (
	"slices"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
	"sigs.k8s.io/yaml"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
)

func TestDexPasswordHashMatches(t *testing.T) {
	if err := bcrypt.CompareHashAndPassword([]byte(dexPasswordHash), []byte(DexPassword)); err != nil {
		t.Fatalf("dexPasswordHash is not bcrypt(DexPassword): %v", err)
	}
}

func TestDexConfig(t *testing.T) {
	cfg := DexConfig("http://dex.ns.svc.cluster.local:5556/dex", "client", "sekrit", "http://ak/source/oauth/callback/s/")
	var parsed struct {
		Issuer        string `json:"issuer"`
		StaticClients []struct {
			ID           string   `json:"id"`
			Secret       string   `json:"secret"`
			RedirectURIs []string `json:"redirectURIs"`
		} `json:"staticClients"`
		StaticPasswords []struct {
			Email string `json:"email"`
			Hash  string `json:"hash"`
		} `json:"staticPasswords"`
	}
	if err := yaml.Unmarshal([]byte(cfg), &parsed); err != nil {
		t.Fatalf("dex config is not valid YAML: %v\n%s", err, cfg)
	}
	if parsed.Issuer != "http://dex.ns.svc.cluster.local:5556/dex" {
		t.Errorf("issuer = %q", parsed.Issuer)
	}
	if len(parsed.StaticClients) != 1 || parsed.StaticClients[0].ID != "client" || parsed.StaticClients[0].Secret != "sekrit" ||
		len(parsed.StaticClients[0].RedirectURIs) != 1 || parsed.StaticClients[0].RedirectURIs[0] != "http://ak/source/oauth/callback/s/" {
		t.Errorf("staticClients = %+v", parsed.StaticClients)
	}
	if len(parsed.StaticPasswords) != 1 || parsed.StaticPasswords[0].Email != DexEmail || parsed.StaticPasswords[0].Hash != dexPasswordHash {
		t.Errorf("staticPasswords = %+v", parsed.StaticPasswords)
	}
}

func TestDexDeploymentRollsOnConfigChange(t *testing.T) {
	tc := &benchv1alpha1.TestCase{Name: "c", Namespace: "ns"}
	a := DexDeployment(tc, "d", "ns", "a")
	b := DexDeployment(tc, "d", "ns", "b")
	if a.Spec.Template.Annotations[annotationConfigHash] == b.Spec.Template.Annotations[annotationConfigHash] {
		t.Error("a config change must change the pod template")
	}
	if a.Spec.Template.Spec.Volumes[0].Secret.SecretName != DexConfigSecret(tc, "d", "ns", "a").Name {
		t.Error("Deployment must mount DexConfigSecret")
	}
	if DexService(tc, "d", "ns").Spec.Selector[labelKeyApp] != a.Spec.Selector.MatchLabels[labelKeyApp] {
		t.Error("Deployment/Service selector must agree")
	}
}

func TestSAMLTestObjects(t *testing.T) {
	tc := &benchv1alpha1.TestCase{Name: "c", Namespace: "ns"}
	sp := SAMLTestSPDeployment(tc, "d", "ns", "http://ak/application/saml/x/metadata/", "entity")
	idp := SAMLTestIdPDeployment(tc, "d", "ns", "http://ak/source/saml/y/metadata/")
	for name, want := range map[string]map[string]string{
		sp.Name: {
			"SP_METADATA_URL": "http://ak/application/saml/x/metadata/",
			"SP_ENTITY_ID":    "entity",
			"SP_ROOT_URL":     "http://saml-test-sp-c-d.ns.svc.cluster.local:9009",
		},
		idp.Name: {
			"IDP_METADATA_URL": "http://ak/source/saml/y/metadata/",
			"IDP_ROOT_URL":     "http://saml-test-idp-c-d.ns.svc.cluster.local:9009",
		},
	} {
		dep := sp
		if name == idp.Name {
			dep = idp
		}
		got := map[string]string{}
		for _, e := range dep.Spec.Template.Spec.Containers[0].Env {
			got[e.Name] = e.Value
		}
		for k, v := range want {
			if got[k] != v {
				t.Errorf("%s: %s = %q, want %q", name, k, got[k], v)
			}
		}
	}
	if !strings.HasPrefix(SAMLTestSPService(tc, "d", "ns").Name, "saml-test-sp-") {
		t.Error("unexpected Service name")
	}
}

func TestDependencyEnvVars(t *testing.T) {
	got := map[string]string{}
	for _, e := range (DependencyEndpoints{SAMLClientHost: "sp", SAMLClientPort: 9009, SAMLSourceSlug: "s"}).EnvVars() {
		got[e.Name] = e.Value
	}
	want := map[string]string{
		"SAML_TEST_SP_HOST":    "sp",
		"SAML_TEST_SP_PORT":    "9009",
		"SAML_IDP_SOURCE_SLUG": "s",
		"SAML_IDP_USERNAME":    SAMLTestIdPUsername,
		"SAML_IDP_PASSWORD":    SAMLTestIdPPassword,
	}
	if len(got) != len(want) {
		t.Errorf("env = %v, want exactly %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}

func envNames(d DependencyEndpoints) []string {
	vars := d.EnvVars()
	names := make([]string, 0, len(vars))
	for _, e := range vars {
		names = append(names, e.Name)
	}
	return names
}

// The oauth-token variables keep their order whatever else is listed: a
// reordered env is a changed pod template, which would roll every running
// oauth-token burn when the operator is upgraded.
func TestDependencyEnvVarsOAuthTokenOrderIsStable(t *testing.T) {
	token := DependencyEndpoints{
		OAuthClientID: "cc", OAuthSecretName: "cc", OAuthExchangeClientID: "te", OAuthExchangeSecretName: "te",
	}
	want := []string{
		"OAUTH_CLIENT_ID", "OAUTH_CLIENT_SECRET", "OAUTH_REDIRECT_URI",
		"OAUTH_EXCHANGE_CLIENT_ID", "OAUTH_EXCHANGE_CLIENT_SECRET",
	}
	if got := envNames(token); !slices.Equal(got, want) {
		t.Errorf("oauth-token env = %v, want %v", got, want)
	}

	both := token
	both.OAuthPoliciesClientID, both.OAuthPoliciesSecretName = "pol", "pol"
	wantBoth := slices.Concat(want, []string{"OAUTH_POLICIES_CLIENT_ID", "OAUTH_POLICIES_CLIENT_SECRET"})
	if got := envNames(both); !slices.Equal(got, wantBoth) {
		t.Errorf("oauth-token and oauth-policies env = %v, want %v (one OAUTH_REDIRECT_URI)", got, wantBoth)
	}
}

func TestDependencyEnvVarsOAuthPolicies(t *testing.T) {
	d := DependencyEndpoints{OAuthPoliciesClientID: "pol", OAuthPoliciesSecretName: "pol-secret"}
	want := []string{"OAUTH_POLICIES_CLIENT_ID", "OAUTH_POLICIES_CLIENT_SECRET", "OAUTH_REDIRECT_URI"}
	if got := envNames(d); !slices.Equal(got, want) {
		t.Fatalf("oauth-policies env = %v, want %v", got, want)
	}
	vars := d.EnvVars()
	if vars[0].Value != "pol" {
		t.Errorf("OAUTH_POLICIES_CLIENT_ID = %q, want pol", vars[0].Value)
	}
	ref := vars[1].ValueFrom
	if ref == nil || ref.SecretKeyRef == nil || ref.SecretKeyRef.Name != "pol-secret" || ref.SecretKeyRef.Key != OAuthClientSecretKey {
		t.Errorf("OAUTH_POLICIES_CLIENT_SECRET source = %+v", ref)
	}
	if vars[2].Value != OAuthRedirectURI {
		t.Errorf("OAUTH_REDIRECT_URI = %q, want %q", vars[2].Value, OAuthRedirectURI)
	}
}
