package render

import (
	"fmt"

	helmv2 "github.com/fluxcd/helm-controller/api/v2"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
)

const (
	GrafanaMCPHelmReleaseName = "grafana-mcp"

	// GrafanaMCPNodePort exposes the cluster's mcp-grafana the same way
	// GrafanaNodePort exposes Grafana: cmd/apiserver's MCP server
	// (internal/api/grafana_mcp.go) connects to it on the same node IP as
	// WorkloadClusterStatus.GrafanaURL.
	GrafanaMCPNodePort = 30913

	// GrafanaMCPPath is mcp-grafana's streamable HTTP endpoint.
	GrafanaMCPPath = "/mcp"

	grafanaMCPPort = 8000
)

// GrafanaMCPHelmRelease installs the official Grafana MCP server next to
// the cluster's Grafana, read-only (--disable-write) and limited to the
// tool categories this project's Grafana has data for. It logs into
// Grafana as the admin and requires callers to present that same admin
// password as a bearer token. Both come from the grafana chart's own
// "grafana" Secret on the workload cluster, so the password never appears
// in this HelmRelease, and reaching the NodePort without it is refused.
func GrafanaMCPHelmRelease(wc *benchv1alpha1.WorkloadCluster) (*helmv2.HelmRelease, error) {
	fromGrafanaSecret := func(key string) map[string]any {
		return map[string]any{"secretKeyRef": map[string]any{"name": GrafanaServiceName, "key": key}}
	}
	values := map[string]any{
		"fullnameOverride": GrafanaMCPHelmReleaseName,
		"grafana":          map[string]any{"url": fmt.Sprintf("http://%s:%d", GrafanaServiceName, GrafanaPort)},
		"envValueFrom": map[string]any{
			"GRAFANA_USERNAME": fromGrafanaSecret("admin-user"),
			"GRAFANA_PASSWORD": fromGrafanaSecret("admin-password"),
		},
		"serverAuth": map[string]any{
			"tokenSecret": map[string]any{"name": GrafanaServiceName, "key": "admin-password"},
		},
		"disableWrite":   true,
		"serviceMonitor": map[string]any{"enabled": true},
		"metrics":        map[string]any{"enabled": true},
		"extraArgs": []string{
			"--transport=streamable-http",
			fmt.Sprintf("--address=0.0.0.0:%d", grafanaMCPPort),
			"--enabled-tools=search,datasource,prometheus,loki,alerting,dashboard,pyroscope,tempo,annotations",
			// Reached by node IP, which mcp-grafana's loopback-only default
			// rejects; the bearer token is the gate.
			"--allowed-hosts=*",
		},
		"service": map[string]any{
			"type": "NodePort", "port": grafanaMCPPort, "nodePort": GrafanaMCPNodePort,
		},
		"readinessProbe": map[string]any{
			"httpGet": map[string]any{"path": "/healthz", "port": "mcp-http"},
		},
	}
	monitoringPlacement(wc, values)
	hr, err := observabilityHelmRelease(wc, GrafanaMCPHelmReleaseName, "grafana-mcp", pinnedChart("grafana-mcp").Version, GrafanaCommunityHelmRepositoryName, values)
	if err != nil {
		return nil, err
	}
	// The "grafana" Secret its env reads comes from the grafana release.
	hr.Spec.DependsOn = append(hr.Spec.DependsOn, helmv2.DependencyReference{Name: AddonHelmReleaseName(wc, GrafanaHelmReleaseName)})
	return hr, nil
}
