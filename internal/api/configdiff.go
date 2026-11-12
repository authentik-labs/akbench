package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	helmv2 "github.com/fluxcd/helm-controller/api/v2"
	"github.com/go-chi/chi/v5"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
	"github.com/authentik-labs/akbench/internal/render"
)

// The config diff answers "do the deployments this test case compares differ
// in anything but the build under test?". A setting that reached only one
// side - extra values that were never attached, a replica count edited on one
// deployment - makes the comparison measure that setting too, and nothing
// else shows it: both sides run, and their metrics look plausible.
//
// It compares, per deployment, the Helm values the operator rendered for it
// (extra values included), its chart, and the few spec fields that act
// outside those values (specConfig). What differs by design is left out: the
// image, the deployment's own names (sameConfigValue), credentials generated
// per deployment, and an external database's connection, which is returned
// as information instead. It must stay silent on a healthy pair, or the
// warning stops being read.

// ConfigDiff is the response body of GET /testcases/{name}/config-diff.
type ConfigDiff struct {
	TestID string `json:"testid"`
	// Targets are the deployments that were compared.
	Targets []string `json:"targets"`
	// Pending are deployments that could not be compared, and why.
	Pending []ConfigPending `json:"pending,omitempty"`
	// Databases are the external databases of the targets in External
	// postgres mode, keyed by deployment. Two builds can't share a database,
	// so these differ on every healthy pair: they are shown, not compared.
	Databases   map[string]ConfigDatabase `json:"databases,omitempty"`
	Differences []ConfigDifference        `json:"differences"`
}

// ConfigPending is a deployment that could not be compared.
type ConfigPending struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// ConfigDatabase is where an External postgres deployment's database is.
type ConfigDatabase struct {
	Host string `json:"host,omitempty"`
	Port string `json:"port,omitempty"`
	Name string `json:"name,omitempty"`
	User string `json:"user,omitempty"`
}

// ConfigDifference is one setting that is not the same on every target of a
// TestCase. Values is keyed by deployment name; a deployment without an
// entry does not set it at all. A credential's value is never returned: it
// is shown as configSet, with Note saying the values differ.
type ConfigDifference struct {
	Path   string            `json:"path"`
	Values map[string]string `json:"values"`
	Note   string            `json:"note,omitempty"`
}

// testCaseConfigDiff answers GET /testcases/{name}/config-diff. A test case
// with fewer than two comparable deployments (a parent, which drives child
// test cases instead, included) has no differences.
func (s *Server) testCaseConfigDiff(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	var tc benchv1alpha1.TestCase
	if err := s.client.Get(r.Context(), types.NamespacedName{Name: name, Namespace: s.cfg.Namespace}, &tc); err != nil {
		writeAPIError(w, r, err)
		return
	}
	diff := ConfigDiff{TestID: name, Targets: []string{}, Differences: []ConfigDifference{}}
	configs := map[string]deploymentConfig{}
	for _, ref := range tc.Deployments() {
		// One deployment that can't be read - gone, not installed yet, or a
		// HelmRelease kind or permission this server lacks - says nothing
		// about the others, so it is reported rather than failing them all.
		config, err := s.deploymentConfig(r.Context(), ref.Name)
		if err != nil {
			diff.Pending = append(diff.Pending, ConfigPending{Name: ref.Name, Reason: err.Error()})
			continue
		}
		diff.Targets = append(diff.Targets, ref.Name)
		configs[ref.Name] = config
		if config.database != nil {
			if diff.Databases == nil {
				diff.Databases = map[string]ConfigDatabase{}
			}
			diff.Databases[ref.Name] = *config.database
		}
	}
	if len(diff.Targets) > 1 {
		diff.Differences = diffConfig(diff.Targets, configs)
	}
	writeJSON(w, r, http.StatusOK, diff)
}

