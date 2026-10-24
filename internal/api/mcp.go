package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
)

// The MCP server (Model Context Protocol, streamable HTTP transport) at
// /api/v1/mcp lets an LLM read the same performance analytics the web UI
// shows: test cases and runs, the compare table, metric/resource time
// series, trace, profile and log breakdowns, and alerts.
//
// It is read-only by construction: every tool answers by dispatching a GET
// to mcpReadRouter, which only registers read handlers, so no tool can
// reach a create/update/delete, the SQL console, a reset, a test run or the
// shell. Each tool reuses its REST endpoint's validation (testid/window
// checks that keep PromQL injection out) rather than duplicating it.
//
// Authentication is the same bearer token (static or OIDC) as the rest of
// /api/v1, so an MCP client is configured with the URL plus an
// "Authorization: Bearer ..." header.

const mcpInstructions = `Read-only access to authentik-bench load-test analytics.

Concepts: an AuthentikDeployment is one authentik install under test (its "target" in metrics). A TestCase is a k6 load test against one or more deployments; its object name is the k6 "testid" every metric is labeled with. Quick TestCases execute as individual TestRuns (one per target); Burn TestCases run continuously, so their only history is in Prometheus.

Typical flow: list_test_cases (or list_test_runs) to find testids and when they ran, then compare_tests for a summary table, get_metric_series / get_resource_usage for time series over a run's window (use the TestRun's status start/completion times as start/end), get_trace_breakdown / get_profile_breakdown to explain where time goes, and get_log_breakdown for what authentik logs warnings and errors about. Durations are in seconds, ratios are 0-1.`

// paramTestIDs/paramStart/paramEnd are the query parameters the metrics
// endpoints share; paramTestID is the breakdown endpoints' single testid.
const (
	paramTestID  = "testid"
	paramTestIDs = "testids"
	paramStart   = "start"
	paramEnd     = "end"
)

// mcpDefaultRange is the trailing range a time-series tool covers when the
// caller gives no start.
const mcpDefaultRange = time.Hour

// mcpMaxResultBytes caps a tool's text result: an LLM's context is the
// scarce resource here, and an unbounded PromQL range query can be huge.
const mcpMaxResultBytes = 256 << 10

func (s *Server) mcpHandler() http.Handler {
	read := s.mcpReadRouter()
	server := s.newMCPServer(read)
	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server },
		// Stateless: every tool call is independent, so no session needs to
		// survive between requests (or pin a client to one replica).
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
}

// mcpReadRouter holds the only routes MCP tools can reach. It sits behind
// /api/v1/mcp's own requireToken, so it is unauthenticated itself.
func (s *Server) mcpReadRouter() http.Handler {
	r := chi.NewRouter()
	for _, path := range []string{"deployments", "testcases", "testmatrices", "testruns", "workloadclusters"} {
		kind := resourceKinds[path]
		r.Get("/"+path, s.listResource(kind))
		r.Get("/"+path+"/{name}", s.getResource(kind))
		r.Get("/"+path+"/{name}/events", s.listObjectEvents(kind))
	}
	r.Get("/metrics/query", s.proxyQuery)
	r.Get("/metrics/compare", s.compare)
	r.Get("/metrics/range", s.metricsRange)
	r.Get("/metrics/resources", s.listResourceMetrics)
	r.Get("/traces/breakdown", s.traceBreakdown)
	r.Get("/profiles/breakdown", s.profileBreakdown)
	r.Get("/logs/breakdown", s.logBreakdown)
	r.Get("/alerts", s.listAlerts)
	return r
}

