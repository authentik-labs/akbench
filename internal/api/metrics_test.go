package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
)

func TestProxyQueryInstant(t *testing.T) {
	var gotPath, gotQuery string
	prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.Query().Get("query")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))
	defer prom.Close()

	s, _ := newServer(t, Config{PrometheusURL: prom.URL})
	rec := do(t, s, http.MethodGet, "/api/v1/metrics/query?query=up", nil)
	requireStatus(t, rec, http.StatusOK)

	if gotPath != "/api/v1/query" {
		t.Fatalf("forwarded to %q, want the instant query endpoint", gotPath)
	}
	if gotQuery != "up" {
		t.Fatalf("forwarded query %q", gotQuery)
	}
}

// start+end must select query_range, and a missing step must be defaulted -
// Prometheus rejects a range query without one.
func TestProxyQueryRange(t *testing.T) {
	var gotPath, gotStep string
	prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotStep = r.URL.Path, r.URL.Query().Get("step")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[]}}`))
	}))
	defer prom.Close()

	s, _ := newServer(t, Config{PrometheusURL: prom.URL})
	rec := do(t, s, http.MethodGet,
		"/api/v1/metrics/query?query=k6_vus&start=1700000000&end=1700003600", nil)
	requireStatus(t, rec, http.StatusOK)

	if gotPath != "/api/v1/query_range" {
		t.Fatalf("forwarded to %q, want the range query endpoint", gotPath)
	}
	if gotStep != "15s" {
		t.Fatalf("step defaulted to %q", gotStep)
	}
}

func TestProxyQueryRequiresQuery(t *testing.T) {
	s, _ := newServer(t, Config{PrometheusURL: "http://prometheus.invalid"})
	requireStatus(t, do(t, s, http.MethodGet, "/api/v1/metrics/query", nil), http.StatusBadRequest)
}

func TestProxyQueryWithoutPrometheus(t *testing.T) {
	s, _ := newServer(t, Config{})
	requireStatus(t, do(t, s, http.MethodGet, "/api/v1/metrics/query?query=up", nil), http.StatusServiceUnavailable)
}

func TestCompare(t *testing.T) {
	seen := map[string]bool{}
	prom := fakePrometheus(t, func(query string) (string, string, bool) {
		seen[query] = true
		switch {
		case strings.Contains(query, "k6_http_reqs_total") && strings.Contains(query, `testid="login-a"`):
			return "120.5", "", true
		case strings.Contains(query, "k6_http_reqs_total") && strings.Contains(query, `testid="login-b"`):
			return "90", "", true
		// Everything else reports no data, which must land as an absent key
		// rather than a zero the UI would render as a real measurement.
		default:
			return "", "", false
		}
	})

	s, _ := newServer(t, Config{PrometheusURL: prom.URL})
	rec := do(t, s, http.MethodGet, "/api/v1/metrics/compare?testids=login-a,login-b", nil)
	requireStatus(t, rec, http.StatusOK)

	body := decodeBody[struct {
		Window  string `json:"window"`
		Results []struct {
			TestID string             `json:"testid"`
			Values map[string]float64 `json:"values"`
		} `json:"results"`
	}](t, rec)

	if body.Window != "5m" {
		t.Fatalf("default window is %q, want 5m", body.Window)
	}
	if len(body.Results) != 2 {
		t.Fatalf("got %d results, want 2", len(body.Results))
	}
	if got := body.Results[0].Values["http_reqs"]; got != 120.5 {
		t.Fatalf("login-a http_reqs is %v, want 120.5", got)
	}
	if _, present := body.Results[0].Values["vus"]; present {
		t.Fatalf("a metric with no data was reported anyway: %v", body.Results[0].Values)
	}
	// The p95 query has to be the native-histogram form: k6 runs with
	// K6_FEATURES=native-histograms, so there is no le label to quantile over.
	// It also has to group `by (target)`, so a testid with more than one
	// deployment shows separate rows instead of one summed line.
	wantP95 := `histogram_quantile(0.95, sum by (target) (rate(k6_http_req_duration_seconds{testid="login-a"}[5m])))`
	if !seen[wantP95] {
		t.Fatalf("p95 query was not %q; queries seen: %v", wantP95, seen)
	}
}

// A long window reads the recording rules, and falls back to the raw query
// for a metric they have nothing for (a Prometheus without the rules).
func TestCompareLongWindowUsesRecordingRules(t *testing.T) {
	seen := map[string]bool{}
	prom := fakePrometheus(t, func(query string) (string, string, bool) {
		seen[query] = true
		if strings.Contains(query, "k6_iterations") {
			return "7", "", !strings.Contains(query, "testid_target:")
		}
		return "3", "", strings.Contains(query, "testid_target:")
	})

	s, _ := newServer(t, Config{PrometheusURL: prom.URL})
	rec := do(t, s, http.MethodGet, "/api/v1/metrics/compare?testids=login-a&window=259200s", nil)
	requireStatus(t, rec, http.StatusOK)

	body := decodeBody[struct {
		Results []struct {
			Values map[string]float64 `json:"values"`
		} `json:"results"`
	}](t, rec)
	if got := body.Results[0].Values; got["http_reqs"] != 3 || got["iterations"] != 7 {
		t.Fatalf("values %v, want http_reqs from the recording rule and iterations from the raw fallback", got)
	}
	if seen[`sum by (target) (rate(k6_http_reqs_total{testid="login-a"}[259200s]))`] {
		t.Fatalf("raw http_reqs query ran despite recorded data; queries seen: %v", seen)
	}
}

// A TestCase driving more than one deployment must show one compare row per
// target rather than collapsing them into a single summed number - the
// entire reason compareMetrics' queries group `by (target)` in the first
// place (see the package doc comment above compareMetrics itself).
func TestCompareMultiDeploymentShowsOneRowPerTarget(t *testing.T) {
	prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		query := r.URL.Query().Get("query")
		if !strings.Contains(query, "k6_http_reqs_total") {
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
			return
		}
		body := map[string]any{
			"status": "success",
			"data": map[string]any{
				"resultType": "vector",
				"result": []any{
					map[string]any{"metric": map[string]string{"target": "dep-a"}, "value": []any{1700000000, "10"}},
					map[string]any{"metric": map[string]string{"target": "dep-b"}, "value": []any{1700000000, "20"}},
				},
			},
		}
		_ = json.NewEncoder(w).Encode(body)
	}))
	defer prom.Close()

	tc := sampleTestCase("login-multi", "", benchv1alpha1.ProfileBurn)
	tc.Spec.DeploymentRefs = []corev1.LocalObjectReference{{Name: "dep-a"}, {Name: "dep-b"}}

	s, _ := newServer(t, Config{PrometheusURL: prom.URL}, sampleDeployment("dep-a"), sampleDeployment("dep-b"), tc)
	rec := do(t, s, http.MethodGet, "/api/v1/metrics/compare?testids=login-multi", nil)
	requireStatus(t, rec, http.StatusOK)

	body := decodeBody[struct {
		Results []CompareResult `json:"results"`
	}](t, rec)
	if len(body.Results) != 2 {
		t.Fatalf("got %d results, want 2 (one per deployment): %+v", len(body.Results), body.Results)
	}
	values := map[string]float64{}
	for _, row := range body.Results {
		if row.TestID != "login-multi" {
			t.Errorf("row testid = %q, want login-multi", row.TestID)
		}
		values[row.Target] = row.Values["http_reqs"]
	}
	if values["dep-a"] != 10 || values["dep-b"] != 20 {
		t.Fatalf("values by target = %+v, want dep-a=10 dep-b=20", values)
	}
}

// TestMetricsResolvesPerClusterPrometheus: a testid whose Deployment has a
// clusterRef must be queried against that WorkloadCluster's own
// status.prometheusURL, not the management cluster's Config.PrometheusURL -
// and a testid with no clusterRef (the management cluster) must still use
// Config.PrometheusURL. Two separate fake Prometheus servers stand in for
// "two different clusters' Prometheus instances" - if a query for the wrong
// testid lands on the wrong server, the response reports no data for it and
// the assertions below catch that.
func TestMetricsResolvesPerClusterPrometheus(t *testing.T) {
	// Target has to match what compareTargetsForTestID itself resolves
	// (the deployment object's own name, since neither sets TargetName) -
	// otherwise the pre-seeded known-target row and the sample-tagged row
	// would land as two separate, unmerged rows instead of one.
	mgmtProm := fakePrometheus(t, func(query string) (string, string, bool) {
		if strings.Contains(query, `testid="login-mgmt"`) {
			return "42", "topo-mgmt", true
		}
		return "", "", false
	})
	wcProm := fakePrometheus(t, func(query string) (string, string, bool) {
		if strings.Contains(query, `testid="login-wc"`) {
			return "7", "topo-wc", true
		}
		return "", "", false
	})

	wc := &benchv1alpha1.WorkloadCluster{
		Name: "wc1", Namespace: testNamespace,
		Status: benchv1alpha1.WorkloadClusterStatus{PrometheusURL: wcProm.URL},
	}
	topoMgmt := sampleDeployment("topo-mgmt")
	topoWC := sampleDeployment("topo-wc")
	topoWC.Spec.ClusterRef = corev1.LocalObjectReference{Name: "wc1"}
	tcMgmt := sampleTestCase("login-mgmt", "topo-mgmt", benchv1alpha1.ProfileBurn)
	tcWC := sampleTestCase("login-wc", "topo-wc", benchv1alpha1.ProfileBurn)

	s, _ := newServer(t, Config{PrometheusURL: mgmtProm.URL}, wc, topoMgmt, topoWC, tcMgmt, tcWC)
	rec := do(t, s, http.MethodGet, "/api/v1/metrics/compare?testids=login-mgmt,login-wc", nil)
	requireStatus(t, rec, http.StatusOK)

	body := decodeBody[struct {
		Results []struct {
			TestID string             `json:"testid"`
			Values map[string]float64 `json:"values"`
		} `json:"results"`
	}](t, rec)
	if len(body.Results) != 2 {
		t.Fatalf("got %d results, want 2", len(body.Results))
	}
	for _, row := range body.Results {
		switch row.TestID {
		case "login-mgmt":
			if got := row.Values["http_reqs"]; got != 42 {
				t.Errorf("login-mgmt http_reqs = %v, want 42 (queried the wrong Prometheus)", got)
			}
		case "login-wc":
			if got := row.Values["http_reqs"]; got != 7 {
				t.Errorf("login-wc http_reqs = %v, want 7 (queried the wrong Prometheus)", got)
			}
		}
	}
}

// TestCompareMultiDeploymentAcrossClusters: one TestCase whose two targets
// live on different clusters (no runnerClusterRef) has each target's k6
// workload remote-write to its own cluster's Prometheus, so compare has to
// query both and merge, not just the first target's.
func TestCompareMultiDeploymentAcrossClusters(t *testing.T) {
	mgmtProm := fakePrometheus(t, func(string) (string, string, bool) {
		return "42", "topo-mgmt", true
	})
	wcProm := fakePrometheus(t, func(string) (string, string, bool) {
		return "7", "topo-wc", true
	})

	wc := &benchv1alpha1.WorkloadCluster{
		Name: "wc1", Namespace: testNamespace,
		Status: benchv1alpha1.WorkloadClusterStatus{PrometheusURL: wcProm.URL},
	}
	topoMgmt := sampleDeployment("topo-mgmt")
	topoWC := sampleDeployment("topo-wc")
	topoWC.Spec.ClusterRef = corev1.LocalObjectReference{Name: "wc1"}
	tc := sampleTestCase("login-split", "", benchv1alpha1.ProfileBurn)
	tc.Spec.DeploymentRefs = []corev1.LocalObjectReference{{Name: "topo-mgmt"}, {Name: "topo-wc"}}

	s, _ := newServer(t, Config{PrometheusURL: mgmtProm.URL}, wc, topoMgmt, topoWC, tc)
	rec := do(t, s, http.MethodGet, "/api/v1/metrics/compare?testids=login-split", nil)
	requireStatus(t, rec, http.StatusOK)

	body := decodeBody[struct {
		Results []CompareResult `json:"results"`
	}](t, rec)
	values := map[string]float64{}
	for _, row := range body.Results {
		values[row.Target] = row.Values["http_reqs"]
	}
	if len(body.Results) != 2 || values["topo-mgmt"] != 42 || values["topo-wc"] != 7 {
		t.Fatalf("results = %+v, want topo-mgmt=42 from the management Prometheus and topo-wc=7 from wc1's", body.Results)
	}
}

// A testid reaches PromQL as a label value, so anything that is not a
// Kubernetes object name has to be refused before it gets interpolated.
func TestCompareRejectsInjection(t *testing.T) {
	s, _ := newServer(t, Config{PrometheusURL: "http://prometheus.invalid"})
	for _, id := range []string{`a"} or up{`, "UPPER", "with space", "-leading"} {
		rec := do(t, s, http.MethodGet, "/api/v1/metrics/compare?testids="+url.QueryEscape(id), nil)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("testid %q got status %d, want 400", id, rec.Code)
		}
	}
}