// deploymentConfig reads the named deployment's configuration. Its error
// says, for a person, why the deployment can't be compared.
func (s *Server) deploymentConfig(ctx context.Context, name string) (deploymentConfig, error) {
	var topo benchv1alpha1.AuthentikDeployment
	if err := s.client.Get(ctx, types.NamespacedName{Name: name, Namespace: s.cfg.Namespace}, &topo); err != nil {
		if apierrors.IsNotFound(err) {
			return deploymentConfig{}, errors.New("the AuthentikDeployment does not exist")
		}
		return deploymentConfig{}, fmt.Errorf("could not read the AuthentikDeployment: %w", err)
	}
	hr := &unstructured.Unstructured{}
	hr.SetGroupVersionKind(helmv2.GroupVersion.WithKind("HelmRelease"))
	if err := s.client.Get(ctx, types.NamespacedName{Name: render.HelmReleaseName(&topo), Namespace: topo.Namespace}, hr); err != nil {
		if apierrors.IsNotFound(err) {
			return deploymentConfig{}, errors.New("it has no HelmRelease yet")
		}
		return deploymentConfig{}, fmt.Errorf("could not read its HelmRelease: %w", err)
	}
	values, _, err := unstructured.NestedMap(hr.Object, "spec", "values")
	if err != nil {
		return deploymentConfig{}, fmt.Errorf("could not read its HelmRelease values: %w", err)
	}
	return newDeploymentConfig(&topo, values), nil
}

// deploymentConfig is one deployment's configuration, ready to compare.
type deploymentConfig struct {
	// names are what the deployment is called, and what the operator names
	// after it: a value made of them on one side matches one made of the
	// other side's (sameConfigValue).
	names map[string]bool
	// facts are keyed by dotted path.
	facts map[string]configFact
	// database is set in External postgres mode.
	database *ConfigDatabase
}

// configFact is one setting's value. A credential's is a hash of it, so a
// difference still shows without the credential leaving the cluster.
type configFact struct {
	value      string
	credential bool
}

// configSet is what a credential is shown as.
const configSet = "(set)"

// configCredentialNote explains a credential's difference, whose values are
// all shown as configSet.
const configCredentialNote = "credential: the values differ and are not shown"

// configIgnoredPaths are settings a comparison varies on purpose: the image
// is the build under test.
var configIgnoredPaths = []string{
	"values.global.image.repository",
	"values.global.image.tag",
	"values.global.image.digest",
}

// configGeneratedCredentials differ between any two deployments, so they are
// not compared at all: the secret key and the embedded postgres password are
// generated per deployment (the controller's ensureRandomSecret), and CNPG
// generates its own. An External database's password goes with the rest of
// its connection (configExternalDatabase).
var configGeneratedCredentials = []string{
	"values.authentik.secret_key",
	"values.authentik.postgresql.password",
	"values.postgresql.auth.password",
}

// configExternalDatabase is an External postgres deployment's connection,
// reported as ConfigDiff.Databases rather than compared. With an embedded or
// CNPG postgres the same paths hold the operator's own service, named after
// the deployment, and the defaults, so they are compared like anything else.
var configExternalDatabase = struct{ host, port, name, user string }{
	host: "values.authentik.postgresql.host",
	port: "values.authentik.postgresql.port",
	name: "values.authentik.postgresql.name",
	user: "values.authentik.postgresql.user",
}

// configCredentialLeaves are setting names, lowercased, that hold a
// credential besides those ending in "password": the authentik chart's
// authentik.secret_key and geoip.licenseKey, and common spellings of keys
// and client secrets in other charts' values.
var configCredentialLeaves = []string{
	"secret_key", "secretkey", "licensekey", "client_secret", "clientsecret", "api_key", "apikey", "token",
}

// configCredentialEnvSuffixes mark an env entry ({name, value}) whose value
// is a credential, like AUTHENTIK_BOOTSTRAP_TOKEN or
// AUTHENTIK_POSTGRESQL__PASSWORD.
var configCredentialEnvSuffixes = []string{"_PASSWORD", "_TOKEN", "_SECRET", "_KEY"}

// configCredential reports whether path, one of flat's, holds a credential.
// It matches whole setting names, not words inside them: a setting about
// credentials (server.automountServiceAccountToken, a secretRef's name) is
// an ordinary one.
func configCredential(path string, flat map[string]string) bool {
	leaf := strings.ToLower(path[strings.LastIndex(path, ".")+1:])
	if strings.HasSuffix(leaf, "password") || strings.HasSuffix(leaf, "_token") || slices.Contains(configCredentialLeaves, leaf) {
		return true
	}
	if prefix, ok := strings.CutSuffix(path, ".value"); ok {
		name := strings.ToUpper(flat[prefix+".name"])
		return slices.ContainsFunc(configCredentialEnvSuffixes, func(suffix string) bool { return strings.HasSuffix(name, suffix) })
	}
	return false
}

