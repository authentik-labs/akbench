package api

import (
	"net/http"
	"strings"
	"testing"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
)

// TestListPostgresMetricsQueriesDeploymentPostgres guards each chart's
// queries: scoped to the deployment's own namespace and postgres pods/PVCs
// for its mode, with every format verb filled in.
func TestListPostgresMetricsQueriesDeploymentPostgres(t *testing.T) {
	cases := []struct {
		mode           benchv1alpha1.PostgresMode
		metric         string
		wantQueries    int
		wantInQuery    string
		wantFirstLabel string
	}{
		{benchv1alpha1.PostgresEmbedded, "cpu", 1, `pod=~"topo-postgresql-[0-9]+"`, ""},
		{benchv1alpha1.PostgresEmbedded, "memory", 1, `container=~"postgres|postgresql"`, ""},
		{benchv1alpha1.PostgresEmbedded, "disk", 2, `persistentvolumeclaim=~"data-topo-postgresql-0"`, "used · "},
		{benchv1alpha1.PostgresCNPG, "disk", 2, `persistentvolumeclaim=~"topo-postgres-[0-9]+"`, "used · "},
		{benchv1alpha1.PostgresCNPG, "io", 2, `pod=~"topo-postgres-[0-9]+"`, "read · "},
	}
	for _, tc := range cases {
		t.Run(string(tc.mode)+"/"+tc.metric, func(t *testing.T) {
			var queries []string
			prom := fakePrometheusRange(t, func(query string) [][2]any {
				queries = append(queries, query)
				return [][2]any{{1700000000, "1"}}
			})
			wc := &benchv1alpha1.WorkloadCluster{}
			wc.Name = "wc1"
			wc.Namespace = testNamespace
			wc.Status.PrometheusURL = prom.URL
			deployment := sampleDeployment("topo")
			deployment.Spec.ClusterRef.Name = "wc1"
			deployment.Spec.Namespace = "ak-topo"
			deployment.Spec.Postgres.Mode = tc.mode

			s, _ := newServer(t, Config{}, wc, deployment)
			rec := do(t, s, http.MethodGet, "/api/v1/deployments/topo/postgres-metrics?metric="+tc.metric+"&start=1700000000&end=1700003600", nil)
			requireStatus(t, rec, http.StatusOK)
			body := decodeBody[rangeResponse](t, rec)

			if len(queries) != tc.wantQueries {
				t.Fatalf("ran %d queries, want %d: %v", len(queries), tc.wantQueries, queries)
			}
			for _, q := range queries {
				if strings.Contains(q, "%!") {
					t.Errorf("query has an unsubstituted format verb: %q", q)
				}
				if !strings.Contains(q, `namespace="ak-topo"`) {
					t.Errorf("query isn't scoped to the deployment's namespace: %q", q)
				}
			}
			if !strings.Contains(queries[0], tc.wantInQuery) {
				t.Errorf("query %q doesn't contain %q", queries[0], tc.wantInQuery)
			}
			if len(body.Series) != tc.wantQueries {
				t.Fatalf("got %d series, want %d", len(body.Series), tc.wantQueries)
			}
			if !strings.HasPrefix(body.Series[0].Target, tc.wantFirstLabel) {
				t.Errorf("first series label = %q, want prefix %q", body.Series[0].Target, tc.wantFirstLabel)
			}
		})
	}
}

func TestListPostgresMetricsRejectsExternal(t *testing.T) {
	deployment := sampleDeployment("ext")
	deployment.Spec.Postgres.Mode = benchv1alpha1.PostgresExternal
	s, _ := newServer(t, Config{}, deployment)
	rec := do(t, s, http.MethodGet, "/api/v1/deployments/ext/postgres-metrics?metric=cpu&start=1700000000&end=1700003600", nil)
	requireStatus(t, rec, http.StatusBadRequest)
}
