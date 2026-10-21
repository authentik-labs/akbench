package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/common/model"
	"k8s.io/apimachinery/pkg/types"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
	"github.com/authentik-labs/akbench/internal/render"
)

// CompareMetric is one column of the test-comparison table. The queries are
// written against the metric names k6's experimental-prometheus-rw output
// actually produces: counters get a _total suffix, rates a _rate suffix,
// gauges none, and trends - which the k6 workloads run with
// K6_FEATURES=native-histograms - become native histograms
// named after their base unit, hence k6_http_req_duration_seconds and values
// in seconds rather than k6's native milliseconds.
type CompareMetric struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Unit  string `json:"unit"`
	// Query is a template with two verbs: the label selector, then the range
	// window.
	Query string `json:"-"`
	// Better records which direction is an improvement, so the UI can
	// highlight a winner without hardcoding per-metric knowledge.
	Better string `json:"better"`
	// Recorded is Query's equivalent over render's k6 recording rules, same
	// two verbs, used for windows of at least recordedMinWindow. Empty for
	// the instant gauges, which are cheap at any window.
	Recorded string `json:"-"`
}

// recordedMinWindow is the shortest compare window answered from the
// recording rules. Below it the raw query is cheap, and the rules' 1m
// resolution would show up as lag.
const recordedMinWindow = model.Duration(time.Hour)

// fill renders Query for one label selector and range window. A query with
// a range selector ("[%s]" or "[%[2]s]") takes both; one without (a plain
// gauge) takes only the selector.
func (m CompareMetric) fill(selector, window string) string {
	if strings.Contains(m.Query, "[%") {
		return fmt.Sprintf(m.Query, selector, window)
	}
	return fmt.Sprintf(m.Query, selector)
}

// betterHigher/betterLower are CompareMetric.Better's two directions.
const (
	betterHigher = "higher"
	betterLower  = "lower"
)

// metricKeyIterations/metricKeyVUs also name TestCaseSpec's json fields of
// the same values - not the same string by coincidence, but two independent
// literals goconst can't tell apart, so kept named here for its sake.
const (
	metricKeyHTTPReqs   = "http_reqs"
	metricKeyIterations = "iterations"
	metricKeyVUs        = "vus"
)

