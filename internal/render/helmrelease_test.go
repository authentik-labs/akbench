package render

import (
	"encoding/json"
	"testing"

	helmv2 "github.com/fluxcd/helm-controller/api/v2"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
)

func exampleDeployment() *benchv1alpha1.AuthentikDeployment {
	return &benchv1alpha1.AuthentikDeployment{
		Name: "authentik-1",
		Spec: benchv1alpha1.AuthentikDeploymentSpec{
			Namespace:  "authentik-1",
			ClusterRef: corev1.LocalObjectReference{Name: "wc-1"},
			Server:     benchv1alpha1.WorkloadSpec{Replicas: 2},
			Worker:     benchv1alpha1.WorkloadSpec{Replicas: 1},
			Postgres:   benchv1alpha1.PostgresSpec{Mode: benchv1alpha1.PostgresEmbedded},
		},
	}
}

func decodeValues(t *testing.T, raw json.RawMessage) AuthentikDeploymentHelmValues {
	t.Helper()
	var v AuthentikDeploymentHelmValues
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("unmarshal values: %v", err)
	}
	return v
}

// TestHelmValuesTargetsAuthentikPool guards the node-pool baseline wiring:
// server, worker and the embedded-postgres primary all get a nodeSelector/
// toleration pair matching the cluster's own authentik-role pool, regardless
// of PlacementMode (Default here - the simplest case, where the old
// Dedicated-only NodeSelector/Tolerations functions would have returned
// nothing at all).
func TestHelmValuesTargetsAuthentikPool(t *testing.T) {
	topo := exampleDeployment()
	wc := &benchv1alpha1.WorkloadCluster{Spec: benchv1alpha1.WorkloadClusterSpec{
		WorkerPools: []benchv1alpha1.WorkerPoolSpec{{Name: "ak", Role: benchv1alpha1.WorkerPoolRoleAuthentik}},
	}}
	raw, err := HelmValues(topo, wc, "the-secret-key", &PostgresConnection{Password: "pw"}, "http://pyroscope.example:4040", "http://tempo.example:4318")
	if err != nil {
		t.Fatalf("HelmValues: %v", err)
	}
	values := decodeValues(t, raw.Raw)

	for _, section := range []struct {
		name string
		w    map[string]any
	}{{"server", values.Server}, {"worker", values.Worker}} {
		sel, _ := section.w["nodeSelector"].(map[string]any)
		if sel[NodePoolLabel] != "authentik" {
			t.Errorf("%s.nodeSelector[%s] = %v, want authentik", section.name, NodePoolLabel, sel[NodePoolLabel])
		}
		if _, ok := section.w["tolerations"]; !ok {
			t.Errorf("%s.tolerations not set", section.name)
		}
	}

	primary, _ := values.Postgres["primary"].(map[string]any)
	if primary == nil {
		t.Fatal("postgresql.primary not set")
	}
	sel, _ := primary["nodeSelector"].(map[string]any)
	if sel[NodePoolLabel] != "authentik" {
		t.Errorf("postgresql.primary.nodeSelector[%s] = %v, want authentik", NodePoolLabel, sel[NodePoolLabel])
	}
	if _, ok := primary["tolerations"]; !ok {
		t.Error("postgresql.primary.tolerations not set")
	}
}

// TestHelmValuesMergesDedicatedNodeSelectorWithPool confirms the two
// mechanisms combine rather than one overriding the other: a Dedicated-mode
// user-supplied nodeSelector/toleration must survive alongside the pool's
// own baseline, not replace it.
func TestHelmValuesMergesDedicatedNodeSelectorWithPool(t *testing.T) {
	topo := exampleDeployment()
	topo.Spec.Placement = benchv1alpha1.PlacementSpec{
		Mode:         benchv1alpha1.PlacementDedicated,
		NodeSelector: map[string]string{"disk": "nvme"},
		Tolerations:  []corev1.Toleration{{Key: "dedicated", Operator: corev1.TolerationOpExists}},
	}
	wc := &benchv1alpha1.WorkloadCluster{Spec: benchv1alpha1.WorkloadClusterSpec{
		WorkerPools: []benchv1alpha1.WorkerPoolSpec{{Name: "ak", Role: benchv1alpha1.WorkerPoolRoleAuthentik}},
	}}
	raw, err := HelmValues(topo, wc, "the-secret-key", &PostgresConnection{Password: "pw"}, "http://pyroscope.example:4040", "http://tempo.example:4318")
	if err != nil {
		t.Fatalf("HelmValues: %v", err)
	}
	values := decodeValues(t, raw.Raw)

	sel, _ := values.Server["nodeSelector"].(map[string]any)
	if sel[NodePoolLabel] != "authentik" || sel["disk"] != "nvme" {
		t.Errorf("server.nodeSelector = %v, want both the pool's own key and the Dedicated-mode one", sel)
	}
	tol, _ := values.Server["tolerations"].([]any)
	if len(tol) != 2 {
		t.Errorf("server.tolerations = %v, want 2 (the pool's own plus the Dedicated-mode one)", tol)
	}
}

