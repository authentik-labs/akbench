package render

import "fmt"

const (
	// defaultAlertRulesKey is this project's own rules' key in
	// kube-prometheus-stack's additionalPrometheusRulesMap (the chart names
	// the PrometheusRule "<fullname>-<key>") - distinct from the many
	// PrometheusRule objects the chart's own defaultRules.* values already
	// create (KubePodCrashLooping, KubeDeploymentReplicasMismatch,
	// KubePersistentVolumeFillingUp, node pressure, etc.). This one adds only
	// what the generic rules can't know: alerts specific to this project's
	// own authentik/postgres/k6 metrics.
	defaultAlertRulesKey = "default-alerts"

	// LegacyDefaultAlertRulesName is the PrometheusRule an older operator
	// applied those rules as, directly rather than through the chart -
	// removed by ensurePrometheus so they aren't defined twice.
	LegacyDefaultAlertRulesName = "authentik-bench-default-alerts"

	// The two severities this project's own alert rules use.
	severityWarning  = "warning"
	severityCritical = "critical"

	// forSustained is how long a backlog or slow-down must last before it alerts.
	forSustained = "15m"
)

// alertRule is one Prometheus alerting rule, in the shape every rule this
// file defines shares (a plain expr/for/severity/summary/description - none
// need anything richer, e.g. extra labels). toMap is the one place
// PrometheusRule's own field names (alert/expr/for/labels/annotations/...)
// are spelled out, rather than repeating them in every rule literal below.
type alertRule struct {
	name        string
	expr        string
	forDuration string
	severity    string
	summary     string
	description string
}

func (ar alertRule) toMap() map[string]any {
	return map[string]any{
		"alert": ar.name,
		"expr":  ar.expr,
		"for":   ar.forDuration,
		"labels": map[string]any{
			"severity": ar.severity,
		},
		"annotations": map[string]any{
			"summary":     ar.summary,
			"description": ar.description,
		},
	}
}

// DefaultAlertRuleGroups builds the alert rules this project ships by
// default, as rule groups for kube-prometheus-stack's
// additionalPrometheusRulesMap value. That value goes through the chart's
// toYaml, never Helm's `tpl` - unlike extraManifests, which did and broke on
// the rules' "{{ $labels... }}" annotation syntax - so it reaches
// Prometheus verbatim, for its rule engine to expand. Every expression here
// was checked against this project's own live Prometheus
// (http://<node-ip>:30909/api/v1/query) before being written down, not
// guessed from documentation - this codebase has been burned twice already
// by dashboard panels querying metric names that sounded right but never
// actually existed (see docs/grafana-dashboards.md's own "Caveats"
// section).
//
//   - AuthentikBenchServiceScrapeDown: up{job=~"authentik|postgres"} == 0 catches an
//     authentik-server/-worker/postgresql ServiceMonitor target going
//     unreachable (job is set by scrapeRelabelings, matching the reference
//     harness's job names; it used to be "<release>-authentik-server-metrics" etc., and
//     several were actually down when this was written, including from the
//     postgres-PVC-full incident this project hit earlier).
//   - AuthentikBenchPostgresDown: pg_up == 0 is a stricter signal than the
//     scrape-level alert above - it means postgres_exporter itself reached
//     Postgres and got told the server isn't accepting connections, not
//     just that the exporter's own /metrics endpoint is unreachable.
//   - AuthentikBenchHighRequestFailureRate: a request-volume-weighted ratio
//     of k6_http_reqs_total{expected_response="false"} over all
//     k6_http_reqs_total, grouped by testid/target/profile - NOT a bare
//     k6_http_req_failed_rate threshold (that was this rule's first version,
//     and it was wrong: k6 tags every metric with the full request context,
//     including its own status code, so k6_http_req_failed_rate emits one
//     series per distinct (method,status,url,...) combination, and every
//     series belonging to a non-2xx/3xx status is pinned at exactly 1 by
//     definition - averaging across these series is not the same as the
//     true proportion of failed requests, and can read as "25% failed" (1
//     failing series out of 4 distinct ones) when the real, volume-weighted
//     failure rate is 0.05%. Confirmed live against a real burn test: 105
//     failed requests out of ~196,150 total. manifests/grafana-dashboards/k6.json's own
//     "Failed requests" panel had the same flaw (an unweighted
//     `avg by (testid) (k6_http_req_failed_rate{...})`) and was fixed
//     alongside this rule - see that file's own history.
//   - AuthentikBenchWorkerTaskErrors: authentik_tasks_errors_total counts
//     authentik's own background task failures (blueprint apply, outpost
//     sync, etc.) - a real signal no generic k8s-level rule could derive.
//
// The rules below DefaultAlertRuleGroups' generic ones come from a
// production incident report against authentik 2026.5.8 (expired-model
// cleanup timing out for weeks, the channel layer tables growing to
// terabytes, /dev/shm filling with prometheus multiprocess files). They catch
// the same failure modes on a burn run. Unlike the rules above, their
// expressions have not yet been checked against a live Prometheus. The
// table rules match both postgres_exporter's pg_stat_user_tables_* (Embedded)
// and CNPG's cnpg_pg_stat_user_tables_* (CNPG).
//
//   - AuthentikBenchExpiredModelCleanup{Stalled,Failing,Slow}:
//     clean_expired_models is scheduled every 5 minutes. Stalled fires when
//     workers process tasks but this one has not run for 30m (a task row left
//     "running" by a killed worker blocks its schedule), Failing when every
//     run in the last hour raised (e.g. TimeLimitExceeded), Slow when its mean
//     duration nears the 10m default time limit.
//   - AuthentikBench{ChannelMessage,GroupChannel,CacheEntry}Backlog: live rows
//     in tables that only clean_expired_models prunes. Channel messages live
//     60s, so a large table means cleanup is not reaching them; a large group
//     channel table means broadcasts fan out to dead channels.
//   - AuthentikBenchPolicyEngineLabelCardinality: prompt validation runs the
//     policy engine on an unsaved PolicyBindingModel, whose random pk becomes
//     the obj_pk label - one new histogram per submission, kept in /dev/shm
//     until the pod restarts.
//   - AuthentikBenchPolicyCacheKeys: each scrape reads every policy cache key
//     from the database to set authentik_policies_cached, so a large policy
//     cache slows scrapes too.
//   - AuthentikBenchMetricsScrapeSlow: the multiprocess collector reads every
//     series file under the GIL, so scrape time grows with that cardinality.
//
// Deliberately NOT duplicated here: node/PVC/pod-restart health, which
// kube-prometheus-stack's own default rules already cover once Alertmanager
// exists to receive them.
// cleanupActor is the expired-model cleanup task's actor_name label.
const cleanupActor = "authentik.core.tasks.clean_expired_models"