// compareMetrics is the fixed summary every test comparison reports. Kept
// small on purpose: anything deeper belongs in the Grafana dashboards the
// compare view embeds alongside this table.
// Every aggregation groups `by (target)`: a testid's own TestCase can now
// drive more than one AuthentikDeployment at once (TestCase.Deployments()),
// each tagged with its own k6 `target`, and collapsing that away here would
// silently sum two deployments' numbers into one meaningless value instead
// of showing them side by side - the opposite of why multi-deployment
// TestCases exist in the first place (comparing a test's own performance
// across its targets). histogram_quantile preserves whatever labels its
// `sum by (...)` argument kept, so target survives through the quantile
// metrics too.
var compareMetrics = []CompareMetric{
	{
		Key: metricKeyHTTPReqs, Label: "HTTP requests/s", Unit: "req/s", Better: betterHigher,
		Query:    `sum by (target) (rate(k6_http_reqs_total{%s}[%s]))`,
		Recorded: `sum by (target) (avg_over_time(` + render.RecordedHTTPReqs + `{%s}[%s]))`,
	},
	{
		Key: metricKeyIterations, Label: "Transactions/s", Unit: "tx/s", Better: betterHigher,
		Query:    `sum by (target) (rate(k6_iterations_total{%s}[%s]))`,
		Recorded: `sum by (target) (avg_over_time(` + render.RecordedIterations + `{%s}[%s]))`,
	},
	{
		Key: "http_req_duration_p95", Label: "Request duration p95", Unit: "s", Better: betterLower,
		Query:    `histogram_quantile(0.95, sum by (target) (rate(k6_http_req_duration_seconds{%s}[%s])))`,
		Recorded: `histogram_quantile(0.95, sum by (target) (sum_over_time(` + render.RecordedHTTPReqDuration + `{%s}[%s])))`,
	},
	{
		Key: "http_req_duration_p99", Label: "Request duration p99", Unit: "s", Better: betterLower,
		Query:    `histogram_quantile(0.99, sum by (target) (rate(k6_http_req_duration_seconds{%s}[%s])))`,
		Recorded: `histogram_quantile(0.99, sum by (target) (sum_over_time(` + render.RecordedHTTPReqDuration + `{%s}[%s])))`,
	},
	{
		Key: "iteration_duration_p95", Label: "Transaction duration p95", Unit: "s", Better: betterLower,
		Query:    `histogram_quantile(0.95, sum by (target) (rate(k6_iteration_duration_seconds{%s}[%s])))`,
		Recorded: `histogram_quantile(0.95, sum by (target) (sum_over_time(` + render.RecordedIterationDuration + `{%s}[%s])))`,
	},
	{
		Key: "http_req_failed", Label: "Failed requests", Unit: "ratio", Better: betterLower,
		// From the counter, not k6_http_req_failed_rate: that gauge is k6's
		// cumulative ratio since the pod started, so on a long Burn it
		// barely moves no matter what the window holds. Same expression the
		// k6 dashboard and the error-rate alert use.
		Query: `sum by (target) (rate(k6_http_reqs_total{%[1]s, expected_response="false"}[%[2]s]))` +
			` / sum by (target) (rate(k6_http_reqs_total{%[1]s}[%[2]s]))`,
		// "or ... * 0": a test without a single failure has no failed series
		// to record, and an empty result would fall back to the slow raw
		// query on every poll.
		Recorded: `(sum by (target) (sum_over_time(` + render.RecordedHTTPReqsFailed + `{%[1]s}[%[2]s]))` +
			` or 0 * sum by (target) (sum_over_time(` + render.RecordedHTTPReqs + `{%[1]s}[%[2]s])))` +
			` / sum by (target) (sum_over_time(` + render.RecordedHTTPReqs + `{%[1]s}[%[2]s]))`,
	},
	{
		Key: "checks", Label: "Passing checks", Unit: "ratio", Better: betterHigher,
		Query: `avg by (target) (k6_checks_rate{%s})`,
	},
	{
		Key: metricKeyVUs, Label: "Peak VUs", Unit: metricKeyVUs, Better: betterHigher,
		Query: `max by (target) (k6_vus{%s})`,
	},
}

// safeLabelValue matches the Kubernetes object names that become testid
// values. Every value interpolated into PromQL is checked against it, which
// is what keeps the compare endpoint from being a PromQL injection point.
var safeLabelValue = regexp.MustCompile(`^[a-z0-9]([-a-z0-9._]*[a-z0-9])?$`)

// safeWindow matches a Prometheus duration literal.
var safeWindow = regexp.MustCompile(`^[0-9]+(ms|s|m|h|d|w|y)$`)

// safeUnixTime matches a unix timestamp in seconds, the only form
// metricsRange accepts start/end in.
var safeUnixTime = regexp.MustCompile(`^[0-9]{1,10}$`)

// defaultStep is the range-query resolution used when the caller doesn't
// specify one.
const defaultStep = "15s"

// queryParam is the PromQL-query url.Values key every query/query_range
// call passes.
const queryParam = "query"

// promStatusSuccess is Prometheus' own query-response status field value on
// success (the only one these handlers ever act on; anything else falls
// through to its own error message).
const promStatusSuccess = "success"

