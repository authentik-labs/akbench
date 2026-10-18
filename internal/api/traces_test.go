package api

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	corev1 "k8s.io/api/core/v1"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
	"github.com/authentik-labs/akbench/internal/render"
)

// A Tempo search response shaped like authentik's real traces: per trace,
// two server spans (1s and 3s), one stage span, and three queries.
const fakeTempoSearch = `{"traces":[{"spanSets":[{"spans":[
 {"durationNanos":"1000000000","attributes":[{"key":"kind","value":{"stringValue":"server"}}]},
 {"durationNanos":"3000000000","attributes":[{"key":"kind","value":{"stringValue":"server"}}]},
 {"durationNanos":"500000000","attributes":[{"key":"kind","value":{"stringValue":"internal"}},{"key":"name","value":{"stringValue":"authentik.stages.password.stage.PasswordStageView"}}]},
 {"durationNanos":"200000000","attributes":[{"key":"kind","value":{"stringValue":"client"}},{"key":"db.statement","value":{"stringValue":"SELECT 1"}}]},
 {"durationNanos":"200000000","attributes":[{"key":"kind","value":{"stringValue":"client"}},{"key":"db.statement","value":{"stringValue":"SELECT 1"}}]},
 {"durationNanos":"600000000","attributes":[{"key":"kind","value":{"stringValue":"client"}},{"key":"db.statement","value":{"stringValue":"UPDATE session"}}]}
]}]}]}`

func approx(t *testing.T, name string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("%s = %v, want %v", name, got, want)
	}
}

func TestTraceBreakdown(t *testing.T) {
	var searches atomic.Int32
	var query string
	grafana := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/grafana-proxy/bench/api/datasources/proxy/uid/tempo/api/search" {
			http.NotFound(w, r)
			return
		}
		if user, pass, ok := r.BasicAuth(); !ok || user != render.GrafanaAdminUser || pass != grafanaTestAdminPassword {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		searches.Add(1)
		query = r.URL.Query().Get("q")
		_, _ = fmt.Fprint(w, fakeTempoSearch)
	}))
	t.Cleanup(grafana.Close)

	wc := &benchv1alpha1.WorkloadCluster{Name: "bench", Namespace: testNamespace}
	wc.Status.GrafanaURL = grafana.URL
	adminSecret := &corev1.Secret{
		Name: render.GrafanaAdminSecretName(wc), Namespace: testNamespace,
		Data: map[string][]byte{"password": []byte(grafanaTestAdminPassword)},
	}
	onCluster := &benchv1alpha1.AuthentikDeployment{Name: "ak-a", Namespace: testNamespace}
	onCluster.Spec.ClusterRef = corev1.LocalObjectReference{Name: "bench"}
	onCluster.Spec.TargetName = "a"
	onManagement := &benchv1alpha1.AuthentikDeployment{Name: "ak-b", Namespace: testNamespace}
	tc := &benchv1alpha1.TestCase{Name: "login", Namespace: testNamespace}
	tc.Spec.DeploymentRefs = []corev1.LocalObjectReference{{Name: "ak-a"}, {Name: "ak-b"}}
	s, _ := newServer(t, Config{}, wc, adminSecret, onCluster, onManagement, tc)

	rec := do(t, s, http.MethodGet, "/api/v1/traces/breakdown?testid=login&window=1h", nil)
	requireStatus(t, rec, http.StatusOK)
	got := decodeBody[TraceBreakdown](t, rec)
	if len(got.Targets) != 2 {
		t.Fatalf("targets = %d, want 2", len(got.Targets))
	}
	if !strings.Contains(query, `x_benchmark_test = "login"`) || !strings.Contains(query, `x_benchmark_target = "a"`) {
		t.Errorf("query not scoped to the test and target: %s", query)
	}

	a := got.Targets[0]
	if a.Error != "" || a.Target != "a" || a.Traces != 1 || a.Requests != 2 {
		t.Fatalf("target a: %+v", a)
	}
	approx(t, "requestP50", a.RequestP50, 1)
	approx(t, "requestP95", a.RequestP95, 3)
	approx(t, "queriesPerRequest", a.QueriesPerRequest, 1.5)
	approx(t, "dbTimePerRequest", a.DBTimePerRequest, 0.5)
	approx(t, "dbShare", a.DBShare, 0.25)
	if len(a.Stages) != 1 || a.Stages[0].Name != "PasswordStageView" {
		t.Errorf("stages: %+v", a.Stages)
	}
	if len(a.TopQueries) != 2 || a.TopQueries[0].Statement != "UPDATE session" || a.TopQueries[1].Count != 2 {
		t.Errorf("top queries not ranked by total time: %+v", a.TopQueries)
	}

	if b := got.Targets[1]; b.Target != "ak-b" || b.Error == "" {
		t.Errorf("a management-cluster target should report why it has no traces: %+v", b)
	}

	// A second request inside the TTL is served from the cache.
	requireStatus(t, do(t, s, http.MethodGet, "/api/v1/traces/breakdown?testid=login&window=1h", nil), http.StatusOK)
	if n := searches.Load(); n != 1 {
		t.Errorf("Tempo searched %d times, want 1 (cached)", n)
	}
}