func TestHelmValuesEmbedded(t *testing.T) {
	topo := exampleDeployment()
	raw, err := HelmValues(topo, nil, "the-secret-key", &PostgresConnection{Password: "pw"}, "http://pyroscope.example:4040", "http://tempo.example:4318")
	if err != nil {
		t.Fatalf("HelmValues: %v", err)
	}
	values := decodeValues(t, raw.Raw)

	if values.Server["replicas"] != float64(2) {
		t.Errorf("server.replicas = %v, want 2", values.Server["replicas"])
	}
	if values.Postgres["enabled"] != true {
		t.Errorf("postgresql.enabled = %v, want true", values.Postgres["enabled"])
	}
	auth, _ := values.Postgres["auth"].(map[string]any)
	if auth["password"] != "pw" {
		t.Errorf("postgresql.auth.password = %v, want pw", auth["password"])
	}
	akPg, _ := values.Authentik["postgresql"].(map[string]any)
	if akPg["password"] != "pw" {
		t.Errorf("authentik.postgresql.password = %v, want pw (must match the bundled subchart's own password)", akPg["password"])
	}
	// authentik.secret_key as a literal value, not authentik.existingSecret:
	// existingSecret makes the chart skip rendering its own config Secret
	// entirely, silently dropping postgresql.*/log_level/error_reporting too
	// (verified against a live install of this exact bug).
	if values.Authentik["secret_key"] != "the-secret-key" {
		t.Errorf("authentik.secret_key = %v, want the-secret-key", values.Authentik["secret_key"])
	}
	if _, ok := values.Authentik["existingSecret"]; ok {
		t.Error("authentik.existingSecret must not be set")
	}
	if _, ok := values.Postgres["tls"]; ok {
		t.Error("postgresql.tls must not be set without embedded.tls")
	}
}

func TestHelmValuesEmbeddedTLS(t *testing.T) {
	topo := exampleDeployment()
	topo.Spec.Postgres.Embedded = &benchv1alpha1.EmbeddedPostgresSpec{TLS: true}
	raw, err := HelmValues(topo, nil, "k", &PostgresConnection{Password: "pw"}, "", "")
	if err != nil {
		t.Fatalf("HelmValues: %v", err)
	}
	values := decodeValues(t, raw.Raw)

	tls, _ := values.Postgres["tls"].(map[string]any)
	if tls["enabled"] != true || tls["autoGenerated"] != true {
		t.Errorf("postgresql.tls = %v, want enabled+autoGenerated", tls)
	}
	akPg, _ := values.Authentik["postgresql"].(map[string]any)
	if akPg["sslmode"] != "require" || akPg["password"] != "pw" {
		t.Errorf("authentik.postgresql = %v, want sslmode=require and password pw", akPg)
	}
}

// TestHelmValuesEnablesMetrics guards the fix for docs/grafana-dashboards.md's
// documented gap ("authentik.json shows resource use, not
// authentik's own metrics... nothing scrapes the release's /metrics
// endpoint until the operator renders a ServiceMonitor for it"): server/
// worker/bundled-postgres metrics + their own chart-native ServiceMonitors
// must all be turned on, for Embedded mode.
func TestHelmValuesEnablesMetrics(t *testing.T) {
	topo := exampleDeployment()
	raw, err := HelmValues(topo, nil, "secret", &PostgresConnection{Password: "pw"}, "http://pyroscope.example:4040", "http://tempo.example:4318")
	if err != nil {
		t.Fatalf("HelmValues: %v", err)
	}
	values := decodeValues(t, raw.Raw)

	assertMetricsEnabled := func(t *testing.T, section map[string]any, path string) {
		t.Helper()
		metrics, ok := section["metrics"].(map[string]any)
		if !ok {
			t.Fatalf("%s.metrics is not a map: %v", path, section["metrics"])
		}
		if metrics["enabled"] != true {
			t.Errorf("%s.metrics.enabled = %v, want true", path, metrics["enabled"])
		}
		sm, ok := metrics["serviceMonitor"].(map[string]any)
		if !ok || sm["enabled"] != true {
			t.Errorf("%s.metrics.serviceMonitor.enabled = %v, want true", path, metrics["serviceMonitor"])
		}
	}
	assertMetricsEnabled(t, values.Server, "server")
	assertMetricsEnabled(t, values.Worker, "worker")
	assertMetricsEnabled(t, values.Postgres, "postgresql")
}