// mcpGet runs one GET against read and returns its JSON body, turning a
// non-2xx answer into an error the LLM sees as a failed tool call.
func mcpGet(ctx context.Context, read http.Handler, path string, params url.Values) (json.RawMessage, error) {
	target := path
	if len(params) > 0 {
		target += "?" + params.Encode()
	}
	// Drop the outer /api/v1/mcp request's chi routing state, or read would
	// route as a subrouter mounted at that path instead of from the root.
	ctx = context.WithValue(ctx, chi.RouteCtxKey, (*chi.Context)(nil))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	rec := &bufferResponse{header: http.Header{}, status: http.StatusOK}
	read.ServeHTTP(rec, req)
	if rec.status < 200 || rec.status > 299 {
		var body errorBody
		if json.Unmarshal(rec.body.Bytes(), &body) == nil && body.Error != "" {
			return nil, fmt.Errorf("%s (HTTP %d)", body.Error, rec.status)
		}
		return nil, fmt.Errorf("HTTP %d: %s", rec.status, strings.TrimSpace(rec.body.String()))
	}
	return rec.body.Bytes(), nil
}

// bufferResponse is an in-memory http.ResponseWriter for mcpGet.
type bufferResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (b *bufferResponse) Header() http.Header         { return b.header }
func (b *bufferResponse) WriteHeader(status int)      { b.status = status }
func (b *bufferResponse) Write(p []byte) (int, error) { return b.body.Write(p) }

// mcpText wraps a JSON body as a tool result, truncating it past
// mcpMaxResultBytes with a note saying so.
func mcpText(body []byte) *mcp.CallToolResult {
	text := string(body)
	if len(text) > mcpMaxResultBytes {
		text = text[:mcpMaxResultBytes] +
			"\n... [truncated: result exceeded the size limit, narrow the time range or increase step]"
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}
}

// trimObjects drops the bulk of a Kubernetes object (or list) that carries
// nothing an analysis needs - managedFields and last-applied annotations -
// so it doesn't crowd out the parts that do.
func trimObjects(raw json.RawMessage) ([]byte, error) {
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	trim := func(obj map[string]any) {
		meta, _ := obj["metadata"].(map[string]any)
		if meta == nil {
			return
		}
		delete(meta, "managedFields")
		if ann, _ := meta["annotations"].(map[string]any); ann != nil {
			delete(ann, "kubectl.kubernetes.io/last-applied-configuration")
			if len(ann) == 0 {
				delete(meta, "annotations")
			}
		}
	}
	if items, ok := doc["items"].([]any); ok {
		for _, item := range items {
			if obj, ok := item.(map[string]any); ok {
				trim(obj)
			}
		}
	} else {
		trim(doc)
	}
	return json.Marshal(doc)
}

// mcpTimeRange resolves a tool's optional unix start/end into the strings
// the range endpoints require, defaulting to the trailing mcpDefaultRange.
func mcpTimeRange(start, end int64) (string, string) {
	if end == 0 {
		end = time.Now().Unix()
	}
	if start == 0 {
		start = end - int64(mcpDefaultRange/time.Second)
	}
	return strconv.FormatInt(start, 10), strconv.FormatInt(end, 10)
}

type mcpNameInput struct {
	Name string `json:"name" jsonschema:"the object's name"`
}

type mcpListTestRunsInput struct {
	TestCase string `json:"testCase,omitempty" jsonschema:"only runs of this TestCase (its name / testid)"`
}

type mcpCompareInput struct {
	TestIDs []string `json:"testids" jsonschema:"TestCase names (k6 testids) to compare"`
	Window  string   `json:"window,omitempty" jsonschema:"trailing window the rates and quantiles are computed over, a Prometheus duration like 5m (default) or 30m"`
	Time    int64    `json:"time,omitempty" jsonschema:"unix timestamp (seconds) the window ends at; defaults to now. Set it to a TestRun's completion time to summarize a finished run"`
}

type mcpSeriesInput struct {
	TestIDs []string `json:"testids" jsonschema:"TestCase names (k6 testids)"`
	Metric  string   `json:"metric" jsonschema:"metric key, see list_metrics"`
	Start   int64    `json:"start,omitempty" jsonschema:"unix timestamp (seconds); defaults to one hour before end"`
	End     int64    `json:"end,omitempty" jsonschema:"unix timestamp (seconds); defaults to now"`
	Window  string   `json:"window,omitempty" jsonschema:"rate() window per point, a Prometheus duration (default 1m)"`
	Step    string   `json:"step,omitempty" jsonschema:"resolution, a Prometheus duration (default 15s); raise it for long ranges"`
}

