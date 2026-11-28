package api

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"time"

	"github.com/prometheus/common/model"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
	"github.com/authentik-labs/akbench/internal/render"
)

// unitCPUSeconds is CPU time, as distinct from the wall-clock durations that
// use "s": the UI must not render the two alike.
const unitCPUSeconds = "cpu-s"

// costMinIterations is how many passed transactions a target needs in the
// stretch for its cost to be reported. With fewer, a k6 pod that has only
// just started, or is crash-looping, divides the deployment's idle CPU by
// next to nothing.
const costMinIterations = 30

// costTimeout bounds the cost queries of one test. They ride on a request
// the UI polls, and its own numbers must not wait long on them.
const costTimeout = 10 * time.Second

// costMetric is a compare metric that is not a k6 series but a deployment's
// own CPU divided by the transactions it served: what one transaction
// costs. It moves far less than latency does between two runs of the same
// build, and it is what a code change actually changes.
//
// It is total CPU seconds over total passed transactions, both counted over
// the same stretch of time: the part of the window k6 was loading the
// target in. Rates would not do: an average rate only describes the minutes
// a series existed, so CPU averaged over a window the load covered half of
// would be set against a transaction rate that is not.
//
// The two halves live in different places - CPU on the Prometheus scraping
// the deployment's cluster, selected by namespace and pod, transactions on
// the one k6 writes to, selected by testid and target - and those can be two
// Prometheus instances, so the division happens here rather than in PromQL.
type costMetric struct {
	CompareMetric
	// containers is the extra cAdvisor matcher for the containers whose
	// CPU counts.
	containers string
	// pods is the name regex of the pods whose CPU counts.
	pods func(deployment string) string
}

// costCPUQuery is the CPU seconds a set of containers used over a stretch:
// %[1]q the namespace, %[2]q the pod name regex, %[3]s the container
// matcher, %[4]d the seconds.
//
// Each container's last sample in the stretch minus its first, summed. Not
// increase(): it extrapolates the sampled part to the edges of the range,
// and a short Quick run's stretch has idle edges (the k6 pod starting, the
// tail after it) that would be charged at the loaded rate, by an amount
// that depends on where the scrapes fall. A restarted container is a new
// cAdvisor series, so a series does not reset within itself. And no join
// on the pods that exist now: one replaced during the stretch still counts
// for the time it ran.
const costCPUQuery = `sum(max_over_time(container_cpu_usage_seconds_total{namespace=%[1]q, pod=~%[2]q, %[3]s}[%[4]ds])` +
	` - min_over_time(container_cpu_usage_seconds_total{namespace=%[1]q, pod=~%[2]q, %[3]s}[%[4]ds]))`

// authentikContainers is every real container of a pod: not cAdvisor's own
// per-pod aggregate and pause-container series.
const authentikContainers = `container!="", container!="POD"`

func authentikPods(component string) func(string) string {
	return func(deployment string) string {
		return regexp.QuoteMeta(render.AuthentikFullname(deployment)) + "-" + component + "-.+"
	}
}

// costMetrics follow compareMetrics in a compare response that asked for
// them (?cost=true). Derived marks them for callers: there is no time
// series behind them, so metricsRange does not know their keys.
//
// A deployment's CPU is everything it did in the stretch, so the figures
// are only one test's cost while that test is the deployment's only load.
// The worker is its own figure, not left out: a login also queues events
// and tasks the worker runs, and a change that moves work there must not
// read as cheaper.
var costMetrics = []costMetric{
	{
		Key: "server_cpu_per_iteration", Label: "Server CPU time per transaction",
		Unit: unitCPUSeconds, Better: betterLower, Derived: true,
		containers: authentikContainers,
		pods:       authentikPods("server"),
	},
	{
		Key: "worker_cpu_per_iteration", Label: "Worker CPU time per transaction",
		Unit: unitCPUSeconds, Better: betterLower, Derived: true,
		containers: authentikContainers,
		pods:       authentikPods("worker"),
	},
	{
		Key: "postgres_cpu_per_iteration", Label: "Postgres CPU time per transaction",
		Unit: unitCPUSeconds, Better: betterLower, Derived: true,
		// The postgres container only: the bundled chart runs a metrics
		// exporter beside it, CNPG does not, and the two must compare.
		containers: postgresContainers,
		// Either managed mode's pods, by name alone: the mode is only on the
		// AuthentikDeployment, which may be gone while its data is not. An
		// external postgres has no pods, matches nothing and gets no cost.
		// Every CNPG instance counts, replicas included: they replay what
		// the load wrote.
		pods: func(deployment string) string {
			d := &benchv1alpha1.AuthentikDeployment{}
			d.Name = deployment
			return "(?:" + regexp.QuoteMeta(render.EmbeddedPostgresStatefulSetName(d)) + "|" +
				regexp.QuoteMeta(render.CNPGClusterName(d)) + ")-[0-9]+"
		},
	},
}

