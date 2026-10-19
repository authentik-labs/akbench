package render

import (
	"encoding/json"
	"strings"
	"testing"

	helmv2 "github.com/fluxcd/helm-controller/api/v2"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
)

// TestKubePrometheusStackHelmValuesTargetsMonitoringPool confirms Prometheus/
// Alertmanager/the operator/kube-state-metrics all get scoped to the
// cluster's own monitoring-role pool, while prometheus-node-exporter - a
// DaemonSet that exists specifically to report every node's own metrics,
// authentik/k6 pools included - is left completely untouched.
func TestKubePrometheusStackHelmValuesTargetsMonitoringPool(t *testing.T) {
	wc := &benchv1alpha1.WorkloadCluster{Spec: benchv1alpha1.WorkloadClusterSpec{
		WorkerPools: []benchv1alpha1.WorkerPoolSpec{{Name: "mon", Role: benchv1alpha1.WorkerPoolRoleMonitoring}},
	}}
	raw, err := KubePrometheusStackHelmValues(wc)
	if err != nil {
		t.Fatalf("KubePrometheusStackHelmValues: %v", err)
	}
	var values map[string]any
	if err := json.Unmarshal(raw.Raw, &values); err != nil {
		t.Fatalf("unmarshal values: %v", err)
	}

	assertPoolSelector := func(path ...string) {
		t.Helper()
		m, _, _ := unstructured.NestedStringMap(values, append(append([]string{}, path...), "nodeSelector")...)
		if m[NodePoolLabel] != "monitoring" {
			t.Errorf("%v.nodeSelector[%s] = %v, want monitoring", path, NodePoolLabel, m[NodePoolLabel])
		}
	}
	assertPoolSelector("prometheus", "prometheusSpec")
	assertPoolSelector("alertmanager", "alertmanagerSpec")
	assertPoolSelector("prometheusOperator")
	assertPoolSelector("kube-state-metrics")
	// The admission-webhook cert patch is a separate one-shot Helm-hook Job,
	// not the operator Deployment itself - a real live-cluster regression
	// once every node in a cluster carries one of the three pool taints:
	// with no toleration of its own, this hook couldn't schedule anywhere
	// at all, blocking the whole HelmRelease upgrade.
	assertPoolSelector("prometheusOperator", "admissionWebhooks", "patch")

	nodeExporter, _ := values["prometheus-node-exporter"].(map[string]any)
	for _, key := range []string{"nodeSelector", "tolerations", "affinity"} {
		if _, ok := nodeExporter[key]; ok {
			t.Errorf(`"prometheus-node-exporter".%s was set - its placement must stay at chart defaults, since it has to report every node's metrics regardless of pool`, key)
		}
	}
}

func TestKubePrometheusStackHelmRepository(t *testing.T) {
	repo := KubePrometheusStackHelmRepository("default")

	if repo.Name != kubePrometheusStackHelmRepositoryName {
		t.Errorf("name = %q, want %q", repo.Name, kubePrometheusStackHelmRepositoryName)
	}
	if repo.Namespace != "default" {
		t.Errorf("namespace = %q, want default", repo.Namespace)
	}
	if repo.Spec.URL != kubePrometheusStackChart.Repository {
		t.Errorf("url = %q, want %q", repo.Spec.URL, kubePrometheusStackChart.Repository)
	}
}

