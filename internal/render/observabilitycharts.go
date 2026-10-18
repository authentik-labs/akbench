package render

import (
	"encoding/json"
	"fmt"

	helmv2 "github.com/fluxcd/helm-controller/api/v2"
	fluxmeta "github.com/fluxcd/pkg/apis/meta"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
)

// Grafana, Tempo, Pyroscope, Loki and Alloy install from their upstream
// Helm charts, like every other add-on: a Flux HelmRelease on the
// management cluster pointed at the workload cluster (spec.kubeConfig), in
// ObservabilityNamespace next to kube-prometheus-stack. Grafana, Tempo and
// Loki moved to the grafana-community repository (the grafana/helm-charts
// copies are deprecated); Pyroscope and Alloy are still published in
// grafana/helm-charts.
const (
	GrafanaCommunityHelmRepositoryName = "grafana-community"
	grafanaCommunityChartRepoURL       = "https://grafana-community.github.io/helm-charts"
	GrafanaHelmRepositoryName          = "grafana"
	grafanaChartRepoURL                = "https://grafana.github.io/helm-charts"

	// Chart versions, pinned like every other add-on's.
	grafanaChartVersion   = "13.5.1"
	tempoChartVersion     = "3.1.0"
	pyroscopeChartVersion = "2.4.0"
	lokiChartVersion      = "18.13.8"
	alloyChartVersion     = "1.13.0"

	GrafanaHelmReleaseName   = "grafana"
	TempoHelmReleaseName     = "tempo"
	PyroscopeHelmReleaseName = "pyroscope"
	LokiHelmReleaseName      = "loki"
	AlloyHelmReleaseName     = "alloy"
)

// GrafanaCommunityHelmRepository serves the grafana, tempo and loki charts.
func GrafanaCommunityHelmRepository(namespace string) *sourcev1.HelmRepository {
	return helmRepository(GrafanaCommunityHelmRepositoryName, namespace, grafanaCommunityChartRepoURL)
}

// GrafanaHelmRepository serves the pyroscope and alloy charts.
func GrafanaHelmRepository(namespace string) *sourcev1.HelmRepository {
	return helmRepository(GrafanaHelmRepositoryName, namespace, grafanaChartRepoURL)
}

func helmRepository(name, namespace, url string) *sourcev1.HelmRepository {
	return &sourcev1.HelmRepository{
		Name:      name,
		Namespace: namespace,
		Spec: sourcev1.HelmRepositorySpec{
			URL:      url,
			Interval: metav1.Duration{Duration: helmRepoInterval},
		},
	}
}

// observabilityHelmRelease installs one observability chart into
// ObservabilityNamespace on wc. Every one of them depends on
// kube-prometheus-stack: each ships a ServiceMonitor, whose CRD that
// release installs.
func observabilityHelmRelease(wc *benchv1alpha1.WorkloadCluster, release, chart, version, repository string, values map[string]any) (*helmv2.HelmRelease, error) {
	raw, err := json.Marshal(values)
	if err != nil {
		return nil, fmt.Errorf("marshal %s helm values: %w", release, err)
	}
	return retryOnFailure(&helmv2.HelmRelease{
		Name:      AddonHelmReleaseName(wc, release),
		Namespace: wc.Namespace,
		Spec: helmv2.HelmReleaseSpec{
			Interval:    metav1.Duration{Duration: helmRepoInterval},
			Timeout:     &metav1.Duration{Duration: helmInstallTimeout},
			ReleaseName: release,
			Chart: &helmv2.HelmChartTemplate{
				Spec: helmv2.HelmChartTemplateSpec{
					Chart:   chart,
					Version: version,
					SourceRef: helmv2.CrossNamespaceObjectReference{
						Kind:      sourcev1.HelmRepositoryKind,
						Name:      repository,
						Namespace: wc.Namespace,
					},
				},
			},
			TargetNamespace: ObservabilityNamespace,
			Install:         &helmv2.Install{CreateNamespace: true},
			DependsOn: []helmv2.DependencyReference{
				{Name: AddonHelmReleaseName(wc, KubePrometheusStackHelmReleaseName)},
			},
			Values: &apiextensionsv1.JSON{Raw: raw},
			KubeConfig: &fluxmeta.KubeConfigReference{
				SecretRef: &fluxmeta.SecretKeyReference{Name: KubeconfigSecretName(wc)},
			},
		},
	}), nil
}

// monitoringPlacement adds the monitoring pool's nodeSelector/tolerations
// to values (a chart's pod-level keys), when wc has such a pool.
func monitoringPlacement(wc *benchv1alpha1.WorkloadCluster, values map[string]any) {
	if sel, tol := MonitoringPoolPlacement(wc); len(sel) > 0 || len(tol) > 0 {
		values["nodeSelector"] = sel
		values["tolerations"] = tol
	}
}

// chartPersistence is the persistence block the grafana, tempo and
// pyroscope charts share.
func chartPersistence(wc *benchv1alpha1.WorkloadCluster, size string) map[string]any {
	p := map[string]any{"enabled": true, "size": size}
	if class := observabilityStorageClass(wc); class != nil {
		p["storageClassName"] = *class
	}
	return p
}
