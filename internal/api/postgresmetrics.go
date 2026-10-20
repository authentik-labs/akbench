package api

import (
	"errors"
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

// deploymentQuery is one line group of a deployment-scoped chart (postgres
// or worker). Its Query's format verbs are the owning chart set's own (see
// postgresMetrics, workerMetrics), and it groups by By - the label each
// resulting line is named after, or nothing for a single line named Name.
type deploymentQuery struct {
	// Name prefixes each line's legend entry, when a chart has more than
	// one query (used/capacity, read/write).
	Name  string
	By    string
	Query string
}

type deploymentChart struct {
	Label   string
	Unit    string
	Queries []deploymentQuery
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

// postgresMetrics is keyed by the ?metric= query param. Each Query takes
// %[1]q the namespace, %[2]q the postgres pods' name regex, %[3]q their data
// PVCs' name regex and %[4]s the rate() window. CPU, memory and IO
// come from cAdvisor; disk from the kubelet's volume stats, the same numbers
// the operator's auto-growth reads (AuthentikDeploymentReconciler's
// reconcilePostgresStorage), so the chart shows what it grows on.
var postgresMetrics = map[string]deploymentChart{
	metricCPU: {Label: "Postgres CPU", Unit: "cores", Queries: []deploymentQuery{{
		By:    byPod,
		Query: `sum by (pod) (rate(container_cpu_usage_seconds_total{namespace=%[1]q, pod=~%[2]q, ` + postgresContainers + `}[%[4]s]))`,
	}}},
	"memory": {Label: "Postgres memory", Unit: unitBytes, Queries: []deploymentQuery{{
		By:    byPod,
		Query: `sum by (pod) (container_memory_working_set_bytes{namespace=%[1]q, pod=~%[2]q, ` + postgresContainers + `})`,
	}}},
	"disk": {Label: "Postgres disk usage", Unit: unitBytes, Queries: []deploymentQuery{
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
	"io": {Label: "Postgres disk IO", Unit: "bytes/s", Queries: []deploymentQuery{
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
	s.serveDeploymentChart(w, r, postgresMetrics, func(d *benchv1alpha1.AuthentikDeployment, query, window string) (string, error) {
		if d.Spec.Postgres.Mode == benchv1alpha1.PostgresExternal {
			return "", errors.New("an external postgres runs outside the cluster and isn't monitored")
		}
		pods, pvcs := postgresSelectors(d)
		return fmt.Sprintf(query, d.Spec.Namespace, pods, pvcs, window), nil
	})
}

// serveDeploymentChart runs one of charts (picked by ?metric=) for the
// {name} AuthentikDeployment over ?start=&end=[&window=][&step=], against
// the Prometheus scraping its cluster. fill renders each query for the
// deployment, or rejects a deployment the charts can't cover (answered 400).
func (s *Server) serveDeploymentChart(
	w http.ResponseWriter, r *http.Request, charts map[string]deploymentChart,
	fill func(d *benchv1alpha1.AuthentikDeployment, query, window string) (string, error),
) {
	q := r.URL.Query()
	metric, ok := charts[q.Get("metric")]
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
	queries := make([]string, 0, len(metric.Queries))
	for _, pq := range metric.Queries {
		query, err := fill(&d, pq.Query, window)
		if err != nil {
			writeError(w, r, http.StatusBadRequest, err.Error())
			return
		}
		queries = append(queries, query)
	}
	promURL := s.prometheusURLForCluster(r.Context(), d.Spec.ClusterRef.Name)

	resp := rangeResponse{
		Metric: CompareMetric{Key: q.Get("metric"), Label: metric.Label, Unit: metric.Unit},
		Series: []RangeSeries{},
	}
	for i, pq := range metric.Queries {
		series, err := s.promRangeMulti(r.Context(), promURL, queries[i], start, end, step)
		if err != nil {
			writeError(w, r, http.StatusBadGateway, err.Error())
			return
		}
		slices.SortFunc(series, func(a, b promSeries) int { return strings.Compare(a.Labels[pq.By], b.Labels[pq.By]) })
		for _, ps := range series {
			var label string
			switch {
			case pq.By == "":
				label = pq.Name
			case pq.Name != "":
				label = pq.Name + " · " + ps.Labels[pq.By]
			default:
				label = ps.Labels[pq.By]
			}
			resp.Series = append(resp.Series, RangeSeries{Target: label, Points: ps.Points})
		}
	}
	writeJSON(w, r, http.StatusOK, resp)
}