func TestCompareRejectsBadWindow(t *testing.T) {
	s, _ := newServer(t, Config{PrometheusURL: "http://prometheus.invalid"})
	rec := do(t, s, http.MethodGet, "/api/v1/metrics/compare?testids=login-a&window=5m])+or+up", nil)
	requireStatus(t, rec, http.StatusBadRequest)
}

// NaN is what an empty native histogram quantiles to, and it cannot be
// encoded as JSON - it has to be dropped, not returned.
func TestCompareDropsNaN(t *testing.T) {
	prom := fakePrometheus(t, func(query string) (string, string, bool) {
		if strings.Contains(query, "histogram_quantile") {
			return "NaN", "", true
		}
		return "1", "", true
	})

	s, _ := newServer(t, Config{PrometheusURL: prom.URL})
	rec := do(t, s, http.MethodGet, "/api/v1/metrics/compare?testids=login-a", nil)
	requireStatus(t, rec, http.StatusOK)

	body := decodeBody[struct {
		Results []struct {
			Values map[string]float64 `json:"values"`
		} `json:"results"`
	}](t, rec)
	if _, present := body.Results[0].Values["http_req_duration_p95"]; present {
		t.Fatalf("NaN was reported as a value: %v", body.Results[0].Values)
	}
	if body.Results[0].Values["http_reqs"] != 1 {
		t.Fatalf("real values were dropped too: %v", body.Results[0].Values)
	}
}

