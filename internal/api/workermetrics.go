package api

import (
	"cmp"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strconv"

	"github.com/go-chi/chi/v5"
	"k8s.io/apimachinery/pkg/types"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
	"github.com/authentik-labs/akbench/internal/render"
)

// workerSelector scopes a query to one deployment's authentik metrics:
// %[1]q its namespace, %[2]q its target label (scrapeRelabelings). Server
// and worker share job="authentik", but the authentik_tasks_* series only
// the worker exports need no pod filter.
const workerSelector = `job="authentik", namespace=%[1]q, target=%[2]q`

// byActor is the label the task series name their actor by.
const byActor = "actor_name"

// workerPods narrows workerSelector to the worker's own pods (%[3]q), for
// the django_db_* series the server exports too.
const workerPods = workerSelector + `, pod=~%[3]q`

// workerMetrics is keyed by the ?metric= query param. The task series come
// from django-dramatiq-postgres' MetricsMiddleware, the database ones from
// django-prometheus. Each Query takes workerSelector's verbs, %[3]q the
// worker pods' name regex and %[4]s the rate() window. Tasks are summed
// across actors rather than broken out per actor: topk in a range query
// picks its top 8 per step, so the legend still filled up with every actor
// that was ever in it.
var workerMetrics = map[string]deploymentChart{
	"tasks": {Label: "Worker tasks processed", Unit: "tasks/s", Queries: []deploymentQuery{{
		Name:  "processed",
		Query: `sum(rate(authentik_tasks_total{` + workerSelector + `}[%[4]s]))`,
	}}},
	// The histogram is in milliseconds; the UI formats seconds.
	"duration": {Label: "Worker task duration", Unit: "s", Queries: []deploymentQuery{
		{
			Name:  "p50",
			Query: `histogram_quantile(0.50, sum by (le) (rate(authentik_tasks_duration_milliseconds_bucket{` + workerSelector + `}[%[4]s]))) / 1000`,
		},
		{
			Name:  "p95",
			Query: `histogram_quantile(0.95, sum by (le) (rate(authentik_tasks_duration_milliseconds_bucket{` + workerSelector + `}[%[4]s]))) / 1000`,
		},
		{
			Name:  "p99",
			Query: `histogram_quantile(0.99, sum by (le) (rate(authentik_tasks_duration_milliseconds_bucket{` + workerSelector + `}[%[4]s]))) / 1000`,
		},
	}},
	"queue": {Label: "Worker queue", Unit: "tasks", Queries: []deploymentQuery{
		{
			Name: "queued",
			// Every server and worker pod reports the same queue depth
			// (it's read from the database at scrape time), so take one
			// per actor rather than summing the copies.
			Query: `sum(max by (queue_name, actor_name) (authentik_tasks_queued{` + workerSelector + `}))`,
		},
		{
			Name:  "in progress",
			Query: `sum(authentik_tasks_in_progress{` + workerSelector + `})`,
		},
		{
			Name:  "delayed",
			Query: `sum(authentik_tasks_delayed_in_progress{` + workerSelector + `})`,
		},
	}},
	"failures": {Label: "Worker task failures", Unit: "tasks/s", Queries: []deploymentQuery{
		{
			Name:  "errors",
			Query: `sum(rate(authentik_tasks_errors_total{` + workerSelector + `}[%[4]s])) or vector(0)`,
		},
		{
			Name:  "retries",
			Query: `sum(rate(authentik_tasks_retries_total{` + workerSelector + `}[%[4]s])) or vector(0)`,
		},
		{
			Name:  "dead-lettered",
			Query: `sum(rate(authentik_tasks_rejected_total{` + workerSelector + `}[%[4]s])) or vector(0)`,
		},
	}},
	"db_queries": {Label: "Worker database queries", Unit: "queries/s", Queries: []deploymentQuery{{
		By:    byPod,
		Query: `sum by (pod) (rate(django_db_execute_total{` + workerPods + `}[%[4]s]))`,
	}}},
	"db_duration": {Label: "Worker database query duration", Unit: "s", Queries: []deploymentQuery{
		{
			Name:  "p50",
			Query: `histogram_quantile(0.50, sum by (le) (rate(django_db_query_duration_seconds_bucket{` + workerPods + `}[%[4]s])))`,
		},
		{
			Name:  "p95",
			Query: `histogram_quantile(0.95, sum by (le) (rate(django_db_query_duration_seconds_bucket{` + workerPods + `}[%[4]s])))`,
		},
		{
			Name:  "p99",
			Query: `histogram_quantile(0.99, sum by (le) (rate(django_db_query_duration_seconds_bucket{` + workerPods + `}[%[4]s])))`,
		},
	}},
}

