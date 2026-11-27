package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"

	helmv2 "github.com/fluxcd/helm-controller/api/v2"
	"github.com/go-chi/chi/v5"
	corev1 "k8s.io/api/core/v1"
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
// warning stops being read: a deployment still being installed, a single
// deployment, and an archived test whose deployments were deleted all say
// nothing.

// ConfigDiff is the response body of GET /testcases/{name}/config-diff and
// GET /config-diff.
type ConfigDiff struct {
	// TestID is the test case, or the test cases joined by commas, whose
	// deployments were compared.
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
	// UnchangedUnderTest are the entries of the test case's configUnderTest
	// that no difference falls under: the deployments are configured the
	// same there, so the comparison doesn't test what it says it does. Only
	// set for a single test case whose every deployment could be compared.
	UnchangedUnderTest []string `json:"unchangedUnderTest,omitempty"`
	// Children are a parent test case's children, each compared on its own:
	// a parent has no deployments itself.
	Children []ConfigDiff `json:"children,omitempty"`
}

// ConfigPending is a deployment that could not be compared. Waiting marks
// one that only isn't installed yet, which every new deployment goes
// through and needs no one's attention.
type ConfigPending struct {
	Name    string `json:"name"`
	Reason  string `json:"reason"`
	Waiting bool   `json:"waiting,omitempty"`
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
// is shown as configSet, with Note saying the values differ. UnderTest
// marks a difference the test case compares on purpose (configUnderTest).
type ConfigDifference struct {
	Path      string            `json:"path"`
	Values    map[string]string `json:"values"`
	Note      string            `json:"note,omitempty"`
	UnderTest bool              `json:"underTest,omitempty"`
}

// testCaseConfigDiff answers GET /testcases/{name}/config-diff. A test case
// with fewer than two deployments has nothing to compare. A parent, which
// drives child test cases instead of deployments, gets each child's
// comparison.
func (s *Server) testCaseConfigDiff(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	var tc benchv1alpha1.TestCase
	if err := s.client.Get(r.Context(), types.NamespacedName{Name: name, Namespace: s.cfg.Namespace}, &tc); err != nil {
		writeAPIError(w, r, err)
		return
	}
	diff := s.configDiff(r.Context(), []*benchv1alpha1.TestCase{&tc})
	if tc.IsParent() {
		for _, template := range tc.Spec.Templates {
			var child benchv1alpha1.TestCase
			// A child that isn't there (not created yet, or deleted) has
			// nothing to compare.
			if err := s.client.Get(r.Context(), types.NamespacedName{Name: tc.ChildName(template), Namespace: s.cfg.Namespace}, &child); err != nil {
				continue
			}
			diff.Children = append(diff.Children, s.configDiff(r.Context(), []*benchv1alpha1.TestCase{&child}))
		}
	}
	writeJSON(w, r, http.StatusOK, diff)
}

// maxConfigDiffTestIDs bounds GET /config-diff: each test case costs a read
// per deployment.
const maxConfigDiffTestIDs = 50

// testCasesConfigDiff answers GET /config-diff?testids=a,b: one comparison
// across every deployment the named test cases run on, for a view that puts
// those tests side by side. A test case that doesn't exist is left out, like
// in GET /metrics/compare; one none of whose deployments are compared (a
// parent) has no say. A difference is under test only if every one of
// the test cases says so.
func (s *Server) testCasesConfigDiff(w http.ResponseWriter, r *http.Request) {
	var tcs []*benchv1alpha1.TestCase
	var asked []string
	seen := map[string]bool{}
	for id := range strings.SplitSeq(r.URL.Query().Get("testids"), ",") {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		asked = append(asked, id)
		if len(seen) > maxConfigDiffTestIDs {
			writeError(w, r, http.StatusBadRequest, fmt.Sprintf("at most %d testids", maxConfigDiffTestIDs))
			return
		}
		var tc benchv1alpha1.TestCase
		if err := s.client.Get(r.Context(), types.NamespacedName{Name: id, Namespace: s.cfg.Namespace}, &tc); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			writeAPIError(w, r, err)
			return
		}
		tcs = append(tcs, &tc)
	}
	if len(seen) == 0 {
		writeError(w, r, http.StatusBadRequest, "testids is required")
		return
	}
	diff := s.configDiff(r.Context(), tcs)
	// As asked, so the caller can tell which question this answers.
	diff.TestID = strings.Join(asked, ",")
	writeJSON(w, r, http.StatusOK, diff)
}

