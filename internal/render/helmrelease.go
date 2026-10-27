package render

import (
	"encoding/json"
	"fmt"
	"time"

	helmv2 "github.com/fluxcd/helm-controller/api/v2"
	fluxmeta "github.com/fluxcd/pkg/apis/meta"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
)

// HelmRepositoryName is shared across every AuthentikDeployment, since they all
// install from the same chart repository.
const HelmRepositoryName = "authentik"

// chartName is the chart to install from that repository - coincidentally
// the same string as HelmRepositoryName, but a different thing (the
// HelmRepository object's own name vs. the chart's name within it), so kept
// as its own constant rather than reusing HelmRepositoryName.
const chartName = "authentik"

// Helm values map keys the chart's own schema defines, repeated across
// server/worker/postgresql sections of HelmValues below.
const (
	valuesKeyReplicas = "replicas"
	valuesKeyPassword = "password"
	valuesKeyName     = "name"

	valuesKeyRelabelings = "relabelings"
)

const defaultChartRepo = "https://charts.goauthentik.io"

// HelmRepository returns the (singleton, namespace-scoped) HelmRepository
// pointing at the authentik chart repo. Callers apply it once per namespace
// and share it across deployments.
func HelmRepository(namespace string, spec benchv1alpha1.ChartSpec) *sourcev1.HelmRepository {
	url := spec.Repo
	if url == "" {
		url = defaultChartRepo
	}
	return &sourcev1.HelmRepository{
		Name:      HelmRepositoryName,
		Namespace: namespace,
		Spec: sourcev1.HelmRepositorySpec{
			URL:      url,
			Interval: metav1.Duration{Duration: helmRepoInterval},
		},
	}
}

const helmRepoInterval = 10 * time.Minute

const helmInstallTimeout = 15 * time.Minute

// AuthentikDeploymentHelmValues is what AuthentikDeploymentHelmRelease renders
// into spec.values. Exported so unit/contract tests can assert on the
// concrete shape without re-parsing JSON.
type AuthentikDeploymentHelmValues struct {
	Global    map[string]any `json:"global,omitempty"`
	Server    map[string]any `json:"server"`
	Worker    map[string]any `json:"worker"`
	Authentik map[string]any `json:"authentik"`
	Postgres  map[string]any `json:"postgresql"`
}

