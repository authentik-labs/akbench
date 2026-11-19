package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
	"github.com/authentik-labs/akbench/internal/render"
)

// helmRelease is the HelmRelease the operator applies for d, carrying values.
func helmRelease(d *benchv1alpha1.AuthentikDeployment, values map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "helm.toolkit.fluxcd.io/v2",
		"kind":       "HelmRelease",
		"metadata":   map[string]any{"name": render.HelmReleaseName(d), "namespace": d.Namespace},
		"spec":       map[string]any{"values": values},
	}}
}

// externalDatabase is an external database of d's own, named unlike d.
func externalDatabase(d *benchv1alpha1.AuthentikDeployment) ConfigDatabase {
	sum := sha256.Sum256([]byte(d.Name))
	id := hex.EncodeToString(sum[:4])
	return ConfigDatabase{Host: "db-" + id + ".example.com", Port: "5432", Name: "authentik_" + id, User: "bench_" + id}
}

// postgresConnection is what the operator resolves for d's postgres mode:
// credentials generated per deployment, CNPG's read-write service, or an
// external database of d's own.
func postgresConnection(d *benchv1alpha1.AuthentikDeployment) *render.PostgresConnection {
	switch d.Spec.Postgres.Mode {
	case benchv1alpha1.PostgresCNPG:
		return &render.PostgresConnection{Host: render.CNPGClusterName(d) + "-rw", Port: 5432, Database: "authentik", User: "authentik", Password: "cnpg-" + d.Name}
	case benchv1alpha1.PostgresExternal:
		db := externalDatabase(d)
		return &render.PostgresConnection{Host: db.Host, Port: 5432, Database: db.Name, User: db.User, Password: "ext-" + d.Name}
	default:
		return &render.PostgresConnection{Password: "pg-" + d.Name}
	}
}

// renderedValues is d's Helm values as the operator renders them, extra
// values merged in.
func renderedValues(t *testing.T, d *benchv1alpha1.AuthentikDeployment, extraValues string) map[string]any {
	t.Helper()
	raw, err := render.HelmValues(d, nil, "secret-key-"+d.Name, postgresConnection(d), "pyroscope.monitoring:4040", "tempo.monitoring:4317")
	if err != nil {
		t.Fatal(err)
	}
	if extraValues != "" {
		if raw, err = render.MergeExtraValues(raw, extraValues); err != nil {
			t.Fatal(err)
		}
	}
	var values map[string]any
	if err := json.Unmarshal(raw.Raw, &values); err != nil {
		t.Fatal(err)
	}
	return values
}

func renderedConfig(t *testing.T, d *benchv1alpha1.AuthentikDeployment, extraValues string) deploymentConfig {
	t.Helper()
	return newDeploymentConfig(d, renderedValues(t, d, extraValues))
}

func requireNoDifferences(t *testing.T, what string, diff []ConfigDifference) {
	t.Helper()
	for _, d := range diff {
		t.Errorf("%s: unexpected difference %s: %v", what, d.Path, d.Values)
	}
}

var postgresModes = map[string]benchv1alpha1.PostgresSpec{
	"Embedded":     {Mode: benchv1alpha1.PostgresEmbedded},
	"Embedded TLS": {Mode: benchv1alpha1.PostgresEmbedded, Embedded: &benchv1alpha1.EmbeddedPostgresSpec{TLS: true}},
	"CNPG":         {Mode: benchv1alpha1.PostgresCNPG, CNPG: &benchv1alpha1.CNPGPostgresSpec{Instances: 2}},
	"External":     {Mode: benchv1alpha1.PostgresExternal, External: &benchv1alpha1.ExternalPostgresSpec{SecretRef: corev1.LocalObjectReference{Name: "external-db"}}},
}

