package render

import (
	"encoding/json"
	"testing"
)

func TestMetricsServerHelmRepository(t *testing.T) {
	repo := MetricsServerHelmRepository("default")

	if repo.Name != MetricsServerHelmRepositoryName {
		t.Errorf("name = %q, want %q", repo.Name, MetricsServerHelmRepositoryName)
	}
	if repo.Namespace != "default" {
		t.Errorf("namespace = %q, want default", repo.Namespace)
	}
	if repo.Spec.URL != metricsServerChart.Repository {
		t.Errorf("url = %q, want %q", repo.Spec.URL, metricsServerChart.Repository)
	}
}

func TestMetricsServerHelmValuesSetsInsecureTLS(t *testing.T) {
	raw, err := MetricsServerHelmValues(nil)
	if err != nil {
		t.Fatalf("MetricsServerHelmValues: %v", err)
	}

	var values map[string]any
	if err := json.Unmarshal(raw.Raw, &values); err != nil {
		t.Fatalf("unmarshal values: %v", err)
	}

	args, ok := values["args"].([]any)
	if !ok || len(args) != 1 || args[0] != "--kubelet-insecure-tls" {
		t.Errorf("values.args = %v, want [--kubelet-insecure-tls]", values["args"])
	}
}

func TestMetricsServerHelmRelease(t *testing.T) {
	values, err := MetricsServerHelmValues(nil)
	if err != nil {
		t.Fatalf("MetricsServerHelmValues: %v", err)
	}

	hr := MetricsServerHelmRelease(addonTestCluster("my-cluster"), values)

	if hr.Name != "metrics-server-my-cluster" || hr.Spec.ReleaseName != MetricsServerHelmReleaseName {
		t.Errorf("name = %q / releaseName = %q, want a per-cluster name and releaseName %q", hr.Name, hr.Spec.ReleaseName, MetricsServerHelmReleaseName)
	}
	if hr.Spec.Chart.Spec.Chart != metricsServerChartName {
		t.Errorf("chart = %q, want %q", hr.Spec.Chart.Spec.Chart, metricsServerChartName)
	}
	if hr.Spec.TargetNamespace != MetricsServerNamespace {
		t.Errorf("targetNamespace = %q, want %q", hr.Spec.TargetNamespace, MetricsServerNamespace)
	}
	if hr.Spec.KubeConfig == nil || hr.Spec.KubeConfig.SecretRef.Name != "my-cluster-kubeconfig" {
		t.Errorf("kubeConfig.secretRef.name = %v, want my-cluster-kubeconfig", hr.Spec.KubeConfig)
	}
}
