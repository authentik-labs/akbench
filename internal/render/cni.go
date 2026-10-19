package render

import (
	helmv2 "github.com/fluxcd/helm-controller/api/v2"
	fluxmeta "github.com/fluxcd/pkg/apis/meta"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
)

// Flannel is installed via its own official Helm chart now (flannel-io.github.io/flannel),
// the same Flux HelmRepository + HelmRelease mechanism csi-incus uses
// (internal/render/csiincus.go) - replacing an earlier vendored
// kube-flannel.yml go:embed. The chart's default podCidr (10.244.0.0/16)
// already matches the pod CIDR render.Cluster sets for every
// WorkloadCluster, so no values overrides are needed.
const (
	CNIHelmRepositoryName = "flannel"
	cniChartName          = "flannel"

	CNIHelmReleaseName = "flannel"

	// CNINamespace matches the original vendored manifest's namespace.
	CNINamespace = "kube-flannel"
)

// cniChart is the version and repository deps/Chart.yaml pins.
var cniChart = pinnedChart(cniChartName)

// CNIHelmRepository is the (singleton, per-namespace) HelmRepository
// pointing at the official Flannel chart repo.
func CNIHelmRepository(namespace string) *sourcev1.HelmRepository {
	return &sourcev1.HelmRepository{
		Name:      CNIHelmRepositoryName,
		Namespace: namespace,
		Spec: sourcev1.HelmRepositorySpec{
			URL:      cniChart.Repository,
			Interval: metav1.Duration{Duration: helmRepoInterval},
		},
	}
}

// CNIHelmRelease builds the Flux HelmRelease that installs Flannel onto a
// workload cluster remotely, through wc's "<cluster>-kubeconfig" Secret CAPI writes (see
// KubeconfigSecretName). Its object name is per-cluster (AddonHelmReleaseName).
// CreateNamespace is set since, unlike csi-incus's kube-system, kube-flannel
// doesn't already exist on a fresh kubeadm cluster.
func CNIHelmRelease(wc *benchv1alpha1.WorkloadCluster) *helmv2.HelmRelease {
	return retryOnFailure(&helmv2.HelmRelease{
		Name:      AddonHelmReleaseName(wc, CNIHelmReleaseName),
		Namespace: wc.Namespace,
		Spec: helmv2.HelmReleaseSpec{
			Interval:    metav1.Duration{Duration: helmRepoInterval},
			Timeout:     &metav1.Duration{Duration: helmInstallTimeout},
			ReleaseName: CNIHelmReleaseName,
			Chart: &helmv2.HelmChartTemplate{
				Spec: helmv2.HelmChartTemplateSpec{
					Chart:   cniChartName,
					Version: cniChart.Version,
					SourceRef: helmv2.CrossNamespaceObjectReference{
						Kind:      sourcev1.HelmRepositoryKind,
						Name:      CNIHelmRepositoryName,
						Namespace: wc.Namespace,
					},
				},
			},
			TargetNamespace: CNINamespace,
			Install:         &helmv2.Install{CreateNamespace: true},
			KubeConfig: &fluxmeta.KubeConfigReference{
				SecretRef: &fluxmeta.SecretKeyReference{Name: KubeconfigSecretName(wc)},
			},
		},
	})
}
