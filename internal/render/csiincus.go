package render

import (
	"encoding/json"
	"fmt"

	helmv2 "github.com/fluxcd/helm-controller/api/v2"
	fluxmeta "github.com/fluxcd/pkg/apis/meta"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
)

// csi-incus (github.com/BeryJu/csi-incus) publishes its own Helm chart to
// GHCR as an OCI artifact on every vX.Y.Z release tag - installed the same
// way AuthentikDeployment's own chart is (internal/render/helmrelease.go): a
// Flux HelmRepository + HelmRelease applied on the management cluster, with
// spec.kubeConfig.secretRef pointing helm-controller at the workload
// cluster to install onto remotely. This replaced an earlier
// vendored-raw-YAML approach (go:embed'd deploy/*.yaml) once the chart
// existed - Flux/helm-controller already handles upgrades, rollback, and
// drift the hand-rolled createAll() helper never did.
const (
	// CSIIncusHelmRepositoryName/csiIncusChartName/csiIncusChart.Repository locate
	// the chart itself.
	CSIIncusHelmRepositoryName = "csi-incus"
	csiIncusChartName          = "csi-incus"

	CSIIncusHelmReleaseName = "csi-incus"

	// CSIIncusNamespace matches the chart's own convention (and the
	// original hand-vendored manifests' hardcoded namespace) for where a
	// CSI driver's controller/node components live.
	CSIIncusNamespace = "kube-system"

	// CSIIncusStoragePool is the Incus storage pool the chart's
	// StorageClass provisions volumes from on every workload cluster.
	CSIIncusStoragePool = "vms"

	// CSIIncusStorageClassName is the sole StorageClass on every workload
	// cluster (marked default - there's no local-path-provisioner fallback
	// any more).
	CSIIncusStorageClassName = "incus-block"
)

// csiIncusChart is the version and repository deps/Chart.yaml pins.
var csiIncusChart = pinnedChart(csiIncusChartName)

// CSIIncusHelmRepository is the (singleton, per-namespace) OCI HelmRepository
// pointing at csi-incus's published chart.
func CSIIncusHelmRepository(namespace string) *sourcev1.HelmRepository {
	return &sourcev1.HelmRepository{
		Name:      CSIIncusHelmRepositoryName,
		Namespace: namespace,
		Spec: sourcev1.HelmRepositorySpec{
			Type:     sourcev1.HelmRepositoryTypeOCI,
			URL:      csiIncusChart.Repository,
			Interval: metav1.Duration{Duration: helmRepoInterval},
		},
	}
}

// CSIIncusHelmValues builds spec.values for the csi-incus chart install,
// remapping this WorkloadCluster's own Incus/LXC connection Secret
// (spec.incus.lxcSecretRef - keys server/client-crt/client-key/server-crt,
// see that field's own doc comment) into the chart's
// incus.addr/incus.tls.* values. Reuses the existing CAPI provisioning
// credentials instead of requiring a second certificate to be generated and
// registered per the chart's own README - the chart creates its own TLS
// Secret from these values directly (see its secret-incus-tls.yaml
// template), so no Secret needs to be built or applied here. Overrides the
// chart's single default StorageClass to provision from the "vms" Incus
// pool and become the cluster's default. Disables the chart's default
// VolumeSnapshotClass: this project doesn't install the
// snapshot.storage.k8s.io CRDs/external-snapshotter controller on any
// workload cluster, so leaving it on fails the whole Helm install
// ("resource mapping not found for name: incus-block ... no matches for
// kind VolumeSnapshotClass") rather than just sitting inert - a pure
// scope cut, not a capability regression from the earlier vendored-YAML
// approach (which never applied a VolumeSnapshotClass either).
func CSIIncusHelmValues(source *corev1.Secret, wc *benchv1alpha1.WorkloadCluster) (*apiextensionsv1.JSON, error) {
	incus := map[string]any{
		"addr": string(source.Data["server"]),
		"tls": map[string]any{
			"clientCert": string(source.Data["client-crt"]),
			"clientKey":  string(source.Data["client-key"]),
			"serverCert": string(source.Data["server-crt"]),
		},
	}
	// The same project cluster-api-provider-incus creates the machines in -
	// csi-incus has to find those instances to attach volumes to them.
	if project := string(source.Data["project"]); project != "" {
		incus["project"] = project
	}
	if string(source.Data["insecure-skip-verify"]) == "true" {
		incus["insecureSkipVerify"] = true
	}
	values := map[string]any{
		"incus": incus,
		"storageClasses": []map[string]any{
			{
				"name":                 CSIIncusStorageClassName,
				"pool":                 CSIIncusStoragePool,
				"contentType":          "block",
				"reclaimPolicy":        "Delete",
				"volumeBindingMode":    "WaitForFirstConsumer",
				"allowVolumeExpansion": true,
				"isDefaultClass":       true,
			},
		},
		"volumeSnapshotClass": map[string]any{
			"create": false,
		},
	}
	// controller.* only - never node.*: the node plugin is a DaemonSet that
	// has to run on every node regardless of pool (it only formats/mounts a
	// device the controller already attached), and the chart's own default
	// already gives it a broad `operator: Exists` toleration for exactly
	// that reason (confirmed against its own values.yaml) - scoping it to
	// one pool would break volume mounting everywhere else.
	if sel, tol := MonitoringPoolPlacement(wc); len(sel) > 0 || len(tol) > 0 {
		values["controller"] = map[string]any{
			"nodeSelector": sel,
			"tolerations":  tol,
		}
	}
	raw, err := json.Marshal(values)
	if err != nil {
		return nil, fmt.Errorf("marshal csi-incus helm values: %w", err)
	}
	return &apiextensionsv1.JSON{Raw: raw}, nil
}

// CSIIncusHelmRelease builds the Flux HelmRelease that installs csi-incus
// onto a workload cluster remotely, through wc's "<cluster>-kubeconfig" Secret CAPI writes (see
// KubeconfigSecretName). Its object name is per-cluster (AddonHelmReleaseName).
func CSIIncusHelmRelease(wc *benchv1alpha1.WorkloadCluster, values *apiextensionsv1.JSON) *helmv2.HelmRelease {
	return retryOnFailure(&helmv2.HelmRelease{
		Name:      AddonHelmReleaseName(wc, CSIIncusHelmReleaseName),
		Namespace: wc.Namespace,
		Spec: helmv2.HelmReleaseSpec{
			Interval:    metav1.Duration{Duration: helmRepoInterval},
			Timeout:     &metav1.Duration{Duration: helmInstallTimeout},
			ReleaseName: CSIIncusHelmReleaseName,
			Chart: &helmv2.HelmChartTemplate{
				Spec: helmv2.HelmChartTemplateSpec{
					Chart:   csiIncusChartName,
					Version: csiIncusChart.Version,
					SourceRef: helmv2.CrossNamespaceObjectReference{
						Kind:      sourcev1.HelmRepositoryKind,
						Name:      CSIIncusHelmRepositoryName,
						Namespace: wc.Namespace,
					},
				},
			},
			TargetNamespace: CSIIncusNamespace,
			Values:          values,
			KubeConfig: &fluxmeta.KubeConfigReference{
				SecretRef: &fluxmeta.SecretKeyReference{Name: KubeconfigSecretName(wc)},
			},
		},
	})
}