type mcpResourceInput struct {
	TestIDs   []string `json:"testids" jsonschema:"TestCase names (k6 testids); usage is summed over each target deployment's namespace"`
	Resource  string   `json:"resource" jsonschema:"cpu (cores) or memory (bytes)"`
	Component string   `json:"component,omitempty" jsonschema:"server or worker to narrow to those authentik pods; omit for the whole namespace incl. Postgres"`
	Start     int64    `json:"start,omitempty" jsonschema:"unix timestamp (seconds); defaults to one hour before end"`
	End       int64    `json:"end,omitempty" jsonschema:"unix timestamp (seconds); defaults to now"`
	Window    string   `json:"window,omitempty" jsonschema:"rate() window per point, a Prometheus duration"`
	Step      string   `json:"step,omitempty" jsonschema:"resolution, a Prometheus duration (default 15s)"`
}

type mcpBreakdownInput struct {
	TestID    string `json:"testid" jsonschema:"TestCase name (k6 testid)"`
	Window    string `json:"window,omitempty" jsonschema:"trailing window: 15m, 1h (default) or 6h"`
	Component string `json:"component,omitempty" jsonschema:"profiles only: server (default) or worker"`
}

type mcpPromQLInput struct {
	Query string `json:"query" jsonschema:"PromQL expression. k6 metrics are labeled testid and target, e.g. k6_http_reqs_total, k6_http_req_duration_seconds (native histogram)"`
	Time  int64  `json:"time,omitempty" jsonschema:"instant query at this unix timestamp; ignored when start and end are set"`
	Start int64  `json:"start,omitempty" jsonschema:"range query start, unix timestamp (seconds)"`
	End   int64  `json:"end,omitempty" jsonschema:"range query end, unix timestamp (seconds)"`
	Step  string `json:"step,omitempty" jsonschema:"range query resolution (default 15s)"`
}

// mcpMetricInfo describes one series key list_metrics reports.
type mcpMetricInfo struct {
	Key    string `json:"key"`
	Label  string `json:"label"`
	Unit   string `json:"unit"`
	Better string `json:"better,omitempty"`
	Tool   string `json:"tool"`
}