// A parent TestCase's children, rendered by the operator's own functions for
// every postgres mode, compare clean - names long enough for BoundedName to
// hash included - and an External pair reports its databases instead.
func TestTestCaseConfigDiffRenderedChildrenMatch(t *testing.T) {
	for mode, postgres := range postgresModes {
		t.Run(mode, func(t *testing.T) {
			parent := &benchv1alpha1.TestCase{
				Name: "pr-12345-benchmark", Namespace: testNamespace,
				Spec: benchv1alpha1.TestCaseSpec{
					Variants: []benchv1alpha1.TestCaseVariant{
						{Name: "pr", Tag: "gh-abc"},
						{Name: "release-2026-6-0", Image: "ghcr.io/goauthentik/server", Tag: "2026.6.0"},
					},
					BaselineVariant: "release-2026-6-0",
					Templates:       []string{"login", "oauth-oidc"},
					Deployment: &benchv1alpha1.AuthentikDeploymentSpec{
						ClusterRef: corev1.LocalObjectReference{Name: "bench"},
						Server:     benchv1alpha1.WorkloadSpec{Replicas: 2},
						Worker:     benchv1alpha1.WorkloadSpec{Replicas: 1},
						Placement:  benchv1alpha1.PlacementSpec{Mode: benchv1alpha1.PlacementDedicated},
						Postgres:   postgres,
						Authentik:  benchv1alpha1.AuthentikSpec{Image: "ghcr.io/goauthentik/dev-server", LogLevel: "info"},
					},
					ExtraValues: "server:\n  automountServiceAccountToken: false\n",
					Profile:     benchv1alpha1.ProfileBurn,
				},
			}
			children, err := render.TestCaseChildren(parent)
			if err != nil {
				t.Fatal(err)
			}
			objects := []client.Object{}
			for _, tc := range children.TestCases {
				objects = append(objects, tc)
			}
			for _, d := range children.Deployments {
				objects = append(objects, d, helmRelease(d, renderedValues(t, d, parent.Spec.ExtraValues)))
			}
			s, _ := newServer(t, Config{}, objects...)

			for _, tc := range children.TestCases {
				rec := do(t, s, http.MethodGet, "/api/v1/testcases/"+tc.Name+"/config-diff", nil)
				requireStatus(t, rec, http.StatusOK)
				got := decodeBody[ConfigDiff](t, rec)
				if len(got.Targets) != 2 || len(got.Pending) != 0 {
					t.Fatalf("%s: targets %v, pending %v", tc.Name, got.Targets, got.Pending)
				}
				requireNoDifferences(t, tc.Name, got.Differences)
				if postgres.Mode != benchv1alpha1.PostgresExternal {
					if got.Databases != nil {
						t.Errorf("%s: databases %v without an external one", tc.Name, got.Databases)
					}
					continue
				}
				for _, d := range children.Deployments {
					name := d.Name
					if !slices.Contains(got.Targets, name) {
						continue
					}
					if want := externalDatabase(d); got.Databases[name] != want {
						t.Errorf("%s: database of %s = %+v, want %+v", tc.Name, name, got.Databases[name], want)
					}
				}
				if strings.Contains(rec.Body.String(), "ext-") {
					t.Errorf("%s: an external database password was returned: %s", tc.Name, rec.Body.String())
				}
			}
		})
	}
}

// deploymentPair is two deployments as one test case compares them: the same
// spec under their own names, namespaces and target names.
type deploymentPair struct {
	a, b                   string
	aNamespace, bNamespace string
	aTarget, bTarget       string
}

func (p deploymentPair) deployments(postgres benchv1alpha1.PostgresSpec) (*benchv1alpha1.AuthentikDeployment, *benchv1alpha1.AuthentikDeployment) {
	deployment := func(name, namespace, target, tag string) *benchv1alpha1.AuthentikDeployment {
		return &benchv1alpha1.AuthentikDeployment{
			Name: name, Namespace: testNamespace,
			Spec: benchv1alpha1.AuthentikDeploymentSpec{
				Namespace:  namespace,
				TargetName: target,
				Server:     benchv1alpha1.WorkloadSpec{Replicas: 2},
				Worker:     benchv1alpha1.WorkloadSpec{Replicas: 1},
				Placement:  benchv1alpha1.PlacementSpec{Mode: benchv1alpha1.PlacementPacked},
				Postgres:   *postgres.DeepCopy(),
				Authentik:  benchv1alpha1.AuthentikSpec{Image: "ghcr.io/goauthentik/server", Tag: tag},
			},
		}
	}
	return deployment(p.a, p.aNamespace, p.aTarget, "2026.6.0"), deployment(p.b, p.bNamespace, p.bTarget, "gh-abc")
}

