package api

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
)

// restartsAt is the time anchor the restarts tests pass, so the window a
// load start is clamped against is fixed.
const restartsAt = 1800000000

// promCall is one query a fake Prometheus received.
type promCall struct {
	Query string
	Time  string
}

// fakePrometheusContainers serves an instant query's result with one sample
// per container, the shape restartsQuery's `sum by (container)` returns, and
// records the queries it was asked. A nil map from handler answers with an
// error.
func fakePrometheusContainers(t *testing.T, handler func(query string) map[string]string) (*httptest.Server, func() []promCall) {
	t.Helper()
	var mu sync.Mutex
	var calls []promCall
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query().Get("query")
		mu.Lock()
		calls = append(calls, promCall{Query: query, Time: r.URL.Query().Get("time")})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		samples := handler(query)
		if samples == nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "error", "error": "unavailable"})
			return
		}
		result := make([]any, 0, len(samples))
		for container, value := range samples {
			result = append(result, map[string]any{
				"metric": map[string]string{"container": container},
				"value":  []any{1700000000, value},
			})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "success",
			"data":   map[string]any{"resultType": "vector", "result": result},
		})
	}))
	t.Cleanup(srv.Close)
	return srv, func() []promCall {
		mu.Lock()
		defer mu.Unlock()
		return append([]promCall(nil), calls...)
	}
}

func noRestarts(string) map[string]string { return map[string]string{} }

func restartsCluster(name, promURL string) *benchv1alpha1.WorkloadCluster {
	wc := &benchv1alpha1.WorkloadCluster{}
	wc.Name = name
	wc.Namespace = testNamespace
	wc.Status.PrometheusURL = promURL
	return wc
}

func restartsDeployment(name, cluster string) *benchv1alpha1.AuthentikDeployment {
	d := sampleDeployment(name)
	d.Spec.ClusterRef.Name = cluster
	return d
}

// burnTestCase is a Burn TestCase driving deployments, each burning since
// the given number of seconds before restartsAt (or not yet, for a negative
// value).
func burnTestCase(name string, burningFor map[string]int) *benchv1alpha1.TestCase {
	tc := sampleTestCase(name, "", benchv1alpha1.ProfileBurn)
	for _, deployment := range slices.Sorted(maps.Keys(burningFor)) {
		tc.Spec.DeploymentRefs = append(tc.Spec.DeploymentRefs, corev1.LocalObjectReference{Name: deployment})
		st := benchv1alpha1.TestCaseDeploymentStatus{Name: deployment}
		if seconds := burningFor[deployment]; seconds >= 0 {
			since := metav1.NewTime(time.Unix(restartsAt-int64(seconds), 0))
			st.BurningSince = &since
		}
		tc.Status.Deployments = append(tc.Status.Deployments, st)
	}
	return tc
}

func restartsQueries(namespace string, seconds int) []string {
	return []string{
		fmt.Sprintf(restartsQuery, namespace, fmt.Sprintf("%ds", seconds)),
		fmt.Sprintf(startedPodsQuery, namespace, fmt.Sprintf("%ds", seconds), seconds),
	}
}

func queriesOf(calls []promCall) []string {
	out := make([]string, len(calls))
	for i, c := range calls {
		out[i] = c.Query
	}
	return out
}

func getRestarts(t *testing.T, s *Server, query string, want int) restartsResponse {
	t.Helper()
	rec := do(t, s, http.MethodGet, "/api/v1/metrics/restarts?"+query, nil)
	requireStatus(t, rec, want)
	if want != http.StatusOK {
		return restartsResponse{}
	}
	return decodeBody[restartsResponse](t, rec)
}

