package api

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
	"github.com/authentik-labs/akbench/internal/render"
)

// Each workload cluster runs the official mcp-grafana server
// (render.GrafanaMCPHelmRelease). The MCP server re-exports every one of
// its read-only tools as "grafana_<name>", with an extra "cluster" argument
// naming the workload cluster to call it on. mcp-grafana's tool set is the
// same on every cluster, so it is discovered once, from the first cluster
// that answers, and added to the server then: until a cluster with Grafana
// exists, there are none. mcp-grafana runs with write tools disabled, and a
// tool not annotated read-only is never re-exported, so this keeps the MCP
// server read-only.

const (
	grafanaToolPrefix  = "grafana_"
	grafanaClusterArg  = "cluster"
	grafanaRediscovery = time.Minute
)

// bearerTransport adds a bearer token to every request.
type bearerTransport struct{ token string }

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

// grafanaMCPSession connects to wc's mcp-grafana, which listens on
// render.GrafanaMCPNodePort of the node Grafana's own URL points at and
// takes the Grafana admin password as its bearer token.
func (s *Server) grafanaMCPSession(ctx context.Context, wc *benchv1alpha1.WorkloadCluster) (*mcp.ClientSession, error) {
	if wc.Status.GrafanaURL == "" {
		return nil, fmt.Errorf("cluster %s has no Grafana yet", wc.Name)
	}
	grafanaURL, err := url.Parse(wc.Status.GrafanaURL)
	if err != nil {
		return nil, fmt.Errorf("invalid recorded Grafana URL: %w", err)
	}
	token, err := s.grafanaAdminPassword(ctx, wc)
	if err != nil {
		return nil, err
	}
	endpoint := url.URL{
		Scheme: grafanaURL.Scheme,
		Host:   net.JoinHostPort(grafanaURL.Hostname(), strconv.Itoa(render.GrafanaMCPNodePort)),
		Path:   render.GrafanaMCPPath,
	}
	mcpClient := mcp.NewClient(&mcp.Implementation{Name: "authentik-bench", Version: Version}, nil)
	return mcpClient.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:             endpoint.String(),
		HTTPClient:           &http.Client{Transport: bearerTransport{token}},
		DisableStandaloneSSE: true,
		MaxRetries:           -1,
	}, nil)
}

// ensureGrafanaTools adds mcp-grafana's tools to server unless that already
// happened, trying at most once per grafanaRediscovery so a cluster that
// doesn't answer can't slow down every MCP request.
func (s *Server) ensureGrafanaTools(ctx context.Context, server *mcp.Server) {
	s.grafanaToolsMu.Lock()
	defer s.grafanaToolsMu.Unlock()
	if s.grafanaToolsAdded || time.Since(s.grafanaToolsTried) < grafanaRediscovery {
		return
	}
	s.grafanaToolsTried = time.Now()

	var clusters benchv1alpha1.WorkloadClusterList
	if err := s.client.List(ctx, &clusters, client.InNamespace(s.cfg.Namespace)); err != nil {
		logf.FromContext(ctx).Error(err, "Failed to list WorkloadClusters for Grafana MCP tools")
		return
	}
	for i := range clusters.Items {
		wc := &clusters.Items[i]
		if wc.Status.GrafanaURL == "" {
			continue
		}
		tools, err := s.listGrafanaTools(ctx, wc)
		if err != nil {
			logf.FromContext(ctx).Info("Failed to list Grafana MCP tools", "cluster", wc.Name, "error", err.Error())
			continue
		}
		for _, remote := range tools {
			if tool, ok := grafanaTool(remote); ok {
				server.AddTool(tool, s.callGrafanaTool(remote.Name))
			}
		}
		s.grafanaToolsAdded = true
		return
	}
}

func (s *Server) listGrafanaTools(ctx context.Context, wc *benchv1alpha1.WorkloadCluster) ([]*mcp.Tool, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	session, err := s.grafanaMCPSession(ctx, wc)
	if err != nil {
		return nil, err
	}
	defer func() { _ = session.Close() }()
	res, err := session.ListTools(ctx, nil)
	if err != nil {
		return nil, err
	}
	return res.Tools, nil
}

// grafanaTool turns one of mcp-grafana's tools into the one re-exported
// here: prefixed, with a required cluster argument added to its schema.
// Tools that aren't read-only, or whose schema isn't an object, are skipped
// (mcp.Server.AddTool would panic on the latter).
func grafanaTool(remote *mcp.Tool) (*mcp.Tool, bool) {
	if remote.Annotations == nil || !remote.Annotations.ReadOnlyHint {
		return nil, false
	}
	var schema map[string]any
	if raw, err := json.Marshal(remote.InputSchema); err != nil || json.Unmarshal(raw, &schema) != nil || schema["type"] != "object" {
		return nil, false
	}
	props, _ := schema["properties"].(map[string]any)
	props = maps.Clone(props)
	if props == nil {
		props = map[string]any{}
	}
	props[grafanaClusterArg] = map[string]any{
		"type":        "string",
		"description": "WorkloadCluster whose Grafana to use, see list_workload_clusters",
	}
	schema["properties"] = props
	required, _ := schema["required"].([]any)
	schema["required"] = append(slices.Clone(required), grafanaClusterArg)

	tool := *remote
	tool.Name = grafanaToolPrefix + remote.Name
	tool.InputSchema = schema
	tool.OutputSchema = nil
	tool.Annotations = &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true}
	return &tool, true
}

// callGrafanaTool calls name on the mcp-grafana of the cluster the call's
// cluster argument names, passing every other argument through.
func (s *Server) callGrafanaTool(name string) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := map[string]any{}
		if len(req.Params.Arguments) > 0 {
			if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
				return nil, err
			}
		}
		cluster, _ := args[grafanaClusterArg].(string)
		delete(args, grafanaClusterArg)
		var wc benchv1alpha1.WorkloadCluster
		if err := s.client.Get(ctx, types.NamespacedName{Name: cluster, Namespace: s.cfg.Namespace}, &wc); err != nil {
			return toolError(err), nil
		}
		session, err := s.grafanaMCPSession(ctx, &wc)
		if err != nil {
			return toolError(err), nil
		}
		defer func() { _ = session.Close() }()
		res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			return toolError(err), nil
		}
		for i, c := range res.Content {
			if text, ok := c.(*mcp.TextContent); ok {
				res.Content[i] = mcpText([]byte(text.Text)).Content[0]
			}
		}
		res.StructuredContent = nil
		return res, nil
	}
}

// toolError reports err as a failed tool call, which the LLM sees, rather
// than a protocol error. (mcp.AddTool's typed handlers do this themselves.)
func toolError(err error) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}}}
}