// tableBacklogRule fires when a table holds more than limit live rows.
func tableBacklogRule(name, table string, limit int, why string) alertRule {
	return alertRule{
		name:        name,
		expr:        fmt.Sprintf(`max by (namespace) ({__name__=~"(cnpg_)?pg_stat_user_tables_n_live_tup", relname=%q}) > %d`, table, limit),
		forDuration: forSustained,
		severity:    severityWarning,
		summary:     fmt.Sprintf("%s in {{ $labels.namespace }} has {{ $value }} live rows", table),
		description: fmt.Sprintf("%s has more than %d live rows. %s", table, limit, why),
	}
}

func DefaultAlertRuleGroups() []any {
	ruleDefs := []alertRule{
		{
			name:        "AuthentikBenchServiceScrapeDown",
			expr:        `up{job=~"authentik|postgres"} == 0`,
			forDuration: "10m",
			severity:    severityWarning,
			summary:     "{{ $labels.job }} on {{ $labels.instance }} has been unreachable for 10m",
			description: "Prometheus can't scrape {{ $labels.job }} ({{ $labels.instance }}) - the pod may be down, crash-looping, or its /metrics port unreachable.",
		},
		{
			name:        "AuthentikBenchPostgresDown",
			expr:        `pg_up == 0`,
			forDuration: "5m",
			severity:    severityCritical,
			summary:     "postgres_exporter on {{ $labels.instance }} reports Postgres is down",
			description: "pg_up == 0 for 5m: postgres_exporter reached its target but Postgres itself isn't accepting connections.",
		},
		{
			name: "AuthentikBenchHighRequestFailureRate",
			expr: `sum by (testid, target, profile) (rate(k6_http_reqs_total{expected_response="false"}[5m]))` +
				` / sum by (testid, target, profile) (rate(k6_http_reqs_total[5m])) > 0.05`,
			forDuration: "5m",
			severity:    severityWarning,
			summary:     "test {{ $labels.testid }} against {{ $labels.target }} has a >5% HTTP failure rate",
			description: "More than 5% of requests failed over the last 5m on testid={{ $labels.testid }} target={{ $labels.target }} profile={{ $labels.profile }}, weighted by actual request volume (not an average across k6's own per-status series).",
		},
		{
			name:        "AuthentikBenchWorkerTaskErrors",
			expr:        `increase(authentik_tasks_errors_total[15m]) > 0`,
			forDuration: "5m",
			severity:    severityWarning,
			summary:     "authentik worker on {{ $labels.instance }} is recording background task errors",
			description: "authentik_tasks_errors_total increased in the last 15m on {{ $labels.instance }} - check that pod's own worker logs for the failing task.",
		},
		{
			name: "AuthentikBenchExpiredModelCleanupStalled",
			expr: `group by (namespace) (authentik_tasks_total) unless on (namespace) (` +
				`sum by (namespace) (increase(authentik_tasks_total{actor_name="` + cleanupActor + `"}[30m])) > 0)`,
			forDuration: forSustained,
			severity:    severityCritical,
			summary:     "clean_expired_models has not run in {{ $labels.namespace }} for 30m",
			description: "Workers in {{ $labels.namespace }} process tasks, but clean_expired_models (scheduled every 5m) has not finished a run in 30m. A task row stuck in state running, e.g. left behind by a killed worker, blocks its schedule.",
		},
		{
			name: "AuthentikBenchExpiredModelCleanupFailing",
			expr: `sum by (namespace) (increase(authentik_tasks_errors_total{actor_name="` + cleanupActor + `"}[1h]))` +
				` >= sum by (namespace) (increase(authentik_tasks_total{actor_name="` + cleanupActor + `"}[1h])) > 0`,
			forDuration: forSustained,
			severity:    severityCritical,
			summary:     "every clean_expired_models run in {{ $labels.namespace }} failed in the last hour",
			description: "No clean_expired_models run in {{ $labels.namespace }} succeeded in the last hour. A run that times out rolls back, so expired rows pile up and the next run has even more to do.",
		},
		{
			name: "AuthentikBenchExpiredModelCleanupSlow",
			expr: `sum by (namespace) (rate(authentik_tasks_duration_milliseconds_sum{actor_name="` + cleanupActor + `"}[1h]))` +
				` / sum by (namespace) (rate(authentik_tasks_duration_milliseconds_count{actor_name="` + cleanupActor + `"}[1h])) > 300000`,
			forDuration: forSustained,
			severity:    severityWarning,
			summary:     "clean_expired_models in {{ $labels.namespace }} takes {{ $value | humanize }}ms on average",
			description: "clean_expired_models' mean duration in {{ $labels.namespace }} is above 5m, half of the default 10m task time limit. A larger backlog will make it time out.",
		},
		tableBacklogRule("AuthentikBenchChannelMessageBacklog", "django_channels_postgres_message", 50000,
			"Channel layer messages expire after 60s, so this table should stay small. Either clean_expired_models does not reach it, or broadcasts fan out to many dead channels."),
		tableBacklogRule("AuthentikBenchGroupChannelBacklog", "django_channels_postgres_groupchannel", 1000,
			"Every group_send inserts one message per channel in the group, expired and ghost channels included, so this multiplies broadcast writes."),
		tableBacklogRule("AuthentikBenchCacheEntryBacklog", "django_postgres_cache_cacheentry", 1000000,
			"Expired cache entries are only removed by clean_expired_models, and a large cache table slows every cache read."),
		{
			name:        "AuthentikBenchPolicyEngineLabelCardinality",
			expr:        `count by (namespace) (authentik_policies_engine_time_total_seconds_count{obj_type="authentik.policies.models.PolicyBindingModel"}) > 100`,
			forDuration: "5m",
			severity:    severityWarning,
			summary:     "{{ $value }} policy engine series for unsaved binding models in {{ $labels.namespace }}",
			description: "Prompt stage validation labels each policy engine run with a random obj_pk. Every new label set is kept in the prometheus multiprocess files in /dev/shm until the pod restarts.",
		},
		{
			name:        "AuthentikBenchPolicyCacheKeys",
			expr:        `max by (namespace) (authentik_policies_cached) > 50000`,
			forDuration: forSustained,
			severity:    severityWarning,
			summary:     "{{ $value }} policy result cache keys in {{ $labels.namespace }}",
			description: "Every /metrics scrape loads all policy cache keys to set authentik_policies_cached. Keys contain the session key, and ak_call_policy adds a random binding UUID, so they are rarely reused and only go away through clean_expired_models.",
		},
		{
			name:        "AuthentikBenchMetricsScrapeSlow",
			expr:        `scrape_duration_seconds{job="authentik"} > 1`,
			forDuration: forSustained,
			severity:    severityWarning,
			summary:     "scraping {{ $labels.instance }} takes {{ $value }}s",
			description: "authentik's /metrics takes more than 1s on {{ $labels.instance }}. The multiprocess collector reads every series file in /dev/shm, so this usually means metric labels with unbounded values.",
		},
	}
	rules := make([]any, len(ruleDefs))
	for i, rd := range ruleDefs {
		rules[i] = rd.toMap()
	}
	return []any{
		map[string]any{
			"name":  "authentik-bench.rules",
			"rules": rules,
		},
	}
}
