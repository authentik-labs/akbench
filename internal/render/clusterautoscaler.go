package render

import (
	"encoding/json"
	"fmt"

	helmv2 "github.com/fluxcd/helm-controller/api/v2"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
)

// cluster-autoscaler is installed via its own official Helm chart
// (kubernetes.github.io/autoscaler), one release per WorkloadCluster that
// actually uses autoscaling (see HasAutoscalingPool) - unlike ensureCNI/
// ensureCSIIncus/ensureMetricsServer/ensurePrometheus, this HelmRelease
// carries no spec.kubeConfig: cluster-autoscaler must run on the SAME
// cluster its target Cluster API objects (MachineDeployment/Cluster) live
// on - the management cluster - so this installs locally, wherever
// helm-controller itself runs, exactly like AuthentikDeploymentReconciler's
// own local objects.
//
// Verified directly against the chart's own templates/deployment.yaml and
// values.yaml (not assumed): clusterAPIMode's four hyphenated values are
// "workloadClusterMode-managementClusterMode", so "kubeconfig-incluster" -
// not the more intuitive-sounding "incluster-kubeconfig" - is the one that
// renders `--kubeconfig=<path>` with no --cloud-config flag, matching
// "autoscaler pod runs where the CAPI objects live (management, in-cluster
// via its own ServiceAccount) but manages a *different* cluster's nodes (the
// workload cluster, via a mounted kubeconfig)". clusterAPIKubeconfigSecret's
// default mount path (/etc/kubernetes/value) already matches the data key
// ("value") CAPI's own generated "<cluster>-kubeconfig" Secret uses, so no
// extra values are needed to wire the two together beyond the Secret's name.
const (
	ClusterAutoscalerHelmRepositoryName = "cluster-autoscaler"
	clusterAutoscalerChartName          = "cluster-autoscaler"
)

// clusterAutoscalerChart is the version and repository deps/Chart.yaml pins.
var clusterAutoscalerChart = pinnedChart(clusterAutoscalerChartName)

// ClusterAutoscalerHelmReleaseName is unique per WorkloadCluster (unlike
// CNI/CSI/metrics-server, which each only ever install once per *workload*
// cluster and so can use a fixed name there) - every cluster-autoscaler
// instance this operator installs lives in the one shared management
// cluster, so a fixed name would collide the moment a second WorkloadCluster
// also wants autoscaling. The chart's own fullname template folds this
// release name into every object it renders, so object names stay unique
// even though every instance now shares one namespace (see
// ClusterAutoscalerNamespace's own doc comment for why).
func ClusterAutoscalerHelmReleaseName(wc *benchv1alpha1.WorkloadCluster) string {
	return "cluster-autoscaler-" + wc.Name
}

// ClusterAutoscalerNamespace is wc.Namespace itself - confirmed live this
// can't be a separate per-cluster namespace the way the release name is
// per-cluster: clusterAPIKubeconfigSecret names a plain Secret volume mount,
// and a Secret volume can only ever be mounted from the same namespace the
// pod itself runs in. The "<cluster>-kubeconfig" Secret CAPI writes lives in
// wc.Namespace, so this pod has to run there too, not in a dedicated
// namespace of its own.
func ClusterAutoscalerNamespace(wc *benchv1alpha1.WorkloadCluster) string {
	return wc.Namespace
}

// HasAutoscalingPool reports whether any of this WorkloadCluster's worker
// pools has autoscaling configured - ensureClusterAutoscaler is a no-op
// without one, the same "only install what's actually used" reasoning
// ensureCSIIncus already applies to spec.incus.
func HasAutoscalingPool(wc *benchv1alpha1.WorkloadCluster) bool {
	for _, pool := range wc.Spec.WorkerPools {
		if pool.Autoscaling != nil {
			return true
		}
	}
	return false
}

// ClusterAutoscalerHelmRepository is the (singleton, per-namespace)
// HelmRepository pointing at the official kubernetes/autoscaler chart repo -
// shared by every WorkloadCluster's own cluster-autoscaler HelmRelease, the
// same way CNIHelmRepository/MetricsServerHelmRepository already are.
func ClusterAutoscalerHelmRepository(namespace string) *sourcev1.HelmRepository {
	return &sourcev1.HelmRepository{
		Name:      ClusterAutoscalerHelmRepositoryName,
		Namespace: namespace,
		Spec: sourcev1.HelmRepositorySpec{
			URL:      clusterAutoscalerChart.Repository,
			Interval: metav1.Duration{Duration: helmRepoInterval},
		},
	}
}

