package api

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
)

// costProm is a fake Prometheus for instant queries that records what it was
// asked. answer returns the one sample's value, or ok false for an empty
// result; a query containing failOn is answered with an error.
type costProm struct {
	*httptest.Server
	mu     sync.Mutex
	calls  []promCall
	failOn string
}

func newCostProm(t *testing.T, answer func(query string) (value string, ok bool)) *costProm {
	t.Helper()
	p := &costProm{}
	p.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query().Get("query")
		p.mu.Lock()
		p.calls = append(p.calls, promCall{Query: query, Time: r.URL.Query().Get("time")})
		failOn := p.failOn
		p.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if failOn != "" && strings.Contains(query, failOn) {
			_, _ = w.Write([]byte(`{"status":"error","error":"boom"}`))
			return
		}
		result := []any{}
		if value, ok := answer(query); ok {
			labels := map[string]string{}
			// The k6 summary queries group by target.
			if strings.Contains(query, "by (target)") {
				labels["target"] = "topo"
			}
			result = append(result, map[string]any{"metric": labels, "value": []any{1700000000, value}})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "success", "data": map[string]any{"resultType": "vector", "result": result},
		})
	}))
	t.Cleanup(p.Close)
	return p
}

// matching returns the recorded calls whose query contains substr.
func (p *costProm) matching(substr string) []promCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []promCall
	for _, c := range p.calls {
		if strings.Contains(c.Query, substr) {
			out = append(out, c)
		}
	}
	return out
}

// loadAnswers is a deployment that used 1080 server, 180 worker and 540
// postgres CPU seconds while k6 passed 18000 transactions: 60, 10 and 30
// CPU-ms each.
func loadAnswers(query string) (string, bool) {
	switch {
	case strings.Contains(query, "k6_iteration_outcomes_total") && strings.Contains(query, `outcome="ok"`) && strings.Contains(query, "increase"):
		return "18000", true
	case strings.Contains(query, "container_cpu_usage_seconds_total") && strings.Contains(query, "-server-"):
		return "1080", true
	case strings.Contains(query, "container_cpu_usage_seconds_total") && strings.Contains(query, "-worker-"):
		return "180", true
	case strings.Contains(query, "container_cpu_usage_seconds_total"):
		return "540", true
	case strings.Contains(query, "k6_iterations_total"):
		return "20", true
	}
	return "", false
}

// costFixture is one Burn TestCase driving the deployment "topo" on a
// workload cluster whose Prometheus is promURL, under load since `since`.
func costFixture(promURL string, since time.Time) (*benchv1alpha1.WorkloadCluster, *benchv1alpha1.AuthentikDeployment, *benchv1alpha1.TestCase) {
	wc := &benchv1alpha1.WorkloadCluster{}
	wc.Name = "wc1"
	wc.Namespace = testNamespace
	wc.Status.PrometheusURL = promURL
	deployment := sampleDeployment("topo")
	deployment.Spec.ClusterRef.Name = "wc1"
	tc := sampleTestCase("login-topo", "topo", benchv1alpha1.ProfileBurn)
	tc.Status.Deployments = []benchv1alpha1.TestCaseDeploymentStatus{{
		Name: "topo", Ready: true, Cluster: "wc1", Namespace: "ak-topo", Target: "topo",
		BurningSince: &metav1.Time{Time: since},
	}}
	return wc, deployment, tc
}

type costBody struct {
	Metrics []CompareMetric `json:"metrics"`
	Results []struct {
		Target string             `json:"target"`
		Values map[string]float64 `json:"values"`
	} `json:"results"`
	Errors map[string]string `json:"errors"`
}

var rangeSeconds = regexp.MustCompile(`\[(\d+)s\]`)

// stretchOf is the range, in seconds, an increase() query covers.
func stretchOf(t *testing.T, query string) int64 {
	t.Helper()
	m := rangeSeconds.FindStringSubmatch(query)
	if m == nil {
		t.Fatalf("query has no [Ns] range: %q", query)
	}
	n, _ := strconv.ParseInt(m[1], 10, 64)
	return n
}