// HelmValues builds the authentik chart values for a deployment, per the
// chart's actual schema (charts.goauthentik.io/authentik, values.yaml):
// global.{image,affinity}, server/worker.{replicas,resources,nodeSelector,
// tolerations}, authentik.{secret_key,log_level,error_reporting,
// postgresql.*}, and top-level postgresql.enabled/auth for the bundled
// subchart.
//
// secretKey is AUTHENTIK_SECRET_KEY's plaintext value (generated and cached
// by the caller, see internal/controller's ensureRandomSecret) - the chart
// does not generate one itself, and authentik refuses to start without it.
// pgConn carries the resolved Postgres connection: for Embedded mode only
// Password is set (paired with the bundled subchart's own generated
// auth.password so both sides agree on the same value); for CNPG/External
// modes the full connection is set and the bundled subchart stays disabled.
// pyroscopeHost/tracingEndpoint are the caller's already-defaulted-or-
// overridden values (see AuthentikDeploymentReconciler's
// defaultPyroscopeHost/defaultTracingEndpoint) - neither has a values.yaml
// hook in the chart (verified against the chart's actual values.yaml: no
// pyroscope key at all, and error_reporting only has enabled/environment/
// send_pii, not an OTLP endpoint or sample rate), so both go in as raw
// AUTHENTIK_* env vars, the same names the Ansible harness this project
// replaces used (roles/authentik/templates/env.j2).
// Blueprint fixtures are deliberately never mounted into the server/worker
// pod template at all - see BlueprintJob's own doc comment for why: a PVC
// mounted onto a long-lived Deployment caused a real RWO-volume
// rolling-update deadlock (old and new pod both wanting the same
// single-attach volume) every time the pod template changed for any
// unrelated reason. A one-shot Job mounts BlueprintPVC instead, and
// releases it as soon as the import completes.
func HelmValues(t *benchv1alpha1.AuthentikDeployment, wc *benchv1alpha1.WorkloadCluster, secretKey string, pgConn *PostgresConnection, pyroscopeHost, tracingEndpoint string) (*apiextensionsv1.JSON, error) {
	embedded := t.Spec.Postgres.Mode == benchv1alpha1.PostgresEmbedded
	// poolSelector/poolTolerations are this deployment's own baseline
	// authentik-pool targeting (see AuthentikPoolPlacement's own doc
	// comment) - applied to every one of this deployment's own pods
	// (server, worker, embedded-postgres below) regardless of
	// PlacementMode, unlike PlacementSpec.NodeSelector/Tolerations further
	// down, which stay a Dedicated-only, user-specified addition on top.
	poolSelector, poolTolerations := AuthentikPoolPlacement(wc)
	// hasServiceMonitorCRD: the ServiceMonitor CRD only exists on a cluster
	// WorkloadClusterReconciler installed kube-prometheus-stack onto (every
	// WorkloadCluster, once its control plane is reachable - see
	// ensurePrometheus) - the management cluster itself never gets one.
	// clusterRef is exactly the signal for which case this is: it always
	// names a WorkloadCluster (see internal/multicluster.Tracker), and an
	// unset clusterRef falls back to the management cluster. Setting
	// metrics.serviceMonitor.enabled unconditionally broke every
	// AuthentikDeployment with no clusterRef - confirmed live: a scratch
	// AuthentikDeployment with clusterRef unset failed Helm install entirely
	// ("no matches for kind ServiceMonitor in version monitoring.coreos.com/v1"),
	// since the chart's own template unconditionally emits a ServiceMonitor
	// object once asked to, with no fallback if the CRD is missing.
	hasServiceMonitorCRD := t.Spec.ClusterRef.Name != ""
	// authentikMetrics turns on the chart's own metrics Service (port 9300)
	// unconditionally (needs no CRD) and its own ServiceMonitor for it
	// (server.metrics{,.serviceMonitor}/worker.metrics{,.serviceMonitor} -
	// confirmed directly against the chart's real values.yaml, both
	// sections identically shaped) only when the CRD actually exists,
	// rather than this project hand-building a competing ServiceMonitor:
	// the chart already owns the metrics Service's labels/selector, so its
	// own ServiceMonitor is guaranteed to match them.
	targetName := t.Spec.TargetName
	if targetName == "" {
		targetName = t.Name
	}
	authentikMetrics := map[string]any{
		"enabled": true,
		"serviceMonitor": map[string]any{
			"enabled":            hasServiceMonitorCRD,
			valuesKeyRelabelings: scrapeRelabelings("authentik", targetName),
		},
	}
	values := AuthentikDeploymentHelmValues{
		Server: map[string]any{
			valuesKeyReplicas: t.Spec.Server.Replicas,
			"metrics":         authentikMetrics,
		},
		Worker: map[string]any{
			valuesKeyReplicas: t.Spec.Worker.Replicas,
			"metrics":         authentikMetrics,
		},
		// secret_key as a literal value, not authentik.existingSecret: the
		// chart only renders its authentik.env-derived config Secret (which
		// is where postgresql.*/log_level/error_reporting all actually get
		// baked into AUTHENTIK_* env vars) when existingSecret.secretName is
		// unset - existingSecret means "you own that whole secret yourself,"
		// not "just override the key" (verified against a live install of
		// this exact bug: postgresql.host silently never made it to the pod).
		Authentik: map[string]any{
			"secret_key": secretKey,
		},
		Postgres: map[string]any{
			"enabled": embedded,
		},
	}
	if embedded {
		// The bundled postgresql subchart is Bitnami-style: commonLabels
		// merges into its own pod labels the same way global.additionalLabels
		// does for server/worker (templates/primary/statefulset.yaml's
		// $podLabels), and primary.affinity is the same
		// mutually-exclusive-with-its-own-preset raw override server.affinity/
		// worker.affinity is - confirmed against that subchart's own
		// templates. Only the cross-deployment term is needed here: embedded
		// mode is always a single instance, so there's no intra-postgres
		// spreading/packing to replicate the way ServerWorkerAffinity does
		// for Dedicated/Packed.
		values.Postgres["commonLabels"] = DeploymentLabels(t.Name)
		primary := map[string]any{
			"affinity": &corev1.Affinity{
				PodAntiAffinity: &corev1.PodAntiAffinity{
					RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{
						crossDeploymentAntiAffinityTerm(t.Name),
					},
				},
			},
		}
		// Same authentik-pool targeting as server/worker above - the bundled
		// subchart's own primary.nodeSelector/primary.tolerations (Bitnami's
		// standard convention, same as primary.affinity above) aren't wired
		// through by the chart's own global.* values the way affinity's
		// mutually-exclusive-with-presets mechanism forced server/worker's
		// own values here, but are real top-level keys under primary all the
		// same.
		if len(poolSelector) > 0 {
			primary["nodeSelector"] = poolSelector
		}
		if len(poolTolerations) > 0 {
			primary["tolerations"] = poolTolerations
		}
		values.Postgres["primary"] = primary
		// The bundled subchart is Bitnami's own postgresql chart (confirmed
		// against its real values.yaml): metrics.enabled runs its
		// postgres_exporter sidecar, metrics.serviceMonitor.enabled asks the
		// subchart to create its own matching ServiceMonitor for it - same
		// "let the chart own its own monitoring object" approach as
		// authentikMetrics above, and the same hasServiceMonitorCRD gate.
		values.Postgres["metrics"] = map[string]any{
			"enabled": true,
			"serviceMonitor": map[string]any{
				"enabled":            hasServiceMonitorCRD,
				valuesKeyRelabelings: scrapeRelabelings("postgres", targetName),
			},
			// The off-by-default collectors the reference harness's own
			// postgres-exporter turns on (roles/authentik/files/compose.yml),
			// for the table, vacuum and transaction detail its dashboards use.
			"collectors": map[string]bool{
				"long_running_transactions": true, "postmaster": true, "process_idle": true,
				"stat_activity_autovacuum": true, "stat_checkpointer": true,
				"stat_progress_vacuum": true, "stat_user_tables": true, "stat_wal_receiver": true,
				"statio_user_indexes": true, "statio_user_tables": true,
			},
		}
	}

	values.Global = map[string]any{
		// additionalLabels reaches the actual pod template labels
		// (authentik.labels, used at server/worker's
		// template.metadata.labels - confirmed against the chart's own
		// templates/{server,worker}/deployment.yaml), which is what lets
		// ServerWorkerAffinity's cross-deployment anti-affinity term
		// (applyPlacement below) find these pods from another deployment's own
		// affinity rule. Replaces this value's former use for
		// global.affinity.podAntiAffinity (the chart's own preset
		// mechanism) - applyPlacement now renders server.affinity/
		// worker.affinity directly for every mode instead (see
		// ServerWorkerAffinity's doc comment for why the two can't be mixed).
		"additionalLabels": DeploymentLabels(t.Name),
	}
	if img := t.Spec.Authentik.Image; img != "" {
		values.Global["image"] = map[string]any{
			"repository": img,
			"tag":        t.Spec.Authentik.Tag,
		}
	}
	if t.Spec.Authentik.LogLevel != "" {
		values.Authentik["log_level"] = t.Spec.Authentik.LogLevel
	}

	// Consumed by the Pyroscope SDK as its server address - always set,
	// unmarked by any enabled flag, matching the reference Ansible harness
	// (env.j2's AUTHENTIK_PYROSCOPE_HOST): profiling is best-effort and
	// silently no-ops if nothing is actually listening there.
	appendEnv := func(w map[string]any, name, value string) {
		env, _ := w["env"].([]map[string]string)
		w["env"] = append(env, map[string]string{valuesKeyName: name, "value": value})
	}
	appendEnv(values.Server, "AUTHENTIK_PYROSCOPE_HOST", pyroscopeHost)
	appendEnv(values.Worker, "AUTHENTIK_PYROSCOPE_HOST", pyroscopeHost)

	if ptr.Deref(t.Spec.Authentik.ErrorReporting.Enabled, true) {
		values.Authentik["error_reporting"] = map[string]any{
			"enabled": true,
			// deployment.environment carries this deployment's target name, so
			// spans from two deployments sharing one Tempo can be told apart.
			"environment": targetName,
		}
		sampleRate := t.Spec.Authentik.ErrorReporting.SampleRate
		if sampleRate == "" {
			sampleRate = "0.1"
		}
		for _, w := range []map[string]any{values.Server, values.Worker} {
			appendEnv(w, "AUTHENTIK_ERROR_REPORTING__OTEL_ENDPOINT", tracingEndpoint)
			appendEnv(w, "AUTHENTIK_ERROR_REPORTING__SAMPLE_RATE", sampleRate)
			// Only meaningful for the server (the worker doesn't serve
			// HTTP), but harmless to set on both - puts the test name that
			// caused a request on its spans as http.request.header.*.
			appendEnv(w, "OTEL_INSTRUMENTATION_HTTP_CAPTURE_HEADERS_SERVER_REQUEST", "x-benchmark-test,x-benchmark-target")
		}
	}

	// Merged (not replaced) with poolSelector/poolTolerations above: the two
	// mechanisms target different label keys, so there's nothing to
	// reconcile between them.
	applyPlacement := func(w map[string]any) {
		sel := poolSelector
		if dedicated := NodeSelector(t.Spec.Placement); len(dedicated) > 0 {
			sel = mergeStringMaps(sel, dedicated)
		}
		if len(sel) > 0 {
			w["nodeSelector"] = sel
		}
		tol := append([]corev1.Toleration{}, poolTolerations...)
		tol = append(tol, Tolerations(t.Spec.Placement)...)
		if len(tol) > 0 {
			w["tolerations"] = tol
		}
		// Always set, not just in Packed mode: ServerWorkerAffinity now
		// always includes the cross-deployment anti-affinity term, on top of
		// whatever (if anything) the placement mode itself adds.
		w["affinity"] = ServerWorkerAffinity(t.Spec.Placement, HelmReleaseName(t), t.Name)
	}
	applyPlacement(values.Server)
	applyPlacement(values.Worker)

	switch {
	case embedded && pgConn != nil:
		// The bundled subchart auto-generates its own password unless told
		// otherwise; we pin it to our generated value so authentik's own
		// AUTHENTIK_POSTGRESQL__PASSWORD (below) actually matches it.
		values.Postgres["auth"] = map[string]any{valuesKeyPassword: pgConn.Password}
		akPg := map[string]any{valuesKeyPassword: pgConn.Password}
		if e := t.Spec.Postgres.Embedded; e != nil && e.TLS {
			// The subchart's self-signed cert can't be verified, so require
			// rather than verify-ca/verify-full.
			values.Postgres["tls"] = map[string]any{"enabled": true, "autoGenerated": true}
			// The authentik chart runs the official postgres image, which
			// ignores the Bitnami POSTGRESQL_ENABLE_TLS env the subchart sets,
			// so turn ssl on via conf.d ourselves (this replaces the chart's
			// own extendedConfiguration, hence max_connections). The certs
			// are where the subchart's copy-certs init container puts them.
			values.Postgres["primary"].(map[string]any)["extendedConfiguration"] = "max_connections = 500\n" +
				"ssl = on\n" +
				"ssl_cert_file = '/opt/bitnami/postgresql/certs/tls.crt'\n" +
				"ssl_key_file = '/opt/bitnami/postgresql/certs/tls.key'\n"
			// copy-certs uses volumePermissions.image, which the authentik
			// chart sets to repository "debian" under the subchart's default
			// registry-1.docker.io - unpullable without the library/ prefix.
			values.Postgres["volumePermissions"] = map[string]any{
				"image": map[string]any{"registry": "docker.io", "repository": "library/debian"},
			}
			akPg["sslmode"] = "require"
		}
		values.Authentik["postgresql"] = akPg
	case !embedded && pgConn != nil:
		values.Authentik["postgresql"] = map[string]any{
			"host":            pgConn.Host,
			"port":            pgConn.Port,
			valuesKeyName:     pgConn.Database,
			"user":            pgConn.User,
			valuesKeyPassword: pgConn.Password,
		}
	}

	raw, err := json.Marshal(values)
	if err != nil {
		return nil, fmt.Errorf("marshal helm values: %w", err)
	}
	return &apiextensionsv1.JSON{Raw: raw}, nil
}