func TestMetricsRange(t *testing.T) {
	seen := map[string]bool{}
	prom := fakePrometheusRange(t, func(query string) [][2]any {
		seen[query] = true
		switch {
		case strings.Contains(query, `testid="login-a"`):
			return [][2]any{{1700000000, "1"}, {1700000015, "2"}}
		case strings.Contains(query, `testid="login-b"`):
			return [][2]any{{1700000000, "3"}}
		default:
			return nil
		}
	})

	s, _ := newServer(t, Config{PrometheusURL: prom.URL})
	rec := do(t, s, http.MethodGet,
		"/api/v1/metrics/range?testids=login-a,login-b&metric=http_reqs&start=1700000000&end=1700000015", nil)
	requireStatus(t, rec, http.StatusOK)

	body := decodeBody[struct {
		Metric struct {
			Key   string `json:"key"`
			Label string `json:"label"`
		} `json:"metric"`
		Series []struct {
			TestID string `json:"testid"`
			Points []struct {
				Time  int64   `json:"time"`
				Value float64 `json:"value"`
			} `json:"points"`
		} `json:"series"`
	}](t, rec)

	if body.Metric.Key != "http_reqs" {
		t.Fatalf("metric key = %q, want http_reqs", body.Metric.Key)
	}
	if len(body.Series) != 2 {
		t.Fatalf("got %d series, want 2", len(body.Series))
	}
	if got := body.Series[0]; got.TestID != "login-a" || len(got.Points) != 2 || got.Points[1].Value != 2 {
		t.Fatalf("login-a series = %+v", got)
	}
	if got := body.Series[1]; got.TestID != "login-b" || len(got.Points) != 1 || got.Points[0].Value != 3 {
		t.Fatalf("login-b series = %+v", got)
	}
	// Default window (1m) has to reach the query, same as compare's default 5m.
	wantQuery := `sum by (target) (rate(k6_http_reqs_total{testid="login-a"}[1m]))`
	if !seen[wantQuery] {
		t.Fatalf("query was not %q; queries seen: %v", wantQuery, seen)
	}
}