// configDiff compares the deployments tcs run on, each deployment once.
func (s *Server) configDiff(ctx context.Context, tcs []*benchv1alpha1.TestCase) ConfigDiff {
	names := make([]string, len(tcs))
	var refs []string
	// A deployment only archived test cases run on may be gone for good:
	// archiving offers to delete it, and its results stay.
	archived := map[string]bool{}
	for i, tc := range tcs {
		names[i] = tc.Name
		for _, ref := range tc.Deployments() {
			if _, ok := archived[ref.Name]; !ok {
				refs = append(refs, ref.Name)
				archived[ref.Name] = true
			}
			archived[ref.Name] = archived[ref.Name] && tc.Spec.Archived
		}
	}
	diff := ConfigDiff{TestID: strings.Join(names, ","), Targets: []string{}, Differences: []ConfigDifference{}}
	if len(refs) < 2 {
		return diff
	}
	configs := map[string]deploymentConfig{}
	for _, ref := range refs {
		// One deployment that can't be read - gone, not installed yet, or a
		// HelmRelease kind or permission this server lacks - says nothing
		// about the others, so it is reported rather than failing them all.
		config, err := s.deploymentConfig(ctx, ref)
		switch {
		case errors.Is(err, errConfigGone) && archived[ref]:
			continue
		case err != nil:
			diff.Pending = append(diff.Pending, ConfigPending{Name: ref, Reason: err.Error(), Waiting: errors.Is(err, errConfigNotInstalled)})
			continue
		}
		diff.Targets = append(diff.Targets, ref)
		configs[ref] = config
		if config.database != nil {
			if diff.Databases == nil {
				diff.Databases = map[string]ConfigDatabase{}
			}
			diff.Databases[ref] = *config.database
		}
	}
	if len(diff.Targets) < 2 {
		return diff
	}
	// An unpinned chart's version is known once it is installed. Until then
	// it is not different from the others', only not known yet.
	for _, config := range configs {
		if !config.chartResolved {
			config.facts[configChartVersion] = configFact{unknown: true}
		}
	}
	diff.Differences = diffConfig(diff.Targets, configs)
	// A test case none of whose deployments were compared has no say in
	// what the comparison is of.
	compared := slices.DeleteFunc(slices.Clone(tcs), func(tc *benchv1alpha1.TestCase) bool {
		return !slices.ContainsFunc(tc.Deployments(), func(ref corev1.LocalObjectReference) bool {
			return slices.Contains(diff.Targets, ref.Name)
		})
	})
	for i := range diff.Differences {
		diff.Differences[i].UnderTest = !slices.ContainsFunc(compared, func(tc *benchv1alpha1.TestCase) bool {
			return !configListed(diff.Differences[i].Path, tc.Spec.ConfigUnderTest)
		})
	}
	// A pending deployment may be the one that differs there.
	if len(tcs) == 1 && len(diff.Pending) == 0 {
		diff.UnchangedUnderTest = unchangedUnderTest(diff.Differences, tcs[0].Spec.ConfigUnderTest)
	}
	return diff
}

// configListed reports whether path falls under one of listed, a test
// case's configUnderTest.
func configListed(path string, listed []string) bool {
	return slices.ContainsFunc(listed, func(entry string) bool { return configPathUnder(path, entry) })
}

// unchangedUnderTest returns the entries of listed, a test case's
// configUnderTest, that none of differences falls under.
func unchangedUnderTest(differences []ConfigDifference, listed []string) []string {
	var unchanged []string
	for _, entry := range listed {
		if !slices.ContainsFunc(differences, func(d ConfigDifference) bool { return configPathUnder(d.Path, entry) }) {
			unchanged = append(unchanged, entry)
		}
	}
	return unchanged
}

// configPathUnder reports whether path is entry or a setting below it: a
// key (entry.key) or a list item (entry[name], entry[0]).
func configPathUnder(path, entry string) bool {
	rest, ok := strings.CutPrefix(path, entry)
	return ok && (rest == "" || rest[0] == '.' || rest[0] == '[')
}

// errConfigGone and errConfigNotInstalled are why deploymentConfig can't
// read a deployment that is otherwise in order.
var (
	errConfigGone         = errors.New("the AuthentikDeployment does not exist")
	errConfigNotInstalled = errors.New("it has no HelmRelease yet")
)

// configChartVersion is the path the chart's version is compared under.
const configChartVersion = "chart.version"