// prometheusURLsForTestID resolves which Prometheus instances actually have
// a given testid's data: a TestCase's object name *is* its k6 testid, and
// each of its targets' k6 workload (via the TestCase's runnerClusterRef
// override, else that target's own clusterRef) runs on either the
// management cluster (s.cfg.PrometheusURL) or a WorkloadCluster - which,
// since WorkloadClusterReconciler's ensurePrometheus runs, has its own
// Prometheus reachable at status.prometheusURL. A management-cluster pod
// can reach a workload cluster's node IP directly (confirmed live - the
// reverse can't), which is what makes that URL usable from here at all.
//
// A TestCase driving several deployments on different clusters (and no
// runnerClusterRef) remote-writes each target's samples to a different
// Prometheus, so this returns every distinct URL, never empty.
//
// A deleted Deployment falls back to the runnerCluster the TestCase's status
// last recorded for it. A target that still can't be resolved (the
// TestCase/WorkloadCluster was deleted, or the WorkloadCluster hasn't reconciled a PrometheusURL yet)
// falls back to s.cfg.PrometheusURL rather than erroring outright -
// historical data for a since-deleted test is still worth trying to show,
// and an empty fallback surfaces the same "no Prometheus configured" 502 a
// caller already knows how to read.
func (s *Server) prometheusURLsForTestID(ctx context.Context, testid string) []string {
	var tc benchv1alpha1.TestCase
	if err := s.client.Get(ctx, types.NamespacedName{Name: testid, Namespace: s.cfg.Namespace}, &tc); err != nil {
		return []string{s.cfg.PrometheusURL}
	}

	var clusterNames []string
	if tc.Spec.RunnerClusterRef != nil && tc.Spec.RunnerClusterRef.Name != "" {
		clusterNames = append(clusterNames, tc.Spec.RunnerClusterRef.Name)
	} else {
		for _, ref := range tc.Deployments() {
			var topo benchv1alpha1.AuthentikDeployment
			key := types.NamespacedName{Name: ref.Name, Namespace: s.cfg.Namespace}
			if err := s.client.Get(ctx, key, &topo); err != nil {
				// A since-deleted deployment (common on an archived
				// TestCase): status still records where its k6 ran.
				clusterNames = append(clusterNames, recordedRunnerCluster(&tc, ref.Name))
				continue
			}
			clusterNames = append(clusterNames, topo.Spec.ClusterRef.Name)
		}
	}

	var urls []string
	for _, name := range clusterNames {
		promURL := s.cfg.PrometheusURL
		if name != "" {
			var wc benchv1alpha1.WorkloadCluster
			key := types.NamespacedName{Name: name, Namespace: s.cfg.Namespace}
			if err := s.client.Get(ctx, key, &wc); err == nil && wc.Status.PrometheusURL != "" {
				promURL = wc.Status.PrometheusURL
			}
		}
		if !slices.Contains(urls, promURL) {
			urls = append(urls, promURL)
		}
	}
	if len(urls) == 0 {
		return []string{s.cfg.PrometheusURL}
	}
	return urls
}

// recordedRunnerCluster is the cluster tc's status last recorded the named
// target's k6 workload on, "" (the management cluster) if none.
func recordedRunnerCluster(tc *benchv1alpha1.TestCase, deployment string) string {
	for _, d := range tc.Status.Deployments {
		if d.Name == deployment {
			return d.RunnerCluster
		}
	}
	return ""
}

