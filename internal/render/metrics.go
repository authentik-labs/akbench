package render

import (
	"encoding/json"
	"fmt"
	"strconv"

	helmv2 "github.com/fluxcd/helm-controller/api/v2"
	fluxmeta "github.com/fluxcd/pkg/apis/meta"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
)

// ObservabilityNamespace is where every per-workload-cluster
// Prometheus/Pyroscope/Tempo instance lives, matching the namespace this
// project's own dev scripts (hack/dev-prometheus.yaml) already used.
const ObservabilityNamespace = "bench-system"

// PrometheusServiceName matches defaultRemoteWriteURL
// (internal/controller/testcase_controller.go): k6 workloads push to
// "prometheus-server.bench-system.svc.cluster.local" regardless of which
// cluster they run on. Prometheus itself is now installed via the
// kube-prometheus-stack chart (see KubePrometheusStackHelmRelease) rather
// than a hand-rolled Deployment, so this exact Service - along with
// PrometheusNodePortServiceName below - is rendered as a small
// project-authored compatibility object (prometheusCompatServices,
// injected into the same HelmRelease via its own extraManifests value)
// selecting the chart-managed Prometheus pod directly: every other
// consumer of these names (Grafana's datasource, k6's remote-write URL,
// cmd/apiserver's cross-cluster resolution) needed zero changes as a
// result.
const PrometheusServiceName = "prometheus-server"

// PrometheusPort is the port Prometheus itself listens on (the chart's own
// default, unchanged from the prior hand-rolled install), and also the port
// the plain ClusterIP Service exposes as "80" for defaultRemoteWriteURL's
// own http://prometheus-server.../api/v1/write (no :9090 in that URL).
const PrometheusPort = 9090

// PrometheusNodePortServiceName/PrometheusNodePort expose the same
// Prometheus outside its own cluster: a management-cluster pod can reach a
// workload cluster's node IPs directly (confirmed live - the reverse
// direction can't), so cmd/apiserver reaches this NodePort at
// <a node's InternalIP>:PrometheusNodePort rather than the cluster-relative
// DNS name k6 uses, which only resolves from inside that same cluster.
// Pinned rather than left to auto-assignment so WorkloadClusterReconciler
// never has to read the Service back just to learn its own port.
const (
	PrometheusNodePortServiceName = "prometheus-server-nodeport"
	PrometheusNodePort            = 30909
)

// kubePrometheusStack's own release/fullname is pinned to a fixed,
// predictable value (cleanPrometheusOperatorObjectNames below drops the
// chart's default "-prometheus" suffix from the generated Prometheus CR
// name) purely so prometheusPodLabels - the selector the two compatibility
// Services above use to reach the chart-managed Prometheus pod - doesn't
// have to guess at Helm's own name-templating rules.
const (
	kubePrometheusStackHelmRepositoryName = "kube-prometheus-stack"
	kubePrometheusStackChartName          = "kube-prometheus-stack"

	KubePrometheusStackHelmReleaseName = "kube-prometheus-stack"
	kubePrometheusStackFullname        = "kube-prometheus-stack"
)

// kubePrometheusStackChart is the version and repository deps/Chart.yaml pins.
var kubePrometheusStackChart = pinnedChart(kubePrometheusStackChartName)

// KubePrometheusStackHelmRepository is the (singleton, per-namespace)
// HelmRepository pointing at the official prometheus-community chart repo.
func KubePrometheusStackHelmRepository(namespace string) *sourcev1.HelmRepository {
	return &sourcev1.HelmRepository{
		Name:      kubePrometheusStackHelmRepositoryName,
		Namespace: namespace,
		Spec: sourcev1.HelmRepositorySpec{
			URL:      kubePrometheusStackChart.Repository,
			Interval: metav1.Duration{Duration: helmRepoInterval},
		},
	}
}