// CPU per transaction is the CPU seconds the deployment's pods used divided
// by the transactions k6 passed, both over the same stretch.
func TestCompareReportsCPUPerTransaction(t *testing.T) {
	prom := newCostProm(t, loadAnswers)
	wc, deployment, tc := costFixture(prom.URL, time.Now().Add(-2*time.Hour))
	s, _ := newServer(t, Config{}, wc, deployment, tc)

	at := time.Now().Add(-time.Minute).Unix()
	rec := do(t, s, http.MethodGet, "/api/v1/metrics/compare?testids=login-topo&window=15m&cost=true&time="+strconv.FormatInt(at, 10), nil)
	requireStatus(t, rec, http.StatusOK)
	body := decodeBody[costBody](t, rec)

	if len(body.Results) != 1 || body.Results[0].Target != "topo" {
		t.Fatalf("results = %+v, want one row for topo", body.Results)
	}
	values := body.Results[0].Values
	if got := values["server_cpu_per_iteration"]; math.Abs(got-0.06) > 1e-9 {
		t.Errorf("server CPU per transaction = %v, want 1080 CPU-s / 18000 = 0.06", got)
	}
	if got := values["postgres_cpu_per_iteration"]; math.Abs(got-0.03) > 1e-9 {
		t.Errorf("postgres CPU per transaction = %v, want 540 CPU-s / 18000 = 0.03", got)
	}
	if got := values["worker_cpu_per_iteration"]; math.Abs(got-0.01) > 1e-9 {
		t.Errorf("worker CPU per transaction = %v, want 180 CPU-s / 18000 = 0.01", got)
	}

	derived := map[string]bool{}
	for _, m := range body.Metrics {
		if m.Derived {
			derived[m.Key] = true
			if m.Unit != unitCPUSeconds || m.Better != betterLower {
				t.Errorf("%s: unit %q better %q, want %s and lower", m.Key, m.Unit, m.Better, unitCPUSeconds)
			}
		}
	}
	if len(derived) != 3 || !derived["server_cpu_per_iteration"] || !derived["worker_cpu_per_iteration"] || !derived["postgres_cpu_per_iteration"] {
		t.Errorf("derived metrics = %v, want the three CPU costs", derived)
	}

	want := map[string]string{
		// Last sample minus first per container, not increase(): see costCPUQuery.
		"server": `sum(max_over_time(container_cpu_usage_seconds_total{namespace="ak-topo", pod=~"topo-authentik-server-.+", container!="", container!="POD"}[900s])` +
			` - min_over_time(container_cpu_usage_seconds_total{namespace="ak-topo", pod=~"topo-authentik-server-.+", container!="", container!="POD"}[900s]))`,
		"worker": `sum(max_over_time(container_cpu_usage_seconds_total{namespace="ak-topo", pod=~"topo-authentik-worker-.+", container!="", container!="POD"}[900s])` +
			` - min_over_time(container_cpu_usage_seconds_total{namespace="ak-topo", pod=~"topo-authentik-worker-.+", container!="", container!="POD"}[900s]))`,
		"postgres": `sum(max_over_time(container_cpu_usage_seconds_total{namespace="ak-topo", pod=~"(?:topo-postgresql|topo-postgres)-[0-9]+", container=~"postgres|postgresql"}[900s])` +
			` - min_over_time(container_cpu_usage_seconds_total{namespace="ak-topo", pod=~"(?:topo-postgresql|topo-postgres)-[0-9]+", container=~"postgres|postgresql"}[900s]))`,
		"k6": `sum(increase(k6_iteration_outcomes_total{testid="login-topo", target="topo", outcome="ok"}[900s]))`,
	}
	for name, query := range want {
		calls := prom.matching(query)
		if len(calls) != 1 {
			t.Errorf("%s query sent %d times, want once: %q\nall calls: %+v", name, len(calls), query, prom.calls)
			continue
		}
		// The same instant as the k6 summary, so both halves cover the same stretch.
		if calls[0].Time != strconv.FormatInt(at, 10) {
			t.Errorf("%s query evaluated at %q, want %d", name, calls[0].Time, at)
		}
	}
	// A pod replaced during the stretch still counts: no join on what exists now.
	if calls := prom.matching("container_cpu_usage_seconds_total"); len(calls) != 3 {
		t.Errorf("got %d CPU queries, want 3", len(calls))
	} else {
		for _, c := range calls {
			if strings.Contains(c.Query, "kube_pod_owner") {
				t.Errorf("CPU query joins on live pods: %q", c.Query)
			}
		}
	}
}