// TestKubePrometheusStackHelmValues guards the values this project actually
// depends on: the remote-write receiver k6 pushes to (no matching
// enable-feature for native histograms - see KubePrometheusStackHelmValues'
// own doc comment for why that's correct, not an oversight), the bundled
// Grafana disabled (this project runs its own Grafana), Alertmanager
// enabled with this project's own default config/rules, and the
// compatibility Services every other consumer of
// PrometheusServiceName/PrometheusNodePortServiceName/
// AlertmanagerServiceName/AlertmanagerNodePortServiceName expects.
func TestKubePrometheusStackHelmValues(t *testing.T) {
	raw, err := KubePrometheusStackHelmValues(nil)
	if err != nil {
		t.Fatalf("KubePrometheusStackHelmValues: %v", err)
	}

	var values map[string]any
	if err := json.Unmarshal(raw.Raw, &values); err != nil {
		t.Fatalf("unmarshal values: %v", err)
	}

	prometheusSpec, _ := values["prometheus"].(map[string]any)["prometheusSpec"].(map[string]any)
	if enabled, _ := prometheusSpec["enableRemoteWriteReceiver"].(bool); !enabled {
		t.Errorf("prometheus.prometheusSpec.enableRemoteWriteReceiver = %v, want true", prometheusSpec["enableRemoteWriteReceiver"])
	}
	// Both false: the chart's own default (true) restricts discovery to
	// ServiceMonitors/PodMonitors carrying this release's own "release: ..."
	// label, which none of the ones this operator creates or asks other
	// charts to create (authentik's own, the bundled postgresql subchart's,
	// CNPG's PodMonitor, this package's own newServiceMonitor) carry.
	if v, ok := prometheusSpec["serviceMonitorSelectorNilUsesHelmValues"].(bool); !ok || v {
		t.Errorf("prometheus.prometheusSpec.serviceMonitorSelectorNilUsesHelmValues = %v, want false", prometheusSpec["serviceMonitorSelectorNilUsesHelmValues"])
	}
	if v, ok := prometheusSpec["podMonitorSelectorNilUsesHelmValues"].(bool); !ok || v {
		t.Errorf("prometheus.prometheusSpec.podMonitorSelectorNilUsesHelmValues = %v, want false", prometheusSpec["podMonitorSelectorNilUsesHelmValues"])
	}
	if v, ok := prometheusSpec["ruleSelectorNilUsesHelmValues"].(bool); !ok || v {
		t.Errorf("prometheus.prometheusSpec.ruleSelectorNilUsesHelmValues = %v, want false", prometheusSpec["ruleSelectorNilUsesHelmValues"])
	}

	if enabled, _ := values["grafana"].(map[string]any)["enabled"].(bool); enabled {
		t.Error("grafana.enabled = true, want false (this project runs its own Grafana)")
	}
	alertmanager, _ := values["alertmanager"].(map[string]any)
	if enabled, _ := alertmanager["enabled"].(bool); !enabled {
		t.Error("alertmanager.enabled = false, want true")
	}
	if _, ok := alertmanager["config"].(map[string]any)["route"]; !ok {
		t.Errorf("alertmanager.config.route missing: %v", alertmanager["config"])
	}

	// Disabled: kubeadm binds scheduler/controller-manager/kube-proxy's
	// metrics ports to 127.0.0.1 by default and this project never
	// overrides that, and every WorkloadCluster has a single-node control
	// plane by design, so etcd's own HA-oriented alerts can never pass
	// either - monitoring components that can never actually be reached or
	// satisfied is the misconfiguration this guards against reintroducing.
	for _, component := range []string{"kubeScheduler", "kubeControllerManager", "kubeProxy", "kubeEtcd"} {
		if enabled, _ := values[component].(map[string]any)["enabled"].(bool); enabled {
			t.Errorf("%s.enabled = true, want false (unreachable/unsatisfiable on this project's workload clusters)", component)
		}
	}

	// The default alert rules are deliberately NOT among these - see
	// KubePrometheusStackHelmValues for why a PrometheusRule containing
	// "{{ $labels... }}" can't survive this chart's extraManifests -> tpl
	// pipeline; they go in additionalPrometheusRulesMap instead
	// (TestDefaultAlertRules covers it).
	manifests, ok := values["extraManifests"].([]any)
	if !ok || len(manifests) != 4 {
		t.Fatalf("extraManifests = %v, want 4 objects (2 Prometheus compat Services, 2 Alertmanager compat Services)", values["extraManifests"])
	}
	var sawClusterIP, sawNodePort, sawAlertmanagerClusterIP, sawAlertmanagerNodePort bool
	for _, m := range manifests {
		obj, ok := m.(map[string]any)
		if !ok {
			t.Fatalf("extraManifests entry is not an object: %v", m)
		}
		if obj["kind"] != "Service" {
			t.Errorf("extraManifests entry kind = %v, want Service", obj["kind"])
			continue
		}
		metadata, _ := obj["metadata"].(map[string]any)
		if metadata["namespace"] != ObservabilityNamespace {
			t.Errorf("extraManifests entry namespace = %v, want %q", metadata["namespace"], ObservabilityNamespace)
		}
		switch metadata["name"] {
		case PrometheusServiceName:
			sawClusterIP = true
		case PrometheusNodePortServiceName:
			sawNodePort = true
			spec, _ := obj["spec"].(map[string]any)
			if spec["type"] != "NodePort" {
				t.Errorf("%s spec.type = %v, want NodePort", PrometheusNodePortServiceName, spec["type"])
			}
		case AlertmanagerServiceName:
			sawAlertmanagerClusterIP = true
		case AlertmanagerNodePortServiceName:
			sawAlertmanagerNodePort = true
			spec, _ := obj["spec"].(map[string]any)
			if spec["type"] != "NodePort" {
				t.Errorf("%s spec.type = %v, want NodePort", AlertmanagerNodePortServiceName, spec["type"])
			}
		}
	}
	for name, saw := range map[string]bool{
		PrometheusServiceName:           sawClusterIP,
		PrometheusNodePortServiceName:   sawNodePort,
		AlertmanagerServiceName:         sawAlertmanagerClusterIP,
		AlertmanagerNodePortServiceName: sawAlertmanagerNodePort,
	} {
		if !saw {
			t.Errorf("extraManifests missing a Service named %q", name)
		}
	}
}