// prometheusPodLabels are the labels prometheus-operator itself puts on
// every pod belonging to the Prometheus custom resource this chart renders
// (confirmed directly against the chart's own templates/prometheus/
// service.yaml selector, not assumed) - app.kubernetes.io/instance equals
// the Prometheus CR's own name, which cleanPrometheusOperatorObjectNames
// (set true below) makes equal to kubePrometheusStackFullname exactly,
// rather than fullname + "-prometheus".
func prometheusPodLabels() map[string]string {
	return map[string]string{
		"app.kubernetes.io/name": "prometheus",
		labelAppInstance:         kubePrometheusStackFullname,
	}
}

// prometheusCompatServices are the two Services every other consumer in
// this codebase already expects by name (see PrometheusServiceName's own
// doc comment) - not part of the chart itself, injected into the same
// HelmRelease via its own extraManifests value so one install produces
// everything, rather than a second apply step against the remote cluster.
func prometheusCompatServices() []map[string]any {
	labels := prometheusPodLabels()
	clusterIP := &corev1.Service{
		Kind: kindService, APIVersion: apiVersionV1,
		Name: PrometheusServiceName, Namespace: ObservabilityNamespace,
		Spec: corev1.ServiceSpec{
			Selector: labels,
			Ports: []corev1.ServicePort{
				{Name: portNameHTTP, Port: 80, TargetPort: intstr.FromInt32(PrometheusPort)},
				{Name: "prom-native", Port: PrometheusPort, TargetPort: intstr.FromInt32(PrometheusPort)},
			},
		},
	}
	nodePort := &corev1.Service{
		Kind: kindService, APIVersion: apiVersionV1,
		Name: PrometheusNodePortServiceName, Namespace: ObservabilityNamespace,
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeNodePort,
			Selector: labels,
			Ports: []corev1.ServicePort{
				{Name: portNameHTTP, Port: PrometheusPort, TargetPort: intstr.FromInt32(PrometheusPort), NodePort: PrometheusNodePort},
			},
		},
	}
	out := make([]map[string]any, 0, 2)
	for _, svc := range []*corev1.Service{clusterIP, nodePort} {
		m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(svc)
		if err != nil {
			// svc is a fixed, entirely-literal object with no user input -
			// a conversion failure here would mean this function itself is
			// broken, not a runtime condition any caller could recover from.
			panic(fmt.Sprintf("convert compat service %q to unstructured: %v", svc.Name, err))
		}
		out = append(out, m)
	}
	return out
}