// Only on request: a caller that does not show the costs must not pay for
// their queries.
func TestCompareWithoutCostRunsNoCostQueries(t *testing.T) {
	prom := newCostProm(t, loadAnswers)
	wc, deployment, tc := costFixture(prom.URL, time.Now().Add(-2*time.Hour))
	s, _ := newServer(t, Config{}, wc, deployment, tc)

	rec := do(t, s, http.MethodGet, "/api/v1/metrics/compare?testids=login-topo&window=15m", nil)
	requireStatus(t, rec, http.StatusOK)
	body := decodeBody[costBody](t, rec)
	for _, m := range body.Metrics {
		if m.Derived {
			t.Errorf("derived metric %s listed without cost=true", m.Key)
		}
	}
	for key := range body.Results[0].Values {
		if strings.HasSuffix(key, "_per_iteration") {
			t.Errorf("%s reported without cost=true", key)
		}
	}
	if calls := prom.matching("container_cpu_usage_seconds_total"); len(calls) != 0 {
		t.Errorf("CPU queried without cost=true: %+v", calls)
	}
	if calls := prom.matching("increase(k6_iteration_outcomes_total"); len(calls) != 0 {
		t.Errorf("transactions counted without cost=true: %+v", calls)
	}
}

// A window longer than the burn has run covers only the burn: before it the
// deployment was installing, not serving the test.
func TestCompareCostCoversOnlyTheLoad(t *testing.T) {
	prom := newCostProm(t, loadAnswers)
	wc, deployment, tc := costFixture(prom.URL, time.Now().Add(-5*time.Minute))
	s, _ := newServer(t, Config{}, wc, deployment, tc)

	rec := do(t, s, http.MethodGet, "/api/v1/metrics/compare?testids=login-topo&window=6h&cost=true", nil)
	requireStatus(t, rec, http.StatusOK)
	for _, substr := range []string{"container_cpu_usage_seconds_total", "increase(k6_iteration_outcomes_total"} {
		calls := prom.matching(substr)
		if len(calls) == 0 {
			t.Fatalf("no %s query", substr)
		}
		for _, c := range calls {
			if got := stretchOf(t, c.Query); got < 295 || got > 305 {
				t.Errorf("%s covers %ds, want the 5 minutes under load: %q", substr, got, c.Query)
			}
		}
	}
}

// A burn that was stopped is costed up to when it stopped: after that the
// deployment idles.
func TestCompareCostEndsWhenTheBurnStopped(t *testing.T) {
	prom := newCostProm(t, loadAnswers)
	started, stopped := time.Now().Add(-40*time.Minute), time.Now().Add(-10*time.Minute).Truncate(time.Second)
	wc, deployment, tc := costFixture(prom.URL, started)
	tc.Status.Deployments[0].BurnStoppedAt = &metav1.Time{Time: stopped}
	s, _ := newServer(t, Config{}, wc, deployment, tc)

	rec := do(t, s, http.MethodGet, "/api/v1/metrics/compare?testids=login-topo&window=1h&cost=true", nil)
	requireStatus(t, rec, http.StatusOK)
	calls := prom.matching("container_cpu_usage_seconds_total")
	if len(calls) != 3 {
		t.Fatalf("got %d CPU queries, want 3", len(calls))
	}
	for _, c := range append(calls, prom.matching("increase(k6_iteration_outcomes_total")...) {
		if got := stretchOf(t, c.Query); got < 1795 || got > 1805 {
			t.Errorf("stretch is %ds, want the 30 minutes from start to stop: %q", got, c.Query)
		}
		if c.Time != strconv.FormatInt(stopped.Unix(), 10) {
			t.Errorf("evaluated at %q, want when the burn stopped (%d)", c.Time, stopped.Unix())
		}
	}
}

