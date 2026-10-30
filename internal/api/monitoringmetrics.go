package api

import (
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"
	"k8s.io/apimachinery/pkg/types"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
	"github.com/authentik-labs/akbench/internal/render"
)

const (
	byContainer = "container"
	// byHost is the node name node-exporter's series carry (render's hostRelabeling).
	byHost = "host"
)

// monitoringMetrics is keyed by the ?metric= query param: the cluster's own
// observability stack (Prometheus, Alertmanager, Grafana, Tempo, Pyroscope,
// ...) in render.ObservabilityNamespace. Each Query takes %[1]q that
// namespace and %[2]s the rate() window. Grouped by container rather than
// pod, so a component keeps one line across restarts. The node-* charts
// are node-exporter's, one line per node, and only use the window.
var monitoringMetrics = map[string]deploymentChart{
	metricCPU: {Label: "Monitoring CPU", Unit: unitCores, Queries: []deploymentQuery{{
		By:    byContainer,
		Query: `sum by (container) (rate(container_cpu_usage_seconds_total{namespace=%[1]q, container!="", container!="POD"}[%[2]s]))`,
	}}},
	metricMemory: {Label: "Monitoring memory", Unit: unitBytes, Queries: []deploymentQuery{{
		By:    byContainer,
		Query: `sum by (container) (container_memory_working_set_bytes{namespace=%[1]q, container!="", container!="POD"})`,
	}}},
	"disk": {Label: "Monitoring disk usage", Unit: unitBytes, Queries: []deploymentQuery{
		{
			Name:  "used",
			By:    byPVC,
			Query: `max by (persistentvolumeclaim) (kubelet_volume_stats_used_bytes{namespace=%[1]q})`,
		},
		{
			Name:  "capacity",
			By:    byPVC,
			Query: `max by (persistentvolumeclaim) (kubelet_volume_stats_capacity_bytes{namespace=%[1]q})`,
		},
	}},
	"node-cpu": {Label: "Node CPU", Unit: unitRatio, Queries: []deploymentQuery{{
		By:    byHost,
		Query: `1 - avg by (host) (rate(node_cpu_seconds_total{mode="idle"}[%[2]s]))`,
	}}},
	"node-memory": {Label: "Node memory", Unit: unitRatio, Queries: []deploymentQuery{{
		By:    byHost,
		Query: `1 - max by (host) (avg_over_time(node_memory_MemAvailable_bytes[%[2]s]) / node_memory_MemTotal_bytes)`,
	}}},
	"node-io": {Label: "Node disk IO", Unit: "bytes/s", Queries: []deploymentQuery{
		{
			Name:  "read",
			By:    byHost,
			Query: `sum by (host) (rate(node_disk_read_bytes_total{device!~"dm-.*|loop.*"}[%[2]s]))`,
		},
		{
			Name:  "write",
			By:    byHost,
			Query: `sum by (host) (rate(node_disk_written_bytes_total{device!~"dm-.*|loop.*"}[%[2]s]))`,
		},
	}},
	"node-steal": {Label: "Node CPU steal", Unit: unitRatio, Queries: []deploymentQuery{{
		By:    byHost,
		Query: `avg by (host) (rate(node_cpu_seconds_total{mode="steal"}[%[2]s]))`,
	}}},
}

// listMonitoringMetrics answers GET
// /clusters/{name}/monitoring-metrics?metric=cpu|memory|disk|node-cpu|node-memory|node-io|node-steal&start=&end=[&window=][&step=]:
// the WorkloadCluster's observability stack over time, from its own
// Prometheus. Same response shape as listPostgresMetrics.
func (s *Server) listMonitoringMetrics(w http.ResponseWriter, r *http.Request) {
	cr, ok := parseChartRange(w, r, monitoringMetrics)
	if !ok {
		return
	}
	var wc benchv1alpha1.WorkloadCluster
	if err := s.client.Get(r.Context(), types.NamespacedName{Name: chi.URLParam(r, "name"), Namespace: s.cfg.Namespace}, &wc); err != nil {
		writeAPIError(w, r, err)
		return
	}
	if wc.Status.PrometheusURL == "" {
		writeError(w, r, http.StatusConflict, "cluster has no Prometheus yet")
		return
	}
	s.writeChart(w, r, cr, wc.Status.PrometheusURL, func(query, window string) (string, error) {
		return fmt.Sprintf(query, render.ObservabilityNamespace, window), nil
	})
}