// deploymentConfig reads the named deployment's configuration. Its error
// says, for a person, why the deployment can't be compared.
func (s *Server) deploymentConfig(ctx context.Context, name string) (deploymentConfig, error) {
	var topo benchv1alpha1.AuthentikDeployment
	if err := s.client.Get(ctx, types.NamespacedName{Name: name, Namespace: s.cfg.Namespace}, &topo); err != nil {
		if apierrors.IsNotFound(err) {
			return deploymentConfig{}, errConfigGone
		}
		return deploymentConfig{}, fmt.Errorf("could not read the AuthentikDeployment: %w", err)
	}
	hr := &unstructured.Unstructured{}
	hr.SetGroupVersionKind(helmv2.GroupVersion.WithKind("HelmRelease"))
	if err := s.client.Get(ctx, types.NamespacedName{Name: render.HelmReleaseName(&topo), Namespace: topo.Namespace}, hr); err != nil {
		if apierrors.IsNotFound(err) {
			return deploymentConfig{}, errConfigNotInstalled
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
	names map[string]configNameRole
	// facts are keyed by dotted path.
	facts map[string]configFact
	// database is set in External postgres mode.
	database *ConfigDatabase
	// chartResolved is false until an unpinned chart's version is known.
	chartResolved bool
}

// configFact is one setting's value. A credential's is compared like any
// other but never returned: it is shown as configSet. An unknown one is not
// compared at all.
type configFact struct {
	value      string
	credential bool
	unknown    bool
}

// configChartUnresolved is render.ChartVersion's answer for a chart that is
// neither pinned nor installed yet.
const configChartUnresolved = "*"

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

// configCredentialSuffixes end a setting name that holds a credential, once
// lowercased and without its '_' and '-', so that secret_key, secretKey and
// SECRET-KEY are one name: the authentik chart's authentik.secret_key,
// email.password, bootstrap_token and geoip.licenseKey, an S3 access key, a
// Sentry DSN, and common spellings in other charts' values.
var configCredentialSuffixes = []string{
	"password", "passphrase", "token", "secret", "dsn",
	"secretkey", "accesskey", "apikey", "licensekey", "privatekey", "signingkey", "encryptionkey",
}

// configNotCredentials are setting names configCredentialSuffixes would take
// for a credential that only say something about one.
var configNotCredentials = []string{"automountserviceaccounttoken"}

// configCredentialEnvSuffixes mark an env entry ({name, value}) whose value
// is a credential, like AUTHENTIK_BOOTSTRAP_TOKEN or
// AUTHENTIK_POSTGRESQL__PASSWORD.
var configCredentialEnvSuffixes = []string{"_PASSWORD", "_PASSPHRASE", "_PASS", "_PWD", "_TOKEN", "_SECRET", "_KEY", "_DSN"}

// configCredentialURL matches a URL that carries who to sign in as, and so
// usually what with - a Sentry DSN, redis://:password@host, a database URL -
// or a credential in its query.
var configCredentialURL = regexp.MustCompile(`(?i)^[a-z][a-z0-9+.-]*://([^/?#@\s]+@|[^#\s]*[?&](password|passwd|token|secret|key)=)`)

// configCredential reports whether path, one of flat's, holds a credential,
// going by the setting's name and, for a URL, by its value. It matches the
// ends of setting names, not words inside them: a secretRef's name is an
// ordinary setting. It errs towards hiding: a setting wrongly taken for a
// credential still shows as different, only without its values.
func configCredential(path string, flat map[string]string) bool {
	leaf := path[strings.LastIndex(path, ".")+1:]
	// A list's items are named after the list: tokens[0].
	leaf, _, _ = strings.Cut(leaf, "[")
	// Plurals too: tokens, apiKeys.
	leaf = strings.TrimSuffix(strings.NewReplacer("_", "", "-", "").Replace(strings.ToLower(leaf)), "s")
	if !slices.Contains(configNotCredentials, leaf) &&
		slices.ContainsFunc(configCredentialSuffixes, func(suffix string) bool { return strings.HasSuffix(leaf, suffix) }) {
		return true
	}
	if prefix, ok := strings.CutSuffix(path, ".value"); ok {
		name := strings.ToUpper(flat[prefix+".name"])
		if slices.ContainsFunc(configCredentialEnvSuffixes, func(suffix string) bool { return strings.HasSuffix(name, suffix) }) {
			return true
		}
	}
	return configCredentialURL.MatchString(flat[path])
}

// newDeploymentConfig is t's configuration, given the Helm values its
// HelmRelease carries.
func newDeploymentConfig(t *benchv1alpha1.AuthentikDeployment, values map[string]any) deploymentConfig {
	flat := specConfig(t)
	flattenConfig("values", values, flat)

	config := deploymentConfig{
		names:         deploymentNames(t),
		facts:         make(map[string]configFact, len(flat)),
		chartResolved: flat[configChartVersion] != configChartUnresolved,
	}
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
			config.facts[path] = configFact{value: value, credential: true}
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
		configChartVersion:     render.ChartVersion(t),
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

// configNameRole is what a deployment's name is the name of. A value made of
// one side's name matches one made of the other side's only where both name
// the same thing: one deployment's namespace is not taken for the other's
// database service.
type configNameRole uint8

const (
	configNameDeployment configNameRole = 1 << iota
	configNameNamespace
	configNameTarget
	configNameRelease
	configNameEmbeddedPostgres
	configNameCNPG
	configNameCNPGService
)

// deploymentNames are t's own names and the ones the operator derives from
// them into its Helm values, exactly: BoundedName hashes a long one, so the
// derived names can't be recognised by the deployment's name inside them. A
// name used for several things (a child deployment's namespace is its name)
// has every one of their roles.
func deploymentNames(t *benchv1alpha1.AuthentikDeployment) map[string]configNameRole {
	targetName := t.Spec.TargetName
	if targetName == "" {
		targetName = t.Name
	}
	names := map[string]configNameRole{}
	for role, name := range map[configNameRole]string{
		configNameDeployment: t.Name,
		configNameNamespace:  t.Spec.Namespace,
		configNameTarget:     targetName,
		configNameRelease:    render.AuthentikFullname(render.HelmReleaseName(t)),
		// The embedded postgres' fullnameOverride and authentik's host for it.
		configNameEmbeddedPostgres: render.EmbeddedPostgresStatefulSetName(t),
		configNameCNPG:             render.CNPGClusterName(t),
		// The host CNPG's app secret gives authentik: the read-write service.
		configNameCNPGService: render.CNPGClusterName(t) + "-rw",
	} {
		if name != "" {
			names[name] |= role
		}
	}
	return names
}

// configToken is a piece of a value: one of the deployment's own names
// (role set), or a run of name characters or of separators between them.
type configToken struct {
	text string
	role configNameRole
}

// configTokens splits s into the deployment's own names, and around them the
// runs of letters, digits and '-' that a Kubernetes name is made of and the
// separators between them: "a-postgresql.ns:5432" is "a-postgresql", ".",
// "ns", ":", "5432". One of names is a token of its own wherever it stands
// as a whole word, whatever characters it is made of: a target called
// "release/2026.6" is one token, not five.
func configTokens(s string, names map[string]configNameRole) []configToken {
	var tokens []configToken
	start := 0
	flush := func(end int) {
		if end > start {
			tokens = append(tokens, configToken{text: s[start:end]})
		}
		start = end
	}
	for i := 0; i < len(s); {
		if name := configNameAt(s, i, names); name != "" {
			flush(i)
			tokens = append(tokens, configToken{text: name, role: names[name]})
			i += len(name)
			start = i
			continue
		}
		if i > start && configNameChar(s[i]) != configNameChar(s[start]) {
			flush(i)
		}
		i++
	}
	flush(len(s))
	return tokens
}

// configNameAt is the longest of names that stands at s[i:] as a whole word:
// not in the middle of a longer run of name characters.
func configNameAt(s string, i int, names map[string]configNameRole) string {
	if i > 0 && configNameChar(s[i-1]) && configNameChar(s[i]) {
		return ""
	}
	found := ""
	for name := range names {
		if len(name) <= len(found) || !strings.HasPrefix(s[i:], name) {
			continue
		}
		end := i + len(name)
		if end < len(s) && configNameChar(s[end]) && configNameChar(s[end-1]) {
			continue
		}
		found = name
	}
	return found
}

func configNameChar(c byte) bool {
	return c == '-' || ('0' <= c && c <= '9') || ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z')
}

// sameConfigValue reports whether a, from a deployment called aNames, and b,
// from one called bNames, are the same setting: equal, or equal but for
// tokens that are each side's own name for the same thing. Only whole
// tokens count, so neither a name inside a word ("pr" in "primary", "test"
// in "x-benchmark-test") nor a name that is also an ordinary value on the
// other side ("authentik" as one namespace and a database name) is taken
// for the deployment's.
func sameConfigValue(a string, aNames map[string]configNameRole, b string, bNames map[string]configNameRole) bool {
	if a == b {
		return true
	}
	at, bt := configTokens(a, aNames), configTokens(b, bNames)
	if len(at) != len(bt) {
		return false
	}
	for i := range at {
		if at[i].text != bt[i].text && at[i].role&bt[i].role == 0 {
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
	differences := []ConfigDifference{}
	for path := range paths {
		// Compared against the first deployment that knows its value.
		var want configFact
		var wantNames map[string]configNameRole
		known := false
		same := true
		values := map[string]string{}
		credential := false
		for _, target := range targets {
			config := configs[target]
			fact, ok := config.facts[path]
			switch {
			case fact.unknown:
				continue
			case !ok:
				fact.value = configEmptyObject
			case fact.credential:
				credential = true
				values[target] = configSet
			default:
				values[target] = fact.value
			}
			if !known {
				want, wantNames, known = fact, config.names, true
				continue
			}
			same = same && fact.credential == want.credential && sameConfigValue(want.value, wantNames, fact.value, config.names)
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