// Too few transactions to divide by: no cost, and no CPU query either.
func TestCompareOmitsCostWithFewTransactions(t *testing.T) {
	prom := newCostProm(t, func(query string) (string, bool) {
		if strings.Contains(query, "increase(k6_iteration_outcomes_total") {
			return "12", true
		}
		return loadAnswers(query)
	})
	wc, deployment, tc := costFixture(prom.URL, time.Now().Add(-2*time.Hour))
	s, _ := newServer(t, Config{}, wc, deployment, tc)

	rec := do(t, s, http.MethodGet, "/api/v1/metrics/compare?testids=login-topo&cost=true", nil)
	requireStatus(t, rec, http.StatusOK)
	body := decodeBody[costBody](t, rec)
	for key := range body.Results[0].Values {
		if strings.HasSuffix(key, "_per_iteration") {
			t.Errorf("%s reported from 12 transactions: %v", key, body.Results[0].Values)
		}
	}
	if calls := prom.matching("container_cpu_usage_seconds_total"); len(calls) != 0 {
		t.Errorf("CPU queried for a target with too few transactions: %+v", calls)
	}
}

// A test that has not started its load has no stretch to cost.
func TestCompareOmitsCostBeforeTheLoadStarts(t *testing.T) {
	prom := newCostProm(t, loadAnswers)
	wc, deployment, tc := costFixture(prom.URL, time.Now().Add(2*time.Minute))
	s, _ := newServer(t, Config{}, wc, deployment, tc)

	rec := do(t, s, http.MethodGet, "/api/v1/metrics/compare?testids=login-topo&cost=true", nil)
	requireStatus(t, rec, http.StatusOK)
	if calls := prom.matching("container_cpu_usage_seconds_total"); len(calls) != 0 {
		t.Errorf("CPU queried before the start gate opened: %+v", calls)
	}
}

// No postgres pods (an external postgres): the server's cost only.
func TestCompareSkipsPostgresCostWithoutPostgresPods(t *testing.T) {
	prom := newCostProm(t, func(query string) (string, bool) {
		if strings.Contains(query, "container_cpu_usage_seconds_total") && !strings.Contains(query, "-authentik-") {
			return "", false
		}
		return loadAnswers(query)
	})
	wc, deployment, tc := costFixture(prom.URL, time.Now().Add(-2*time.Hour))
	deployment.Spec.Postgres.Mode = benchv1alpha1.PostgresExternal
	s, _ := newServer(t, Config{}, wc, deployment, tc)

	rec := do(t, s, http.MethodGet, "/api/v1/metrics/compare?testids=login-topo&cost=true", nil)
	requireStatus(t, rec, http.StatusOK)
	values := decodeBody[costBody](t, rec).Results[0].Values
	if got := values["server_cpu_per_iteration"]; math.Abs(got-0.06) > 1e-9 {
		t.Errorf("server CPU per transaction = %v, want 0.06", got)
	}
	if _, present := values["postgres_cpu_per_iteration"]; present {
		t.Errorf("postgres cost reported without postgres pods: %v", values)
	}
}

// A CPU query that fails costs the comparison nothing but that figure, and
// says so.
func TestCompareCostFailureKeepsTheComparison(t *testing.T) {
	prom := newCostProm(t, loadAnswers)
	prom.failOn = "container_cpu_usage_seconds_total"
	wc, deployment, tc := costFixture(prom.URL, time.Now().Add(-2*time.Hour))
	s, _ := newServer(t, Config{}, wc, deployment, tc)

	rec := do(t, s, http.MethodGet, "/api/v1/metrics/compare?testids=login-topo&cost=true", nil)
	requireStatus(t, rec, http.StatusOK)
	body := decodeBody[costBody](t, rec)
	if got := body.Results[0].Values["iterations"]; got != 20 {
		t.Errorf("transactions/s = %v, want the k6 value kept", got)
	}
	if _, present := body.Results[0].Values["server_cpu_per_iteration"]; present {
		t.Errorf("a cost reported although its query failed: %v", body.Results[0].Values)
	}
	if msg := body.Errors["login-topo"]; !strings.Contains(msg, "CPU per transaction") {
		t.Errorf("errors = %v, want one naming CPU per transaction", body.Errors)
	}
}

