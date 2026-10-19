package render

import (
	"fmt"

	helmv2 "github.com/fluxcd/helm-controller/api/v2"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
)

// Unlike traces and profiles, authentik doesn't push its logs anywhere: it
// writes them to stdout. So logs take two charts per workload cluster -
// Loki to store and query them, and an Alloy DaemonSet tailing every pod's
// container logs on its node and pushing them to Loki.
const (
	// LokiServiceName/LokiPort are Loki's HTTP API: what Alloy pushes to and
	// Grafana's Loki datasource (and, through it, cmd/apiserver's log
	// breakdown) queries. The chart's fullnameOverride keeps its Service
	// named this.
	LokiServiceName = "loki"
	LokiPort        = 3100

	// lokiStore keeps chunks, the index and delete requests on Loki's own
	// persistent volume.
	lokiStore = "filesystem"

	// lokiRetention bounds how long logs are kept, on top of
	// LokiStorageSize - enough to compare a week of benchmark runs.
	lokiRetention = "168h"

	// alloyFullname names the log collector's DaemonSet.
	alloyFullname = "alloy-logs"
)

// Labels every log stream Alloy ships carries - what the log breakdown
// selects a target's pods by.
const (
	LokiLabelNamespace = "namespace"
	LokiLabelPod       = "pod"
	LokiLabelContainer = "container"
)

// LokiHelmRelease installs Loki from its upstream chart in monolithic
// (single binary) mode with filesystem storage on a persistent volume: this
// project's no-HA, benchmark-scale posture, same as Tempo and Pyroscope.
// Multi-tenancy, the nginx gateway, memcached caches, the canary and the
// chart's self-test stay off, and logs older than lokiRetention are
// compacted away.
func LokiHelmRelease(wc *benchv1alpha1.WorkloadCluster) (*helmv2.HelmRelease, error) {
	persistence := map[string]any{"enabled": true, "size": LokiStorageSize}
	if class := observabilityStorageClass(wc); class != nil {
		// Loki's chart calls it storageClass, not storageClassName.
		persistence["storageClass"] = *class
	}
	singleBinary := map[string]any{valuesKeyReplicas: 1, "persistence": persistence}
	monitoringPlacement(wc, singleBinary)
	off := map[string]any{"enabled": false}
	return observabilityHelmRelease(wc, LokiHelmReleaseName, "loki", pinnedChart("loki").Version, GrafanaCommunityHelmRepositoryName, map[string]any{
		"fullnameOverride": LokiServiceName,
		"deploymentMode":   "Monolithic",
		"loki": map[string]any{
			"auth_enabled":  false,
			"analytics":     map[string]any{"reporting_enabled": false},
			"commonConfig":  map[string]any{"replication_factor": 1},
			"storage":       map[string]any{"type": lokiStore},
			"limits_config": map[string]any{"retention_period": lokiRetention},
			"compactor":     map[string]any{"retention_enabled": true, "delete_request_store": lokiStore},
			"schemaConfig": map[string]any{"configs": []map[string]any{{
				"from": "2024-04-01", "store": "tsdb", "object_store": lokiStore, "schema": "v13",
				"index": map[string]any{"prefix": "index_", "period": "24h"},
			}}},
		},
		"singleBinary": singleBinary,
		// Monolithic mode runs none of the scalable targets.
		"read":         map[string]any{valuesKeyReplicas: 0},
		"write":        map[string]any{valuesKeyReplicas: 0},
		"backend":      map[string]any{valuesKeyReplicas: 0},
		"gateway":      off,
		"chunksCache":  off,
		"resultsCache": off,
		"lokiCanary":   off,
		"test":         off,
		"monitoring":   map[string]any{"serviceMonitor": map[string]any{"enabled": true}},
	})
}

// alloyLogsConfig tails the container log files of every pod on Alloy's
// own node (/var/log/pods, mounted by the chart's mounts.varlog) and pushes
// them to Loki, labelled with their namespace, pod and container. Selecting
// only this node's pods (K8S_NODE_NAME is set by the chart) keeps each line
// shipped exactly once.
func alloyLogsConfig() string {
	return fmt.Sprintf(`discovery.kubernetes "pods" {
  role = "pod"
  selectors {
    role  = "pod"
    field = "spec.nodeName=" + sys.env("K8S_NODE_NAME")
  }
}

discovery.relabel "pods" {
  targets = discovery.kubernetes.pods.targets
  rule {
    source_labels = ["__meta_kubernetes_namespace"]
    target_label  = %[1]q
  }
  rule {
    source_labels = ["__meta_kubernetes_pod_name"]
    target_label  = %[2]q
  }
  rule {
    source_labels = ["__meta_kubernetes_pod_container_name"]
    target_label  = %[3]q
  }
  rule {
    source_labels = ["__meta_kubernetes_pod_uid", "__meta_kubernetes_pod_container_name"]
    separator     = "/"
    replacement   = "/var/log/pods/*$1/*.log"
    target_label  = "__path__"
  }
}

local.file_match "pods" {
  path_targets = discovery.relabel.pods.output
}

loki.source.file "pods" {
  targets    = local.file_match.pods.targets
  forward_to = [loki.process.pods.receiver]
}

loki.process "pods" {
  stage.cri {}
  forward_to = [loki.write.default.receiver]
}

loki.write "default" {
  endpoint {
    url = "http://%[4]s.%[5]s.svc.cluster.local:%[6]d/loki/api/v1/push"
  }
}
`, LokiLabelNamespace, LokiLabelPod, LokiLabelContainer, LokiServiceName, ObservabilityNamespace, LokiPort)
}

// AlloyHelmRelease installs Grafana Alloy as the log collector: a
// DaemonSet tolerating every taint, since authentik's own pools may be
// tainted and their logs are the point. Usage reporting is off.
func AlloyHelmRelease(wc *benchv1alpha1.WorkloadCluster) (*helmv2.HelmRelease, error) {
	return observabilityHelmRelease(wc, AlloyHelmReleaseName, "alloy", pinnedChart("alloy").Version, GrafanaHelmRepositoryName, map[string]any{
		"fullnameOverride": alloyFullname,
		"alloy": map[string]any{
			"configMap":       map[string]any{"content": alloyLogsConfig()},
			"enableReporting": false,
			"mounts":          map[string]any{"varlog": true},
		},
		"controller": map[string]any{
			"type":        "daemonset",
			"tolerations": []map[string]any{{"operator": "Exists"}},
		},
		"serviceMonitor": map[string]any{"enabled": true},
	})
}
