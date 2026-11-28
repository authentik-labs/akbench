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
	// Names a tokeniser for Kubernetes names would cut into pieces.
	"odd target names": {a: "bench-main", b: "bench-pr", aNamespace: "ak-main", bNamespace: "ak-pr", aTarget: "release/2026.6", bTarget: "pr_123"},
	"dotted names":     {a: "bench.main", b: "bench.pr.1", aNamespace: "ak-main", bNamespace: "ak-pr"},
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
		"values.server.env[SENTRY].name":                         "SENTRY_URL",
		"values.server.env[SENTRY].value":                        "https://abc123@sentry.example.com/4",
		"values.authentik.redis.url":                             "redis://:hunter2@redis:6379/0",
		"values.authentik.web.url":                               "https://authentik.example.com/",
		"values.x.query":                                         "https://h.example.com/hook?a=1&token=abc",
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
		"values.authentik.error_reporting.sentry_dsn":             true,
		"values.authentik.storage.media.s3.access_key":            true,
		"values.authentik.storage.media.s3.secretKey":             true,
		"values.server.env[SENTRY].value":                         true,
		"values.authentik.redis.url":                              true,
		"values.authentik.web.url":                                false,
		"values.server.tolerations[0].key":                        false,
		"values.authentik.storage.s3.secretAccessKey":             true,
		"values.x.authToken":                                      true,
		"values.x.sentryDsn":                                      true,
		"values.x.tokens[0]":                                      true,
		"values.server.topologyKey":                               false,
		"values.x.query":                                          true,
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
			{Name: "pr-1-new", Reason: "it has no HelmRelease yet", Waiting: true},
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
	main := map[string]configNameRole{
		"main": configNameDeployment | configNameTarget, "authentik": configNameNamespace, "main-authentik": configNameRelease,
	}
	pr := map[string]configNameRole{
		"pr": configNameDeployment, "pr/1_a.b": configNameTarget, "authentik-pr": configNameNamespace, "pr-authentik": configNameRelease,
	}
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
		// A name made of more than name characters is still one name.
		{"main", "pr/1_a.b", true},
		{"target=main,x", "target=pr/1_a.b,x", true},
		{"main", "pr/1_a.bc", false},
		// Each side's name, but for different things: a real difference.
		{"main", "authentik-pr", false},
		{"authentik", "pr-authentik", false},
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