// TestHelmValuesNoServiceMonitorWithoutClusterRef guards a real regression
// caught live: a deployment with no clusterRef falls back to the management
// cluster (see AuthentikDeploymentReconciler), which never gets
// kube-prometheus-stack's ServiceMonitor CRD (only a WorkloadCluster does -
// see ensurePrometheus). Setting server/worker/postgresql.metrics.serviceMonitor.enabled
// unconditionally broke a scratch AuthentikDeployment with clusterRef unset:
// its whole HelmRelease failed to install ("no matches for kind
// ServiceMonitor in version monitoring.coreos.com/v1"), not just its
// metrics - the chart's own template has no fallback for a missing CRD.
// metrics.enabled itself must stay on (it needs no CRD, just a Service).
func TestHelmValuesNoServiceMonitorWithoutClusterRef(t *testing.T) {
	topo := exampleDeployment()
	topo.Spec.ClusterRef = corev1.LocalObjectReference{}
	raw, err := HelmValues(topo, nil, "secret", &PostgresConnection{Password: "pw"}, "http://pyroscope.example:4040", "http://tempo.example:4318")
	if err != nil {
		t.Fatalf("HelmValues: %v", err)
	}
	values := decodeValues(t, raw.Raw)

	assertNoServiceMonitor := func(t *testing.T, section map[string]any, path string) {
		t.Helper()
		metrics, ok := section["metrics"].(map[string]any)
		if !ok {
			t.Fatalf("%s.metrics is not a map: %v", path, section["metrics"])
		}
		if metrics["enabled"] != true {
			t.Errorf("%s.metrics.enabled = %v, want true (needs no CRD)", path, metrics["enabled"])
		}
		sm, ok := metrics["serviceMonitor"].(map[string]any)
		if !ok || sm["enabled"] != false {
			t.Errorf("%s.metrics.serviceMonitor.enabled = %v, want false (no clusterRef -> management cluster -> no ServiceMonitor CRD)", path, metrics["serviceMonitor"])
		}
	}
	assertNoServiceMonitor(t, values.Server, "server")
	assertNoServiceMonitor(t, values.Worker, "worker")
	assertNoServiceMonitor(t, values.Postgres, "postgresql")
}

// TestHelmValuesPlacementUsesGlobalAffinityPreset guards against regressing
// into setting server.affinity/worker.affinity directly: verified against a
// live chart install that those fields are a raw corev1.Affinity override in
// this chart (not the preset mechanism), so a preset string there fails with
// "cannot unmarshal string into Go struct field ... PodAntiAffinity".
// affinityOf decodes a server/worker values map's "affinity" key (always a
// *corev1.Affinity now, for every PlacementMode - see ServerWorkerAffinity)
// back into a plain map for assertions.
func affinityOf(t *testing.T, w map[string]any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(w["affinity"])
	if err != nil {
		t.Fatalf("affinity: marshal: %v", err)
	}
	if !json.Valid(raw) || string(raw) == "null" {
		t.Fatalf("affinity was not set")
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("affinity: unmarshal: %v", err)
	}
	return decoded
}