// costIterationsQuery is the passed transactions of one target over a
// stretch: %[1]q the testid, %[2]q the target, %[3]d the seconds. Failed
// ones are left out, or a build that fails its logins at the first request
// would look cheap.
const costIterationsQuery = `sum(increase(k6_iteration_outcomes_total{testid=%[1]q, target=%[2]q, outcome="ok"}[%[3]ds]))`

// costRecordedIterationsQuery is costIterationsQuery from the recording
// rules, same verbs: every outcome's per-minute rate less the failed ones',
// each summed over the stretch's minutes and turned back into a count. Used
// for a long stretch, where the raw counter, pushed every second, is a lot
// to read on every poll of a page showing a week-long burn.
const costRecordedIterationsQuery = `60 * (sum(sum_over_time(` + render.RecordedIterationOutcomes + `{testid=%[1]q, target=%[2]q}[%[3]ds]))` +
	` - (sum(sum_over_time(` + render.RecordedIterationsFailed + `{testid=%[1]q, target=%[2]q}[%[3]ds])) or vector(0)))`

// compareResponseMetrics is every metric a compare response can carry a
// value for: the k6 ones, then the derived costs when asked for.
func compareResponseMetrics(withCost bool) []CompareMetric {
	if !withCost {
		return compareMetrics
	}
	metrics := make([]CompareMetric, 0, len(compareMetrics)+len(costMetrics))
	metrics = append(metrics, compareMetrics...)
	for _, m := range costMetrics {
		metrics = append(metrics, m.CompareMetric)
	}
	return metrics
}

// costStretch is the part of a compare window one target was under load in.
type costStretch struct {
	deploymentTarget
	// end is the instant the queries are evaluated at, seconds their range.
	end     time.Time
	seconds int64
}

// costStretches resolves, for each deployment testid drives, the stretch of
// the window ending at end that k6 was loading it in: from when the load
// started (loadChecks) to when it stopped, if that came before end - a burn
// that was stopped, or a Quick test's last run completing. Outside it the
// deployment's CPU is not the test's: it is still importing its blueprints
// before, and idling after.
func (s *Server) costStretches(ctx context.Context, testid string, windowStart, end time.Time) []costStretch {
	// The error only names what could not be checked; those targets simply
	// get no cost.
	tc, runs, checks, _ := s.loadChecks(ctx, testid, windowStart, end)
	stretches := make([]costStretch, 0, len(checks))
	for _, check := range checks {
		stretch := costStretch{deploymentTarget: check.deploymentTarget, end: end, seconds: check.seconds}
		if stopped := loadEnd(tc, runs, check.Deployment, windowStart, end); !stopped.IsZero() {
			stretch.seconds -= int64(end.Sub(stopped) / time.Second)
			stretch.end = stopped
		}
		if stretch.seconds > 0 {
			stretches = append(stretches, stretch)
		}
	}
	return stretches
}

// loadEnd is when k6 stopped loading deployment before end, or zero if it
// was still loading it then: a Burn target's burnStoppedAt, or the latest
// completion of the Quick runs loadStart counts, once every one of them has
// completed.
func loadEnd(tc *benchv1alpha1.TestCase, runs []benchv1alpha1.TestRun, deployment string, windowStart, end time.Time) time.Time {
	if tc.Spec.Profile == benchv1alpha1.ProfileBurn {
		for _, st := range tc.Status.Deployments {
			if st.Name == deployment && st.BurnStoppedAt != nil && st.BurnStoppedAt.Time.Before(end) {
				return st.BurnStoppedAt.Time
			}
		}
		return time.Time{}
	}
	var last time.Time
	for _, run := range runs {
		runDeployment := ""
		if run.Spec.DeploymentRef != nil && run.Spec.DeploymentRef.Name != "" {
			runDeployment = run.Spec.DeploymentRef.Name
		} else if refs := tc.Deployments(); len(refs) > 0 {
			runDeployment = refs[0].Name
		}
		if runDeployment != deployment || run.Status.StartTime == nil || !run.Status.StartTime.Time.Before(end) {
			continue
		}
		done := run.Status.CompletionTime
		if done != nil && !done.After(windowStart) {
			continue
		}
		// One still running at end: the load had not stopped.
		if done == nil || !done.Time.Before(end) {
			return time.Time{}
		}
		if done.After(last) {
			last = done.Time
		}
	}
	return last
}

