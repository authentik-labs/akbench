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

// descheduler (kubernetes-sigs.github.io/descheduler) is installed via its
// own official Helm chart, the same Flux HelmRepository + HelmRelease
// mechanism every other cluster-wide addon in this file uses. It
// periodically re-evicts pods that have drifted into a suboptimal placement
// (piled up on the same node, violating anti-affinity/node-taints/topology
// spread) so the default scheduler - which only ever places *new* pods, it
// never rebalances ones already running - gets another chance to place them
// better. The standing case this addon exists for: a worker pool's node
// count or role changes (this project's own 3-pool default, worker-pool
// resize, cluster-autoscaler scaling a pool down and back up) can easily
// leave pods crowded onto whichever nodes happened to be around at the
// time, with nothing to ever spread them back out again on its own.
//
// The chart's own default deschedulerPolicy (profiles: default -
// RemoveDuplicates/RemovePodsViolatingNodeAffinity/
// RemovePodsViolatingNodeTaints/RemovePodsViolatingInterPodAntiAffinity/
// RemovePodsViolatingTopologySpreadConstraint/LowNodeUtilization, with
// DefaultEvictor's own podProtections already extraEnabling "PodsWithPVC" -
// i.e. CNPG's own data pods stay protected from eviction regardless)
// already matches what this project wants with zero overrides - confirmed
// directly against the chart's own values.yaml rather than guessed, this
// project's own established discipline elsewhere. The only overrides here
// are the CronJob schedule (loosened from the chart's own aggressive
// 2-minute default - rebalancing a live k6 Burn/authentik pod mid-benchmark-
// run would skew the very numbers this project exists to produce, so
// "occasionally" means every 15 minutes here, not every 2) and this
// cluster's own monitoring-pool nodeSelector/tolerations.
const (
	DeschedulerHelmRepositoryName = "descheduler"
	deschedulerChartName          = "descheduler"

	DeschedulerHelmReleaseName = "descheduler"

	// DeschedulerNamespace matches this chart's own default namespace
	// convention for a cluster-wide, scheduling-adjacent addon - the same
	// kube-system every other such addon in this file (metrics-server,
	// csi-incus) already installs into.
	DeschedulerNamespace = "kube-system"

	// deschedulerSchedule runs well below the chart's own 2-minute default -
	// see this file's own doc comment for why.
	deschedulerSchedule = "*/15 * * * *"
)

// deschedulerChart is the version and repository deps/Chart.yaml pins.
var deschedulerChart = pinnedChart(deschedulerChartName)

// DeschedulerHelmRepository is the (singleton, per-namespace) HelmRepository
// pointing at the official descheduler chart repo.
func DeschedulerHelmRepository(namespace string) *sourcev1.HelmRepository {
	return &sourcev1.HelmRepository{
		Name:      DeschedulerHelmRepositoryName,
		Namespace: namespace,
		Spec: sourcev1.HelmRepositorySpec{
			URL:      deschedulerChart.Repository,
			Interval: metav1.Duration{Duration: helmRepoInterval},
		},
	}
}

// DeschedulerHelmValues builds spec.values for the descheduler chart
// install: just the schedule override and this cluster's own monitoring-pool
// nodeSelector/tolerations - see this file's own doc comment, the chart's
// own default policy is otherwise used entirely unchanged.
func DeschedulerHelmValues(wc *benchv1alpha1.WorkloadCluster) (*apiextensionsv1.JSON, error) {
	values := map[string]any{
		"schedule": deschedulerSchedule,
	}
	if sel, tol := MonitoringPoolPlacement(wc); len(sel) > 0 || len(tol) > 0 {
		values["nodeSelector"] = sel
		values["tolerations"] = tol
	}
	raw, err := json.Marshal(values)
	if err != nil {
		return nil, fmt.Errorf("marshal descheduler helm values: %w", err)
	}
	return &apiextensionsv1.JSON{Raw: raw}, nil
}

// DeschedulerHelmRelease builds the Flux HelmRelease that installs
// descheduler onto a workload cluster remotely, through wc's "<cluster>-kubeconfig" Secret CAPI writes (see
// KubeconfigSecretName). Its object name is per-cluster (AddonHelmReleaseName).
func DeschedulerHelmRelease(wc *benchv1alpha1.WorkloadCluster, values *apiextensionsv1.JSON) *helmv2.HelmRelease {
	return retryOnFailure(&helmv2.HelmRelease{
		Name:      AddonHelmReleaseName(wc, DeschedulerHelmReleaseName),
		Namespace: wc.Namespace,
		Spec: helmv2.HelmReleaseSpec{
			Interval:    metav1.Duration{Duration: helmRepoInterval},
			Timeout:     &metav1.Duration{Duration: helmInstallTimeout},
			ReleaseName: DeschedulerHelmReleaseName,
			Chart: &helmv2.HelmChartTemplate{
				Spec: helmv2.HelmChartTemplateSpec{
					Chart:   deschedulerChartName,
					Version: deschedulerChart.Version,
					SourceRef: helmv2.CrossNamespaceObjectReference{
						Kind:      sourcev1.HelmRepositoryKind,
						Name:      DeschedulerHelmRepositoryName,
						Namespace: wc.Namespace,
					},
				},
			},
			TargetNamespace: DeschedulerNamespace,
			Values:          values,
			KubeConfig: &fluxmeta.KubeConfigReference{
				SecretRef: &fluxmeta.SecretKeyReference{Name: KubeconfigSecretName(wc)},
			},
		},
	})
}