// Short and common names that occur inside other words and values of the
// rendered configuration: "pr" in "primary", "process_idle" and
// "matchExpressions", "test" in the x-benchmark-test header, "authentik" as a
// namespace and as a chart key and database name.
var deploymentPairs = map[string]deploymentPair{
	"main/pr in authentik/authentik-pr": {a: "main", b: "pr", aNamespace: "authentik", bNamespace: "authentik-pr"},
	"main/pr":                           {a: "main", b: "pr", aNamespace: "main", bNamespace: "pr"},
	"baseline/test":                     {a: "baseline", b: "test", aNamespace: "baseline", bNamespace: "test"},
	"target names":                      {a: "bench-main", b: "bench-pr", aNamespace: "ak-main", bNamespace: "ak-pr", aTarget: "main", bTarget: "pr"},
	"hashed names": {
		a: "a-deployment-name-long-enough-to-be-hashed-main", aNamespace: "a-deployment-name-long-enough-to-be-hashed-main",
		b: "a-deployment-name-long-enough-to-be-hashed-pr", bNamespace: "a-deployment-name-long-enough-to-be-hashed-pr",
	},
}

func TestConfigDiffRenderedPairsMatch(t *testing.T) {
	for pairName, pair := range deploymentPairs {
		for mode, postgres := range postgresModes {
			a, b := pair.deployments(postgres)
			diff := diffConfig([]string{a.Name, b.Name}, map[string]deploymentConfig{
				a.Name: renderedConfig(t, a, ""),
				b.Name: renderedConfig(t, b, ""),
			})
			requireNoDifferences(t, pairName+", "+mode, diff)
		}
	}
}

// Error reporting left at its defaults on one side and spelled out on the
// other renders the same values, so it is the same setting.
func TestConfigDiffComparesRenderedValuesNotSpelling(t *testing.T) {
	a, b := deploymentPairs["main/pr"].deployments(postgresModes["Embedded"])
	b.Spec.Authentik.ErrorReporting = benchv1alpha1.ErrorReportingSpec{Enabled: ptr.To(true), SampleRate: "0.1"}
	b.Spec.Chart.Repo = "https://charts.goauthentik.io"
	diff := diffConfig([]string{"main", "pr"}, map[string]deploymentConfig{
		"main": renderedConfig(t, a, ""),
		"pr":   renderedConfig(t, b, "server:\n  podAnnotations: {}\n"),
	})
	requireNoDifferences(t, "defaults spelled out", diff)
}

// A real difference still shows, once, under the values it changes, on
// pairs whose names alone would compare clean.
func TestConfigDiffReportsRealDifferences(t *testing.T) {
	for pairName, pair := range deploymentPairs {
		a, b := pair.deployments(postgresModes["Embedded"])
		b.Spec.Server.Replicas = 3
		diff := diffConfig([]string{a.Name, b.Name}, map[string]deploymentConfig{
			a.Name: renderedConfig(t, a, ""),
			b.Name: renderedConfig(t, b, "authentik:\n  log_level: debug\n"),
		})
		want := []ConfigDifference{
			{Path: "values.authentik.log_level", Values: map[string]string{b.Name: "debug"}},
			{Path: "values.server.replicas", Values: map[string]string{a.Name: "2", b.Name: "3"}},
		}
		if !reflect.DeepEqual(diff, want) {
			t.Errorf("%s: got %+v, want %+v", pairName, diff, want)
		}
	}
}

// Settings named after credentials without holding one are compared and
// shown like any other; credentials are compared without being shown.
func TestConfigDiffSensitiveWords(t *testing.T) {
	external := postgresModes["External"]
	a, b := deploymentPairs["main/pr"].deployments(external)
	b.Spec.Postgres.External.SecretRef.Name = "other-db"
	shared := "global:\n  env:\n    - {name: AUTHENTIK_BOOTSTRAP_PASSWORD, value: same}\n"
	diff := diffConfig([]string{"main", "pr"}, map[string]deploymentConfig{
		"main": renderedConfig(t, a, shared+
			"  envFrom:\n    - secretRef: {name: main-env}\n"+
			"server:\n  automountServiceAccountToken: true\n"+
			"geoip:\n  licenseKey: license-a\n"+
			"worker:\n  env:\n    - {name: AUTHENTIK_BOOTSTRAP_TOKEN, value: token-a}\n"),
		"pr": renderedConfig(t, b, shared+
			"  envFrom:\n    - secretRef: {name: other-env}\n"+
			"server:\n  automountServiceAccountToken: false\n"+
			"geoip:\n  licenseKey: license-b\n"+
			"worker:\n  env:\n    - {name: AUTHENTIK_BOOTSTRAP_TOKEN, value: token-b}\n"),
	})
	want := []ConfigDifference{
		{Path: "spec.postgres.external.secretRef.name", Values: map[string]string{"main": "external-db", "pr": "other-db"}},
		{Path: "values.geoip.licenseKey", Values: map[string]string{"main": configSet, "pr": configSet}, Note: configCredentialNote},
		{Path: "values.global.envFrom[0].secretRef.name", Values: map[string]string{"main": "main-env", "pr": "other-env"}},
		{Path: "values.server.automountServiceAccountToken", Values: map[string]string{"main": "true", "pr": "false"}},
		{Path: "values.worker.env[AUTHENTIK_BOOTSTRAP_TOKEN].value", Values: map[string]string{"main": configSet, "pr": configSet}, Note: configCredentialNote},
	}
	if !reflect.DeepEqual(diff, want) {
		t.Fatalf("got %+v\nwant %+v", diff, want)
	}
}