// TestHelmValuesDedicatedSetsServerWorkerAffinity guards that Dedicated mode
// now renders server.affinity/worker.affinity directly (a required
// anti-affinity term for this release's own pods, plus the
// always-present cross-deployment term) instead of the chart's own
// global.affinity.podAntiAffinity preset, which server.affinity/
// worker.affinity being set at all makes the chart skip entirely.
func TestHelmValuesDedicatedSetsServerWorkerAffinity(t *testing.T) {
	topo := exampleDeployment()
	topo.Spec.Placement = benchv1alpha1.PlacementSpec{Mode: benchv1alpha1.PlacementDedicated}
	raw, err := HelmValues(topo, nil, "secret", &PostgresConnection{Password: "pw"}, "http://pyroscope.example:4040", "http://tempo.example:4318")
	if err != nil {
		t.Fatalf("HelmValues: %v", err)
	}
	values := decodeValues(t, raw.Raw)

	if _, ok := values.Global["affinity"]; ok {
		t.Error("global.affinity must not be set any more - the chart's preset mechanism is no longer used")
	}
	additionalLabels, _ := values.Global["additionalLabels"].(map[string]any)
	if additionalLabels["bench.goauthentik.io/deployment"] != topo.Name {
		t.Errorf("global.additionalLabels[deployment] = %v, want %q", additionalLabels, topo.Name)
	}
	for _, name := range []string{"server", "worker"} {
		w := values.Server
		if name == "worker" {
			w = values.Worker
		}
		decoded := affinityOf(t, w)
		podAntiAffinity, _ := decoded["podAntiAffinity"].(map[string]any)
		terms, _ := podAntiAffinity["requiredDuringSchedulingIgnoredDuringExecution"].([]any)
		if len(terms) != 2 {
			t.Errorf("%s.affinity: got %d required anti-affinity terms, want 2 (cross-deployment + own-release)", name, len(terms))
		}
	}
}

// TestHelmValuesPacked guards that Packed mode's own pack-together
// affinity coexists with the always-present cross-deployment anti-affinity
// term in the same server.affinity/worker.affinity value.
func TestHelmValuesPacked(t *testing.T) {
	topo := exampleDeployment()
	topo.Spec.Placement = benchv1alpha1.PlacementSpec{Mode: benchv1alpha1.PlacementPacked}
	raw, err := HelmValues(topo, nil, "secret", &PostgresConnection{Password: "pw"}, "http://pyroscope.example:4040", "http://tempo.example:4318")
	if err != nil {
		t.Fatalf("HelmValues: %v", err)
	}
	values := decodeValues(t, raw.Raw)

	for name, w := range map[string]map[string]any{"server": values.Server, "worker": values.Worker} {
		decoded := affinityOf(t, w)
		podAffinity, _ := decoded["podAffinity"].(map[string]any)
		if podAffinity == nil {
			t.Fatalf("%s.affinity.podAffinity missing: %v", name, decoded)
		}
		podAntiAffinity, _ := decoded["podAntiAffinity"].(map[string]any)
		terms, _ := podAntiAffinity["requiredDuringSchedulingIgnoredDuringExecution"].([]any)
		if len(terms) != 1 {
			t.Errorf("%s.affinity: got %d required anti-affinity terms, want 1 (cross-deployment only)", name, len(terms))
		}
	}
}

// TestHelmValuesEmbeddedPostgresGetsCrossDeploymentAffinity guards that the
// bundled postgresql subchart also gets the deployment label and
// cross-deployment anti-affinity - it's part of a deployment's own footprint
// just as much as server/worker, so it must never share a node with
// another deployment's pods either.
func TestHelmValuesEmbeddedPostgresGetsCrossDeploymentAffinity(t *testing.T) {
	topo := exampleDeployment()
	raw, err := HelmValues(topo, nil, "secret", &PostgresConnection{Password: "pw"}, "http://pyroscope.example:4040", "http://tempo.example:4318")
	if err != nil {
		t.Fatalf("HelmValues: %v", err)
	}
	values := decodeValues(t, raw.Raw)

	commonLabels, _ := values.Postgres["commonLabels"].(map[string]any)
	if commonLabels["bench.goauthentik.io/deployment"] != topo.Name {
		t.Errorf("postgresql.commonLabels[deployment] = %v, want %q", commonLabels, topo.Name)
	}
	primary, _ := values.Postgres["primary"].(map[string]any)
	decoded := affinityOf(t, primary)
	podAntiAffinity, _ := decoded["podAntiAffinity"].(map[string]any)
	terms, _ := podAntiAffinity["requiredDuringSchedulingIgnoredDuringExecution"].([]any)
	if len(terms) != 1 {
		t.Errorf("postgresql.primary.affinity: got %d required anti-affinity terms, want 1", len(terms))
	}
}

