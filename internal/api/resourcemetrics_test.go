package api

import (
	"net/http"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
)

// TestListResourceMetricsResolvesDeploymentNamespace guards the one thing
// this endpoint actually does differently from metricsRange: it queries a
// testid's own deployment namespace, not a testid label (cAdvisor never
// tags container metrics with a k6 testid), and sums across every pod
// rather than breaking the result out per pod - an earlier version did the
// latter and was confirmed live to be unreadable (a deployment mid-rollout
// leaves its old ReplicaSet generation's pods in the same window as the
// new one, turning one test into 6+ tangled lines).
func TestListResourceMetricsResolvesDeploymentNamespace(t *testing.T) {
	var gotQuery string
	prom := fakePrometheusRange(t, func(query string) [][2]any {
		gotQuery = query
		return [][2]any{{1700000000, "1.5"}, {1700000015, "1.75"}}
	})

	wc := &benchv1alpha1.WorkloadCluster{}
	wc.Name = "wc1"
	wc.Namespace = testNamespace
	wc.Status.PrometheusURL = prom.URL

	deployment := sampleDeployment("topo")
	deployment.Spec.ClusterRef.Name = "wc1"
	deployment.Spec.Namespace = "ak-topo"
	tc := sampleTestCase("login-topo", "topo", benchv1alpha1.ProfileBurn)

	s, _ := newServer(t, Config{}, wc, deployment, tc)
	rec := do(t, s, http.MethodGet, "/api/v1/metrics/resources?testids=login-topo&resource=cpu&start=1700000000&end=1700003600", nil)
	requireStatus(t, rec, http.StatusOK)

	body := decodeBody[struct {
		Metric CompareMetric `json:"metric"`
		Series []RangeSeries `json:"series"`
	}](t, rec)
	if body.Metric.Key != "cpu" || body.Metric.Unit != "cores" {
		t.Errorf("metric = %+v, want key=cpu unit=cores", body.Metric)
	}
	if len(body.Series) != 1 {
		t.Fatalf("got %d series, want 1 (one summed line, not per-pod): %+v", len(body.Series), body.Series)
	}
	if body.Series[0].TestID != "login-topo" {
		t.Errorf("series testid = %q, want login-topo", body.Series[0].TestID)
	}
	if len(body.Series[0].Points) != 2 {
		t.Fatalf("got %d points, want 2", len(body.Series[0].Points))
	}

	if strings.Contains(gotQuery, "%!") || strings.Contains(gotQuery, "MISSING") {
		t.Fatalf("query has an unsubstituted format verb: %q", gotQuery)
	}
	if !strings.Contains(gotQuery, `namespace="ak-topo"`) {
		t.Errorf("query doesn't scope to the deployment's own namespace: %q", gotQuery)
	}
	if !strings.Contains(gotQuery, "[1m]") {
		t.Errorf("cpu query missing its rate() window: %q", gotQuery)
	}
	if strings.Contains(gotQuery, "by (pod)") {
		t.Errorf("query still breaks the result out by pod, want a single summed series: %q", gotQuery)
	}
	if !strings.Contains(gotQuery, `kube_pod_owner{namespace="ak-topo", owner_kind!="Job"}`) {
		t.Errorf("query doesn't exclude one-shot Job pods (blueprint/postgres-role-fix): %q", gotQuery)
	}
}

func TestListResourceMetricsMemoryQueryHasNoWindow(t *testing.T) {
	var gotQuery string
	prom := fakePrometheusRange(t, func(query string) [][2]any {
		gotQuery = query
		return [][2]any{{1700000000, "536870912"}}
	})

	wc := &benchv1alpha1.WorkloadCluster{}
	wc.Name = "wc1"
	wc.Namespace = testNamespace
	wc.Status.PrometheusURL = prom.URL

	deployment := sampleDeployment("topo2")
	deployment.Spec.ClusterRef.Name = "wc1"
	deployment.Spec.Namespace = "ak-topo2"
	tc := sampleTestCase("login-topo2", "topo2", benchv1alpha1.ProfileBurn)

	s, _ := newServer(t, Config{}, wc, deployment, tc)
	rec := do(t, s, http.MethodGet, "/api/v1/metrics/resources?testids=login-topo2&resource=memory&start=1700000000&end=1700003600", nil)
	requireStatus(t, rec, http.StatusOK)

	if strings.Contains(gotQuery, "%!") || strings.Contains(gotQuery, "MISSING") {
		t.Fatalf("query has an unsubstituted format verb: %q", gotQuery)
	}
	if !strings.Contains(gotQuery, `namespace="ak-topo2"`) {
		t.Errorf("query doesn't scope to the deployment's own namespace: %q", gotQuery)
	}
}