// k6 on one cluster, the deployment on another: transactions come from the
// runner's Prometheus, CPU from the deployment's.
func TestCompareCostAcrossClusters(t *testing.T) {
	k6Prom := newCostProm(t, loadAnswers)
	cpuProm := newCostProm(t, loadAnswers)
	wc, deployment, tc := costFixture(cpuProm.URL, time.Now().Add(-2*time.Hour))
	runner := &benchv1alpha1.WorkloadCluster{}
	runner.Name = "runner"
	runner.Namespace = testNamespace
	runner.Status.PrometheusURL = k6Prom.URL
	tc.Spec.RunnerClusterRef = &corev1.LocalObjectReference{Name: "runner"}
	s, _ := newServer(t, Config{}, wc, runner, deployment, tc)

	rec := do(t, s, http.MethodGet, "/api/v1/metrics/compare?testids=login-topo&cost=true", nil)
	requireStatus(t, rec, http.StatusOK)
	values := decodeBody[costBody](t, rec).Results[0].Values
	if got := values["server_cpu_per_iteration"]; math.Abs(got-0.06) > 1e-9 {
		t.Errorf("server CPU per transaction = %v, want 0.06", got)
	}
	if calls := k6Prom.matching("container_cpu_usage_seconds_total"); len(calls) != 0 {
		t.Errorf("CPU asked of the runner's Prometheus: %+v", calls)
	}
	if calls := cpuProm.matching("k6_"); len(calls) != 0 {
		t.Errorf("k6 series asked of the deployment's Prometheus: %+v", calls)
	}
	if calls := k6Prom.matching("increase(k6_iteration_outcomes_total"); len(calls) != 1 {
		t.Errorf("transactions counted %d times on the runner's Prometheus, want once", len(calls))
	}
}

// A derived metric has no series of its own to chart.
func TestMetricsRangeRejectsDerivedMetric(t *testing.T) {
	s, _ := newServer(t, Config{PrometheusURL: "http://prometheus.invalid"})
	rec := do(t, s, http.MethodGet,
		"/api/v1/metrics/range?testids=login&metric=server_cpu_per_iteration&start=1700000000&end=1700000900", nil)
	requireStatus(t, rec, http.StatusBadRequest)
}

// A Quick test is costed over its runs: from the run starting to the run
// completing, not on into the idle minute the UI anchors its window past.
func TestCompareCostOfAQuickRun(t *testing.T) {
	prom := newCostProm(t, loadAnswers)
	wc, deployment, tc := costFixture(prom.URL, time.Time{})
	tc.Spec.Profile = benchv1alpha1.ProfileQuick
	tc.Status.Deployments[0].BurningSince = nil
	started := time.Now().Add(-10 * time.Minute).Truncate(time.Second)
	completed := started.Add(150 * time.Second)
	run := &benchv1alpha1.TestRun{}
	run.Name, run.Namespace = "login-topo-run", testNamespace
	run.Spec.TestCaseRef = corev1.LocalObjectReference{Name: "login-topo"}
	run.Spec.DeploymentRef = &corev1.LocalObjectReference{Name: "topo"}
	run.Status.StartTime, run.Status.CompletionTime = &metav1.Time{Time: started}, &metav1.Time{Time: completed}
	s, _ := newServer(t, Config{}, wc, deployment, tc, run)

	at := completed.Add(time.Minute).Unix()
	rec := do(t, s, http.MethodGet, "/api/v1/metrics/compare?testids=login-topo&window=5m&cost=true&time="+strconv.FormatInt(at, 10), nil)
	requireStatus(t, rec, http.StatusOK)
	if got := decodeBody[costBody](t, rec).Results[0].Values["server_cpu_per_iteration"]; math.Abs(got-0.06) > 1e-9 {
		t.Errorf("server CPU per transaction = %v, want 0.06", got)
	}
	calls := append(prom.matching("container_cpu_usage_seconds_total"), prom.matching("increase(k6_iteration_outcomes_total")...)
	if len(calls) != 4 {
		t.Fatalf("got %d cost queries, want 3 CPU and 1 k6: %+v", len(calls), calls)
	}
	for _, c := range calls {
		if got := stretchOf(t, c.Query); got != 150 {
			t.Errorf("stretch is %ds, want the run's 150s: %q", got, c.Query)
		}
		if c.Time != strconv.FormatInt(completed.Unix(), 10) {
			t.Errorf("evaluated at %q, want when the run completed (%d)", c.Time, completed.Unix())
		}
	}
}

