package api

import (
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestGrafanaTool(t *testing.T) {
	readOnly := &mcp.ToolAnnotations{ReadOnlyHint: true}
	remote := &mcp.Tool{
		Name:        "query_loki_logs",
		Annotations: readOnly,
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"logql": map[string]any{"type": "string"}},
			"required":   []any{"logql"},
		},
	}
	tool, ok := grafanaTool(remote)
	if !ok || tool.Name != "grafana_query_loki_logs" || !tool.Annotations.ReadOnlyHint {
		t.Fatalf("tool = %+v, %v", tool, ok)
	}
	schema := tool.InputSchema.(map[string]any)
	if _, ok := schema["properties"].(map[string]any)["cluster"]; !ok {
		t.Errorf("no cluster property: %v", schema)
	}
	if req := schema["required"].([]any); len(req) != 2 || req[1] != "cluster" {
		t.Errorf("required = %v", req)
	}
	// The remote tool's own schema is left alone.
	if _, ok := remote.InputSchema.(map[string]any)["properties"].(map[string]any)["cluster"]; ok {
		t.Error("remote schema modified")
	}

	for name, skipped := range map[string]*mcp.Tool{
		"not read-only":    {Name: "update_dashboard", InputSchema: map[string]any{"type": "object"}},
		"non-object input": {Name: "x", Annotations: readOnly, InputSchema: map[string]any{"type": "string"}},
	} {
		if _, ok := grafanaTool(skipped); ok {
			t.Errorf("%s: re-exported", name)
		}
	}
}