// ClusterAutoscalerHelmValues builds spec.values for one WorkloadCluster's
// cluster-autoscaler install:
//   - cloudProvider/clusterAPIMode as this file's own doc comment explains.
//   - clusterAPIKubeconfigSecret names the exact "<cluster>-kubeconfig"
//     Secret CAPI already writes on the management cluster (render.KubeconfigSecretName) -
//     no copying/relabeling needed, it already lives right next to where
//     this HelmRelease installs.
//   - autoDiscovery.namespace/clusterName scope this instance to exactly
//     this WorkloadCluster's own MachineDeployments: every WorkloadCluster's
//     CAPI objects live in the same management-cluster namespace
//     (wc.Namespace), so without this a second cluster's autoscaler
//     instance would also see (and potentially scale) this one's node
//     groups.
//   - rbac.additionalRules grants read access to LXCMachineTemplate - the
//     chart's own default ClusterRole only covers the provider-agnostic
//     Cluster API kinds (MachineDeployment/MachineSet/Machine/MachinePool),
//     never an infrastructure provider's own machine-template CRD, which
//     cloudprovider/clusterapi's own README documents as required for
//     scale-from-zero (estimating a new node's capacity with no existing
//     Machine to sample). Confirmed live: without this, the pod logs a
//     continuous "cannot list resource lxcmachinetemplates" watch error.
//     Incus-specific for now, matching every other Incus-only piece of this
//     package (e.g. csiincus.go) - a second infrastructure provider would
//     need its own equivalent rule here.
func ClusterAutoscalerHelmValues(wc *benchv1alpha1.WorkloadCluster) (*apiextensionsv1.JSON, error) {
	raw, err := json.Marshal(map[string]any{
		"cloudProvider":              "clusterapi",
		"clusterAPIMode":             "kubeconfig-incluster",
		"clusterAPIKubeconfigSecret": KubeconfigSecretName(wc),
		"autoDiscovery": map[string]any{
			"namespace":   wc.Namespace,
			"clusterName": wc.Name,
		},
		"rbac": map[string]any{
			"additionalRules": []map[string]any{
				{
					"apiGroups": []string{"infrastructure.cluster.x-k8s.io"},
					"resources": []string{"lxcmachinetemplates"},
					"verbs":     []string{"get", "list", "watch"},
				},
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("marshal cluster-autoscaler helm values: %w", err)
	}
	return &apiextensionsv1.JSON{Raw: raw}, nil
}

// ClusterAutoscalerHelmRelease builds the Flux HelmRelease that installs
// cluster-autoscaler for one WorkloadCluster - deliberately no
// spec.kubeConfig (see this file's own doc comment for why): this installs
// on whichever cluster helm-controller itself runs on, the management
// cluster, not remotely onto the workload cluster the way every other addon
// in this package does.
func ClusterAutoscalerHelmRelease(wc *benchv1alpha1.WorkloadCluster, values *apiextensionsv1.JSON) *helmv2.HelmRelease {
	name := ClusterAutoscalerHelmReleaseName(wc)
	return retryOnFailure(&helmv2.HelmRelease{
		Name:      name,
		Namespace: wc.Namespace,
		Spec: helmv2.HelmReleaseSpec{
			Interval:    metav1.Duration{Duration: helmRepoInterval},
			Timeout:     &metav1.Duration{Duration: helmInstallTimeout},
			ReleaseName: name,
			Chart: &helmv2.HelmChartTemplate{
				Spec: helmv2.HelmChartTemplateSpec{
					Chart:   clusterAutoscalerChartName,
					Version: clusterAutoscalerChart.Version,
					SourceRef: helmv2.CrossNamespaceObjectReference{
						Kind:      sourcev1.HelmRepositoryKind,
						Name:      ClusterAutoscalerHelmRepositoryName,
						Namespace: wc.Namespace,
					},
				},
			},
			TargetNamespace: ClusterAutoscalerNamespace(wc),
			Install:         &helmv2.Install{CreateNamespace: true},
			Values:          values,
		},
	})
}