func TestHelmValuesExternalDisablesBundledPostgres(t *testing.T) {
	topo := exampleDeployment()
	topo.Spec.Postgres = benchv1alpha1.PostgresSpec{
		Mode:     benchv1alpha1.PostgresExternal,
		External: &benchv1alpha1.ExternalPostgresSpec{},
	}
	raw, err := HelmValues(topo, nil, "secret", &PostgresConnection{
		Host: "pg.example.com", Port: 5432, Database: "authentik", User: "authentik", Password: "s3cret",
	}, "http://pyroscope.example:4040", "http://tempo.example:4318")
	if err != nil {
		t.Fatalf("HelmValues: %v", err)
	}
	values := decodeValues(t, raw.Raw)

	if values.Postgres["enabled"] != false {
		t.Errorf("postgresql.enabled = %v, want false for External mode", values.Postgres["enabled"])
	}
	if _, hasAuth := values.Postgres["auth"]; hasAuth {
		t.Error("postgresql.auth should not be set for External mode")
	}
	akPg, _ := values.Authentik["postgresql"].(map[string]any)
	if akPg["host"] != "pg.example.com" || akPg["password"] != "s3cret" {
		t.Errorf("authentik.postgresql = %+v, want the external connection", akPg)
	}
}

func TestHelmReleaseUsesExplicitReleaseName(t *testing.T) {
	topo := exampleDeployment()
	raw, err := HelmValues(topo, nil, "secret", nil, "http://pyroscope.example:4040", "http://tempo.example:4318")
	if err != nil {
		t.Fatalf("HelmValues: %v", err)
	}
	hr := HelmRelease(topo, "", raw)

	// Flux's own default release-name composition is "[TargetNamespace-]Name",
	// which would double up here since both are "authentik-1" - pinning
	// ReleaseName explicitly is what keeps ServiceEndpoint's
	// "<release>-authentik-server" naming correct.
	if hr.Spec.ReleaseName != "authentik-1" {
		t.Errorf("releaseName = %q, want %q", hr.Spec.ReleaseName, "authentik-1")
	}
	if got, want := ServiceEndpoint(topo), "authentik-1-authentik-server.authentik-1.svc.cluster.local:80"; got != want {
		t.Errorf("ServiceEndpoint() = %q, want %q", got, want)
	}
}

func TestHelmReleaseChartVersion(t *testing.T) {
	for name, tc := range map[string]struct {
		spec, status, want string
	}{
		"first install follows the latest chart": {"", "", "*"},
		"unpinned stays on the installed chart":  {"", "2026.8.3", "2026.8.3"},
		"pinned version wins":                    {"2026.8.4", "2026.8.3", "2026.8.4"},
	} {
		topo := exampleDeployment()
		topo.Spec.Chart.Version = tc.spec
		topo.Status.ChartVersion = tc.status
		if got := HelmRelease(topo, "", nil).Spec.Chart.Spec.Version; got != tc.want {
			t.Errorf("%s: chart version = %q, want %q", name, got, tc.want)
		}
	}
}

func envValue(t *testing.T, w map[string]any, name string) (string, bool) {
	t.Helper()
	raw, err := json.Marshal(w["env"])
	if err != nil {
		t.Fatalf("marshal env: %v", err)
	}
	var entries []map[string]string
	if err := json.Unmarshal(raw, &entries); err != nil {
		t.Fatalf("unmarshal env: %v", err)
	}
	for _, e := range entries {
		if e["name"] == name {
			return e["value"], true
		}
	}
	return "", false
}

// TestHelmValuesPyroscopeAlwaysSet guards the chart's actual values.yaml
// having no pyroscope key at all (checked against a live pulled chart
// tarball) - profiling only reaches authentik via a raw env var, on both
// workloads, unconditionally (no separate "pyroscope enabled" toggle exists,
// matching the Ansible harness this project replaces).
func TestHelmValuesPyroscopeAlwaysSet(t *testing.T) {
	topo := exampleDeployment()
	raw, err := HelmValues(topo, nil, "secret", &PostgresConnection{Password: "pw"}, "http://pyroscope.example:4040", "http://tempo.example:4318")
	if err != nil {
		t.Fatalf("HelmValues: %v", err)
	}
	values := decodeValues(t, raw.Raw)

	for name, w := range map[string]map[string]any{"server": values.Server, "worker": values.Worker} {
		got, ok := envValue(t, w, "AUTHENTIK_PYROSCOPE_HOST")
		if !ok || got != "http://pyroscope.example:4040" {
			t.Errorf("%s env AUTHENTIK_PYROSCOPE_HOST = %q, ok=%v, want http://pyroscope.example:4040", name, got, ok)
		}
	}
}

