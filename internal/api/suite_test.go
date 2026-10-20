package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
)

const (
	testNamespace = "authentik-bench"
	testToken     = "s3cret-token"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("add client-go scheme: %v", err)
	}
	if err := benchv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add bench scheme: %v", err)
	}
	return s
}

// newServer builds a Server over a fake management cluster seeded with the
// given objects. The fake client is enough here because the backend only ever
// does plain CRUD - everything that needs a real API server (defaulting,
// CRD validation) is the operator's envtest suite's job.
func newServer(t *testing.T, cfg Config, objects ...client.Object) (*Server, client.Client) {
	t.Helper()
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(objects...).
		WithStatusSubresource(&benchv1alpha1.AuthentikDeployment{}, &benchv1alpha1.TestCase{}, &benchv1alpha1.TestRun{}).
		Build()
	if cfg.Namespace == "" {
		cfg.Namespace = testNamespace
	}
	if cfg.Token == "" {
		cfg.Token = testToken
	}
	return New(c, cfg), c
}

// do issues an authenticated request against the router and returns the
// recorded response.
func do(t *testing.T, s *Server, method, target string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, target, reader)
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	return rec
}

func decodeBody[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response %q: %v", rec.Body.String(), err)
	}
	return out
}

func requireStatus(t *testing.T, rec *httptest.ResponseRecorder, want int) {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("got status %d, want %d (body: %s)", rec.Code, want, rec.Body.String())
	}
}

// sampleDeployment is a Ready deployment, since a TestCase referencing one that
// is not Ready is the operator's problem rather than the API's.
func sampleDeployment(name string) *benchv1alpha1.AuthentikDeployment {
	return &benchv1alpha1.AuthentikDeployment{
		Name: name, Namespace: testNamespace,
		Spec: benchv1alpha1.AuthentikDeploymentSpec{
			Namespace: "ak-" + name,
			Server:    benchv1alpha1.WorkloadSpec{Replicas: 2},
			Worker:    benchv1alpha1.WorkloadSpec{Replicas: 1},
			Postgres:  benchv1alpha1.PostgresSpec{Mode: benchv1alpha1.PostgresEmbedded},
		},
		Status: benchv1alpha1.AuthentikDeploymentStatus{
			ServiceEndpoint: "authentik-" + name + "-server.ak-" + name + ".svc.cluster.local:80",
			Conditions: []metav1.Condition{{
				Type:               benchv1alpha1.ConditionReady,
				Status:             metav1.ConditionTrue,
				Reason:             benchv1alpha1.ReasonReady,
				LastTransitionTime: metav1.Now(),
			}},
		},
	}
}

func sampleTestCase(name, deployment string, profile benchv1alpha1.TestProfile) *benchv1alpha1.TestCase {
	tc := &benchv1alpha1.TestCase{
		Name: name, Namespace: testNamespace,
		Spec: benchv1alpha1.TestCaseSpec{
			Profile:    profile,
			Template:   "login",
			VUs:        8,
			Iterations: 1000,
		},
	}
	if deployment != "" {
		tc.Spec.DeploymentRefs = []corev1.LocalObjectReference{{Name: deployment}}
	}
	return tc
}

// fakePrometheus serves the instant-query shape the compare endpoint parses,
// answering every query with the value the test hands it, tagged with the
// `target` label the handler returns - compareMetrics' queries now group
// `by (target)`, so a test asserting cross-target behavior needs to control
// it; most tests don't care and can return "".
func fakePrometheus(t *testing.T, handler func(query string) (value, target string, ok bool)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		value, target, ok := handler(r.URL.Query().Get("query"))
		w.Header().Set("Content-Type", "application/json")
		if !ok {
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
			return
		}
		body := map[string]any{
			"status": "success",
			"data": map[string]any{
				"resultType": "vector",
				"result": []any{map[string]any{
					"metric": map[string]string{"target": target},
					"value":  []any{1700000000, value},
				}},
			},
		}
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// fakePrometheusRange serves the query_range (matrix) shape metricsRange
// parses, answering every query with the points the test hands it.
func fakePrometheusRange(t *testing.T, handler func(query string) [][2]any) *httptest.Server {
	t.Helper()
	return fakePrometheusRangeLabels(t, func(query string) (map[string]string, [][2]any) {
		return map[string]string{}, handler(query)
	})
}

// fakePrometheusRangeLabels is fakePrometheusRange with the one series'
// labels set by handler too.
func fakePrometheusRangeLabels(t *testing.T, handler func(query string) (map[string]string, [][2]any)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		labels, points := handler(r.URL.Query().Get("query"))
		w.Header().Set("Content-Type", "application/json")
		values := make([]any, len(points))
		for i, p := range points {
			values[i] = []any{p[0], p[1]}
		}
		result := []any{}
		if points != nil {
			result = []any{map[string]any{"metric": labels, "values": values}}
		}
		body := map[string]any{
			"status": "success",
			"data":   map[string]any{"resultType": "matrix", "result": result},
		}
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}