func (s *Server) newMCPServer(read http.Handler) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "authentik-bench", Version: Version},
		&mcp.ServerOptions{Instructions: mcpInstructions})

	readOnly := &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true}
	listObjects := func(path string) mcp.ToolHandlerFor[struct{}, any] {
		return func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
			return mcpObjects(ctx, read, "/"+path, nil)
		}
	}
	getObject := func(path string) mcp.ToolHandlerFor[mcpNameInput, any] {
		return func(ctx context.Context, _ *mcp.CallToolRequest, in mcpNameInput) (*mcp.CallToolResult, any, error) {
			if in.Name == "" {
				return nil, nil, fmt.Errorf("name is required")
			}
			return mcpObjects(ctx, read, "/"+path+"/"+url.PathEscape(in.Name), nil)
		}
	}
	tool := func(name, title, description string) *mcp.Tool {
		return &mcp.Tool{Name: name, Title: title, Description: description, Annotations: readOnly}
	}

	mcp.AddTool(server, tool("list_deployments", "List deployments",
		"List AuthentikDeployments (the authentik installs under test) with their spec - replicas, resources, placement, Postgres mode, version - and status."),
		listObjects("deployments"))
	mcp.AddTool(server, tool("get_deployment", "Get deployment", "Get one AuthentikDeployment by name."),
		getObject("deployments"))
	mcp.AddTool(server, tool("list_test_cases", "List test cases",
		"List TestCases: the k6 load tests, their template, profile (Quick/Burn), load shape (vus, duration) and target deployments. A TestCase's name is its metrics testid."),
		listObjects("testcases"))
	mcp.AddTool(server, tool("get_test_case", "Get test case", "Get one TestCase by name."),
		getObject("testcases"))
	mcp.AddTool(server, tool("list_test_matrices", "List test matrices",
		"List TestMatrices: authentik builds (variants) crossed with test case templates. Each template runs as its own TestCase driving one deployment per variant; status.rows names them."),
		listObjects("testmatrices"))
	mcp.AddTool(server, tool("list_test_runs", "List test runs",
		"List TestRuns (executions of Quick TestCases, one per target deployment) with their status: phase, start and completion times, and k6's end-of-test summary where available."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in mcpListTestRunsInput) (*mcp.CallToolResult, any, error) {
			params := url.Values{}
			if in.TestCase != "" {
				if !safeLabelValue.MatchString(in.TestCase) {
					return nil, nil, fmt.Errorf("invalid testCase %q", in.TestCase)
				}
				params.Set("labelSelector", benchv1alpha1.LabelTestCase+"="+in.TestCase)
			}
			return mcpObjects(ctx, read, "/testruns", params)
		})
	mcp.AddTool(server, tool("get_test_run", "Get test run", "Get one TestRun by name."),
		getObject("testruns"))
	mcp.AddTool(server, tool("list_workload_clusters", "List workload clusters",
		"List the WorkloadClusters deployments and k6 runners are placed on, with their machine pools and status."),
		listObjects("workloadclusters"))
	mcp.AddTool(server, tool("get_events", "Get object events",
		"Get the Kubernetes events recorded for a deployment, test case, test run or workload cluster - useful to explain a failed or slow run."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
			Kind string `json:"kind" jsonschema:"deployments, testcases, testruns or workloadclusters"`
			Name string `json:"name" jsonschema:"the object's name"`
		}) (*mcp.CallToolResult, any, error) {
			if _, ok := resourceKinds[in.Kind]; !ok || in.Kind == "testcasetemplates" || in.Name == "" {
				return nil, nil, fmt.Errorf("kind (deployments, testcases, testruns or workloadclusters) and name are required")
			}
			return mcpJSON(ctx, read, "/"+in.Kind+"/"+url.PathEscape(in.Name)+"/events", nil)
		})

	mcp.AddTool(server, tool("list_metrics", "List metrics",
		"List the metric keys get_metric_series and get_resource_usage accept, with units and which direction is better."),
		func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
			metrics := make([]mcpMetricInfo, 0, len(compareMetrics)+len(resourceMetrics))
			for _, m := range compareMetrics {
				metrics = append(metrics, mcpMetricInfo{Key: m.Key, Label: m.Label, Unit: m.Unit, Better: m.Better, Tool: "get_metric_series"})
			}
			for _, key := range slices.Sorted(maps.Keys(resourceMetrics)) {
				m := resourceMetrics[key]
				metrics = append(metrics, mcpMetricInfo{Key: key, Label: m.Label, Unit: m.Unit, Better: betterLower, Tool: "get_resource_usage"})
			}
			body, err := json.Marshal(struct {
				Items []mcpMetricInfo `json:"items"`
			}{metrics})
			if err != nil {
				return nil, nil, err
			}
			return mcpText(body), nil, nil
		})
	mcp.AddTool(server, tool("compare_tests", "Compare tests",
		"Summary table per (testid, target): request and transaction throughput, p95/p99 request duration, p95 transaction duration, failed-request ratio, passing-check ratio and peak VUs, computed over a trailing window. A metric with no data is absent, not zero."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in mcpCompareInput) (*mcp.CallToolResult, any, error) {
			params := url.Values{paramTestIDs: {strings.Join(in.TestIDs, ",")}}
			if in.Window != "" {
				params.Set("window", in.Window)
			}
			if in.Time != 0 {
				params.Set("time", strconv.FormatInt(in.Time, 10))
			}
			return mcpJSON(ctx, read, "/metrics/compare", params)
		})
	mcp.AddTool(server, tool("get_metric_series", "Get metric time series",
		"Time series of one k6 metric (see list_metrics) per (testid, target) over a time range."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in mcpSeriesInput) (*mcp.CallToolResult, any, error) {
			start, end := mcpTimeRange(in.Start, in.End)
			params := url.Values{paramTestIDs: {strings.Join(in.TestIDs, ",")}, "metric": {in.Metric}, paramStart: {start}, paramEnd: {end}}
			setIf(params, "window", in.Window)
			setIf(params, "step", in.Step)
			return mcpJSON(ctx, read, "/metrics/range", params)
		})
	mcp.AddTool(server, tool("get_resource_usage", "Get resource usage",
		"CPU or memory usage time series of each testid's target deployments (summed over their namespace, or one authentik component) over a time range."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in mcpResourceInput) (*mcp.CallToolResult, any, error) {
			start, end := mcpTimeRange(in.Start, in.End)
			params := url.Values{paramTestIDs: {strings.Join(in.TestIDs, ",")}, "resource": {in.Resource}, paramStart: {start}, paramEnd: {end}}
			setIf(params, "component", in.Component)
			setIf(params, "window", in.Window)
			setIf(params, "step", in.Step)
			return mcpJSON(ctx, read, "/metrics/resources", params)
		})
	mcp.AddTool(server, tool("get_trace_breakdown", "Get trace breakdown",
		"Where request time goes, from a sample of Tempo traces per target: per stage/endpoint span counts and duration quantiles over a trailing window."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in mcpBreakdownInput) (*mcp.CallToolResult, any, error) {
			params := url.Values{paramTestID: {in.TestID}}
			setIf(params, "window", in.Window)
			return mcpJSON(ctx, read, "/traces/breakdown", params)
		})
	mcp.AddTool(server, tool("get_profile_breakdown", "Get CPU profile breakdown",
		"Where CPU time goes, from Pyroscope: the top functions and code categories of the authentik server or worker per target over a trailing window."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in mcpBreakdownInput) (*mcp.CallToolResult, any, error) {
			params := url.Values{paramTestID: {in.TestID}}
			setIf(params, "window", in.Window)
			setIf(params, "component", in.Component)
			return mcpJSON(ctx, read, "/profiles/breakdown", params)
		})
	mcp.AddTool(server, tool("get_log_breakdown", "Get log breakdown",
		"What authentik logs, from Loki: per target, log lines per server/worker process by level, and the most frequent warning-or-worse events (level, logger, event) over a trailing window."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in mcpBreakdownInput) (*mcp.CallToolResult, any, error) {
			params := url.Values{paramTestID: {in.TestID}}
			setIf(params, "window", in.Window)
			return mcpJSON(ctx, read, "/logs/breakdown", params)
		})
	mcp.AddTool(server, tool("list_alerts", "List alerts",
		"Currently firing alerts across every workload cluster's Alertmanager, plus clusters whose alerting can't be trusted right now."),
		func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
			return mcpJSON(ctx, read, "/alerts", nil)
		})
	mcp.AddTool(server, tool("query_prometheus", "Query Prometheus",
		"Run a raw PromQL instant or range query against the management cluster's Prometheus, for anything the other tools don't cover. Returns Prometheus' own response format."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in mcpPromQLInput) (*mcp.CallToolResult, any, error) {
			params := url.Values{queryParam: {in.Query}}
			switch {
			case in.Start != 0 && in.End != 0:
				params.Set(paramStart, strconv.FormatInt(in.Start, 10))
				params.Set(paramEnd, strconv.FormatInt(in.End, 10))
				setIf(params, "step", in.Step)
			case in.Time != 0:
				params.Set("time", strconv.FormatInt(in.Time, 10))
			}
			return mcpJSON(ctx, read, "/metrics/query", params)
		})

	return server
}

func mcpJSON(ctx context.Context, read http.Handler, path string, params url.Values) (*mcp.CallToolResult, any, error) {
	body, err := mcpGet(ctx, read, path, params)
	if err != nil {
		return nil, nil, err
	}
	return mcpText(body), nil, nil
}

func mcpObjects(ctx context.Context, read http.Handler, path string, params url.Values) (*mcp.CallToolResult, any, error) {
	body, err := mcpGet(ctx, read, path, params)
	if err != nil {
		return nil, nil, err
	}
	trimmed, err := trimObjects(body)
	if err != nil {
		return nil, nil, err
	}
	return mcpText(trimmed), nil, nil
}

func setIf(params url.Values, key, value string) {
	if value != "" {
		params.Set(key, value)
	}
}
