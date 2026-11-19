package render

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
)

// Persistent storage for the observability stack, so a restart or a pod
// rescheduled onto another monitoring node (e.g. after the node it ran on
// went NotReady) keeps its data: metrics, alerts' silences, Grafana's
// annotations (which TestRunReconciler posts per run), traces, profiles and logs.
// Every one of them is installed from a Helm chart, which creates the
// volumes itself from these sizes (chartPersistence, volumeClaimTemplate).
const (
	AlertmanagerStorageSize = "1Gi"
	GrafanaStorageSize      = "2Gi"
	TempoStorageSize        = "10Gi"
	PyroscopeStorageSize    = "10Gi"
	LokiStorageSize         = "10Gi"

	// Defaults for an unset spec.prometheus: two months of history (the
	// chart's own default is 10d) at the ~1GB/day a busy cluster writes.
	defaultPrometheusStorageSize = "100Gi"
	defaultPrometheusRetention   = "60d"
)

// prometheusStorage returns wc's Prometheus volume size, retention, and the
// retentionSize that keeps the TSDB at 85% of the volume (WAL and
// compaction need headroom), so a long Burn run can't fill it.
func prometheusStorage(wc *benchv1alpha1.WorkloadCluster) (size, retention, retentionSize string) {
	size, retention = defaultPrometheusStorageSize, defaultPrometheusRetention
	if wc != nil && wc.Spec.Prometheus != nil {
		if wc.Spec.Prometheus.StorageSize != "" {
			size = wc.Spec.Prometheus.StorageSize
		}
		if wc.Spec.Prometheus.Retention != "" {
			retention = wc.Spec.Prometheus.Retention
		}
	}
	q, err := resource.ParseQuantity(size)
	if err != nil {
		size = defaultPrometheusStorageSize
		q = resource.MustParse(size)
	}
	// Prometheus' MB is MiB.
	return size, retention, fmt.Sprintf("%dMB", q.Value()*85/100/(1<<20))
}

// observabilityStorageClass is the StorageClass observability volumes use:
// csi-incus' on an Incus cluster (ensureCSIIncus installs it as the only
// one), otherwise the cluster's default.
func observabilityStorageClass(wc *benchv1alpha1.WorkloadCluster) *string {
	if wc == nil || wc.Spec.Incus == nil {
		return nil
	}
	return new(CSIIncusStorageClassName)
}

// volumeClaimTemplate is the kube-prometheus-stack chart's storageSpec
// shape for Prometheus/Alertmanager, whose StatefulSets create their own
// PVCs.
func volumeClaimTemplate(wc *benchv1alpha1.WorkloadCluster, size string) map[string]any {
	spec := map[string]any{
		"accessModes": []string{string(corev1.ReadWriteOnce)},
		"resources":   map[string]any{"requests": map[string]any{"storage": size}},
	}
	if class := observabilityStorageClass(wc); class != nil {
		spec["storageClassName"] = *class
	}
	return map[string]any{"volumeClaimTemplate": map[string]any{"spec": spec}}
}