func TestConfigCredential(t *testing.T) {
	flat := map[string]string{
		"values.server.env[AUTHENTIK_POSTGRESQL__PASSWORD].name": "AUTHENTIK_POSTGRESQL__PASSWORD",
		"values.server.env[AUTHENTIK_LOG_LEVEL].name":            "AUTHENTIK_LOG_LEVEL",
		"values.server.env[3].name":                              "AUTHENTIK_SECRET_KEY",
		"values.server.tolerations[0].key":                       "pool",
	}
	for path, want := range map[string]bool{
		"values.authentik.email.password":                         true,
		"values.postgresql.auth.postgresPassword":                 true,
		"values.authentik.secret_key":                             true,
		"values.geoip.licenseKey":                                 true,
		"values.authentik.bootstrap_token":                        true,
		"values.server.env[AUTHENTIK_POSTGRESQL__PASSWORD].value": true,
		"values.server.env[3].value":                              true,
		"values.server.env[AUTHENTIK_LOG_LEVEL].value":            false,
		"values.server.tolerations[0].value":                      false,
		"values.server.automountServiceAccountToken":              false,
		"values.global.envFrom[0].secretRef.name":                 false,
		"values.authentik.existingSecret.secretName":              false,
		"spec.postgres.external.secretRef.name":                   false,
	} {
		if got := configCredential(path, flat); got != want {
			t.Errorf("configCredential(%q) = %v, want %v", path, got, want)
		}
	}
}

// Two deployments that differ only by design, plus one setting that reached
// only one of them: that setting is the one difference reported, a
// deployment that is gone or not installed yet is pending, and generated
// credentials are neither compared nor returned.
func TestTestCaseConfigDiff(t *testing.T) {
	tc := &benchv1alpha1.TestCase{
		Name: "pr-1-login", Namespace: testNamespace,
		Spec: benchv1alpha1.TestCaseSpec{
			DeploymentRefs: []corev1.LocalObjectReference{{Name: "pr-1-head"}, {Name: "pr-1-main"}, {Name: "pr-1-gone"}, {Name: "pr-1-new"}},
			Profile:        benchv1alpha1.ProfileBurn,
		},
	}
	head, main := sampleDeployment("pr-1-head"), sampleDeployment("pr-1-main")
	s, _ := newServer(t, Config{}, tc, head, main, sampleDeployment("pr-1-new"),
		helmRelease(head, renderedValues(t, head, "authentik:\n  postgresql:\n    use_pool: true\n")),
		helmRelease(main, renderedValues(t, main, "")))

	rec := do(t, s, http.MethodGet, "/api/v1/testcases/pr-1-login/config-diff", nil)
	requireStatus(t, rec, http.StatusOK)
	got := decodeBody[ConfigDiff](t, rec)
	want := ConfigDiff{
		TestID:  "pr-1-login",
		Targets: []string{"pr-1-head", "pr-1-main"},
		Pending: []ConfigPending{
			{Name: "pr-1-gone", Reason: "the AuthentikDeployment does not exist"},
			{Name: "pr-1-new", Reason: "it has no HelmRelease yet"},
		},
		Differences: []ConfigDifference{{
			Path:   "values.authentik.postgresql.use_pool",
			Values: map[string]string{"pr-1-head": "true"},
		}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if body := rec.Body.String(); strings.Contains(body, "secret-key-") || strings.Contains(body, "pg-pr-1") {
		t.Fatalf("a credential was returned: %s", body)
	}
}

// A deployment whose HelmRelease can't be read for another reason than not
// existing - the kind isn't installed, the server may not read it - is
// pending with that reason, and the others are still compared.
func TestTestCaseConfigDiffUnreadableHelmRelease(t *testing.T) {
	tc := &benchv1alpha1.TestCase{
		Name: "pr-1-login", Namespace: testNamespace,
		Spec: benchv1alpha1.TestCaseSpec{
			DeploymentRefs: []corev1.LocalObjectReference{{Name: "a"}, {Name: "b"}, {Name: "forbidden"}, {Name: "nokind"}},
			Profile:        benchv1alpha1.ProfileBurn,
		},
	}
	objects := make([]client.Object, 0, 1+2*len(tc.Spec.DeploymentRefs))
	objects = append(objects, tc)
	for _, ref := range tc.Spec.DeploymentRefs {
		d := sampleDeployment(ref.Name)
		objects = append(objects, d, helmRelease(d, renderedValues(t, d, "")))
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objects...).
		WithInterceptorFuncs(interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if obj.GetObjectKind().GroupVersionKind().Kind == "HelmRelease" {
				switch key.Name {
				case "forbidden":
					return apierrors.NewForbidden(schema.GroupResource{Group: "helm.toolkit.fluxcd.io", Resource: "helmreleases"}, key.Name, nil)
				case "nokind":
					return &meta.NoKindMatchError{GroupKind: schema.GroupKind{Group: "helm.toolkit.fluxcd.io", Kind: "HelmRelease"}}
				}
			}
			return c.Get(ctx, key, obj, opts...)
		}}).Build()
	s := New(c, Config{Namespace: testNamespace, Token: testToken})

	rec := do(t, s, http.MethodGet, "/api/v1/testcases/pr-1-login/config-diff", nil)
	requireStatus(t, rec, http.StatusOK)
	got := decodeBody[ConfigDiff](t, rec)
	if !reflect.DeepEqual(got.Targets, []string{"a", "b"}) || len(got.Differences) != 0 {
		t.Fatalf("targets %v, differences %+v", got.Targets, got.Differences)
	}
	if len(got.Pending) != 2 || got.Pending[0].Name != "forbidden" || !strings.Contains(got.Pending[0].Reason, "forbidden") ||
		got.Pending[1].Name != "nokind" || !strings.Contains(got.Pending[1].Reason, "no matches for kind") {
		t.Fatalf("pending %+v", got.Pending)
	}
}