// Restarts are counted in the deployment's own namespace over the requested
// window, at the requested time, rounded to whole restarts, and containers
// that stayed up are left out.
func TestListRestarts(t *testing.T) {
	prom, calls := fakePrometheusContainers(t, func(query string) map[string]string {
		if strings.Contains(query, "kube_pod_start_time") {
			return map[string]string{"worker": "2"}
		}
		return map[string]string{"server": "3.02", "worker": "0", "postgresql": "0.4"}
	})
	tc := burnTestCase("login-topo", map[string]int{"topo": 3600})
	s, _ := newServer(t, Config{}, restartsCluster("wc1", prom.URL), restartsDeployment("topo", "wc1"), tc)

	body := getRestarts(t, s, fmt.Sprintf("testids=login-topo&window=15m&time=%d", restartsAt), http.StatusOK)
	if body.Window != "15m" {
		t.Errorf("window = %q, want 15m", body.Window)
	}
	if len(body.Targets) != 1 {
		t.Fatalf("got %d targets, want 1: %+v", len(body.Targets), body.Targets)
	}
	if got := body.Targets[0]; got.TestID != "login-topo" || got.Target != "topo" || !reflect.DeepEqual(got.Restarts, map[string]int{"server": 3}) {
		t.Errorf("target = %+v, want topo of login-topo with 3 server restarts only", got)
	}
	if got := body.Targets[0].Started; !reflect.DeepEqual(got, map[string]int{"worker": 2}) {
		t.Errorf("started = %+v, want 2 new worker pods", got)
	}
	if len(body.Errors) != 0 {
		t.Errorf("errors = %+v, want none", body.Errors)
	}
	want := []string{
		`sum by (container) (increase(kube_pod_container_status_restarts_total{namespace="ak-topo"}[900s])` +
			` unless on (pod, uid) max_over_time(kube_pod_owner{namespace="ak-topo", owner_kind="Job"}[900s]))`,
		`count by (container) (group by (container, pod, uid) (max_over_time(kube_pod_container_info{namespace="ak-topo"}[900s]))` +
			` and on (pod, uid) (max_over_time(kube_pod_start_time{namespace="ak-topo"}[900s]) > time() - 900)` +
			` unless on (pod, uid) max_over_time(kube_pod_owner{namespace="ak-topo", owner_kind="Job"}[900s]))`,
	}
	got := calls()
	if !reflect.DeepEqual(queriesOf(got), want) {
		t.Errorf("queries = %q, want %q", queriesOf(got), want)
	}
	for _, c := range got {
		if c.Time != fmt.Sprint(restartsAt) {
			t.Errorf("query at time %q, want %d", c.Time, restartsAt)
		}
	}
}

// A Burn target is only checked from when it started burning - each target
// from its own start, so one added later is checked over less - and one that
// has not started by the end of the window is left out: its rollout's first
// pods and start-up crashes are not in the k6 numbers.
func TestListRestartsClampsBurnToLoadStart(t *testing.T) {
	prom, calls := fakePrometheusContainers(t, noRestarts)
	tc := burnTestCase("login-topo", map[string]int{"early": 3600, "late": 120, "waiting": -1})
	future := metav1.NewTime(time.Unix(restartsAt+60, 0))
	scheduled := burnTestCase("login-scheduled", map[string]int{"early": 3600})
	scheduled.Status.Deployments[0].BurningSince = &future
	s, _ := newServer(t, Config{}, restartsCluster("wc1", prom.URL),
		restartsDeployment("early", "wc1"), restartsDeployment("late", "wc1"), restartsDeployment("waiting", "wc1"),
		tc, scheduled)

	body := getRestarts(t, s, fmt.Sprintf("testids=login-topo,login-scheduled&window=5m&time=%d", restartsAt), http.StatusOK)
	targets := make([]string, 0, len(body.Targets))
	for _, target := range body.Targets {
		targets = append(targets, target.TestID+"/"+target.Target)
	}
	if want := []string{"login-topo/early", "login-topo/late"}; !reflect.DeepEqual(targets, want) {
		t.Errorf("targets = %q, want %q", targets, want)
	}
	if len(body.Errors) != 0 {
		t.Errorf("errors = %+v, want none", body.Errors)
	}
	want := append(restartsQueries("ak-early", 300), restartsQueries("ak-late", 120)...)
	if got := queriesOf(calls()); !reflect.DeepEqual(got, want) {
		t.Errorf("queries = %q, want %q", got, want)
	}
}

// A Quick target is only checked from the start of the earliest of its own
// runs that overlaps the window; a target without such a run is left out.
func TestListRestartsClampsQuickToRunStart(t *testing.T) {
	prom, calls := fakePrometheusContainers(t, noRestarts)
	tc := sampleTestCase("login-quick", "", benchv1alpha1.ProfileQuick)
	tc.Spec.DeploymentRefs = []corev1.LocalObjectReference{{Name: "first"}, {Name: "second"}, {Name: "idle"}}

	run := func(name, deployment string, startAgo, doneAgo int) *benchv1alpha1.TestRun {
		r := &benchv1alpha1.TestRun{}
		r.Name = name
		r.Namespace = testNamespace
		r.Spec.TestCaseRef.Name = "login-quick"
		if deployment != "" {
			r.Spec.DeploymentRef = &corev1.LocalObjectReference{Name: deployment}
		}
		start := metav1.NewTime(time.Unix(restartsAt-int64(startAgo), 0))
		r.Status.StartTime = &start
		if doneAgo >= 0 {
			done := metav1.NewTime(time.Unix(restartsAt-int64(doneAgo), 0))
			r.Status.CompletionTime = &done
		}
		return r
	}
	objects := []client.Object{
		restartsCluster("wc1", prom.URL), tc,
		restartsDeployment("first", "wc1"), restartsDeployment("second", "wc1"), restartsDeployment("idle", "wc1"),
		// Ended before the window: not part of its numbers.
		run("old", "first", 3600, 3450),
		// No deploymentRef: ran against the first deployment.
		run("legacy", "", 200, 60),
		// The run the window is anchored to.
		run("latest", "second", 150, 0),
		// Started after the window ended.
		run("next", "idle", -30, -1),
		// Another test case's run against the same deployment.
		func() *benchv1alpha1.TestRun {
			r := run("other", "idle", 250, 100)
			r.Spec.TestCaseRef.Name = "login-other"
			return r
		}(),
	}
	s, _ := newServer(t, Config{}, objects...)

	body := getRestarts(t, s, fmt.Sprintf("testids=login-quick&window=5m&time=%d", restartsAt), http.StatusOK)
	targets := make([]string, 0, len(body.Targets))
	for _, target := range body.Targets {
		targets = append(targets, target.Target)
	}
	if want := []string{"first", "second"}; !reflect.DeepEqual(targets, want) {
		t.Errorf("targets = %q, want %q", targets, want)
	}
	want := append(restartsQueries("ak-first", 200), restartsQueries("ak-second", 150)...)
	if got := queriesOf(calls()); !reflect.DeepEqual(got, want) {
		t.Errorf("queries = %q, want %q", got, want)
	}
}