// A series with no samples in the window has to serialize as "points":[],
// not "points":null - the frontend iterates every series unconditionally,
// and null is not iterable in JS.
func TestMetricsRangeEmptySeriesIsNotNull(t *testing.T) {
	prom := fakePrometheusRange(t, func(query string) [][2]any { return nil })

	s, _ := newServer(t, Config{PrometheusURL: prom.URL})
	rec := do(t, s, http.MethodGet,
		"/api/v1/metrics/range?testids=login-a&metric=http_reqs&start=1700000000&end=1700000015", nil)
	requireStatus(t, rec, http.StatusOK)

	if strings.Contains(rec.Body.String(), `"points":null`) {
		t.Fatalf("an empty series serialized as null: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"points":[]`) {
		t.Fatalf("an empty series did not serialize as []: %s", rec.Body.String())
	}
}

func TestMetricsRangeRejectsUnknownMetric(t *testing.T) {
	s, _ := newServer(t, Config{PrometheusURL: "http://prometheus.invalid"})
	rec := do(t, s, http.MethodGet,
		"/api/v1/metrics/range?testids=login-a&metric=not_a_real_metric&start=1700000000&end=1700000015", nil)
	requireStatus(t, rec, http.StatusBadRequest)
}

func TestMetricsRangeRejectsBadTimestamps(t *testing.T) {
	s, _ := newServer(t, Config{PrometheusURL: "http://prometheus.invalid"})
	for _, target := range []string{
		"/api/v1/metrics/range?testids=login-a&metric=http_reqs&start=not-a-time&end=1700000015",
		"/api/v1/metrics/range?testids=login-a&metric=http_reqs&start=1700000000&end=",
	} {
		rec := do(t, s, http.MethodGet, target, nil)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s got status %d, want 400", target, rec.Code)
		}
	}
}