func TestTestCaseConfigDiffUnknownTestCase(t *testing.T) {
	s, _ := newServer(t, Config{})
	rec := do(t, s, http.MethodGet, "/api/v1/testcases/does-not-exist/config-diff", nil)
	requireStatus(t, rec, http.StatusNotFound)
}

func TestSameConfigValue(t *testing.T) {
	main := map[string]bool{"main": true, "authentik": true, "main-authentik": true}
	pr := map[string]bool{"pr": true, "authentik-pr": true, "pr-authentik": true}
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"main", "pr", true},
		{"main-authentik", "pr-authentik", true},
		{"main.authentik.svc:5432", "pr.authentik-pr.svc:5432", true},
		// The same value on both sides, though a name on one of them.
		{"authentik", "authentik", true},
		{"authentik.authentik.svc", "authentik.authentik-pr.svc", true},
		{"main", "other", false},
		{"primary", "prmary", false},
		{"main-replica", "pr-replica", false},
		{"main_idle", "pr_idle", true},
		{"main.svc", "pr-svc", false},
		{"main", "pr.x", false},
	} {
		if got := sameConfigValue(c.a, main, c.b, pr); got != c.want {
			t.Errorf("sameConfigValue(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestDiffConfig(t *testing.T) {
	config := func(facts map[string]string) deploymentConfig {
		c := deploymentConfig{facts: map[string]configFact{}}
		for path, value := range facts {
			c.facts[path] = configFact{value: value}
		}
		return c
	}
	configs := map[string]deploymentConfig{
		"head": config(map[string]string{"a": "1", "b": "x", "c": "same", "only-head": "y", "empty": "{}"}),
		"main": config(map[string]string{"a": "2", "b": "x", "c": "same"}),
	}
	got := diffConfig([]string{"head", "main"}, configs)
	want := []ConfigDifference{
		{Path: "a", Values: map[string]string{"head": "1", "main": "2"}},
		{Path: "only-head", Values: map[string]string{"head": "y"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestNamedListFallsBackToIndex(t *testing.T) {
	out := map[string]string{}
	flattenConfig("x", []any{map[string]any{"name": "a"}, map[string]any{"name": "a"}, "plain"}, out)
	if _, ok := out["x[2]"]; !ok {
		t.Fatalf("expected index keys, got %v", out)
	}
}
