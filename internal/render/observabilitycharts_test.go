package render

import (
	"encoding/json"
	"strings"
	"testing"

	helmv2 "github.com/fluxcd/helm-controller/api/v2"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
)

func observabilityCluster() *benchv1alpha1.WorkloadCluster {
	wc := addonTestCluster("c1")
	wc.Spec.Incus = &benchv1alpha1.IncusProviderSpec{}
	wc.Spec.WorkerPools = []benchv1alpha1.WorkerPoolSpec{{Name: "monitoring", Role: benchv1alpha1.WorkerPoolRoleMonitoring}}
	return wc
}

func valuesOf(t *testing.T, hr *helmv2.HelmRelease) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal(hr.Spec.Values.Raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestObservabilityHelmReleases(t *testing.T) {
	wc := observabilityCluster()
	grafana, err := GrafanaHelmRelease(wc)
	if err != nil {
		t.Fatal(err)
	}
	tempo, err := TempoHelmRelease(wc)
	if err != nil {
		t.Fatal(err)
	}
	pyroscope, err := PyroscopeHelmRelease(wc)
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		hr      *helmv2.HelmRelease
		repo    string
		service string
	}{
		"grafana":   {grafana, GrafanaCommunityHelmRepositoryName, GrafanaServiceName},
		"tempo":     {tempo, GrafanaCommunityHelmRepositoryName, TempoServiceName},
		"pyroscope": {pyroscope, GrafanaHelmRepositoryName, PyroscopeServiceName},
	} {
		hr := tc.hr
		if hr.Name != name+"-c1" || hr.Spec.ReleaseName != name || hr.Spec.TargetNamespace != ObservabilityNamespace {
			t.Errorf("%s: name %q release %q target %q", name, hr.Name, hr.Spec.ReleaseName, hr.Spec.TargetNamespace)
		}
		if hr.Spec.Chart.Spec.SourceRef.Name != tc.repo || hr.Spec.Chart.Spec.Version == "" {
			t.Errorf("%s: chart %+v", name, hr.Spec.Chart.Spec)
		}
		if hr.Spec.KubeConfig == nil || hr.Spec.KubeConfig.SecretRef.Name != "c1-kubeconfig" {
			t.Errorf("%s: not installed on the workload cluster", name)
		}
		// Its ServiceMonitor needs kube-prometheus-stack's CRDs first.
		if len(hr.Spec.DependsOn) != 1 || hr.Spec.DependsOn[0].Name != "kube-prometheus-stack-c1" {
			t.Errorf("%s: dependsOn %+v", name, hr.Spec.DependsOn)
		}
		if hr.Spec.Install.Strategy == nil || hr.Spec.Install.Strategy.Name != string(helmv2.ActionStrategyRetryOnFailure) {
			t.Errorf("%s: no retry strategy", name)
		}
		v := valuesOf(t, hr)
		// The Service keeps the name authentik / Grafana's datasources use.
		if v["fullnameOverride"] != tc.service {
			t.Errorf("%s: fullnameOverride %v, want %s", name, v["fullnameOverride"], tc.service)
		}
		raw := string(hr.Spec.Values.Raw)
		for _, want := range []string{`"storageClassName":"incus-block"`, `"serviceMonitor":{"enabled":true}`, `"bench.goauthentik.io/pool":"monitoring"`} {
			if !strings.Contains(raw, want) {
				t.Errorf("%s: values missing %s", name, want)
			}
		}
	}

	// Pyroscope: no bundled Alloy agent or MinIO.
	pv := valuesOf(t, pyroscope)
	if pv["alloy"].(map[string]any)["enabled"] != false || pv["minio"].(map[string]any)["enabled"] != false {
		t.Errorf("pyroscope extras enabled: %v %v", pv["alloy"], pv["minio"])
	}
}

func TestGrafanaHelmReleaseWiring(t *testing.T) {
	wc := observabilityCluster()
	hr, err := GrafanaHelmRelease(wc)
	if err != nil {
		t.Fatal(err)
	}
	v := valuesOf(t, hr)

	// cmd/apiserver's proxy reaches Grafana on this fixed NodePort.
	svc := v["service"].(map[string]any)
	if svc["type"] != "NodePort" || svc["nodePort"] != float64(GrafanaNodePort) || svc["port"] != float64(GrafanaPort) {
		t.Errorf("service = %v", svc)
	}
	// The admin password comes from the management-cluster Secret, never
	// the HelmRelease itself.
	if _, inline := v["adminPassword"]; inline {
		t.Error("admin password written into the HelmRelease values")
	}
	if len(hr.Spec.ValuesFrom) != 1 || hr.Spec.ValuesFrom[0].Name != GrafanaAdminSecretName(wc) || hr.Spec.ValuesFrom[0].TargetPath != "adminPassword" {
		t.Errorf("valuesFrom = %+v", hr.Spec.ValuesFrom)
	}
	ini := v["grafana.ini"].(map[string]any)
	if ini["server"].(map[string]any)["root_url"] != "/grafana-proxy/c1/" || ini["security"].(map[string]any)["allow_embedding"] != true {
		t.Errorf("grafana.ini = %v", ini)
	}

	// Datasources point at this project's Service names, and every
	// dashboard is provisioned.
	raw := string(hr.Spec.Values.Raw)
	for _, want := range []string{
		"http://prometheus-server.bench-system.svc.cluster.local:9090",
		"http://pyroscope.bench-system.svc.cluster.local:4040",
		"http://tempo.bench-system.svc.cluster.local:3200",
		`"uid":"` + TempoDatasourceUID + `"`,
		"http://loki.bench-system.svc.cluster.local:3100",
		`"uid":"` + LokiDatasourceUID + `"`,
		`"path":"` + grafanaDashboardsPath + `"`,
	} {
		if !strings.Contains(raw, want) {
			t.Errorf("values missing %s", want)
		}
	}
	dashboards := v["dashboards"].(map[string]any)["benchmarks"].(map[string]any)
	for _, d := range []string{"compare-tests", "k6", "authentik", "postgres", "nodes", "node_exporter", "summary"} {
		entry, ok := dashboards[d].(map[string]any)
		if !ok || !json.Valid([]byte(entry["json"].(string))) {
			t.Errorf("dashboard %s missing or not valid JSON", d)
		}
	}
}

