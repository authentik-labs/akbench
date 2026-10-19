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

// metrics-server is installed via its own official Helm chart now
// (kubernetes-sigs.github.io/metrics-server), the same Flux
// HelmRepository + HelmRelease mechanism csi-incus uses
// (internal/render/csiincus.go) - replacing an earlier vendored
// components.yaml go:embed. The only customization needed is
// --kubelet-insecure-tls, appended via the chart's own "args" value:
// kubeadm-bootstrapped clusters (every WorkloadCluster this operator
// creates) don't wire up a kubelet serving certificate signed by a CA
// metrics-server trusts by default, so without this flag every kubelet
// scrape fails with "x509: certificate signed by unknown authority" and
// `kubectl top nodes/pods` never returns data - a well-known, standard
// requirement for kubeadm clusters specifically (cloud-managed clusters
// normally don't need it).
const (
	MetricsServerHelmRepositoryName = "metrics-server"
	metricsServerChartName          = "metrics-server"

	MetricsServerHelmReleaseName = "metrics-server"

	// MetricsServerNamespace matches the original vendored manifest's
	// namespace - already exists on every cluster, so no createNamespace
	// needed.
	MetricsServerNamespace = "kube-system"
)

// metricsServerChart is the version and repository deps/Chart.yaml pins.
var metricsServerChart = pinnedChart(metricsServerChartName)

// MetricsServerHelmRepository is the (singleton, per-namespace)
// HelmRepository pointing at the official metrics-server chart repo.
func MetricsServerHelmRepository(namespace string) *sourcev1.HelmRepository {
	return &sourcev1.HelmRepository{
		Name:      MetricsServerHelmRepositoryName,
		Namespace: namespace,
		Spec: sourcev1.HelmRepositorySpec{
			URL:      metricsServerChart.Repository,
			Interval: metav1.Duration{Duration: helmRepoInterval},
		},
	}
}

// MetricsServerHelmValues builds spec.values for the metrics-server chart
// install: --kubelet-insecure-tls (see this file's own doc comment) plus
// this cluster's own monitoring-pool nodeSelector/tolerations (top-level
// keys on this chart, confirmed against its own values.yaml), when it has
// one.
func MetricsServerHelmValues(wc *benchv1alpha1.WorkloadCluster) (*apiextensionsv1.JSON, error) {
	values := map[string]any{
		"args": []string{"--kubelet-insecure-tls"},
	}
	if sel, tol := MonitoringPoolPlacement(wc); len(sel) > 0 || len(tol) > 0 {
		values["nodeSelector"] = sel
		values["tolerations"] = tol
	}
	raw, err := json.Marshal(values)
	if err != nil {
		return nil, fmt.Errorf("marshal metrics-server helm values: %w", err)
	}
	return &apiextensionsv1.JSON{Raw: raw}, nil
}

// MetricsServerHelmRelease builds the Flux HelmRelease that installs
// metrics-server onto a workload cluster remotely - kubeConfigSecretName is
// the "<cluster>-kubeconfig" Secret CAPI writes (see
// render.KubeconfigSecretName).
func MetricsServerHelmRelease(wc *benchv1alpha1.WorkloadCluster, values *apiextensionsv1.JSON) *helmv2.HelmRelease {
	return retryOnFailure(&helmv2.HelmRelease{
		Name:      AddonHelmReleaseName(wc, MetricsServerHelmReleaseName),
		Namespace: wc.Namespace,
		Spec: helmv2.HelmReleaseSpec{
			Interval:    metav1.Duration{Duration: helmRepoInterval},
			Timeout:     &metav1.Duration{Duration: helmInstallTimeout},
			ReleaseName: MetricsServerHelmReleaseName,
			Chart: &helmv2.HelmChartTemplate{
				Spec: helmv2.HelmChartTemplateSpec{
					Chart:   metricsServerChartName,
					Version: metricsServerChart.Version,
					SourceRef: helmv2.CrossNamespaceObjectReference{
						Kind:      sourcev1.HelmRepositoryKind,
						Name:      MetricsServerHelmRepositoryName,
						Namespace: wc.Namespace,
					},
				},
			},
			TargetNamespace: MetricsServerNamespace,
			Values:          values,
			KubeConfig: &fluxmeta.KubeConfigReference{
				SecretRef: &fluxmeta.SecretKeyReference{Name: KubeconfigSecretName(wc)},
			},
		},
	})
}