// proxyQuery forwards a PromQL query to the management cluster's own
// Prometheus. Only the parameters below are passed through, and only the
// two query endpoints are reachable, so this stays a query proxy rather
// than a general-purpose hole through to Prometheus' admin API. Unlike
// compare/metricsRange, this endpoint takes no testids to resolve a
// per-workload-cluster Prometheus from, so it can only ever reach the one
// central to the management cluster.
func (s *Server) proxyQuery(w http.ResponseWriter, r *http.Request) {
	if s.cfg.PrometheusURL == "" {
		writeError(w, r, http.StatusServiceUnavailable, "no Prometheus configured")
		return
	}
	q := r.URL.Query()
	query := q.Get(queryParam)
	if query == "" {
		writeError(w, r, http.StatusBadRequest, "query is required")
		return
	}

	params := url.Values{queryParam: {query}}
	path := "/api/v1/query"
	if start, end := q.Get("start"), q.Get("end"); start != "" && end != "" {
		path = "/api/v1/query_range"
		params.Set("start", start)
		params.Set("end", end)
		step := q.Get("step")
		if step == "" {
			step = defaultStep
		}
		params.Set("step", step)
	} else if ts := q.Get("time"); ts != "" {
		params.Set("time", ts)
	}

	resp, err := s.promGet(r.Context(), s.cfg.PrometheusURL, path, params)
	if err != nil {
		writeError(w, r, http.StatusBadGateway, err.Error())
		return
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxPromResponseBytes))
	if err != nil {
		writeError(w, r, http.StatusBadGateway, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	if _, err := w.Write(body); err != nil {
		return
	}
}

// maxPromResponseBytes caps a proxied Prometheus response. Range queries over
// a long window can be large, but nothing the UI asks for is near this.
const maxPromResponseBytes = 32 << 20

// CompareResult is one row of the comparison table: a (test, target) pair,
// and its summary values keyed by CompareMetric.Key. A metric with no data is
// absent rather than zero, so "the test never reported this" reads
// differently from "it reported zero". Target is always populated, even for
// a single-deployment TestCase - threading one response shape through
// regardless of deployment count is simpler than branching the frontend on
// whether a given testid happens to have more than one.
type CompareResult struct {
	TestID string             `json:"testid"`
	Target string             `json:"target"`
	Values map[string]float64 `json:"values"`
}

// compareTargetsForTestID returns the k6 `target` tag values a testid's own
// TestCase currently drives (one per TestCase.Deployments() entry), so
// compare/metricsRange can seed one row/series per known target even before
// Prometheus has any samples for it yet (a freshly reconciled multi-
// deployment TestCase should show every target immediately, not only the
// ones that already have data). Best-effort: a deployment that can't be
// resolved still gets a row, falling back to its own object name as the
// display target.
func (s *Server) compareTargetsForTestID(ctx context.Context, testid string) []string {
	var tc benchv1alpha1.TestCase
	if err := s.client.Get(ctx, types.NamespacedName{Name: testid, Namespace: s.cfg.Namespace}, &tc); err != nil {
		return nil
	}
	targets := make([]string, 0, len(tc.Deployments()))
	for _, ref := range tc.Deployments() {
		var topo benchv1alpha1.AuthentikDeployment
		key := types.NamespacedName{Name: ref.Name, Namespace: s.cfg.Namespace}
		if err := s.client.Get(ctx, key, &topo); err != nil {
			targets = append(targets, ref.Name)
			continue
		}
		name := topo.Spec.TargetName
		if name == "" {
			name = topo.Name
		}
		targets = append(targets, name)
	}
	return targets
}

type compareResponse struct {
	Window  string          `json:"window"`
	Metrics []CompareMetric `json:"metrics"`
	Results []CompareResult `json:"results"`
	// Errors is keyed by testid: a Prometheus that one testid resolves to
	// can be unreachable (its WorkloadCluster torn down, say) without that
	// costing every other testid in the same request its data.
	Errors map[string]string `json:"errors,omitempty"`
}

// promErrors collects per-testid Prometheus failures for compare and
// metricsRange. Only when not a single Prometheus query of the request
// succeeded does the request as a whole fail, so a dashboard asking about
// many testids at once still renders the ones it can reach.
type promErrors struct {
	byTestID  map[string]string
	succeeded bool
}

func (e *promErrors) add(testid, baseURL string, err error) {
	if e.byTestID == nil {
		e.byTestID = map[string]string{}
	}
	msg := fmt.Sprintf("query Prometheus %q: %s", baseURL, err)
	if baseURL == "" {
		msg = err.Error()
	}
	if prev, ok := e.byTestID[testid]; ok {
		msg = prev + "; " + msg
	}
	e.byTestID[testid] = msg
}

// failed reports whether every query failed, with one of the errors to
// return as the request's own.
func (e *promErrors) failed() (string, bool) {
	if e.succeeded || len(e.byTestID) == 0 {
		return "", false
	}
	ids := slices.Sorted(maps.Keys(e.byTestID))
	return fmt.Sprintf("%s: %s", ids[0], e.byTestID[ids[0]]), true
}

// compare answers GET /metrics/compare?testids=a,b,c[&window=5m][&time=...],
// the numeric half of the compare view (the other half being an embedded
// Grafana dashboard).
func (s *Server) compare(w http.ResponseWriter, r *http.Request) {
	raw := r.URL.Query().Get("testids")
	if raw == "" {
		writeError(w, r, http.StatusBadRequest, "testids is required")
		return
	}
	var testids []string
	for id := range strings.SplitSeq(raw, ",") {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if !safeLabelValue.MatchString(id) {
			writeError(w, r, http.StatusBadRequest, fmt.Sprintf("invalid testid %q", id))
			return
		}
		testids = append(testids, id)
	}
	if len(testids) == 0 {
		writeError(w, r, http.StatusBadRequest, "testids is required")
		return
	}

	window := r.URL.Query().Get("window")
	if window == "" {
		window = "5m"
	}
	if !safeWindow.MatchString(window) {
		writeError(w, r, http.StatusBadRequest, fmt.Sprintf("invalid window %q", window))
		return
	}
	at := r.URL.Query().Get("time")

	resp := compareResponse{Window: window, Metrics: compareMetrics}
	var errs promErrors
	for _, id := range testids {
		baseURLs := s.prometheusURLsForTestID(r.Context(), id)
		selector := fmt.Sprintf("testid=%q", id)

		rows := map[string]*CompareResult{}
		var order []string
		ensureRow := func(target string) *CompareResult {
			row, ok := rows[target]
			if !ok {
				row = &CompareResult{TestID: id, Target: target, Values: map[string]float64{}}
				rows[target] = row
				order = append(order, target)
			}
			return row
		}
		for _, target := range s.compareTargetsForTestID(r.Context(), id) {
			ensureRow(target)
		}

		for _, baseURL := range baseURLs {
			for _, m := range compareMetrics {
				samples, err := s.compareSamples(r.Context(), baseURL, m, selector, window, at)
				if err != nil {
					// The rest of this Prometheus' metrics would fail the
					// same way; move on to the next one.
					errs.add(id, baseURL, err)
					break
				}
				errs.succeeded = true
				for _, sample := range samples {
					ensureRow(sample.Target).Values[m.Key] = sample.Value
				}
			}
		}
		if len(order) == 0 {
			// Neither a known deployment nor any actual data: still emit one
			// empty row for this testid rather than none, matching
			// metricsRange's own fallback.
			ensureRow("")
		}
		for _, target := range order {
			resp.Results = append(resp.Results, *rows[target])
		}
	}
	if msg, failed := errs.failed(); failed {
		writeError(w, r, http.StatusBadGateway, msg)
		return
	}
	resp.Errors = errs.byTestID
	writeJSON(w, r, http.StatusOK, resp)
}

// compareSamples answers one metric of compare, from the recording rules
// for a long window. An empty recorded result falls back to the raw query:
// a Prometheus without the rules (the management cluster's), or a test
// whose data predates them. A test that started before the rules existed
// is summarised over only the part they cover.
func (s *Server) compareSamples(
	ctx context.Context, baseURL string, m CompareMetric, selector, window, at string,
) ([]promSample, error) {
	if d, err := model.ParseDuration(window); err == nil && m.Recorded != "" && d >= recordedMinWindow {
		samples, err := s.promInstantMulti(ctx, baseURL, fmt.Sprintf(m.Recorded, selector, window), at)
		if err != nil || len(samples) > 0 {
			return samples, err
		}
	}
	return s.promInstantMulti(ctx, baseURL, m.fill(selector, window), at)
}

// RangePoint is one (timestamp, value) sample of a metricsRange result.
type RangePoint struct {
	Time  int64   `json:"time"`
	Value float64 `json:"value"`
}

// RangeSeries is one (test, target) pair's time series for a single
// CompareMetric. Target is always populated, same reasoning as
// CompareResult.Target.
type RangeSeries struct {
	TestID string       `json:"testid"`
	Target string       `json:"target"`
	Points []RangePoint `json:"points"`
}

type rangeResponse struct {
	Metric CompareMetric `json:"metric"`
	Series []RangeSeries `json:"series"`
	// Errors is keyed by testid, same as compareResponse.Errors.
	Errors map[string]string `json:"errors,omitempty"`
}

// metricsRange answers GET
// /metrics/range?testids=a,b&metric=<key>&start=&end=[&window=][&step=], the
// per-test time series behind the compare view's charts and a TestCase's own
// metrics panel. It is the only place a Burn TestCase's metrics can be
// viewed at all: Burn has no TestRun history the way Quick does, since it
// runs as a self-healing Deployment rather than a one-shot Job, so its only
// record of what happened is whatever Prometheus still retains.
func (s *Server) metricsRange(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	raw := q.Get("testids")
	if raw == "" {
		writeError(w, r, http.StatusBadRequest, "testids is required")
		return
	}
	var testids []string
	for id := range strings.SplitSeq(raw, ",") {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if !safeLabelValue.MatchString(id) {
			writeError(w, r, http.StatusBadRequest, fmt.Sprintf("invalid testid %q", id))
			return
		}
		testids = append(testids, id)
	}
	if len(testids) == 0 {
		writeError(w, r, http.StatusBadRequest, "testids is required")
		return
	}

	var metric *CompareMetric
	for i := range compareMetrics {
		if compareMetrics[i].Key == q.Get("metric") {
			metric = &compareMetrics[i]
			break
		}
	}
	if metric == nil {
		writeError(w, r, http.StatusBadRequest, fmt.Sprintf("unknown metric %q", q.Get("metric")))
		return
	}

	start, end := q.Get("start"), q.Get("end")
	if !safeUnixTime.MatchString(start) || !safeUnixTime.MatchString(end) {
		writeError(w, r, http.StatusBadRequest, "start and end must be unix timestamps")
		return
	}

	window := q.Get("window")
	if window == "" {
		window = "1m"
	}
	if !safeWindow.MatchString(window) {
		writeError(w, r, http.StatusBadRequest, fmt.Sprintf("invalid window %q", window))
		return
	}
	step := q.Get("step")
	if step == "" {
		step = defaultStep
	}
	if !safeWindow.MatchString(step) {
		writeError(w, r, http.StatusBadRequest, fmt.Sprintf("invalid step %q", step))
		return
	}

	resp := rangeResponse{Metric: *metric}
	var errs promErrors
	for _, id := range testids {
		selector := fmt.Sprintf("testid=%q", id)
		query := metric.fill(selector, window)
		var series []promSeries
		for _, baseURL := range s.prometheusURLsForTestID(r.Context(), id) {
			got, err := s.promRangeMulti(r.Context(), baseURL, query, start, end, step)
			if err != nil {
				errs.add(id, baseURL, err)
				continue
			}
			errs.succeeded = true
			series = append(series, got...)
		}
		byTarget := make(map[string][]RangePoint, len(series))
		for _, ser := range series {
			byTarget[ser.Target] = ser.Points
		}
		seen := make(map[string]bool, len(byTarget))
		rowsBefore := len(resp.Series)
		for _, target := range s.compareTargetsForTestID(r.Context(), id) {
			points := byTarget[target]
			if points == nil {
				// A nil slice marshals to JSON null; the frontend iterates
				// this unconditionally, and a target with no data yet in the
				// window is a normal, common case, not an error.
				points = []RangePoint{}
			}
			resp.Series = append(resp.Series, RangeSeries{TestID: id, Target: target, Points: points})
			seen[target] = true
		}
		// Any target Prometheus actually reported that the live TestCase
		// doesn't currently know about (e.g. a target just removed from
		// deploymentRefs, whose historical data is still worth showing).
		for _, ser := range series {
			if !seen[ser.Target] {
				resp.Series = append(resp.Series, RangeSeries{TestID: id, Target: ser.Target, Points: ser.Points})
				seen[ser.Target] = true
			}
		}
		// Neither a known deployment nor any actual data: still emit one
		// empty row for this testid rather than none, so a caller asking
		// about it gets something back instead of it silently vanishing
		// from the response.
		if len(resp.Series) == rowsBefore {
			resp.Series = append(resp.Series, RangeSeries{TestID: id, Points: []RangePoint{}})
		}
	}
	if msg, failed := errs.failed(); failed {
		writeError(w, r, http.StatusBadGateway, msg)
		return
	}
	resp.Errors = errs.byTestID
	writeJSON(w, r, http.StatusOK, resp)
}

// promMatrixResponse is the query_range shape: one entry per distinct label
// set. Every CompareMetric.Query and resourceMetric.Query aggregates down to
// a single series (sum(...)/avg(...)/max(...) with no "by"), so promRange
// only ever reads the first result entry.
type promMatrixResponse struct {
	Status string `json:"status"`
	Error  string `json:"error"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string    `json:"metric"`
			Values [][2]json.RawMessage `json:"values"`
		} `json:"result"`
	} `json:"data"`
}

// promRange runs a range query and returns its single series, dropping
// non-finite samples the same way promInstant drops a NaN quantile.
func (s *Server) promRange(ctx context.Context, baseURL, query, start, end, step string) ([]RangePoint, error) {
	params := url.Values{queryParam: {query}, "start": {start}, "end": {end}, "step": {step}}
	resp, err := s.promGet(ctx, baseURL, "/api/v1/query_range", params)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	var parsed promMatrixResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxPromResponseBytes)).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("decode Prometheus response: %w", err)
	}
	if parsed.Status != promStatusSuccess {
		if parsed.Error != "" {
			return nil, fmt.Errorf("prometheus: %s", parsed.Error)
		}
		return nil, fmt.Errorf("prometheus returned an error status")
	}
	// []RangePoint{}, not nil: a nil slice marshals to JSON null, and the
	// frontend iterates this array unconditionally (a test with no data in
	// the window is a legitimate, common case - not an error to special-case).
	if len(parsed.Data.Result) == 0 {
		return []RangePoint{}, nil
	}
	raw := parsed.Data.Result[0].Values
	points := make([]RangePoint, 0, len(raw))
	for _, sample := range raw {
		var ts float64
		if err := json.Unmarshal(sample[0], &ts); err != nil {
			return nil, fmt.Errorf("decode sample timestamp: %w", err)
		}
		var text string
		if err := json.Unmarshal(sample[1], &text); err != nil {
			return nil, fmt.Errorf("decode sample value: %w", err)
		}
		value, err := strconv.ParseFloat(text, 64)
		if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
			continue
		}
		points = append(points, RangePoint{Time: int64(ts), Value: value})
	}
	return points, nil
}

// promSeries is one distinct `target`-labeled series of a `by (target)`
// range query's result - what promRangeMulti returns, one entry per target
// compareMetrics' queries are now grouped by.
type promSeries struct {
	Target string
	// Labels is the series' full label set, for callers grouping by
	// something other than target.
	Labels map[string]string
	Points []RangePoint
}

// promRangeMulti runs a range query and returns every distinct series in its
// result (keyed by the `target` label the compareMetrics queries now group
// by), unlike promRange which only ever reads the first - used by
// metricsRange, where a testid's own TestCase can drive more than one
// deployment at once.
func (s *Server) promRangeMulti(ctx context.Context, baseURL, query, start, end, step string) ([]promSeries, error) {
	params := url.Values{queryParam: {query}, "start": {start}, "end": {end}, "step": {step}}
	resp, err := s.promGet(ctx, baseURL, "/api/v1/query_range", params)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	var parsed promMatrixResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxPromResponseBytes)).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("decode Prometheus response: %w", err)
	}
	if parsed.Status != promStatusSuccess {
		if parsed.Error != "" {
			return nil, fmt.Errorf("prometheus: %s", parsed.Error)
		}
		return nil, fmt.Errorf("prometheus returned an error status")
	}
	series := make([]promSeries, 0, len(parsed.Data.Result))
	for _, result := range parsed.Data.Result {
		points := make([]RangePoint, 0, len(result.Values))
		for _, sample := range result.Values {
			var ts float64
			if err := json.Unmarshal(sample[0], &ts); err != nil {
				return nil, fmt.Errorf("decode sample timestamp: %w", err)
			}
			var text string
			if err := json.Unmarshal(sample[1], &text); err != nil {
				return nil, fmt.Errorf("decode sample value: %w", err)
			}
			value, err := strconv.ParseFloat(text, 64)
			if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
				continue
			}
			points = append(points, RangePoint{Time: int64(ts), Value: value})
		}
		series = append(series, promSeries{Target: result.Metric["target"], Labels: result.Metric, Points: points})
	}
	return series, nil
}

func (s *Server) promGet(ctx context.Context, baseURL, path string, params url.Values) (*http.Response, error) {
	if baseURL == "" {
		return nil, fmt.Errorf("no Prometheus configured")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimSuffix(baseURL, "/")+path+"?"+params.Encode(), nil)
	if err != nil {
		return nil, err
	}
	return s.http.Do(req)
}

// promVectorResponse is the slice of Prometheus' response format the compare
// endpoint needs: the first sample of an instant vector.
type promVectorResponse struct {
	Status string `json:"status"`
	Error  string `json:"error"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string `json:"metric"`
			Value  []json.RawMessage `json:"value"`
		} `json:"result"`
	} `json:"data"`
}