// A long stretch counts its transactions from the recording rules, not from
// a counter k6 pushes every second.
func TestCompareCostOfALongStretchUsesRecordingRules(t *testing.T) {
	prom := newCostProm(t, func(query string) (string, bool) {
		if strings.Contains(query, "testid_target:k6_iteration_outcomes:rate1m") {
			return "36000", true
		}
		return loadAnswers(query)
	})
	wc, deployment, tc := costFixture(prom.URL, time.Now().Add(-48*time.Hour))
	s, _ := newServer(t, Config{}, wc, deployment, tc)

	rec := do(t, s, http.MethodGet, "/api/v1/metrics/compare?testids=login-topo&window=6h&cost=true", nil)
	requireStatus(t, rec, http.StatusOK)
	if got := decodeBody[costBody](t, rec).Results[0].Values["server_cpu_per_iteration"]; math.Abs(got-0.03) > 1e-9 {
		t.Errorf("server CPU per transaction = %v, want 1080 / 36000 = 0.03", got)
	}
	want := `60 * (sum(sum_over_time(testid_target:k6_iteration_outcomes:rate1m{testid="login-topo", target="topo"}[21600s]))` +
		` - (sum(sum_over_time(testid_target:k6_iteration_outcomes_failed:rate1m{testid="login-topo", target="topo"}[21600s])) or vector(0)))`
	if calls := prom.matching(want); len(calls) != 1 {
		t.Errorf("recorded transactions query sent %d times, want once: %q", len(calls), want)
	}
	if calls := prom.matching("increase(k6_iteration_outcomes_total"); len(calls) != 0 {
		t.Errorf("the raw counter was read although the recording rules answered: %+v", calls)
	}
}

// Without the rules (the management cluster's Prometheus, or a test older
// than them) the raw counter answers.
func TestCompareCostOfALongStretchFallsBackToTheCounter(t *testing.T) {
	prom := newCostProm(t, loadAnswers)
	wc, deployment, tc := costFixture(prom.URL, time.Now().Add(-48*time.Hour))
	s, _ := newServer(t, Config{}, wc, deployment, tc)

	rec := do(t, s, http.MethodGet, "/api/v1/metrics/compare?testids=login-topo&window=6h&cost=true", nil)
	requireStatus(t, rec, http.StatusOK)
	if got := decodeBody[costBody](t, rec).Results[0].Values["server_cpu_per_iteration"]; math.Abs(got-0.06) > 1e-9 {
		t.Errorf("server CPU per transaction = %v, want 0.06 from the raw counter", got)
	}
	if calls := prom.matching(`increase(k6_iteration_outcomes_total{testid="login-topo", target="topo", outcome="ok"}[21600s])`); len(calls) != 1 {
		t.Errorf("raw counter read %d times, want once", len(calls))
	}
}

// One of a test's k6 Prometheus instances being down must not cost a target
// whose samples are on another its figure.
func TestSumInstantSkipsAnUnreachablePrometheus(t *testing.T) {
	up := newCostProm(t, func(string) (string, bool) { return "18000", true })
	down := newCostProm(t, func(string) (string, bool) { return "", false })
	down.failOn = "k6_"
	s, _ := newServer(t, Config{})

	got, err := s.sumInstant(t.Context(), []string{down.URL, up.URL}, "sum(k6_x)", "")
	if err != nil || got != 18000 {
		t.Errorf("sumInstant = %v, %v; want 18000 from the instance that answered", got, err)
	}
	if _, err := s.sumInstant(t.Context(), []string{down.URL}, "sum(k6_x)", ""); err == nil {
		t.Error("no error although no Prometheus answered")
	}
}

// The time anchor is checked whether or not costs are asked for.
func TestCompareRejectsBadTime(t *testing.T) {
	s, _ := newServer(t, Config{PrometheusURL: "http://prometheus.invalid"})
	for _, query := range []string{"time=1700000000.5", "time=2026-11-27T00:00:00Z", "time=1700000000.5&cost=true"} {
		rec := do(t, s, http.MethodGet, "/api/v1/metrics/compare?testids=login&"+query, nil)
		requireStatus(t, rec, http.StatusBadRequest)
	}
}