// PostgresConnection is the resolved connection info passed to HelmValues.
// For Embedded mode only Password is meaningful (see above); for CNPG/
// External modes the full connection is read from the relevant Secret by the
// caller - the operator never manages the lifecycle of that Secret itself
// for External, and only reads CNPG's generated one for CNPG.
type PostgresConnection struct {
	Host     string
	Port     int32
	Database string
	User     string
	Password string
}

// HelmReleaseName is deterministic from the deployment name, so the operator
// can look it up without storing it, though it's still recorded in
// status.helmReleaseRef for clarity.
func HelmReleaseName(t *benchv1alpha1.AuthentikDeployment) string {
	return t.Name
}

// BlueprintMountPath is where BlueprintPVC is mounted into a BlueprintJob's
// pod (see internal/render/blueprintjob.go) - must stay under authentik's
// own configured blueprints_dir (verified live: "/blueprints"), since ak
// apply_blueprint rejects any path outside it.
const BlueprintMountPath = "/blueprints/custom"

// BlueprintFileName is the file a BlueprintJob copies its blueprint
// ConfigMap's content into, under BlueprintMountPath, before running
// ak apply_blueprint against it.
const BlueprintFileName = "bench-fixtures.yaml"

// BlueprintPVCName is deterministic from the deployment name, mirroring
// HelmReleaseName, so the operator can look it up without storing it.
func BlueprintPVCName(t *benchv1alpha1.AuthentikDeployment) string {
	return t.Name + "-bench-fixtures"
}

