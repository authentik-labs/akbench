package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
)

// connectMCP serves s over HTTP and returns an MCP client session for it.
func connectMCP(t *testing.T, s *Server) *mcp.ClientSession {
	t.Helper()
	srv := httptest.NewServer(s.Router())
	t.Cleanup(srv.Close)
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "v0"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint:             srv.URL + "/api/v1/mcp",
		HTTPClient:           &http.Client{Transport: bearerTransport{testToken}},
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func callTool(t *testing.T, session *mcp.ClientSession, name string, args any) (string, bool) {
	t.Helper()
	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	if len(res.Content) != 1 {
		t.Fatalf("%s returned %d content items", name, len(res.Content))
	}
	return res.Content[0].(*mcp.TextContent).Text, res.IsError
}

func TestMCPRequiresToken(t *testing.T) {
	s, _ := newServer(t, Config{})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/mcp", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	requireStatus(t, rec, http.StatusUnauthorized)
}

// Every tool must be marked read-only, and none may reach a write route.
func TestMCPToolsAreReadOnly(t *testing.T) {
	s, _ := newServer(t, Config{})
	session := connectMCP(t, s)

	tools, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	if len(tools.Tools) == 0 {
		t.Fatal("no tools registered")
	}
	for _, tool := range tools.Tools {
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			t.Errorf("tool %s is not annotated read-only", tool.Name)
		}
	}

	// The read router answers nothing but GET.
	read := s.mcpReadRouter()
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		req := httptest.NewRequest(method, "/testcases/x", nil)
		rec := httptest.NewRecorder()
		read.ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s reached the read router with status %d", method, rec.Code)
		}
	}
}

func TestMCPListTestRunsFiltersAndTrims(t *testing.T) {
	run := func(name, testCase string) *benchv1alpha1.TestRun {
		r := &benchv1alpha1.TestRun{}
		r.Name, r.Namespace = name, testNamespace
		r.Labels = map[string]string{benchv1alpha1.LabelTestCase: testCase}
		r.Annotations = map[string]string{"kubectl.kubernetes.io/last-applied-configuration": "{}"}
		r.Spec.TestCaseRef.Name = testCase
		return r
	}
	s, _ := newServer(t, Config{}, run("login-1", "login"), run("saml-1", "saml"))
	session := connectMCP(t, s)

	text, isErr := callTool(t, session, "list_test_runs", map[string]any{"testCase": "login"})
	if isErr {
		t.Fatalf("list_test_runs failed: %s", text)
	}
	var body struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal([]byte(text), &body); err != nil {
		t.Fatalf("decode %q: %v", text, err)
	}
	if len(body.Items) != 1 || body.Items[0]["metadata"].(map[string]any)["name"] != "login-1" {
		t.Fatalf("got %s, want only login-1", text)
	}
	if strings.Contains(text, "last-applied-configuration") {
		t.Fatalf("result was not trimmed: %s", text)
	}

	if text, isErr := callTool(t, session, "list_test_runs", map[string]any{"testCase": `x",foo="bar`}); !isErr {
		t.Fatalf("an invalid testCase was accepted: %s", text)
	}
}

func TestMCPCompareTests(t *testing.T) {
	prom := fakePrometheus(t, func(query string) (string, string, bool) {
		if strings.Contains(query, "k6_http_reqs_total") && strings.Contains(query, `testid="login-a"`) {
			return "120.5", "a", true
		}
		return "", "", false
	})
	s, _ := newServer(t, Config{PrometheusURL: prom.URL})
	session := connectMCP(t, s)

	text, isErr := callTool(t, session, "compare_tests", map[string]any{"testids": []string{"login-a"}, "window": "10m"})
	if isErr {
		t.Fatalf("compare_tests failed: %s", text)
	}
	body := struct {
		Window  string          `json:"window"`
		Results []CompareResult `json:"results"`
	}{}
	if err := json.Unmarshal([]byte(text), &body); err != nil {
		t.Fatalf("decode %q: %v", text, err)
	}
	if body.Window != "10m" || len(body.Results) != 1 || body.Results[0].Values["http_reqs"] != 120.5 {
		t.Fatalf("unexpected compare result: %s", text)
	}

	// The REST endpoint's validation still applies.
	text, isErr = callTool(t, session, "compare_tests", map[string]any{"testids": []string{`a"}`}})
	if !isErr || !strings.Contains(text, "invalid testid") {
		t.Fatalf("an injected testid was accepted: %s", text)
	}
}

func TestMCPMetricSeriesDefaultsTimeRange(t *testing.T) {
	prom := fakePrometheusRange(t, func(string) [][2]any {
		return [][2]any{{1700000000, "1.5"}}
	})
	s, _ := newServer(t, Config{PrometheusURL: prom.URL})
	session := connectMCP(t, s)

	text, isErr := callTool(t, session, "get_metric_series", map[string]any{"testids": []string{"login-a"}, "metric": "http_reqs"})
	if isErr {
		t.Fatalf("get_metric_series failed: %s", text)
	}
	if !strings.Contains(text, `"value":1.5`) {
		t.Fatalf("unexpected series: %s", text)
	}
}

func TestMCPListMetrics(t *testing.T) {
	s, _ := newServer(t, Config{})
	session := connectMCP(t, s)
	text, isErr := callTool(t, session, "list_metrics", map[string]any{})
	if isErr {
		t.Fatalf("list_metrics failed: %s", text)
	}
	for _, key := range []string{`"http_req_duration_p95"`, `"cpu"`, `"memory"`} {
		if !strings.Contains(text, key) {
			t.Errorf("list_metrics is missing %s: %s", key, text)
		}
	}
}

func TestMCPTruncatesLargeResults(t *testing.T) {
	res := mcpText([]byte(strings.Repeat("x", mcpMaxResultBytes+10)))
	text := res.Content[0].(*mcp.TextContent).Text
	if !strings.HasSuffix(text, "increase step]") {
		t.Fatalf("large result was not truncated with a note")
	}
}