// listWorkerMetrics answers GET
// /deployments/{name}/worker-metrics?metric=tasks|duration|queue|failures|db_queries|db_duration&start=&end=[&window=][&step=]:
// the deployment's own authentik worker over time, independent of any test,
// from the metrics its worker pods export. Same response shape as
// listPostgresMetrics.
func (s *Server) listWorkerMetrics(w http.ResponseWriter, r *http.Request) {
	s.serveDeploymentChart(w, r, workerMetrics, func(d *benchv1alpha1.AuthentikDeployment, query, window string) (string, error) {
		target := d.Spec.TargetName
		if target == "" {
			target = d.Name
		}
		pods := regexp.QuoteMeta(render.AuthentikFullname(render.HelmReleaseName(d))) + "-worker-.+"
		return fmt.Sprintf(query, d.Spec.Namespace, target, pods, window), nil
	})
}

// workerActorLimit is how many actors listWorkerActors returns, busiest first.
const workerActorLimit = 10

// WorkerActor is one actor's row in listWorkerActors' table: what it did over
// the whole window.
type WorkerActor struct {
	Actor  string   `json:"actor"`
	Tasks  float64  `json:"tasks"`
	P95    *float64 `json:"p95,omitempty"`
	Errors float64  `json:"errors"`
}

// listWorkerActors answers GET /deployments/{name}/worker-actors?start=&end=:
// the worker's busiest actors over [start, end] by tasks processed, with each
// one's p95 duration (seconds) and error count. Instant queries at end over
// the whole window, unlike the per-step topk the charts used to have, so the
// top N is the same N for the window.
func (s *Server) listWorkerActors(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	start, end := q.Get("start"), q.Get("end")
	if !safeUnixTime.MatchString(start) || !safeUnixTime.MatchString(end) {
		writeError(w, r, http.StatusBadRequest, "start and end must be unix timestamps")
		return
	}
	startSeconds, _ := strconv.ParseInt(start, 10, 64)
	endSeconds, _ := strconv.ParseInt(end, 10, 64)
	// rate() needs at least a couple of scrapes in its window.
	window := fmt.Sprintf("%ds", max(endSeconds-startSeconds, 60))

	var d benchv1alpha1.AuthentikDeployment
	if err := s.client.Get(r.Context(), types.NamespacedName{Name: chi.URLParam(r, "name"), Namespace: s.cfg.Namespace}, &d); err != nil {
		writeAPIError(w, r, err)
		return
	}
	target := d.Spec.TargetName
	if target == "" {
		target = d.Name
	}
	selector := fmt.Sprintf(workerSelector, d.Spec.Namespace, target)
	promURL := s.prometheusURLForCluster(r.Context(), d.Spec.ClusterRef.Name)
	query := func(query string) ([]promSample, error) {
		return s.promInstantMulti(r.Context(), promURL, fmt.Sprintf(query, selector, window), end)
	}

	tasks, err := query(fmt.Sprintf(`topk(%d, sum by (actor_name) (increase(authentik_tasks_total{%%s}[%%s])))`, workerActorLimit))
	if err != nil {
		writeError(w, r, http.StatusBadGateway, err.Error())
		return
	}
	// The histogram is in milliseconds.
	p95, err := query(`histogram_quantile(0.95, sum by (le, actor_name) (rate(authentik_tasks_duration_milliseconds_bucket{%s}[%s]))) / 1000`)
	if err != nil {
		writeError(w, r, http.StatusBadGateway, err.Error())
		return
	}
	errs, err := query(`sum by (actor_name) (increase(authentik_tasks_errors_total{%s}[%s]))`)
	if err != nil {
		writeError(w, r, http.StatusBadGateway, err.Error())
		return
	}

	actors := make([]WorkerActor, 0, len(tasks))
	for _, t := range tasks {
		row := WorkerActor{Actor: t.Labels[byActor], Tasks: t.Value}
		for _, p := range p95 {
			if p.Labels[byActor] == row.Actor {
				row.P95 = &p.Value
			}
		}
		for _, e := range errs {
			if e.Labels[byActor] == row.Actor {
				row.Errors = e.Value
			}
		}
		actors = append(actors, row)
	}
	slices.SortFunc(actors, func(a, b WorkerActor) int { return cmp.Compare(b.Tasks, a.Tasks) })
	writeJSON(w, r, http.StatusOK, actors)
}