// addCostValues fills the costMetrics of testid's rows (keyed by target),
// for the targets that were under load in the window ending at "at" (now if
// empty). A query that fails is recorded in errs and leaves that value
// absent; it never fails the comparison, whose k6 half already answered.
func (s *Server) addCostValues(ctx context.Context, testid string, rows map[string]*CompareResult, window, at string, errs *promErrors) {
	length, err := model.ParseDuration(window)
	if err != nil {
		return
	}
	end := time.Now()
	if at != "" {
		seconds, err := strconv.ParseInt(at, 10, 64)
		if err != nil {
			return
		}
		end = time.Unix(seconds, 0)
	}
	ctx, cancel := context.WithTimeout(ctx, costTimeout)
	defer cancel()

	k6URLs := s.prometheusURLsForTestID(ctx, testid)
	for _, stretch := range s.costStretches(ctx, testid, end.Add(-time.Duration(length)), end) {
		row := rows[stretch.Target]
		if row == nil {
			continue
		}
		fail := func(err error) {
			errs.add(testid, "", fmt.Errorf("CPU per transaction of %s: %w", stretch.Target, err))
		}
		evalAt := strconv.FormatInt(stretch.end.Unix(), 10)
		iterations, err := s.costIterations(ctx, k6URLs, testid, stretch, evalAt)
		if err != nil {
			fail(err)
			continue
		}
		if iterations < costMinIterations {
			continue
		}
		for _, m := range costMetrics {
			query := fmt.Sprintf(costCPUQuery, stretch.Namespace, m.pods(stretch.Deployment), m.containers, stretch.seconds)
			samples, err := s.promInstantMulti(ctx, stretch.PrometheusURL, query, evalAt)
			if err != nil {
				fail(err)
				break
			}
			// No sample at all: no such pods (an external postgres), which
			// is not the same as pods that used no CPU.
			if len(samples) == 0 {
				continue
			}
			row.Values[m.Key] = samples[0].Value / iterations
		}
	}
}

// costIterations is the passed transactions k6 counted for a stretch, from
// the recording rules for a long one and the raw counter otherwise - or
// when the rules have nothing (a Prometheus without them, or a test older
// than them), like compareSamples.
func (s *Server) costIterations(ctx context.Context, k6URLs []string, testid string, stretch costStretch, evalAt string) (float64, error) {
	if stretch.seconds >= int64(time.Duration(recordedMinWindow)/time.Second) {
		recorded, err := s.sumInstant(ctx, k6URLs, fmt.Sprintf(costRecordedIterationsQuery, testid, stretch.Target, stretch.seconds), evalAt)
		if err == nil && recorded > 0 {
			return recorded, nil
		}
	}
	return s.sumInstant(ctx, k6URLs, fmt.Sprintf(costIterationsQuery, testid, stretch.Target, stretch.seconds), evalAt)
}

// sumInstant adds up a one-sample instant query across several Prometheus
// instances: a test's k6 samples are on one of them, or split across the
// clusters its targets' runners are on. One that cannot be reached is
// skipped while another answers, so a target whose samples are on a healthy
// cluster keeps its figure; it is an error only when none answered.
func (s *Server) sumInstant(ctx context.Context, baseURLs []string, query, at string) (float64, error) {
	total, answered := 0.0, false
	var failures []error
	for _, baseURL := range baseURLs {
		samples, err := s.promInstantMulti(ctx, baseURL, query, at)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		answered = true
		for _, sample := range samples {
			total += sample.Value
		}
	}
	if !answered {
		return 0, errors.Join(failures...)
	}
	return total, nil
}
