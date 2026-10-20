package api

import (
	"fmt"
	"net/http"
	"regexp"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
	"github.com/authentik-labs/akbench/internal/render"
)

const byActor = "actor_name"

// workerSelector scopes a query to one deployment's authentik metrics:
// %[1]q its namespace, %[2]q its target label (scrapeRelabelings). Server
// and worker share job="authentik", but the authentik_tasks_* series only
// the worker exports need no pod filter.
const workerSelector = `job="authentik", namespace=%[1]q, target=%[2]q`

// workerPods narrows workerSelector to the worker's own pods (%[3]q), for
// the django_db_* series the server exports too.
const workerPods = workerSelector + `, pod=~%[3]q`

// workerMetrics is keyed by the ?metric= query param. The task series come
// from django-dramatiq-postgres' MetricsMiddleware, the database ones from
// django-prometheus, both labelled by the worker's own actor names. Each
// Query takes workerSelector's verbs, %[3]q the worker pods' name regex
// and %[4]s the rate() window. Per-actor charts keep the top 8, so a
// login-heavy run's long tail of rare actors doesn't bury the legend.
var workerMetrics = map[string]deploymentChart{
	"tasks": {Label: "Worker tasks processed", Unit: "tasks/s", Queries: []deploymentQuery{{
		By:    byActor,
		Query: `topk(8, sum by (actor_name) (rate(authentik_tasks_total{` + workerSelector + `}[%[4]s])))`,
	}}},
	"duration": {Label: "Worker task duration p95", Unit: "s", Queries: []deploymentQuery{{
		By: byActor,
		// The histogram is in milliseconds; the UI formats seconds.
		Query: `topk(8, histogram_quantile(0.95, sum by (le, actor_name) ` +
			`(rate(authentik_tasks_duration_milliseconds_bucket{` + workerSelector + `}[%[4]s]))) / 1000)`,
	}}},
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
		pods := regexp.QuoteMeta(render.HelmReleaseName(d)) + "-authentik-worker-.+"
		return fmt.Sprintf(query, d.Spec.Namespace, target, pods, window), nil
	})
}