// A setting the test case lists in configUnderTest is reported as under
// test, not hidden; one it lists that doesn't differ is reported as
// unchanged; anything else still differs as before.
func TestTestCaseConfigDiffUnderTest(t *testing.T) {
	tc := &benchv1alpha1.TestCase{
		Name: "pool-login", Namespace: testNamespace,
		Spec: benchv1alpha1.TestCaseSpec{
			DeploymentRefs:  []corev1.LocalObjectReference{{Name: "pool-on"}, {Name: "pool-off"}},
			Profile:         benchv1alpha1.ProfileBurn,
			ConfigUnderTest: []string{"values.authentik.postgresql", "values.authentik.cache"},
		},
	}
	on, off := sampleDeployment("pool-on"), sampleDeployment("pool-off")
	s, _ := newServer(t, Config{}, tc, on, off,
		helmRelease(on, renderedValues(t, on, "authentik:\n  postgresql:\n    use_pool: true\nworker:\n  replicas: 3\n")),
		helmRelease(off, renderedValues(t, off, "")))

	rec := do(t, s, http.MethodGet, "/api/v1/testcases/pool-login/config-diff", nil)
	requireStatus(t, rec, http.StatusOK)
	got := decodeBody[ConfigDiff](t, rec)
	want := ConfigDiff{
		TestID:  "pool-login",
		Targets: []string{"pool-on", "pool-off"},
		Differences: []ConfigDifference{
			{Path: "values.authentik.postgresql.use_pool", Values: map[string]string{"pool-on": "true"}, UnderTest: true},
			{Path: "values.worker.replicas", Values: map[string]string{"pool-on": "3", "pool-off": "1"}},
		},
		UnchangedUnderTest: []string{"values.authentik.cache"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestConfigPathUnder(t *testing.T) {
	for _, c := range []struct {
		path, entry string
		want        bool
	}{
		{"values.authentik.postgresql.use_pool", "values.authentik.postgresql.use_pool", true},
		{"values.authentik.postgresql.use_pool", "values.authentik.postgresql", true},
		{"values.server.env[AUTHENTIK_X].value", "values.server.env", true},
		{"values.server.env[AUTHENTIK_X].value", "values.server.env[AUTHENTIK_X]", true},
		{"values.authentik.postgresql_extra", "values.authentik.postgresql", false},
		{"values.authentik.postgresql", "values.authentik.postgresql.use_pool", false},
		{"values.worker.replicas", "values.server", false},
	} {
		if got := configPathUnder(c.path, c.entry); got != c.want {
			t.Errorf("configPathUnder(%q, %q) = %v, want %v", c.path, c.entry, got, c.want)
		}
	}
}

// A deployment that couldn't be compared may be the one that differs on a
// listed setting, so nothing is reported as unchanged then.
func TestTestCaseConfigDiffUnderTestPending(t *testing.T) {
	tc := &benchv1alpha1.TestCase{
		Name: "pool-login", Namespace: testNamespace,
		Spec: benchv1alpha1.TestCaseSpec{
			DeploymentRefs:  []corev1.LocalObjectReference{{Name: "pool-a"}, {Name: "pool-b"}, {Name: "pool-new"}},
			Profile:         benchv1alpha1.ProfileBurn,
			ConfigUnderTest: []string{"values.authentik.postgresql"},
		},
	}
	a, b := sampleDeployment("pool-a"), sampleDeployment("pool-b")
	s, _ := newServer(t, Config{}, tc, a, b, sampleDeployment("pool-new"),
		helmRelease(a, renderedValues(t, a, "")), helmRelease(b, renderedValues(t, b, "")))
	rec := do(t, s, http.MethodGet, "/api/v1/testcases/pool-login/config-diff", nil)
	requireStatus(t, rec, http.StatusOK)
	if got := decodeBody[ConfigDiff](t, rec); len(got.Pending) != 1 || got.UnchangedUnderTest != nil {
		t.Fatalf("got %+v, want pool-new pending and nothing unchanged", got)
	}
}

// With fewer than two deployments to compare, nothing is compared, so
// nothing listed is reported as unchanged either.
func TestTestCaseConfigDiffUnderTestSingleTarget(t *testing.T) {
	tc := &benchv1alpha1.TestCase{
		Name: "solo", Namespace: testNamespace,
		Spec: benchv1alpha1.TestCaseSpec{
			DeploymentRefs:  []corev1.LocalObjectReference{{Name: "solo-a"}},
			Profile:         benchv1alpha1.ProfileBurn,
			ConfigUnderTest: []string{"values.authentik.postgresql"},
		},
	}
	a := sampleDeployment("solo-a")
	s, _ := newServer(t, Config{}, tc, a, helmRelease(a, renderedValues(t, a, "")))
	rec := do(t, s, http.MethodGet, "/api/v1/testcases/solo/config-diff", nil)
	requireStatus(t, rec, http.StatusOK)
	if got := decodeBody[ConfigDiff](t, rec); got.UnchangedUnderTest != nil || len(got.Differences) != 0 {
		t.Fatalf("got %+v, want no differences and nothing unchanged", got)
	}
}

// What every healthy test goes through says nothing: one deployment has
// nothing to compare against, an unpinned chart's version is not known until
// it is installed, and an archived test's deployments may be deleted.
func TestTestCaseConfigDiffQuietWhenNothingIsWrong(t *testing.T) {
	testCase := func(name string, archived bool, refs ...string) *benchv1alpha1.TestCase {
		tc := &benchv1alpha1.TestCase{Name: name, Namespace: testNamespace, Spec: benchv1alpha1.TestCaseSpec{
			Profile: benchv1alpha1.ProfileBurn, Archived: archived,
		}}
		for _, ref := range refs {
			tc.Spec.DeploymentRefs = append(tc.Spec.DeploymentRefs, corev1.LocalObjectReference{Name: ref})
		}
		return tc
	}
	installed, installing := sampleDeployment("installed"), sampleDeployment("installing")
	installed.Spec.Chart.Version, installing.Spec.Chart.Version = "", ""
	installed.Status.ChartVersion = "2026.6.0"
	s, _ := newServer(t, Config{},
		testCase("solo", false, "not-there"),
		testCase("archived", true, "installed", "deleted"),
		testCase("fresh", false, "installed", "installing"),
		installed, installing,
		helmRelease(installed, renderedValues(t, installed, "")),
		helmRelease(installing, renderedValues(t, installing, "")))
	for _, name := range []string{"solo", "archived", "fresh"} {
		rec := do(t, s, http.MethodGet, "/api/v1/testcases/"+name+"/config-diff", nil)
		requireStatus(t, rec, http.StatusOK)
		if got := decodeBody[ConfigDiff](t, rec); len(got.Pending) != 0 || len(got.Differences) != 0 {
			t.Errorf("%s: got %+v, want nothing pending and no differences", name, got)
		}
	}
}

// A chart version that is known on every side is compared.
func TestTestCaseConfigDiffChartVersion(t *testing.T) {
	a, b := sampleDeployment("a"), sampleDeployment("b")
	a.Spec.Chart.Version, b.Spec.Chart.Version = "", ""
	a.Status.ChartVersion, b.Status.ChartVersion = "2026.6.0", "2026.5.2"
	tc := &benchv1alpha1.TestCase{Name: "charts", Namespace: testNamespace, Spec: benchv1alpha1.TestCaseSpec{
		DeploymentRefs: []corev1.LocalObjectReference{{Name: "a"}, {Name: "b"}}, Profile: benchv1alpha1.ProfileBurn,
	}}
	s, _ := newServer(t, Config{}, tc, a, b, helmRelease(a, renderedValues(t, a, "")), helmRelease(b, renderedValues(t, b, "")))
	rec := do(t, s, http.MethodGet, "/api/v1/testcases/charts/config-diff", nil)
	requireStatus(t, rec, http.StatusOK)
	want := []ConfigDifference{{Path: "chart.version", Values: map[string]string{"a": "2026.6.0", "b": "2026.5.2"}}}
	if got := decodeBody[ConfigDiff](t, rec); !reflect.DeepEqual(got.Differences, want) {
		t.Fatalf("got %+v, want %+v", got.Differences, want)
	}
}

// A parent has no deployments of its own: it reports each child's.
func TestTestCaseConfigDiffParent(t *testing.T) {
	parent := &benchv1alpha1.TestCase{
		Name: "pr-7", Namespace: testNamespace,
		Spec: benchv1alpha1.TestCaseSpec{
			Variants:  []benchv1alpha1.TestCaseVariant{{Name: "pr", Tag: "gh-abc"}, {Name: "main", Tag: "gh-main"}},
			Templates: []string{"login", "oauth-oidc"},
			Deployment: &benchv1alpha1.AuthentikDeploymentSpec{
				Server: benchv1alpha1.WorkloadSpec{Replicas: 2}, Worker: benchv1alpha1.WorkloadSpec{Replicas: 1},
				Postgres:  benchv1alpha1.PostgresSpec{Mode: benchv1alpha1.PostgresEmbedded},
				Authentik: benchv1alpha1.AuthentikSpec{Image: "ghcr.io/goauthentik/dev-server"},
			},
			Profile: benchv1alpha1.ProfileBurn,
		},
	}
	children, err := render.TestCaseChildren(parent)
	if err != nil {
		t.Fatal(err)
	}
	// The second child was never created.
	child := children.TestCases[0]
	objects := make([]client.Object, 0, 2+2*len(children.Deployments))
	objects = append(objects, parent, child)
	edited := child.Spec.DeploymentRefs[0].Name
	for _, d := range children.Deployments {
		extra := ""
		if d.Name == edited {
			extra = "worker:\n  replicas: 4\n"
		}
		objects = append(objects, d, helmRelease(d, renderedValues(t, d, extra)))
	}
	s, _ := newServer(t, Config{}, objects...)
	rec := do(t, s, http.MethodGet, "/api/v1/testcases/pr-7/config-diff", nil)
	requireStatus(t, rec, http.StatusOK)
	got := decodeBody[ConfigDiff](t, rec)
	if len(got.Targets) != 0 || len(got.Differences) != 0 || len(got.Children) != 1 {
		t.Fatalf("got %+v, want only the one existing child's comparison", got)
	}
	if c := got.Children[0]; c.TestID != child.Name || len(c.Differences) != 1 || c.Differences[0].Path != "values.worker.replicas" {
		t.Fatalf("child: got %+v, want values.worker.replicas to differ", c)
	}
}

// GET /config-diff compares across test cases: two tests on one deployment
// each, which neither test's own comparison covers. A difference is under
// test only if every test case lists it.
func TestTestCasesConfigDiff(t *testing.T) {
	testCase := func(name, ref string, underTest ...string) *benchv1alpha1.TestCase {
		return &benchv1alpha1.TestCase{Name: name, Namespace: testNamespace, Spec: benchv1alpha1.TestCaseSpec{
			DeploymentRefs: []corev1.LocalObjectReference{{Name: ref}}, Profile: benchv1alpha1.ProfileBurn, ConfigUnderTest: underTest,
		}}
	}
	a, b := sampleDeployment("a"), sampleDeployment("b")
	s, _ := newServer(t, Config{},
		testCase("login-a", "a", "values.worker", "values.server"), testCase("login-b", "b", "values.worker"), testCase("oidc-a", "a"),
		a, b,
		helmRelease(a, renderedValues(t, a, "worker:\n  replicas: 4\nserver:\n  replicas: 5\n")),
		helmRelease(b, renderedValues(t, b, "")))

	rec := do(t, s, http.MethodGet, "/api/v1/config-diff?testids=login-a,login-b,gone,login-a", nil)
	requireStatus(t, rec, http.StatusOK)
	want := ConfigDiff{
		TestID:  "login-a,login-b,gone",
		Targets: []string{"a", "b"},
		Differences: []ConfigDifference{
			{Path: "values.server.replicas", Values: map[string]string{"a": "5", "b": "2"}},
			{Path: "values.worker.replicas", Values: map[string]string{"a": "4", "b": "1"}, UnderTest: true},
		},
	}
	if got := decodeBody[ConfigDiff](t, rec); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}

	// A test case that brings no deployment of its own (a parent) has no say
	// in what is under test.
	rec = do(t, s, http.MethodGet, "/api/v1/config-diff?testids=login-b,login-a,gone", nil)
	if got := decodeBody[ConfigDiff](t, rec); len(got.Differences) != 2 || !got.Differences[1].UnderTest {
		t.Fatalf("got %+v, want values.worker.replicas under test", got)
	}

	// Tests on the same deployment have nothing to compare.
	rec = do(t, s, http.MethodGet, "/api/v1/config-diff?testids=login-a,oidc-a", nil)
	requireStatus(t, rec, http.StatusOK)
	if got := decodeBody[ConfigDiff](t, rec); len(got.Targets) != 0 || len(got.Differences) != 0 {
		t.Fatalf("got %+v, want nothing compared", got)
	}
	requireStatus(t, do(t, s, http.MethodGet, "/api/v1/config-diff", nil), http.StatusBadRequest)
}

// A credential that only differs by the deployment's own name inside it is
// the same on both sides, and a real difference never shows its values.
func TestDiffConfigCredentials(t *testing.T) {
	config := func(name, url, key string) deploymentConfig {
		return deploymentConfig{names: map[string]configNameRole{name: configNameNamespace}, facts: map[string]configFact{
			"values.redis.url": {value: url, credential: true},
			"values.api_key":   {value: key, credential: true},
		}}
	}
	got := diffConfig([]string{"a", "b"}, map[string]deploymentConfig{
		"a": config("ak-main", "redis://default@redis.ak-main:6379/0", "one"),
		"b": config("ak-pr", "redis://default@redis.ak-pr:6379/0", "two"),
	})
	want := []ConfigDifference{{Path: "values.api_key", Values: map[string]string{"a": configSet, "b": configSet}, Note: configCredentialNote}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

// A chart version that isn't known yet on one side doesn't hide a
// difference between the sides where it is.
func TestDiffConfigUnknown(t *testing.T) {
	fact := func(f configFact) deploymentConfig {
		return deploymentConfig{facts: map[string]configFact{"chart.version": f}}
	}
	got := diffConfig([]string{"new", "a", "b"}, map[string]deploymentConfig{
		"new": fact(configFact{unknown: true}), "a": fact(configFact{value: "1.0.0"}), "b": fact(configFact{value: "2.0.0"}),
	})
	want := []ConfigDifference{{Path: "chart.version", Values: map[string]string{"a": "1.0.0", "b": "2.0.0"}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	got = diffConfig([]string{"new", "a"}, map[string]deploymentConfig{"new": fact(configFact{unknown: true}), "a": fact(configFact{value: "1.0.0"})})
	requireNoDifferences(t, "one side unknown", got)
}