func TestMetricsRangeRejectsInjection(t *testing.T) {
	s, _ := newServer(t, Config{PrometheusURL: "http://prometheus.invalid"})
	rec := do(t, s, http.MethodGet,
		"/api/v1/metrics/range?testids="+url.QueryEscape(`a"} or up{`)+"&metric=http_reqs&start=1700000000&end=1700000015", nil)
	requireStatus(t, rec, http.StatusBadRequest)
}

// TestMetricsRangeWithoutPrometheus: unlike proxyQuery, metricsRange has no
// upfront "is Prometheus configured" check any more - which Prometheus to
// query is resolved per testid (prometheusURLsForTestID), since a testid on
// a workload cluster and one on the management cluster can resolve to two
// different Prometheus instances. An unresolvable testid (as here - no
// TestCase named "login-a" exists) falls back to the empty
// Config.PrometheusURL, which promGet now reports as a 502 the same way it
// reports any other Prometheus-unreachable condition, rather than a
// dedicated upfront 503.
func TestMetricsRangeWithoutPrometheus(t *testing.T) {
	s, _ := newServer(t, Config{})
	rec := do(t, s, http.MethodGet,
		"/api/v1/metrics/range?testids=login-a&metric=http_reqs&start=1700000000&end=1700000015", nil)
	requireStatus(t, rec, http.StatusBadGateway)
}