// BlueprintPVC is the (small - a benchmark-scale blueprint is at most a few
// MB of YAML) PVC a BlueprintJob mounts to give ak apply_blueprint somewhere
// writable under blueprints_dir to read its argument from - never mounted
// into the server/worker Deployment itself (see BlueprintJob's own doc
// comment for why).
func BlueprintPVC(t *benchv1alpha1.AuthentikDeployment) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		Name:      BlueprintPVCName(t),
		Namespace: t.Spec.Namespace,
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("256Mi")},
			},
		},
	}
}

// ChartVersion is the authentik chart version the HelmRelease requests: the
// pinned spec.chart.version, else the version first installed
// (status.chartVersion), else "*" for the latest chart on first install.
// Without the middle step every unpinned deployment would upgrade - and
// restart - whenever upstream publishes a new chart.
func ChartVersion(t *benchv1alpha1.AuthentikDeployment) string {
	if t.Spec.Chart.Version != "" {
		return t.Spec.Chart.Version
	}
	if t.Status.ChartVersion != "" {
		return t.Status.ChartVersion
	}
	return "*"
}

// HelmRelease builds the Flux HelmRelease that installs the authentik chart
// for this deployment. kubeConfigSecretName is empty for a deployment on the
// management cluster itself (M1-M3); from M4 on it names the
// "<clusterRef>-kubeconfig" Secret CAPI produces, so Flux's helm-controller
// installs remotely.
func HelmRelease(t *benchv1alpha1.AuthentikDeployment, kubeConfigSecretName string, values *apiextensionsv1.JSON) *helmv2.HelmRelease {
	version := ChartVersion(t)
	hr := &helmv2.HelmRelease{
		Name:      HelmReleaseName(t),
		Namespace: t.Namespace,
		Spec: helmv2.HelmReleaseSpec{
			Interval: metav1.Duration{Duration: helmRepoInterval},
			// helm-controller's own default (5m) isn't enough on a cold
			// cluster: pulling the authentik/postgresql images and letting
			// postgres finish its first-boot initdb can alone take longer
			// than that, well before authentik itself is even up. A timed
			// out attempt is retried (retryOnFailure), but each one burns
			// this long first.
			Timeout: &metav1.Duration{Duration: helmInstallTimeout},
			// Explicit, rather than Flux's default "[TargetNamespace-]Name"
			// composition, so ServiceEndpoint's "<release>-server" naming
			// stays correct regardless of what the target namespace is
			// named.
			ReleaseName: HelmReleaseName(t),
			Chart: &helmv2.HelmChartTemplate{
				Spec: helmv2.HelmChartTemplateSpec{
					Chart:   chartName,
					Version: version,
					SourceRef: helmv2.CrossNamespaceObjectReference{
						Kind:      sourcev1.HelmRepositoryKind,
						Name:      HelmRepositoryName,
						Namespace: t.Namespace,
					},
				},
			},
			TargetNamespace: t.Spec.Namespace,
			Values:          values,
		},
	}
	if kubeConfigSecretName != "" {
		hr.Spec.KubeConfig = &fluxmeta.KubeConfigReference{
			SecretRef: &fluxmeta.SecretKeyReference{Name: kubeConfigSecretName},
		}
	}
	return retryOnFailure(hr)
}

