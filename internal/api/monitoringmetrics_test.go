package api

import (
	"net/http"
	"strings"
	"testing"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
)

// TestListMonitoringMetricsQueriesObservabilityNamespace guards each
// monitoring chart: run against the cluster's own Prometheus, scoped to
// bench-system (node charts excepted), with every format verb filled in.
func TestListMonitoringMetricsQueriesObservabilityNamespace(t *testing.T) {
	for metric, wantQueries := range map[string]int{"cpu": 1, "memory": 1, "disk": 2, "node-cpu": 1, "node-memory": 1, "node-io": 2, "node-steal": 1} {
		t.Run(metric, func(t *testing.T) {
			var queries []string
			prom := fakePrometheusRangeLabels(t, func(query string) (map[string]string, [][2]any) {
				queries = append(queries, query)
				return map[string]string{"container": "prometheus", "persistentvolumeclaim": "data", "host": "node1"}, [][2]any{{1700000000, "1"}}
			})
			wc := &benchv1alpha1.WorkloadCluster{}
			wc.Name = "wc1"
			wc.Namespace = testNamespace
			wc.Status.PrometheusURL = prom.URL

			s, _ := newServer(t, Config{}, wc)
			rec := do(t, s, http.MethodGet, "/api/v1/clusters/wc1/monitoring-metrics?metric="+metric+"&start=1700000000&end=1700003600", nil)
			requireStatus(t, rec, http.StatusOK)
			body := decodeBody[rangeResponse](t, rec)

			if len(queries) != wantQueries || len(body.Series) != wantQueries {
				t.Fatalf("ran %d queries for %d series, want %d: %v", len(queries), len(body.Series), wantQueries, queries)
			}
			for _, q := range queries {
				if strings.Contains(q, "%!") || (!strings.HasPrefix(metric, "node-") && !strings.Contains(q, `namespace="bench-system"`)) {
					t.Errorf("query isn't scoped to bench-system: %q", q)
				}
			}
		})
	}
}

func TestListMonitoringMetricsWithoutPrometheus(t *testing.T) {
	wc := &benchv1alpha1.WorkloadCluster{}
	wc.Name = "wc1"
	wc.Namespace = testNamespace
	s, _ := newServer(t, Config{}, wc)
	rec := do(t, s, http.MethodGet, "/api/v1/clusters/wc1/monitoring-metrics?metric=cpu&start=1700000000&end=1700003600", nil)
	requireStatus(t, rec, http.StatusConflict)
}
