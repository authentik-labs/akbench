package render

import (
	helmv2 "github.com/fluxcd/helm-controller/api/v2"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
)

// PyroscopeServiceName/PyroscopePort must match defaultPyroscopeHost
// (internal/controller/authentikdeployment_controller.go:
// "http://pyroscope.bench-system.svc.cluster.local:4040") exactly - that's
// the cluster-relative address every authentik pod pushes continuous
// profiles to regardless of which cluster it runs on, so each workload
// cluster's own Pyroscope must answer at this exact name. The chart's
// fullnameOverride keeps its Service named this.
const (
	PyroscopeServiceName = "pyroscope"
	PyroscopePort        = 4040
)

// PyroscopeHelmRelease installs Grafana Pyroscope from its upstream chart
// in its default single-binary (monolithic) shape, with local storage on a
// persistent volume. The chart's bundled Alloy agent and MinIO object store
// stay off: authentik pushes profiles itself, and a single binary on local
// disk is this project's no-HA, benchmark-scale posture.
func PyroscopeHelmRelease(wc *benchv1alpha1.WorkloadCluster) (*helmv2.HelmRelease, error) {
	pyroscope := map[string]any{"persistence": chartPersistence(wc, PyroscopeStorageSize)}
	monitoringPlacement(wc, pyroscope)
	return observabilityHelmRelease(wc, PyroscopeHelmReleaseName, "pyroscope", pinnedChart("pyroscope").Version, GrafanaHelmRepositoryName, map[string]any{
		"fullnameOverride": PyroscopeServiceName,
		"alloy":            map[string]any{"enabled": false},
		"minio":            map[string]any{"enabled": false},
		"pyroscope":        pyroscope,
		"serviceMonitor":   map[string]any{"enabled": true},
	})
}