// newDeploymentConfig is t's configuration, given the Helm values its
// HelmRelease carries.
func newDeploymentConfig(t *benchv1alpha1.AuthentikDeployment, values map[string]any) deploymentConfig {
	flat := specConfig(t)
	flattenConfig("values", values, flat)

	config := deploymentConfig{names: deploymentNames(t), facts: make(map[string]configFact, len(flat))}
	db := configExternalDatabase
	external := t.Spec.Postgres.Mode == benchv1alpha1.PostgresExternal
	if external {
		config.database = &ConfigDatabase{Host: flat[db.host], Port: flat[db.port], Name: flat[db.name], User: flat[db.user]}
	}
	for path, value := range flat {
		switch {
		case slices.Contains(configIgnoredPaths, path), slices.Contains(configGeneratedCredentials, path):
		case external && (path == db.host || path == db.port || path == db.name || path == db.user):
		case value != "" && configCredential(path, flat):
			sum := sha256.Sum256([]byte(value))
			config.facts[path] = configFact{value: hex.EncodeToString(sum[:]), credential: true}
		default:
			config.facts[path] = configFact{value: value}
		}
	}
	return config
}

// specConfig is the part of t's configuration that its Helm values don't
// carry. Everything the values do carry is compared there instead of in the
// spec, where the same setting can be written several ways (a default left
// out or spelled out) and would be listed twice. Rendered into the values,
// so not taken from the spec: replicas, placement (as affinity, node
// selectors and tolerations), log level, error reporting, the pyroscope
// host, embedded postgres TLS and extra values. Not used by the operator at
// all, so not compared: server/worker resources (only the capacity check
// reads them), placement.topologyKey, the embedded postgres' storageSize and
// storageClassName, and external.sslMode. What's left:
//   - clusterRef: which workload cluster it runs on; the values only show
//     whether there is one.
//   - the chart's repository and version, which the values are for.
//   - the postgres mode, and what the operator does with it outside the
//     chart: the CNPG Cluster it renders, the size it grows volumes up to,
//     the Secret an external database is read from.
func specConfig(t *benchv1alpha1.AuthentikDeployment) map[string]string {
	flat := map[string]string{
		"spec.clusterRef.name": t.Spec.ClusterRef.Name,
		"chart.repository":     render.HelmRepository("", t.Spec.Chart).Spec.URL,
		"chart.version":        render.ChartVersion(t),
		"spec.postgres.mode":   string(t.Spec.Postgres.Mode),
	}
	if size := render.PostgresMaxStorageSize(t); size != "" {
		flat["postgres.maxStorageSize"] = size
	}
	switch t.Spec.Postgres.Mode {
	case benchv1alpha1.PostgresCNPG:
		// Rendered without status, which holds the size auto-growth has
		// grown the volume to: that is the database filling up, not
		// configuration.
		configured := t.DeepCopy()
		configured.Status = benchv1alpha1.AuthentikDeploymentStatus{}
		flattenConfig("cnpg", render.CNPGCluster(configured, nil).Object["spec"], flat)
	case benchv1alpha1.PostgresExternal:
		if ext := t.Spec.Postgres.External; ext != nil {
			flat["spec.postgres.external.secretRef.name"] = ext.SecretRef.Name
		}
	}
	return flat
}

// deploymentNames are t's own names and the ones the operator derives from
// them into its Helm values, exactly: BoundedName hashes a long one, so the
// derived names can't be recognised by the deployment's name inside them.
func deploymentNames(t *benchv1alpha1.AuthentikDeployment) map[string]bool {
	targetName := t.Spec.TargetName
	if targetName == "" {
		targetName = t.Name
	}
	names := map[string]bool{}
	for _, name := range []string{
		t.Name, t.Spec.Namespace, targetName,
		render.AuthentikFullname(render.HelmReleaseName(t)),
		// The embedded postgres' fullnameOverride and authentik's host for it.
		render.EmbeddedPostgresStatefulSetName(t),
		render.CNPGClusterName(t),
		// The host CNPG's app secret gives authentik: the read-write service.
		render.CNPGClusterName(t) + "-rw",
	} {
		if name != "" {
			names[name] = true
		}
	}
	return names
}