// TestDefaultAlertRules guards the one thing that actually broke live when
// this was first wired through extraManifests instead (see
// KubePrometheusStackHelmValues' own doc comment): every alert's
// annotations must keep their "{{ $labels... }}" Prometheus/Alertmanager
// templating syntax intact, verbatim, since nothing in this project's own
// code ever touches it - only Prometheus's rule engine expands it, at alert
// time.
func TestDefaultAlertRules(t *testing.T) {
	values, err := KubePrometheusStackHelmValues(nil)
	if err != nil {
		t.Fatal(err)
	}
	// Through the chart's additionalPrometheusRulesMap (toYaml), never
	// extraManifests (tpl). Round-tripped through JSON like the HelmRelease
	// values themselves.
	var decoded map[string]any
	if err := json.Unmarshal(values.Raw, &decoded); err != nil {
		t.Fatal(err)
	}
	rulesMap, _ := decoded["additionalPrometheusRulesMap"].(map[string]any)
	entry, ok := rulesMap[defaultAlertRulesKey].(map[string]any)
	if !ok {
		t.Fatalf("additionalPrometheusRulesMap[%q] missing: %v", defaultAlertRulesKey, rulesMap)
	}
	groups, _ := entry["groups"].([]any)
	if len(groups) == 0 {
		t.Fatalf("no rule groups: %v", entry)
	}
	group, ok := groups[0].(map[string]any)
	if !ok {
		t.Fatalf("groups[0] is not an object: %v", groups[0])
	}
	rules, ok := group["rules"].([]any)
	if !ok || len(rules) == 0 {
		t.Fatalf("groups[0].rules missing or empty: %v", group["rules"])
	}

	wantAlerts := map[string]bool{
		"AuthentikBenchServiceScrapeDown":      false,
		"AuthentikBenchPostgresDown":           false,
		"AuthentikBenchHighRequestFailureRate": false,
		"AuthentikBenchWorkerTaskErrors":       false,
	}
	for _, r := range rules {
		rr, ok := r.(map[string]any)
		if !ok {
			t.Fatalf("rule entry is not an object: %v", r)
		}
		name, _ := rr["alert"].(string)
		if _, known := wantAlerts[name]; !known {
			t.Errorf("unexpected alert name %q", name)
			continue
		}
		wantAlerts[name] = true
		if _, ok := rr["expr"].(string); !ok || rr["expr"] == "" {
			t.Errorf("%s: expr missing", name)
		}
		if _, ok := rr["for"].(string); !ok {
			t.Errorf("%s: for missing", name)
		}
		// Every alert's summary references at least one $labels field -
		// confirming it, specifically, keeps its literal "{{ }}" intact is
		// what actually caught the extraManifests/tpl bug live, not a
		// generic "annotations exist" check.
		annotations, _ := rr["annotations"].(map[string]any)
		summary, _ := annotations["summary"].(string)
		if !strings.Contains(summary, "{{") || !strings.Contains(summary, "}}") {
			t.Errorf("%s: annotations.summary = %q, want it to keep its \"{{ $labels... }}\" templating syntax verbatim", name, summary)
		}
		if _, ok := annotations["description"].(string); !ok {
			t.Errorf("%s: annotations.description missing", name)
		}
	}
	for name, saw := range wantAlerts {
		if !saw {
			t.Errorf("missing alert %q", name)
		}
	}
}