func TestLokiHelmRelease(t *testing.T) {
	wc := observabilityCluster()
	hr, err := LokiHelmRelease(wc)
	if err != nil {
		t.Fatal(err)
	}
	if hr.Name != "loki-c1" || hr.Spec.Chart.Spec.SourceRef.Name != GrafanaCommunityHelmRepositoryName || hr.Spec.TargetNamespace != ObservabilityNamespace {
		t.Errorf("loki: name %q chart %+v target %q", hr.Name, hr.Spec.Chart.Spec, hr.Spec.TargetNamespace)
	}
	if len(hr.Spec.DependsOn) != 1 || hr.Spec.DependsOn[0].Name != "kube-prometheus-stack-c1" {
		t.Errorf("loki: dependsOn %+v", hr.Spec.DependsOn)
	}
	v := valuesOf(t, hr)
	if v["fullnameOverride"] != LokiServiceName || v["deploymentMode"] != "Monolithic" {
		t.Errorf("loki: fullnameOverride %v deploymentMode %v", v["fullnameOverride"], v["deploymentMode"])
	}
	loki := v["loki"].(map[string]any)
	// Alloy pushes without a tenant, so multi-tenancy must be off.
	if loki["auth_enabled"] != false || loki["storage"].(map[string]any)["type"] != "filesystem" {
		t.Errorf("loki config = %v", loki)
	}
	single := v["singleBinary"].(map[string]any)
	if single["replicas"] != float64(1) || single["persistence"].(map[string]any)["storageClass"] != "incus-block" {
		t.Errorf("singleBinary = %v", single)
	}
	raw := string(hr.Spec.Values.Raw)
	for _, want := range []string{`"serviceMonitor":{"enabled":true}`, `"bench.goauthentik.io/pool":"monitoring"`, `"gateway":{"enabled":false}`} {
		if !strings.Contains(raw, want) {
			t.Errorf("loki: values missing %s", want)
		}
	}
}

func TestAlloyHelmRelease(t *testing.T) {
	wc := observabilityCluster()
	hr, err := AlloyHelmRelease(wc)
	if err != nil {
		t.Fatal(err)
	}
	if hr.Name != "alloy-c1" || hr.Spec.Chart.Spec.SourceRef.Name != GrafanaHelmRepositoryName || hr.Spec.TargetNamespace != ObservabilityNamespace {
		t.Errorf("alloy: name %q chart %+v target %q", hr.Name, hr.Spec.Chart.Spec, hr.Spec.TargetNamespace)
	}
	v := valuesOf(t, hr)
	// A DaemonSet on every node, tainted or not, reading the node's pod logs.
	controller := v["controller"].(map[string]any)
	if controller["type"] != "daemonset" || len(controller["tolerations"].([]any)) != 1 {
		t.Errorf("controller = %v", controller)
	}
	alloy := v["alloy"].(map[string]any)
	if alloy["mounts"].(map[string]any)["varlog"] != true || alloy["enableReporting"] != false {
		t.Errorf("alloy = %v", alloy)
	}
	config := alloy["configMap"].(map[string]any)["content"].(string)
	for _, want := range []string{
		`url = "http://loki.bench-system.svc.cluster.local:3100/loki/api/v1/push"`,
		`target_label  = "namespace"`,
		`target_label  = "pod"`,
		`"spec.nodeName=" + sys.env("K8S_NODE_NAME")`,
		"stage.cri {}",
	} {
		if !strings.Contains(config, want) {
			t.Errorf("alloy config missing %s:\n%s", want, config)
		}
	}
}

// The endpoints authentik is configured with
// (internal/controller/authentikdeployment_controller.go) must keep working.
func TestObservabilityServiceContracts(t *testing.T) {
	if TempoServiceName != "tempo" || TempoOTLPHTTPPort != 4318 || PyroscopeServiceName != "pyroscope" || PyroscopePort != 4040 || ObservabilityNamespace != "bench-system" {
		t.Error("observability Service names/ports changed - authentik's defaults point at them")
	}
}
