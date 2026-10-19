package api

import (
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"github.com/go-chi/chi/v5"
	"k8s.io/apimachinery/pkg/types"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
	"github.com/authentik-labs/akbench/internal/render"
)

// postgresQuery is one line group of a postgres chart. Query takes %[1]q
// the namespace, %[2]q the postgres pods' name regex, %[3]q their data
// PVCs' name regex and %[4]s the rate() window, and groups by By - the
// pod or PVC each resulting line is named after.
type postgresQuery struct {
	// Name prefixes each line's legend entry, when a chart has more than
	// one query (used/capacity, read/write).
	Name  string
	By    string
	Query string
}

type postgresMetric struct {
	Label   string
	Unit    string
	Queries []postgresQuery
}

const (
	metricCPU = "cpu"
	unitBytes = "bytes"
	byPod     = "pod"
	byPVC     = "persistentvolumeclaim"
)

// postgresContainers is the postgres container in either managed mode:
// "postgresql" in the bundled Bitnami subchart, "postgres" in a CNPG
// instance pod. Leaving out sidecars (the metrics exporter) keeps the
// charts to the database server itself.
const postgresContainers = `container=~"postgres|postgresql"`

// postgresMetrics is keyed by the ?metric= query param. CPU, memory and IO
// come from cAdvisor; disk from the kubelet's volume stats, the same numbers
// the operator's auto-growth reads (AuthentikDeploymentReconciler's
// reconcilePostgresStorage), so the chart shows what it grows on.
var postgresMetrics = map[string]postgresMetric{
	metricCPU: {Label: "Postgres CPU", Unit: "cores", Queries: []postgresQuery{{
		By:    byPod,
		Query: `sum by (pod) (rate(container_cpu_usage_seconds_total{namespace=%[1]q, pod=~%[2]q, ` + postgresContainers + `}[%[4]s]))`,
	}}},
	"memory": {Label: "Postgres memory", Unit: unitBytes, Queries: []postgresQuery{{
		By:    byPod,
		Query: `sum by (pod) (container_memory_working_set_bytes{namespace=%[1]q, pod=~%[2]q, ` + postgresContainers + `})`,
	}}},
	"disk": {Label: "Postgres disk usage", Unit: unitBytes, Queries: []postgresQuery{
		{
			Name:  "used",
			By:    byPVC,
			Query: `max by (persistentvolumeclaim) (kubelet_volume_stats_used_bytes{namespace=%[1]q, persistentvolumeclaim=~%[3]q})`,
		},
		{
			Name:  "capacity",
			By:    byPVC,
			Query: `max by (persistentvolumeclaim) (kubelet_volume_stats_capacity_bytes{namespace=%[1]q, persistentvolumeclaim=~%[3]q})`,
		},
	}},
	"io": {Label: "Postgres disk IO", Unit: "bytes/s", Queries: []postgresQuery{
		{
			Name:  "read",
			By:    byPod,
			Query: `sum by (pod) (rate(container_fs_reads_bytes_total{namespace=%[1]q, pod=~%[2]q, ` + postgresContainers + `}[%[4]s]))`,
		},
		{
			Name:  "write",
			By:    byPod,
			Query: `sum by (pod) (rate(container_fs_writes_bytes_total{namespace=%[1]q, pod=~%[2]q, ` + postgresContainers + `}[%[4]s]))`,
		},
	}},
}

// postgresSelectors are the name regexes of a deployment's postgres pods and
// their data PVCs: the bundled subchart's single StatefulSet pod, or one pod
// (and one same-named PVC) per CNPG instance.
func postgresSelectors(d *benchv1alpha1.AuthentikDeployment) (pods, pvcs string) {
	if d.Spec.Postgres.Mode == benchv1alpha1.PostgresCNPG {
		instance := regexp.QuoteMeta(render.CNPGClusterName(d)) + "-[0-9]+"
		return instance, instance
	}
	return regexp.QuoteMeta(render.EmbeddedPostgresStatefulSetName(d)) + "-[0-9]+",
		regexp.QuoteMeta(render.EmbeddedPostgresPVCName(d))
}

// listPostgresMetrics answers GET
// /deployments/{name}/postgres-metrics?metric=cpu|memory|disk|io&start=&end=[&window=][&step=]:
// the deployment's own postgres over time, independent of any test, from the
// Prometheus scraping its cluster. One line per postgres pod (or PVC, for
// disk), per query. Uses metricsRange's response shape, with each line's
// legend in target.
func (s *Server) listPostgresMetrics(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	metric, ok := postgresMetrics[q.Get("metric")]
	if !ok {
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

	var d benchv1alpha1.AuthentikDeployment
	if err := s.client.Get(r.Context(), types.NamespacedName{Name: chi.URLParam(r, "name"), Namespace: s.cfg.Namespace}, &d); err != nil {
		writeAPIError(w, r, err)
		return
	}
	if d.Spec.Postgres.Mode == benchv1alpha1.PostgresExternal {
		writeError(w, r, http.StatusBadRequest, "an external postgres runs outside the cluster and isn't monitored")
		return
	}
	promURL := s.prometheusURLForCluster(r.Context(), d.Spec.ClusterRef.Name)

	pods, pvcs := postgresSelectors(&d)
	resp := rangeResponse{
		Metric: CompareMetric{Key: q.Get("metric"), Label: metric.Label, Unit: metric.Unit},
		Series: []RangeSeries{},
	}
	for _, pq := range metric.Queries {
		query := fmt.Sprintf(pq.Query, d.Spec.Namespace, pods, pvcs, window)
		series, err := s.promRangeMulti(r.Context(), promURL, query, start, end, step)
		if err != nil {
			writeError(w, r, http.StatusBadGateway, err.Error())
			return
		}
		slices.SortFunc(series, func(a, b promSeries) int { return strings.Compare(a.Labels[pq.By], b.Labels[pq.By]) })
		for _, ps := range series {
			label := ps.Labels[pq.By]
			if pq.Name != "" {
				label = pq.Name + " · " + label
			}
			resp.Series = append(resp.Series, RangeSeries{Target: label, Points: ps.Points})
		}
	}
	writeJSON(w, r, http.StatusOK, resp)
}