func TestKubePrometheusStackHelmRelease(t *testing.T) {
	values, err := KubePrometheusStackHelmValues(nil)
	if err != nil {
		t.Fatalf("KubePrometheusStackHelmValues: %v", err)
	}

	hr := KubePrometheusStackHelmRelease(addonTestCluster("my-cluster"), values)

	if hr.Name != "kube-prometheus-stack-my-cluster" || hr.Spec.ReleaseName != KubePrometheusStackHelmReleaseName {
		t.Errorf("name = %q / releaseName = %q, want a per-cluster name and releaseName %q", hr.Name, hr.Spec.ReleaseName, KubePrometheusStackHelmReleaseName)
	}
	if hr.Spec.Chart.Spec.Chart != kubePrometheusStackChartName {
		t.Errorf("chart = %q, want %q", hr.Spec.Chart.Spec.Chart, kubePrometheusStackChartName)
	}
	if hr.Spec.TargetNamespace != ObservabilityNamespace {
		t.Errorf("targetNamespace = %q, want %q", hr.Spec.TargetNamespace, ObservabilityNamespace)
	}
	if hr.Spec.KubeConfig == nil || hr.Spec.KubeConfig.SecretRef.Name != "my-cluster-kubeconfig" {
		t.Errorf("kubeConfig.secretRef.name = %v, want my-cluster-kubeconfig", hr.Spec.KubeConfig)
	}
	// The chart's own CRDs (Prometheus/ServiceMonitor/PrometheusRule/...)
	// are exactly what this install exists to provide - both install and
	// upgrade must actually apply them, not just on first install.
	if hr.Spec.Install == nil || hr.Spec.Install.CRDs != helmv2.CreateReplace {
		t.Errorf("install.crds = %v, want CreateReplace", hr.Spec.Install)
	}
	if hr.Spec.Upgrade == nil || hr.Spec.Upgrade.CRDs != helmv2.CreateReplace {
		t.Errorf("upgrade.crds = %v, want CreateReplace", hr.Spec.Upgrade)
	}
}

func TestPrometheusResourcesFollowMonitoringPool(t *testing.T) {
	wc := addonTestCluster("c1")
	if got := prometheusResources(wc); got["limits"].(map[string]any)["memory"] != "2048Mi" {
		t.Errorf("no monitoring pool: %v", got)
	}
	wc.Spec.WorkerPools = []benchv1alpha1.WorkerPoolSpec{
		{Name: "ak", Role: benchv1alpha1.WorkerPoolRoleAuthentik, Incus: benchv1alpha1.IncusMachineSpec{Flavor: "c16-m32"}},
		{Name: "monitoring", Role: benchv1alpha1.WorkerPoolRoleMonitoring, Incus: benchv1alpha1.IncusMachineSpec{Flavor: "c4-m8"}},
	}
	got := prometheusResources(wc)
	if got["limits"].(map[string]any)["memory"] != "4915Mi" || got["requests"].(map[string]any)["memory"] != "2457Mi" {
		t.Errorf("c4-m8 monitoring pool: %v", got)
	}

	values, err := KubePrometheusStackHelmValues(wc)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(values.Raw), `"retentionSize":"16GB"`) || !strings.Contains(string(values.Raw), `"memory":"4915Mi"`) {
		t.Errorf("values missing resources/retentionSize: %s", values.Raw)
	}
}
