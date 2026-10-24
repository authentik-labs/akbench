package render

import (
	"encoding/json"
	"fmt"
	"reflect"

	helmv2 "github.com/fluxcd/helm-controller/api/v2"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
)

// WorkerUpgradeStep is where a Dedicated-placement upgrade stands, see
// WorkerUpgrade.
type WorkerUpgradeStep int

const (
	// WorkerUpgradeNone: no upgrade in flight, apply desired as is.
	WorkerUpgradeNone WorkerUpgradeStep = iota
	// WorkerUpgradeDrain: re-apply the live release with only the worker
	// scaled to zero, holding desired back until the worker pods are gone.
	WorkerUpgradeDrain
	// WorkerUpgradeHold: apply desired with the worker scaled to zero.
	WorkerUpgradeHold
	// WorkerUpgradeRelease: the upgrade finished with the worker at zero;
	// desired scales it back up, so the release isn't Ready yet even if
	// a stale read of the live HelmRelease says so.
	WorkerUpgradeRelease
)

// WorkerUpgrade decides whether desired should go out with the worker scaled
// to zero. Only Dedicated placement needs this: one pod of the release
// per node leaves a rolling update's surge server pod nowhere to schedule, so
// the worker gives up its nodes until the new server pods are running, then
// scales back up.
//
// An upgrade is any change to what existing (the live HelmRelease) installs
// other than worker.replicas itself. It first drains the worker on the old
// release (workerPods counts the worker pods still around, terminating ones
// included): scaling it down in the same helm upgrade that rolls the server
// races the surge server pod, which then stays Pending. It then holds the
// worker at zero until
// helm-controller reports the upgraded release Ready (existingReady, which
// helm-controller only sets once the rollout finished). spec.worker.replicas
// is at least 1, so a live worker.replicas of zero is the marker of a hold in
// progress - no extra state needed. Nothing is held on first install
// (existing nil), where there's no old pod in the way.
func WorkerUpgrade(t *benchv1alpha1.AuthentikDeployment, existing, desired *helmv2.HelmRelease, existingReady bool, workerPods int) (WorkerUpgradeStep, error) {
	if t.Spec.Placement.Mode != benchv1alpha1.PlacementDedicated || existing == nil {
		return WorkerUpgradeNone, nil
	}
	existingValues, existingWorkers, err := splitWorkerReplicas(existing.Spec.Values)
	if err != nil {
		return WorkerUpgradeNone, fmt.Errorf("decode live HelmRelease values: %w", err)
	}
	desiredValues, desiredWorkers, err := splitWorkerReplicas(desired.Spec.Values)
	if err != nil {
		return WorkerUpgradeNone, fmt.Errorf("decode desired HelmRelease values: %w", err)
	}
	switch {
	case chartVersionChanged(t, existing, desired) || !reflect.DeepEqual(existingValues, desiredValues):
		if existingWorkers != 0 || workerPods > 0 {
			return WorkerUpgradeDrain, nil
		}
		return WorkerUpgradeHold, nil
	case existingWorkers != 0 || desiredWorkers == 0:
		return WorkerUpgradeNone, nil
	case !existingReady:
		return WorkerUpgradeHold, nil
	default:
		return WorkerUpgradeRelease, nil
	}
}

// chartVersionChanged ignores the "*" -> concrete version switch
// ChartVersion makes right after first install: that's the same chart.
func chartVersionChanged(t *benchv1alpha1.AuthentikDeployment, existing, desired *helmv2.HelmRelease) bool {
	if existing.Spec.Chart == nil || desired.Spec.Chart == nil {
		return existing.Spec.Chart != desired.Spec.Chart
	}
	from, to := existing.Spec.Chart.Spec.Version, desired.Spec.Chart.Spec.Version
	if from == "*" && to == t.Status.ChartVersion {
		return false
	}
	return from != to
}

// splitWorkerReplicas decodes values, returning them without worker.replicas
// alongside that replica count (-1 when unset).
func splitWorkerReplicas(values *apiextensionsv1.JSON) (map[string]any, float64, error) {
	m := map[string]any{}
	if values != nil && len(values.Raw) > 0 {
		if err := json.Unmarshal(values.Raw, &m); err != nil {
			return nil, 0, err
		}
	}
	replicas := float64(-1)
	if worker, ok := m["worker"].(map[string]any); ok {
		if n, ok := worker[valuesKeyReplicas].(float64); ok {
			replicas = n
		}
		delete(worker, valuesKeyReplicas)
	}
	return m, replicas, nil
}

// WorkerPodLabels selects t's authentik worker pods.
func WorkerPodLabels(t *benchv1alpha1.AuthentikDeployment) map[string]string {
	return map[string]string{labelAppInstance: HelmReleaseName(t), "app.kubernetes.io/component": "worker"}
}

// WithWorkerReplicas returns values with worker.replicas set to replicas.
func WithWorkerReplicas(values *apiextensionsv1.JSON, replicas int32) (*apiextensionsv1.JSON, error) {
	m := map[string]any{}
	if values != nil && len(values.Raw) > 0 {
		if err := json.Unmarshal(values.Raw, &m); err != nil {
			return nil, fmt.Errorf("decode helm values: %w", err)
		}
	}
	worker, _ := m["worker"].(map[string]any)
	if worker == nil {
		worker = map[string]any{}
		m["worker"] = worker
	}
	worker[valuesKeyReplicas] = replicas
	raw, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("marshal helm values: %w", err)
	}
	return &apiextensionsv1.JSON{Raw: raw}, nil
}