// traceFixture is one TestCase whose single target's traces live behind
// grafana.
func traceFixture(t *testing.T, grafana http.Handler) *Server {
	t.Helper()
	srv := httptest.NewServer(grafana)
	t.Cleanup(srv.Close)
	wc := &benchv1alpha1.WorkloadCluster{Name: "bench", Namespace: testNamespace}
	wc.Status.GrafanaURL = srv.URL
	adminSecret := &corev1.Secret{
		Name: render.GrafanaAdminSecretName(wc), Namespace: testNamespace,
		Data: map[string][]byte{"password": []byte(grafanaTestAdminPassword)},
	}
	d := &benchv1alpha1.AuthentikDeployment{Name: "ak-a", Namespace: testNamespace}
	d.Spec.ClusterRef = corev1.LocalObjectReference{Name: "bench"}
	d.Spec.Namespace = "ak-a"
	tc := &benchv1alpha1.TestCase{Name: "login", Namespace: testNamespace}
	tc.Spec.DeploymentRefs = []corev1.LocalObjectReference{{Name: "ak-a"}}
	s, _ := newServer(t, Config{}, wc, adminSecret, d, tc)
	return s
}

func TestTraceBreakdownSurvivesCancelledRequest(t *testing.T) {
	var searches atomic.Int32
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	s := traceFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		searches.Add(1)
		started <- struct{}{}
		<-release
		_, _ = fmt.Fprint(w, fakeTempoSearch)
	}))

	// The browser goes away while Tempo is still searching...
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/api/v1/traces/breakdown?testid=login&window=1h", nil).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+testToken)
	done := make(chan struct{})
	go func() {
		s.Router().ServeHTTP(httptest.NewRecorder(), req)
		close(done)
	}()
	<-started
	cancel()
	<-done

	// ...the search still completes, and the next request gets its result
	// rather than a cached "context canceled".
	close(release)
	rec := do(t, s, http.MethodGet, "/api/v1/traces/breakdown?testid=login&window=1h", nil)
	requireStatus(t, rec, http.StatusOK)
	got := decodeBody[TraceBreakdown](t, rec)
	if len(got.Targets) != 1 || got.Targets[0].Error != "" || got.Targets[0].Requests != 2 {
		t.Fatalf("expected the completed search's result, got %+v", got.Targets)
	}
	if n := searches.Load(); n != 1 {
		t.Errorf("Tempo searched %d times, want 1 (shared with the cancelled request)", n)
	}
}

func TestTraceBreakdownDoesNotCacheFailures(t *testing.T) {
	var searches atomic.Int32
	s := traceFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if searches.Add(1) == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = fmt.Fprint(w, fakeTempoSearch)
	}))

	rec := do(t, s, http.MethodGet, "/api/v1/traces/breakdown?testid=login&window=1h", nil)
	requireStatus(t, rec, http.StatusOK)
	if got := decodeBody[TraceBreakdown](t, rec); got.Targets[0].Error == "" {
		t.Fatalf("expected the first search's error, got %+v", got.Targets[0])
	}
	rec = do(t, s, http.MethodGet, "/api/v1/traces/breakdown?testid=login&window=1h", nil)
	if got := decodeBody[TraceBreakdown](t, rec); got.Targets[0].Error != "" {
		t.Errorf("a failed search must not be cached: %+v", got.Targets[0])
	}
}

func TestTraceBreakdownValidatesWindow(t *testing.T) {
	s, _ := newServer(t, Config{})
	requireStatus(t, do(t, s, http.MethodGet, "/api/v1/traces/breakdown?testid=x&window=7d", nil), http.StatusBadRequest)
	requireStatus(t, do(t, s, http.MethodGet, "/api/v1/traces/breakdown", nil), http.StatusBadRequest)
}

// TestTraceBreakdownRejectsUnsafeTargetName is a regression test for a
// TraceQL-injection risk: spec.targetName is a free-form field (unlike the
// object name, which the apiserver already restricts to a DNS subdomain),
// and traceBreakdownQuery interpolates it directly into a TraceQL search.
// A value with TraceQL metacharacters must never reach Tempo's query API at
// all - it should surface as a per-target error instead.
func TestTraceBreakdownRejectsUnsafeTargetName(t *testing.T) {
	var searched atomic.Bool
	grafana := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		searched.Store(true)
		_, _ = fmt.Fprint(w, fakeTempoSearch)
	}))
	t.Cleanup(grafana.Close)

	wc := &benchv1alpha1.WorkloadCluster{Name: "bench", Namespace: testNamespace}
	wc.Status.GrafanaURL = grafana.URL
	malicious := &benchv1alpha1.AuthentikDeployment{Name: "ak-evil", Namespace: testNamespace}
	malicious.Spec.ClusterRef = corev1.LocalObjectReference{Name: "bench"}
	malicious.Spec.TargetName = `a" } || { true } //`
	tc := &benchv1alpha1.TestCase{Name: "login", Namespace: testNamespace}
	tc.Spec.DeploymentRefs = []corev1.LocalObjectReference{{Name: "ak-evil"}}
	s, _ := newServer(t, Config{}, wc, malicious, tc)

	rec := do(t, s, http.MethodGet, "/api/v1/traces/breakdown?testid=login&window=1h", nil)
	requireStatus(t, rec, http.StatusOK)
	got := decodeBody[TraceBreakdown](t, rec)
	if len(got.Targets) != 1 || got.Targets[0].Error == "" {
		t.Fatalf("expected one target with an error, got: %+v", got.Targets)
	}
	if searched.Load() {
		t.Error("Tempo was queried with an unvalidated target name")
	}
}