// configTokens splits s into the runs of letters, digits and '-' that a
// Kubernetes name is made of, and the separators between them:
// "a-postgresql.ns:5432" is "a-postgresql", ".", "ns", ":", "5432".
func configTokens(s string) []string {
	var tokens []string
	start := 0
	for i := 1; i <= len(s); i++ {
		if i == len(s) || configNameChar(s[i]) != configNameChar(s[start]) {
			tokens = append(tokens, s[start:i])
			start = i
		}
	}
	return tokens
}

func configNameChar(c byte) bool {
	return c == '-' || ('0' <= c && c <= '9') || ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z')
}

// sameConfigValue reports whether a, from a deployment called aNames, and b,
// from one called bNames, are the same setting: equal, or equal but for
// tokens that are one of each side's own names. Only whole tokens count, so
// neither a name inside a word ("pr" in "primary", "test" in
// "x-benchmark-test") nor a name that is also an ordinary value on the
// other side ("authentik" as one namespace and a database name) is taken
// for the deployment's.
func sameConfigValue(a string, aNames map[string]bool, b string, bNames map[string]bool) bool {
	if a == b {
		return true
	}
	at, bt := configTokens(a), configTokens(b)
	if len(at) != len(bt) {
		return false
	}
	for i := range at {
		if at[i] != bt[i] && (!aNames[at[i]] || !bNames[bt[i]]) {
			return false
		}
	}
	return true
}

// namedList reports whether every item of list is an object with its own
// distinct "name", like a container's env. Such a list is compared by name
// rather than by position, so the same entries in another order are equal.
func namedList(list []any) bool {
	seen := map[string]bool{}
	for _, item := range list {
		obj, ok := item.(map[string]any)
		if !ok {
			return false
		}
		name, ok := obj["name"].(string)
		if !ok || name == "" || seen[name] {
			return false
		}
		seen[name] = true
	}
	return true
}

// flattenConfig writes every leaf under v into out, keyed by its dotted
// path below prefix.
func flattenConfig(prefix string, v any, out map[string]string) {
	switch v := v.(type) {
	case map[string]any:
		if len(v) == 0 {
			out[prefix] = configEmptyObject
			return
		}
		for key, child := range v {
			flattenConfig(prefix+"."+key, child, out)
		}
	case []any:
		if len(v) == 0 {
			out[prefix] = "[]"
			return
		}
		named := namedList(v)
		for i, child := range v {
			key := strconv.Itoa(i)
			if named {
				key = child.(map[string]any)["name"].(string)
			}
			flattenConfig(prefix+"["+key+"]", child, out)
		}
	case string:
		out[prefix] = v
	default:
		raw, err := json.Marshal(v)
		if err != nil {
			raw = []byte(fmt.Sprint(v))
		}
		out[prefix] = string(raw)
	}
}

// configEmptyObject is how flattenConfig writes an empty object. diffConfig
// takes it for the same as not setting the path at all: merged into a
// chart's values, an empty object changes nothing.
const configEmptyObject = "{}"

// diffConfig lists the paths that are not set to the same value on every one
// of targets, sorted by path.
func diffConfig(targets []string, configs map[string]deploymentConfig) []ConfigDifference {
	paths := map[string]bool{}
	for _, target := range targets {
		for path := range configs[target].facts {
			paths[path] = true
		}
	}
	first := configs[targets[0]]
	differences := []ConfigDifference{}
	for path := range paths {
		want, ok := first.facts[path]
		if !ok {
			want.value = configEmptyObject
		}
		same := true
		values := map[string]string{}
		credential := false
		for _, target := range targets {
			config := configs[target]
			fact, ok := config.facts[path]
			switch {
			case !ok:
				fact.value = configEmptyObject
			case fact.credential:
				credential = true
				values[target] = configSet
			default:
				values[target] = fact.value
			}
			switch {
			case fact.credential || want.credential:
				same = same && fact == want
			default:
				same = same && sameConfigValue(want.value, first.names, fact.value, config.names)
			}
		}
		if !same {
			difference := ConfigDifference{Path: path, Values: values}
			if credential {
				difference.Note = configCredentialNote
			}
			differences = append(differences, difference)
		}
	}
	slices.SortFunc(differences, func(a, b ConfigDifference) int { return strings.Compare(a.Path, b.Path) })
	return differences
}