// promSample is one distinct `target`-labeled sample of a `by (target)`
// instant query's result.
type promSample struct {
	Target string
	Value  float64
	Labels map[string]string
}

// promInstantMulti runs an instant query and returns every distinct sample in
// its result (keyed by the `target` label the compareMetrics queries now
// group by) - a testid with no samples at all in the window returns an empty
// slice rather than an error, same as the old single-value promInstant did
// with its ok=false.
func (s *Server) promInstantMulti(ctx context.Context, baseURL, query, at string) ([]promSample, error) {
	params := url.Values{queryParam: {query}}
	if at != "" {
		params.Set("time", at)
	}
	resp, err := s.promGet(ctx, baseURL, "/api/v1/query", params)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	var parsed promVectorResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxPromResponseBytes)).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("decode Prometheus response: %w", err)
	}
	if parsed.Status != promStatusSuccess {
		if parsed.Error != "" {
			return nil, fmt.Errorf("prometheus: %s", parsed.Error)
		}
		return nil, fmt.Errorf("prometheus returned %s", resp.Status)
	}
	samples := make([]promSample, 0, len(parsed.Data.Result))
	for _, result := range parsed.Data.Result {
		if len(result.Value) != 2 {
			continue
		}
		// A vector sample is [timestamp, "value"] - the value is a JSON string.
		var text string
		if err := json.Unmarshal(result.Value[1], &text); err != nil {
			return nil, fmt.Errorf("decode sample value: %w", err)
		}
		value, err := strconv.ParseFloat(text, 64)
		if err != nil {
			return nil, fmt.Errorf("parse sample value %q: %w", text, err)
		}
		// histogram_quantile over an empty histogram yields NaN, which is
		// both meaningless here and unrepresentable in JSON - drop it,
		// same as a metric with no data at all.
		if math.IsNaN(value) || math.IsInf(value, 0) {
			continue
		}
		samples = append(samples, promSample{Target: result.Metric["target"], Value: value, Labels: result.Metric})
	}
	return samples, nil
}