// TestMetricsToleratesOneUnreachablePrometheus: one testid whose
// Prometheus is gone (its WorkloadCluster torn down, say) must not cost
// every other testid in the same request its data - the dashboard asks
// about every Burn TestCase at once.
func TestMetricsToleratesOneUnreachablePrometheus(t *testing.T) {
	mgmtProm := fakePrometheus(t, func(string) (string, string, bool) {
		return "42", "topo-mgmt", true
	})
	wc := &benchv1alpha1.WorkloadCluster{
		Name: "wc1", Namespace: testNamespace,
		Status: benchv1alpha1.WorkloadClusterStatus{PrometheusURL: "http://127.0.0.1:1"},
	}
	topoMgmt := sampleDeployment("topo-mgmt")
	topoWC := sampleDeployment("topo-wc")
	topoWC.Spec.ClusterRef = corev1.LocalObjectReference{Name: "wc1"}
	tcMgmt := sampleTestCase("login-mgmt", "topo-mgmt", benchv1alpha1.ProfileBurn)
	tcWC := sampleTestCase("login-wc", "topo-wc", benchv1alpha1.ProfileBurn)
	s, _ := newServer(t, Config{PrometheusURL: mgmtProm.URL}, wc, topoMgmt, topoWC, tcMgmt, tcWC)

	rec := do(t, s, http.MethodGet, "/api/v1/metrics/compare?testids=login-mgmt,login-wc", nil)
	requireStatus(t, rec, http.StatusOK)
	compare := decodeBody[compareResponse](t, rec)
	if _, ok := compare.Errors["login-wc"]; !ok || len(compare.Errors) != 1 {
		t.Errorf("compare errors = %v, want only login-wc", compare.Errors)
	}
	for _, row := range compare.Results {
		if row.TestID == "login-mgmt" && row.Values["http_reqs"] != 42 {
			t.Errorf("login-mgmt http_reqs = %v, want 42", row.Values["http_reqs"])
		}
	}

	rec = do(t, s, http.MethodGet,
		"/api/v1/metrics/range?testids=login-mgmt,login-wc&metric=http_reqs&start=1700000000&end=1700000015", nil)
	requireStatus(t, rec, http.StatusOK)
	rng := decodeBody[rangeResponse](t, rec)
	if _, ok := rng.Errors["login-wc"]; !ok || len(rng.Errors) != 1 {
		t.Errorf("range errors = %v, want only login-wc", rng.Errors)
	}

	// Asked about on its own, the unreachable testid is still an error.
	rec = do(t, s, http.MethodGet, "/api/v1/metrics/compare?testids=login-wc", nil)
	requireStatus(t, rec, http.StatusBadGateway)
}

// TestMetricsFallsBackToRecordedRunnerCluster: an archived TestCase whose
// AuthentikDeployments have since been deleted still has its data in the
// workload cluster's Prometheus its k6 ran on, which only its status
// remembers.
func TestMetricsFallsBackToRecordedRunnerCluster(t *testing.T) {
	wcProm := fakePrometheus(t, func(string) (string, string, bool) {
		return "7", "topo-gone", true
	})
	wc := &benchv1alpha1.WorkloadCluster{
		Name: "wc1", Namespace: testNamespace,
		Status: benchv1alpha1.WorkloadClusterStatus{PrometheusURL: wcProm.URL},
	}
	tc := sampleTestCase("login-archived", "topo-gone", benchv1alpha1.ProfileBurn)
	tc.Spec.Archived = true
	tc.Status.Deployments = []benchv1alpha1.TestCaseDeploymentStatus{{Name: "topo-gone", RunnerCluster: "wc1"}}

	// No management-cluster Prometheus, as on a deployment that only has
	// per-workload-cluster ones.
	s, _ := newServer(t, Config{}, wc, tc)
	rec := do(t, s, http.MethodGet, "/api/v1/metrics/compare?testids=login-archived", nil)
	requireStatus(t, rec, http.StatusOK)
	body := decodeBody[compareResponse](t, rec)
	if len(body.Results) != 1 || body.Results[0].Values["http_reqs"] != 7 {
		t.Fatalf("results = %+v, want topo-gone http_reqs=7 from wc1's Prometheus", body.Results)
	}
}