// TestHelmValuesTracingOnlyWhenErrorReportingEnabled guards the chart's
// error_reporting section having no OTLP endpoint/sample-rate key either
// (only enabled/environment/send_pii) - both go in as raw env vars, and only
// when errorReporting.enabled is set, matching env.j2's {% if
// authentik_error_reporting %} guard in the Ansible harness this project
// replaces.
func TestHelmValuesTracingOnlyWhenErrorReportingEnabled(t *testing.T) {
	topo := exampleDeployment()
	topo.Spec.Authentik.ErrorReporting.Enabled = ptr.To(false)
	if _, ok := envValue(t, mustValues(t, topo).Server, "AUTHENTIK_ERROR_REPORTING__OTEL_ENDPOINT"); ok {
		t.Error("AUTHENTIK_ERROR_REPORTING__OTEL_ENDPOINT must not be set when errorReporting.enabled is false")
	}

	topo.Spec.Authentik.ErrorReporting = benchv1alpha1.ErrorReportingSpec{SampleRate: "0.25"}
	values := mustValues(t, topo)

	er, _ := values.Authentik["error_reporting"].(map[string]any)
	if er["enabled"] != true {
		t.Errorf("authentik.error_reporting.enabled = %v, want true", er["enabled"])
	}
	if er["environment"] != "authentik-1" {
		t.Errorf("authentik.error_reporting.environment = %v, want the deployment's target name", er["environment"])
	}
	for name, w := range map[string]map[string]any{"server": values.Server, "worker": values.Worker} {
		if got, ok := envValue(t, w, "AUTHENTIK_ERROR_REPORTING__OTEL_ENDPOINT"); !ok || got != "http://tempo.example:4318" {
			t.Errorf("%s env AUTHENTIK_ERROR_REPORTING__OTEL_ENDPOINT = %q, ok=%v", name, got, ok)
		}
		if got, ok := envValue(t, w, "AUTHENTIK_ERROR_REPORTING__SAMPLE_RATE"); !ok || got != "0.25" {
			t.Errorf("%s env AUTHENTIK_ERROR_REPORTING__SAMPLE_RATE = %q, ok=%v, want 0.25", name, got, ok)
		}
	}
}

func mustValues(t *testing.T, topo *benchv1alpha1.AuthentikDeployment) AuthentikDeploymentHelmValues {
	t.Helper()
	raw, err := HelmValues(topo, nil, "secret", &PostgresConnection{Password: "pw"}, "http://pyroscope.example:4040", "http://tempo.example:4318")
	if err != nil {
		t.Fatalf("HelmValues: %v", err)
	}
	return decodeValues(t, raw.Raw)
}

// Every HelmRelease this package renders must retry a failed install or
// upgrade on its own (retryOnFailure) - helm-controller's default stalls
// it for good after one failure.
func TestHelmReleasesRetryOnFailure(t *testing.T) {
	wc := addonTestCluster("c1")
	for name, hr := range map[string]*helmv2.HelmRelease{
		"authentik":             HelmRelease(exampleDeployment(), "c1-kubeconfig", nil),
		"cni":                   CNIHelmRelease(wc),
		"csi-incus":             CSIIncusHelmRelease(wc, nil),
		"metrics-server":        MetricsServerHelmRelease(wc, nil),
		"kube-prometheus-stack": KubePrometheusStackHelmRelease(wc, nil),
		"descheduler":           DeschedulerHelmRelease(wc, nil),
		"cluster-autoscaler":    ClusterAutoscalerHelmRelease(wc, nil),
	} {
		install, upgrade := hr.Spec.Install, hr.Spec.Upgrade
		if install == nil || install.Strategy == nil || install.Strategy.Name != string(helmv2.ActionStrategyRetryOnFailure) || install.Strategy.RetryInterval == nil {
			t.Errorf("%s: install strategy = %+v", name, install)
		}
		if upgrade == nil || upgrade.Strategy == nil || upgrade.Strategy.Name != string(helmv2.ActionStrategyRetryOnFailure) || upgrade.Strategy.RetryInterval == nil {
			t.Errorf("%s: upgrade strategy = %+v", name, upgrade)
		}
	}
	// Existing install/upgrade settings survive.
	kps := KubePrometheusStackHelmRelease(wc, nil)
	if !kps.Spec.Install.CreateNamespace || kps.Spec.Install.CRDs != helmv2.CreateReplace || kps.Spec.Upgrade.CRDs != helmv2.CreateReplace {
		t.Errorf("kube-prometheus-stack lost its install/upgrade settings: %+v %+v", kps.Spec.Install, kps.Spec.Upgrade)
	}
}