// One target's Prometheus failing is reported for its testid without
// costing the other targets theirs.
func TestListRestartsReportsAFailingTarget(t *testing.T) {
	up, _ := fakePrometheusContainers(t, func(string) map[string]string { return map[string]string{"server": "1"} })
	down, _ := fakePrometheusContainers(t, func(string) map[string]string { return nil })
	tc := burnTestCase("login-topo", map[string]int{"a": 3600, "b": 3600})
	s, _ := newServer(t, Config{},
		restartsCluster("wc-up", up.URL), restartsCluster("wc-down", down.URL),
		restartsDeployment("a", "wc-up"), restartsDeployment("b", "wc-down"), tc)

	body := getRestarts(t, s, "testids=login-topo&time="+fmt.Sprint(restartsAt), http.StatusOK)
	if len(body.Targets) != 1 || body.Targets[0].Target != "a" {
		t.Errorf("targets = %+v, want a only", body.Targets)
	}
	if !strings.Contains(body.Errors["login-topo"], down.URL) {
		t.Errorf("errors = %+v, want login-topo's failing Prometheus reported", body.Errors)
	}
}

// When every query fails the request does too.
func TestListRestartsFailsWhenPrometheusDoes(t *testing.T) {
	down, _ := fakePrometheusContainers(t, func(string) map[string]string { return nil })
	tc := burnTestCase("login-topo", map[string]int{"a": 3600})
	s, _ := newServer(t, Config{}, restartsCluster("wc1", down.URL), restartsDeployment("a", "wc1"), tc)
	getRestarts(t, s, "testids=login-topo&time="+fmt.Sprint(restartsAt), http.StatusBadGateway)
}

// A deployment that cannot be checked and a test case that does not exist
// are reported, by name, so neither reads the same as one that stayed up;
// with nothing to ask Prometheus, the request still succeeds.
func TestListRestartsReportsWhatItCouldNotCheck(t *testing.T) {
	prom, _ := fakePrometheusContainers(t, noRestarts)
	tc := burnTestCase("login-topo", map[string]int{"topo": 3600, "gone": 3600})
	onlyGone := burnTestCase("login-gone", map[string]int{"gone": 3600})
	s, _ := newServer(t, Config{}, restartsCluster("wc1", prom.URL), restartsDeployment("topo", "wc1"), tc, onlyGone)

	body := getRestarts(t, s, "testids=login-topo,login-missing&time="+fmt.Sprint(restartsAt), http.StatusOK)
	if len(body.Targets) != 1 || body.Targets[0].TestID != "login-topo" || body.Targets[0].Target != "topo" {
		t.Errorf("targets = %+v, want topo of login-topo only", body.Targets)
	}
	if got := body.Errors["login-topo"]; !strings.Contains(got, `"gone"`) || strings.Contains(got, `"topo"`) {
		t.Errorf("login-topo error = %q, want only the gone deployment named", got)
	}
	if got := body.Errors["login-missing"]; got != "test case not found" {
		t.Errorf("login-missing error = %q, want test case not found", got)
	}

	body = getRestarts(t, s, "testids=login-gone,login-missing&time="+fmt.Sprint(restartsAt), http.StatusOK)
	if len(body.Targets) != 0 || len(body.Errors) != 2 {
		t.Errorf("response = %+v, want no targets and both testids reported", body)
	}
}

func TestListRestartsRejectsBadInput(t *testing.T) {
	s, _ := newServer(t, Config{})
	for _, target := range []string{
		"/api/v1/metrics/restarts",
		"/api/v1/metrics/restarts?testids=" + url.QueryEscape(`a"} or up{`),
		"/api/v1/metrics/restarts?testids=a&window=5m])+or+up",
		"/api/v1/metrics/restarts?testids=a&time=now",
	} {
		rec := do(t, s, http.MethodGet, target, nil)
		requireStatus(t, rec, http.StatusBadRequest)
	}
}