// helmRetryInterval is how long helm-controller waits before retrying a
// failed install/upgrade (retryOnFailure).
const helmRetryInterval = 2 * time.Minute

// retryOnFailure makes helm-controller keep retrying a failed install or
// upgrade every helmRetryInterval, instead of its default
// (RemediateOnFailure with zero retries): one failure - a rollout timing
// out while a pool scales up from zero, a volume that can't attach yet, a
// slow image pull - left the HelmRelease Stalled for good, needing a manual
// `flux reconcile --force` even once the cause had cleared. RetryOnFailure
// re-runs the same action without uninstalling or rolling back first, so
// nothing already deployed (e.g. an embedded Postgres' data) is churned.
// Every HelmRelease this package renders goes through it; existing
// Install/Upgrade settings are kept.
func retryOnFailure(hr *helmv2.HelmRelease) *helmv2.HelmRelease {
	interval := &metav1.Duration{Duration: helmRetryInterval}
	if hr.Spec.Install == nil {
		hr.Spec.Install = &helmv2.Install{}
	}
	hr.Spec.Install.Strategy = &helmv2.InstallStrategy{Name: string(helmv2.ActionStrategyRetryOnFailure), RetryInterval: interval}
	if hr.Spec.Upgrade == nil {
		hr.Spec.Upgrade = &helmv2.Upgrade{}
	}
	hr.Spec.Upgrade.Strategy = &helmv2.UpgradeStrategy{Name: string(helmv2.ActionStrategyRetryOnFailure), RetryInterval: interval}
	return hr
}

// ServiceEndpoint is the authentik chart's own service-name convention:
// "<release>-authentik-server" in the target namespace, serving HTTP on the
// chart's server.service.servicePortHttp (default 80). Confirmed against a
// live install of chart version 2026.8.0 - the chart's fullname template
// always includes "-authentik-", it isn't just the release name.
func ServiceEndpoint(t *benchv1alpha1.AuthentikDeployment) string {
	return fmt.Sprintf("%s-authentik-server.%s.svc.cluster.local:80", HelmReleaseName(t), t.Spec.Namespace)
}