// A TestCase driving more than one deployment must report one resource
// series per target, each scoped to that deployment's own namespace -
// mirrors TestCompareMultiDeploymentShowsOneRowPerTarget's guard for the k6
// metrics side.
func TestListResourceMetricsMultiDeployment(t *testing.T) {
	queriesByNamespace := map[string]string{}
	prom := fakePrometheusRange(t, func(query string) [][2]any {
		switch {
		case strings.Contains(query, `namespace="ak-dep-a"`):
			queriesByNamespace["ak-dep-a"] = query
			return [][2]any{{1700000000, "1"}}
		case strings.Contains(query, `namespace="ak-dep-b"`):
			queriesByNamespace["ak-dep-b"] = query
			return [][2]any{{1700000000, "2"}}
		default:
			return nil
		}
	})

	depA := sampleDeployment("dep-a")
	depA.Spec.Namespace = "ak-dep-a"
	depB := sampleDeployment("dep-b")
	depB.Spec.Namespace = "ak-dep-b"

	tc := sampleTestCase("login-multi", "", benchv1alpha1.ProfileBurn)
	tc.Spec.DeploymentRefs = []corev1.LocalObjectReference{{Name: "dep-a"}, {Name: "dep-b"}}

	s, _ := newServer(t, Config{PrometheusURL: prom.URL}, depA, depB, tc)
	rec := do(t, s, http.MethodGet, "/api/v1/metrics/resources?testids=login-multi&resource=cpu&start=1700000000&end=1700003600", nil)
	requireStatus(t, rec, http.StatusOK)

	body := decodeBody[struct {
		Series []RangeSeries `json:"series"`
	}](t, rec)
	if len(body.Series) != 2 {
		t.Fatalf("got %d series, want 2 (one per deployment): %+v", len(body.Series), body.Series)
	}
	targets := map[string]float64{}
	for _, series := range body.Series {
		if len(series.Points) != 1 {
			t.Fatalf("series %+v has %d points, want 1", series, len(series.Points))
		}
		targets[series.Target] = series.Points[0].Value
	}
	if targets["dep-a"] != 1 || targets["dep-b"] != 2 {
		t.Fatalf("series by target = %+v, want dep-a=1 dep-b=2", targets)
	}
	if queriesByNamespace["ak-dep-a"] == "" || queriesByNamespace["ak-dep-b"] == "" {
		t.Fatalf("did not query both deployments' own namespaces: %+v", queriesByNamespace)
	}
}

func TestListResourceMetricsRejectsUnknownResource(t *testing.T) {
	s, _ := newServer(t, Config{})
	rec := do(t, s, http.MethodGet, "/api/v1/metrics/resources?testids=a&resource=disk&start=1700000000&end=1700003600", nil)
	requireStatus(t, rec, http.StatusBadRequest)
}

func TestListResourceMetricsSkipsUnresolvableTestID(t *testing.T) {
	s, _ := newServer(t, Config{})
	rec := do(t, s, http.MethodGet, "/api/v1/metrics/resources?testids=nope&resource=cpu&start=1700000000&end=1700003600", nil)
	requireStatus(t, rec, http.StatusOK)
	body := decodeBody[struct {
		Series []RangeSeries `json:"series"`
	}](t, rec)
	if len(body.Series) != 0 {
		t.Errorf("got %d series for an unresolvable testid, want 0: %+v", len(body.Series), body.Series)
	}
}

// component narrows the sum to that authentik process's own pods, the
// same <deployment>-authentik-<component>- names profiles.go selects by.
func TestListResourceMetricsComponentFiltersPods(t *testing.T) {
	var gotQuery string
	prom := fakePrometheusRange(t, func(query string) [][2]any {
		gotQuery = query
		return [][2]any{{1700000000, "1"}}
	})

	deployment := sampleDeployment("topo3")
	deployment.Spec.Namespace = "ak-topo3"
	tc := sampleTestCase("login-topo3", "topo3", benchv1alpha1.ProfileBurn)

	s, _ := newServer(t, Config{PrometheusURL: prom.URL}, deployment, tc)
	rec := do(t, s, http.MethodGet, "/api/v1/metrics/resources?testids=login-topo3&resource=memory&component=worker&start=1700000000&end=1700003600", nil)
	requireStatus(t, rec, http.StatusOK)

	if strings.Contains(gotQuery, "%!") || strings.Contains(gotQuery, "MISSING") {
		t.Fatalf("query has an unsubstituted format verb: %q", gotQuery)
	}
	if !strings.Contains(gotQuery, `pod=~"topo3-authentik-worker-.+"`) {
		t.Errorf("query doesn't filter to the worker's pods: %q", gotQuery)
	}

	rec = do(t, s, http.MethodGet, "/api/v1/metrics/resources?testids=login-topo3&resource=memory&component=postgres&start=1700000000&end=1700003600", nil)
	requireStatus(t, rec, http.StatusBadRequest)
}

// TestListResourceMetricsFallsBackToRecordedDeployment: an archived
// TestCase whose AuthentikDeployment has since been deleted still finds its
// pods' history from what its status recorded.
func TestListResourceMetricsFallsBackToRecordedDeployment(t *testing.T) {
	var gotQuery string
	prom := fakePrometheusRange(t, func(query string) [][2]any {
		gotQuery = query
		return [][2]any{{1700000000, "1.5"}}
	})
	wc := &benchv1alpha1.WorkloadCluster{}
	wc.Name = "wc1"
	wc.Namespace = testNamespace
	wc.Status.PrometheusURL = prom.URL

	tc := sampleTestCase("login-archived", "topo-gone", benchv1alpha1.ProfileBurn)
	tc.Spec.Archived = true
	tc.Status.Deployments = []benchv1alpha1.TestCaseDeploymentStatus{{Name: "topo-gone", Cluster: "wc1", Namespace: "ak-gone", Target: "gone"}}

	s, _ := newServer(t, Config{}, wc, tc)
	rec := do(t, s, http.MethodGet, "/api/v1/metrics/resources?testids=login-archived&resource=cpu&start=1700000000&end=1700003600", nil)
	requireStatus(t, rec, http.StatusOK)
	body := decodeBody[struct {
		Series []RangeSeries `json:"series"`
	}](t, rec)
	if len(body.Series) != 1 || body.Series[0].Target != "gone" {
		t.Fatalf("series = %+v, want one for target gone", body.Series)
	}
	if !strings.Contains(gotQuery, `namespace="ak-gone"`) {
		t.Errorf("query doesn't scope to the recorded namespace: %q", gotQuery)
	}
}