// KubePrometheusStackHelmValues builds spec.values for the
// kube-prometheus-stack chart install:
//   - enableRemoteWriteReceiver, for k6's own experimental-prometheus-rw
//     output. Native-histogram ingestion (K6_FEATURES=native-histograms)
//     needed no matching --enable-feature flag here - confirmed directly
//     against the chart's own pinned Prometheus image (v3.14.0-distroless)
//     that "native-histograms" isn't even a recognized feature name on
//     Prometheus v3.x (it shipped enabled by default there; the flag this
//     chart's own values.yaml exposes is for a different, still-experimental
//     set of features), unlike the hand-rolled v2.55.1 install this
//     replaces, which needed it explicitly.
//   - grafana.enabled false: this project already installs its own Grafana
//     (render.GrafanaHelmRelease, pre-wired with Prometheus/Pyroscope/Tempo
//     datasources), matching the "no HA, dev-scale, only what's actually
//     used" posture the rest of this addon set already follows.
//   - alertmanager.enabled true, with alertmanager.config set to
//     defaultAlertmanagerConfig()'s "null"-receiver config (see that
//     function's own doc comment for why no real notification integration
//     is wired yet) and ruleSelectorNilUsesHelmValues: false alongside the
//     two selectors already below it, so PrometheusRules from outside this
//     release are picked up too (this package's own DefaultAlertRuleGroups()
//     now ships through the chart's additionalPrometheusRulesMap, so it
//     carries the release label either way).
//   - nodeExporter/kubeStateMetrics left at the chart's own enabled-by-default
//     value - node-exporter is the actual point of this chart (per-node CPU/
//     memory/disk/network metrics the hand-rolled install never had), and
//     kube-state-metrics comes along for free at no extra operational cost.
//   - extraManifests carries the two compatibility Services (see their own
//     doc comment) so this one HelmRelease produces everything needed.
//   - serviceMonitorSelectorNilUsesHelmValues/podMonitorSelectorNilUsesHelmValues
//     both false, with the chart's own default empty selectors left as-is:
//     the chart's own comment on the first key says a nil/{} value there
//     "will cause the prometheus resource to be created with selectors based
//     on values in the helm deployment" - i.e. restricted to ServiceMonitors/
//     PodMonitors carrying this release's own "release: kube-prometheus-stack"
//     label, which every ServiceMonitor/PodMonitor this operator creates
//     itself (see newServiceMonitor) or asks another chart to create (the
//     authentik chart's own server.metrics.serviceMonitor, the bundled
//     postgresql subchart's metrics.serviceMonitor, CNPG's
//     spec.monitoring.enablePodMonitor) does not carry. False here instead
//     leaves prometheusSpec.serviceMonitorSelector/podMonitorSelector at
//     their own already-empty-map chart defaults, which - confirmed directly
//     against the chart's own values.yaml comment - means "select
//     everything," in every namespace (serviceMonitorNamespaceSelector/
//     podMonitorNamespaceSelector already default to {} too).
func KubePrometheusStackHelmValues(wc *benchv1alpha1.WorkloadCluster) (*apiextensionsv1.JSON, error) {
	// The default alert rules go in additionalPrometheusRulesMap below, not
	// extraManifests alongside the compat Services: confirmed live that this
	// chart's extra-objects.yaml template runs every extraManifests entry
	// through Helm's own `tpl`, which fails on the rules' "{{ $labels.job }}"
	// annotations (meant for Prometheus's rule engine, not Helm) with
	// "undefined variable $labels". additionalPrometheusRulesMap is rendered
	// with toYaml only.
	extraManifests := append(prometheusCompatServices(), alertmanagerCompatServices()...)

	// poolSelector/poolTolerations scope every Deployment this chart renders
	// to the monitoring pool - deliberately NOT prometheus-node-exporter
	// (its own DaemonSet, left at the chart's default: it exists to report
	// per-node metrics for *every* node, authentik/k6 pools included, and
	// already tolerates any NoSchedule taint by default - confirmed against
	// its own subchart values.yaml - so scoping it here would defeat the
	// entire point of it).
	poolSelector, poolTolerations := MonitoringPoolPlacement(wc)
	prometheusSpec := map[string]any{
		"enableRemoteWriteReceiver":               true,
		"serviceMonitorSelectorNilUsesHelmValues": false,
		"podMonitorSelectorNilUsesHelmValues":     false,
		"ruleSelectorNilUsesHelmValues":           false,
		"resources":                               prometheusResources(wc),
		// Persistent, and capped below the volume's size so a long Burn run
		// can't fill it.
		"storageSpec":   volumeClaimTemplate(wc, PrometheusStorageSize),
		"retentionSize": prometheusRetentionSize,
	}
	alertmanagerSpec := map[string]any{
		// Silences and notification state survive a restart.
		"storage": volumeClaimTemplate(wc, AlertmanagerStorageSize),
	}
	prometheusOperator := map[string]any{}
	kubeStateMetrics := map[string]any{}
	if len(poolSelector) > 0 || len(poolTolerations) > 0 {
		for _, m := range []map[string]any{prometheusSpec, alertmanagerSpec, prometheusOperator, kubeStateMetrics} {
			m["nodeSelector"] = poolSelector
			m["tolerations"] = poolTolerations
		}
		// admissionWebhooks.patch is a separate one-shot Job (not the
		// operator Deployment itself) the chart runs as a Helm hook to
		// provision the operator's webhook cert - its own nodeSelector/
		// tolerations keys, confirmed against the chart's own values.yaml,
		// are nested here rather than inherited from prometheusOperator's
		// top-level ones above. Missed this the first time a live cluster
		// had every node tainted across all three pools: with no
		// toleration of its own, this hook Job couldn't land anywhere at
		// all ("0/N nodes are available: N node(s) had untolerated
		// taint(s)"), blocking the whole HelmRelease upgrade until its own
		// 15-minute timeout.
		prometheusOperator["admissionWebhooks"] = map[string]any{
			"patch": map[string]any{
				"nodeSelector": poolSelector,
				"tolerations":  poolTolerations,
			},
		}
	}

	raw, err := json.Marshal(map[string]any{
		"fullnameOverride":                   kubePrometheusStackFullname,
		"cleanPrometheusOperatorObjectNames": true,
		"grafana":                            map[string]any{"enabled": false},
		"alertmanager": map[string]any{
			"enabled":          true,
			"config":           defaultAlertmanagerConfig(),
			"alertmanagerSpec": alertmanagerSpec,
		},
		"prometheus": map[string]any{
			"prometheusSpec": prometheusSpec,
		},
		"prometheusOperator": prometheusOperator,
		"additionalPrometheusRulesMap": map[string]any{
			defaultAlertRulesKey: map[string]any{"groups": DefaultAlertRuleGroups()},
		},
		"kube-state-metrics": kubeStateMetrics,
		// host is the label the reference harness's dashboards select nodes
		// by (its prometheus.yml.j2 sets it on every job).
		"prometheus-node-exporter": map[string]any{
			"prometheus": map[string]any{"monitor": map[string]any{valuesKeyRelabelings: []map[string]any{hostRelabeling}}},
		},
		// kubeScheduler/kubeControllerManager/kubeProxy disabled: confirmed
		// live (Alertmanager's own KubeSchedulerInstanceUnreachable/
		// KubeControllerManagerInstanceUnreachable/KubeProxyInstanceUnreachable/
		// TargetDown alerts, permanently firing on every workload cluster this
		// operator has ever provisioned) that kubeadm binds these components'
		// metrics ports to 127.0.0.1 by default (scheduler :10259,
		// controller-manager :10257, kube-proxy :10249) - this project's own
		// WorkloadClusterReconciler/render.LXCCluster never configures a
		// --bind-address override for any of them, so the chart's own default
		// ServiceMonitors for these three can never succeed on a workload
		// cluster this project creates. kubeEtcd disabled for a second,
		// independent reason on top of the same bind-address issue (etcd's
		// own :2381 metrics port is also loopback-only by default): every
		// WorkloadCluster this project renders has a single-node control
		// plane by design (dev-scale, no HA - see this package's own "no HA"
		// posture elsewhere), so even a reachable etcd would permanently fail
		// the chart's own etcdInsufficientMembers/etcdMembersDown alerts,
		// which assume a multi-member quorum. Monitoring components that
		// structurally can never be reached, or that always violate an
		// HA-oriented threshold for a deliberately non-HA topology, is the
		// actual misconfiguration here - disabling it is the fix, not a
		// workaround. kubeApiServer/coreDns/kubelet are untouched: all three
		// are reachable through their normal in-cluster Service/mTLS
		// endpoints regardless of node-local bind addresses, so their own
		// ServiceMonitors already work.
		"kubeScheduler":         map[string]any{"enabled": false},
		"kubeControllerManager": map[string]any{"enabled": false},
		"kubeProxy":             map[string]any{"enabled": false},
		"kubeEtcd":              map[string]any{"enabled": false},
		"extraManifests":        extraManifests,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal kube-prometheus-stack helm values: %w", err)
	}
	return &apiextensionsv1.JSON{Raw: raw}, nil
}

// KubePrometheusStackHelmRelease builds the Flux HelmRelease that installs
// kube-prometheus-stack onto a workload cluster remotely -
// kubeConfigSecretName is the "<cluster>-kubeconfig" Secret CAPI writes (see
// render.KubeconfigSecretName). CRDs default to Create-only on Helm's own
// install action and Skip on upgrade (see the fluxcd/helm-controller API's
// own doc comment on this) - both explicitly set to CreateReplace instead,
// since prometheus-operator's Prometheus/ServiceMonitor/PrometheusRule/etc.
// CRDs (bundled in the chart's own crds/ subchart) are exactly what this
// install exists to provide, and bumping kubePrometheusStackChart.Version
// later should actually update them rather than silently stay on whatever
// version first got installed.
func KubePrometheusStackHelmRelease(wc *benchv1alpha1.WorkloadCluster, values *apiextensionsv1.JSON) *helmv2.HelmRelease {
	return retryOnFailure(&helmv2.HelmRelease{
		Name:      AddonHelmReleaseName(wc, KubePrometheusStackHelmReleaseName),
		Namespace: wc.Namespace,
		Spec: helmv2.HelmReleaseSpec{
			Interval:    metav1.Duration{Duration: helmRepoInterval},
			Timeout:     &metav1.Duration{Duration: helmInstallTimeout},
			ReleaseName: KubePrometheusStackHelmReleaseName,
			Chart: &helmv2.HelmChartTemplate{
				Spec: helmv2.HelmChartTemplateSpec{
					Chart:   kubePrometheusStackChartName,
					Version: kubePrometheusStackChart.Version,
					SourceRef: helmv2.CrossNamespaceObjectReference{
						Kind:      sourcev1.HelmRepositoryKind,
						Name:      kubePrometheusStackHelmRepositoryName,
						Namespace: wc.Namespace,
					},
				},
			},
			TargetNamespace: ObservabilityNamespace,
			Install:         &helmv2.Install{CreateNamespace: true, CRDs: helmv2.CreateReplace},
			Upgrade:         &helmv2.Upgrade{CRDs: helmv2.CreateReplace},
			Values:          values,
			KubeConfig: &fluxmeta.KubeConfigReference{
				SecretRef: &fluxmeta.SecretKeyReference{Name: KubeconfigSecretName(wc)},
			},
		},
	})
}

// prometheusMemoryShare is the fraction of a monitoring pool node's memory
// Prometheus may use, leaving the rest for everything else the pool runs
// (Grafana, Tempo, Pyroscope, Alertmanager, the operator, csi-incus'
// controller, kubelet).
const prometheusMemoryShare = 0.6

// prometheusResources sizes Prometheus from its node. Without any requests
// or limits the scheduler had no idea what it needed, and a monitoring node
// under load (k6 remote write, scraping every node) ran out of memory as a
// whole - starving the kubelet until the node went NotReady and took the
// entire observability stack down with it. With a limit, Prometheus alone
// is OOM-killed and restarted instead. The limit tracks the monitoring
// pool's flavor (cX-mY); with no dedicated monitoring pool, or a flavor
// that isn't cX-mY, it falls back to a conservative fixed size.
func prometheusResources(wc *benchv1alpha1.WorkloadCluster) map[string]any {
	limitMi := 2048
	if wc != nil {
		for _, pool := range wc.Spec.WorkerPools {
			if pool.Role != benchv1alpha1.WorkerPoolRoleMonitoring {
				continue
			}
			if m := incusFlavorPattern.FindStringSubmatch(pool.Incus.Flavor); m != nil {
				gi, _ := strconv.Atoi(m[2])
				limitMi = int(float64(gi*1024) * prometheusMemoryShare)
			}
			break
		}
	}
	return map[string]any{
		"requests": map[string]any{"cpu": "250m", "memory": fmt.Sprintf("%dMi", limitMi/2)},
		"limits":   map[string]any{"memory": fmt.Sprintf("%dMi", limitMi)},
	}
}

const relabelTargetLabel = "targetLabel"

// hostRelabeling sets host to the scraped pod's node, the closest thing to
// the reference harness's per-machine host label.
var hostRelabeling = map[string]any{
	"sourceLabels": []string{"__meta_kubernetes_pod_node_name"}, relabelTargetLabel: "host",
}

// scrapeRelabelings gives a ServiceMonitor the job/target/host labels the
// reference harness's prometheus.yml.j2 puts on its authentik and postgres
// jobs, which its dashboards select on (target matches k6's own target tag).
func scrapeRelabelings(job, target string) []map[string]any {
	return []map[string]any{
		{relabelTargetLabel: "job", "replacement": job},
		{relabelTargetLabel: "target", "replacement": target},
		hostRelabeling,
	}
}
