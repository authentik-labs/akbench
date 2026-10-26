package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
)

// TestListWorkerMetricsQueriesDeploymentWorker guards each worker chart's
// queries: scoped to the deployment's namespace and target, the database
// ones to its worker pods, with every format verb filled in.
func TestListWorkerMetricsQueriesDeploymentWorker(t *testing.T) {
	cases := []struct {
		metric      string
		wantQueries int
		wantInQuery string
		wantLabel   string
	}{
		{"tasks", 1, `rate(authentik_tasks_total{`, "processed"},
		{"duration", 3, `authentik_tasks_duration_milliseconds_bucket{`, "p50"},
		{"queue", 3, `authentik_tasks_queued{`, "queued"},
		{"failures", 3, `authentik_tasks_errors_total{`, "errors"},
		{"db_queries", 1, `pod=~"topo-authentik-worker-.+"`, "topo-authentik-worker-abc"},
		{"db_duration", 3, `pod=~"topo-authentik-worker-.+"`, "p50"},
	}
	for _, tc := range cases {
		t.Run(tc.metric, func(t *testing.T) {
			var queries []string
			prom := fakePrometheusRangeLabels(t, func(query string) (map[string]string, [][2]any) {
				queries = append(queries, query)
				return map[string]string{"actor_name": "actor", "pod": "topo-authentik-worker-abc"}, [][2]any{{1700000000, "1"}}
			})
			wc := &benchv1alpha1.WorkloadCluster{}
			wc.Name = "wc1"
			wc.Namespace = testNamespace
			wc.Status.PrometheusURL = prom.URL
			deployment := sampleDeployment("topo")
			deployment.Spec.ClusterRef.Name = "wc1"
			deployment.Spec.Namespace = "ak-topo"
			deployment.Spec.TargetName = "baseline"

			s, _ := newServer(t, Config{}, wc, deployment)
			rec := do(t, s, http.MethodGet, "/api/v1/deployments/topo/worker-metrics?metric="+tc.metric+"&start=1700000000&end=1700003600", nil)
			requireStatus(t, rec, http.StatusOK)
			body := decodeBody[rangeResponse](t, rec)

			if len(queries) != tc.wantQueries {
				t.Fatalf("ran %d queries, want %d: %v", len(queries), tc.wantQueries, queries)
			}
			for _, q := range queries {
				if strings.Contains(q, "%!") {
					t.Errorf("query has an unsubstituted format verb: %q", q)
				}
				if !strings.Contains(q, `namespace="ak-topo", target="baseline"`) {
					t.Errorf("query isn't scoped to the deployment's namespace and target: %q", q)
				}
			}
			if !strings.Contains(queries[0], tc.wantInQuery) {
				t.Errorf("query %q doesn't contain %q", queries[0], tc.wantInQuery)
			}
			if len(body.Series) != tc.wantQueries {
				t.Fatalf("got %d series, want %d", len(body.Series), tc.wantQueries)
			}
			if body.Series[0].Target != tc.wantLabel {
				t.Errorf("first series label = %q, want %q", body.Series[0].Target, tc.wantLabel)
			}
		})
	}
}

// TestListWorkerActorsJoinsByActor guards the top-actors table: busiest
// first, each actor's p95 and errors joined in by name, the window covering
// all of [start, end].
func TestListWorkerActorsJoinsByActor(t *testing.T) {
	var queries []string
	prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query().Get("query")
		queries = append(queries, query)
		var samples map[string]string
		switch {
		case strings.Contains(query, "authentik_tasks_total"):
			samples = map[string]string{"quiet": "5", "busy": "50"}
		case strings.Contains(query, "duration"):
			samples = map[string]string{"busy": "0.25"}
		default:
			samples = map[string]string{"busy": "2"}
		}
		result := make([]any, 0, len(samples))
		for actor, value := range samples {
			result = append(result, map[string]any{"metric": map[string]string{"actor_name": actor}, "value": []any{1700003600, value}})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": map[string]any{"resultType": "vector", "result": result}})
	}))
	t.Cleanup(prom.Close)
	wc := &benchv1alpha1.WorkloadCluster{}
	wc.Name = "wc1"
	wc.Namespace = testNamespace
	wc.Status.PrometheusURL = prom.URL
	deployment := sampleDeployment("topo")
	deployment.Spec.ClusterRef.Name = "wc1"
	deployment.Spec.Namespace = "ak-topo"

	s, _ := newServer(t, Config{}, wc, deployment)
	rec := do(t, s, http.MethodGet, "/api/v1/deployments/topo/worker-actors?start=1700000000&end=1700003600", nil)
	requireStatus(t, rec, http.StatusOK)
	actors := decodeBody[[]WorkerActor](t, rec)

	if len(actors) != 2 || actors[0].Actor != "busy" || actors[1].Actor != "quiet" {
		t.Fatalf("actors = %+v, want busy then quiet", actors)
	}
	if actors[0].P95 == nil || *actors[0].P95 != 0.25 || actors[0].Errors != 2 {
		t.Errorf("busy = %+v, want p95 0.25 and 2 errors", actors[0])
	}
	if actors[1].P95 != nil || actors[1].Errors != 0 {
		t.Errorf("quiet = %+v, want no p95 and no errors", actors[1])
	}
	for _, q := range queries {
		if !strings.Contains(q, "[3600s]") || !strings.Contains(q, `namespace="ak-topo"`) {
			t.Errorf("query isn't over the whole window for the deployment: %q", q)
		}
	}
}
