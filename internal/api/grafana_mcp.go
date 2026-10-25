package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"k8s.io/apimachinery/pkg/types"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
	"github.com/authentik-labs/akbench/internal/render"
)

// Each workload cluster runs the official mcp-grafana server
// (render.GrafanaMCPHelmRelease). The MCP server re-exposes it through two
// tools rather than mirroring its tools one by one, since which clusters
// exist is only known per call: list_grafana_tools and call_grafana_tool,
// both taking the cluster to talk to. mcp-grafana runs with --disable-write,
// and call_grafana_tool also refuses any tool not annotated read-only, so
// this keeps the MCP server read-only.

type mcpGrafanaToolsInput struct {
	Cluster string `json:"cluster" jsonschema:"WorkloadCluster name, see list_workload_clusters"`
}

type mcpGrafanaCallInput struct {
	Cluster   string         `json:"cluster" jsonschema:"WorkloadCluster name, see list_workload_clusters"`
	Tool      string         `json:"tool" jsonschema:"tool name, see list_grafana_tools"`
	Arguments map[string]any `json:"arguments,omitempty" jsonschema:"the tool's arguments, matching its inputSchema"`
}

// bearerTransport adds a bearer token to every request.
type bearerTransport struct{ token string }

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

// grafanaMCPSession connects to cluster's mcp-grafana, which listens on
// render.GrafanaMCPNodePort of the node Grafana's own URL points at and
// takes the Grafana admin password as its bearer token.
func (s *Server) grafanaMCPSession(ctx context.Context, cluster string) (*mcp.ClientSession, error) {
	if cluster == "" {
		return nil, fmt.Errorf("cluster is required")
	}
	var wc benchv1alpha1.WorkloadCluster
	if err := s.client.Get(ctx, types.NamespacedName{Name: cluster, Namespace: s.cfg.Namespace}, &wc); err != nil {
		return nil, err
	}
	if wc.Status.GrafanaURL == "" {
		return nil, fmt.Errorf("cluster %s has no Grafana yet", cluster)
	}
	grafanaURL, err := url.Parse(wc.Status.GrafanaURL)
	if err != nil {
		return nil, fmt.Errorf("invalid recorded Grafana URL: %w", err)
	}
	token, err := s.grafanaAdminPassword(ctx, &wc)
	if err != nil {
		return nil, err
	}
	endpoint := url.URL{
		Scheme: grafanaURL.Scheme,
		Host:   net.JoinHostPort(grafanaURL.Hostname(), strconv.Itoa(render.GrafanaMCPNodePort)),
		Path:   render.GrafanaMCPPath,
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "authentik-bench", Version: Version}, nil)
	return client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:             endpoint.String(),
		HTTPClient:           &http.Client{Transport: bearerTransport{token}},
		DisableStandaloneSSE: true,
		MaxRetries:           -1,
	}, nil)
}

func (s *Server) addGrafanaMCPTools(server *mcp.Server, tool func(name, title, description string) *mcp.Tool) {
	mcp.AddTool(server, tool("list_grafana_tools", "List Grafana tools",
		"List the tools of a workload cluster's Grafana MCP server (the official mcp-grafana, read-only), with their input schemas. It covers that cluster's Prometheus, Loki, Tempo and Pyroscope plus its dashboards, alert rules and annotations. Call them with call_grafana_tool."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in mcpGrafanaToolsInput) (*mcp.CallToolResult, any, error) {
			session, err := s.grafanaMCPSession(ctx, in.Cluster)
			if err != nil {
				return nil, nil, err
			}
			defer func() { _ = session.Close() }()
			tools, err := session.ListTools(ctx, nil)
			if err != nil {
				return nil, nil, err
			}
			body, err := json.Marshal(tools.Tools)
			if err != nil {
				return nil, nil, err
			}
			return mcpText(body), nil, nil
		})
	mcp.AddTool(server, tool("call_grafana_tool", "Call Grafana tool",
		"Call one of a workload cluster's Grafana MCP tools (see list_grafana_tools) and return its result. Datasource UIDs are prometheus, loki, tempo and pyroscope."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in mcpGrafanaCallInput) (*mcp.CallToolResult, any, error) {
			session, err := s.grafanaMCPSession(ctx, in.Cluster)
			if err != nil {
				return nil, nil, err
			}
			defer func() { _ = session.Close() }()
			tools, err := session.ListTools(ctx, nil)
			if err != nil {
				return nil, nil, err
			}
			readOnly := false
			for _, t := range tools.Tools {
				if t.Name == in.Tool {
					readOnly = t.Annotations != nil && t.Annotations.ReadOnlyHint
				}
			}
			if !readOnly {
				return nil, nil, fmt.Errorf("unknown or non-read-only Grafana tool %q", in.Tool)
			}
			res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: in.Tool, Arguments: in.Arguments})
			if err != nil {
				return nil, nil, err
			}
			for i, c := range res.Content {
				if text, ok := c.(*mcp.TextContent); ok {
					res.Content[i] = mcpText([]byte(text.Text)).Content[0]
				}
			}
			return res, nil, nil
		})
}
