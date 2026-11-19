package api

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/common/model"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
)

// Container restarts answer "was this target up for the whole window?".
// A target whose server is being restarted - by a failing liveness probe
// under more load than it can take, an OOM kill, a crash - still reports
// plausible throughput and latency, but they include its downtime and cold
// starts, so they do not compare with a target that stayed up. Nothing else
// on the compare view shows that.

// restartsQuery counts container restarts in a deployment's namespace, per
// container name, from kube-state-metrics. Job pods are left out: the
// postgres role fix (internal/render/postgresrolefix.go) retries with
// RestartPolicy OnFailure, and those retries are a setup step failing, not
// the deployment under load going down. It takes %[1]q the namespace and
// %[2]s the window.
const restartsQuery = `sum by (container) (` +
	`increase(kube_pod_container_status_restarts_total{namespace=%[1]q}[%[2]s])` +
	` unless on (pod, uid) max_over_time(kube_pod_owner{namespace=%[1]q, owner_kind="Job"}[%[2]s]))`

// startedPodsQuery counts, per container name, the pods in a deployment's
// namespace that started within the window: a pod that was replaced (an
// eviction, a lost node, a rollout) starts its restart counter afresh, so
// restartsQuery never sees it, but it went through the same downtime and
// cold start. Job pods are left out, as in restartsQuery. It takes %[1]q
// the namespace, %[2]s the window and %[3]d the window in seconds;
// max_over_time keeps pods that are gone again by the end of the window.
//
// A pod is counted once per (pod, uid), not per series: kube_pod_container_info
// gets a new container_id/image_id series on every start and restart, a
// replaced kube-state-metrics pod changes instance, and a replaced
// StatefulSet pod keeps its name but not its uid.
const startedPodsQuery = `count by (container) (` +
	`group by (container, pod, uid) (max_over_time(kube_pod_container_info{namespace=%[1]q}[%[2]s]))` +
	` and on (pod, uid) (max_over_time(kube_pod_start_time{namespace=%[1]q}[%[2]s]) > time() - %[3]d)` +
	` unless on (pod, uid) max_over_time(kube_pod_owner{namespace=%[1]q, owner_kind="Job"}[%[2]s]))`

// RestartsTarget is one deployment's container restarts within the window.
type RestartsTarget struct {
	TestID string `json:"testid"`
	Target string `json:"target"`
	// Restarts is keyed by container name (server, worker, postgresql, ...).
	// A container that did not restart is left out, so a target that stayed
	// up has an empty map.
	Restarts map[string]int `json:"restarts"`
	// Started counts, the same way, the pods that started within the window:
	// replaced pods, whose restarts Restarts cannot see.
	Started map[string]int `json:"started"`
}

type restartsResponse struct {
	Window  string           `json:"window"`
	Targets []RestartsTarget `json:"targets"`
	// Errors is keyed by testid, same as compareResponse.Errors.
	Errors map[string]string `json:"errors,omitempty"`
}

// restartsCheck is one deployment to check and the part of the window to
// check it over.
type restartsCheck struct {
	deploymentTarget
	seconds int64
}

// listRestarts answers GET /metrics/restarts?testids=a,b[&window=5m][&time=],
// with the same window and time anchor as compare, so the two describe the
// same stretch of a test: one entry per deployment each testid drives.
//
// Each deployment is only checked from when k6 started loading it, if that
// is later than the window's start: the k6 numbers have nothing from before
// then, but the deployment's own rollout does - its first pods starting and
// crash-looping while postgres and the migrations come up - and would read
// as a target going down under load. That is decided here rather than in
// the UI, which would need every TestRun and the per-target burn starts to
// do it. A deployment k6 had not started on by the end of the window is
// left out.
func (s *Server) listRestarts(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	raw := q.Get("testids")
	if raw == "" {
		writeError(w, r, http.StatusBadRequest, "testids is required")
		return
	}
	var testids []string
	for id := range strings.SplitSeq(raw, ",") {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if !safeLabelValue.MatchString(id) {
			writeError(w, r, http.StatusBadRequest, fmt.Sprintf("invalid testid %q", id))
			return
		}
		testids = append(testids, id)
	}
	if len(testids) == 0 {
		writeError(w, r, http.StatusBadRequest, "testids is required")
		return
	}

	window := q.Get("window")
	if window == "" {
		window = "5m"
	}
	if !safeWindow.MatchString(window) {
		writeError(w, r, http.StatusBadRequest, fmt.Sprintf("invalid window %q", window))
		return
	}
	at := q.Get("time")
	if at != "" && !safeUnixTime.MatchString(at) {
		writeError(w, r, http.StatusBadRequest, "time must be a unix timestamp")
		return
	}

	windowLength, err := model.ParseDuration(window)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, fmt.Sprintf("invalid window %q", window))
		return
	}
	end := time.Now()
	if at != "" {
		seconds, err := strconv.ParseInt(at, 10, 64)
		if err != nil {
			writeError(w, r, http.StatusBadRequest, "time must be a unix timestamp")
			return
		}
		end = time.Unix(seconds, 0)
	}
	windowStart := end.Add(-time.Duration(windowLength))

	resp := restartsResponse{Window: window, Targets: []RestartsTarget{}}
	// Kept apart so that only Prometheus failing fails the request: a
	// deployment nobody could query is reported, not an upstream error.
	var queryErrs, unchecked promErrors
	for _, id := range testids {
		checks, err := s.restartsChecks(r.Context(), id, windowStart, end)
		if err != nil {
			// Said rather than left out: a target nobody could check must
			// not read the same as one that stayed up.
			unchecked.add(id, "", err)
		}
		for _, check := range checks {
			length := fmt.Sprintf("%ds", check.seconds)
			restarts, err := s.containerCounts(r, check.PrometheusURL, fmt.Sprintf(restartsQuery, check.Namespace, length), at)
			if err != nil {
				queryErrs.add(id, check.PrometheusURL, err)
				continue
			}
			started, err := s.containerCounts(r, check.PrometheusURL, fmt.Sprintf(startedPodsQuery, check.Namespace, length, check.seconds), at)
			if err != nil {
				queryErrs.add(id, check.PrometheusURL, err)
				continue
			}
			queryErrs.succeeded = true
			resp.Targets = append(resp.Targets, RestartsTarget{TestID: id, Target: check.Target, Restarts: restarts, Started: started})
		}
	}
	if msg, failed := queryErrs.failed(); failed {
		writeError(w, r, http.StatusBadGateway, msg)
		return
	}
	for id, msg := range queryErrs.byTestID {
		unchecked.add(id, "", errors.New(msg))
	}
	resp.Errors = unchecked.byTestID
	writeJSON(w, r, http.StatusOK, resp)
}

// restartsChecks resolves the deployments testid's TestCase drives and, for
// each, the part of [windowStart, end] k6 was loading it in. The TestCase is
// read once, so the deployments checked and the ones reported as unchecked
// come from the same version of it. The error names what could not be
// checked; the checks that could be made are returned with it.
func (s *Server) restartsChecks(ctx context.Context, testid string, windowStart, end time.Time) ([]restartsCheck, error) {
	var tc benchv1alpha1.TestCase
	if err := s.client.Get(ctx, types.NamespacedName{Name: testid, Namespace: s.cfg.Namespace}, &tc); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, errors.New("test case not found")
		}
		return nil, fmt.Errorf("read test case: %w", err)
	}
	targets, skipped := s.resolveDeploymentTargets(ctx, &tc)

	var runs []benchv1alpha1.TestRun
	if tc.Spec.Profile != benchv1alpha1.ProfileBurn {
		var list benchv1alpha1.TestRunList
		if err := s.client.List(ctx, &list, client.InNamespace(s.cfg.Namespace)); err != nil {
			return nil, fmt.Errorf("list test runs: %w", err)
		}
		for _, run := range list.Items {
			if run.Spec.TestCaseRef.Name == testid {
				runs = append(runs, run)
			}
		}
	}

	var checks []restartsCheck
	for _, target := range targets {
		// The management cluster's Prometheus may not be configured at all.
		if target.PrometheusURL == "" {
			skipped = append(skipped, target.Deployment)
			continue
		}
		since := loadStart(&tc, runs, target.Deployment, windowStart, end)
		if since.IsZero() {
			continue
		}
		if since.Before(windowStart) {
			since = windowStart
		}
		seconds := int64(end.Sub(since) / time.Second)
		if seconds <= 0 {
			continue
		}
		checks = append(checks, restartsCheck{deploymentTarget: target, seconds: seconds})
	}
	if len(skipped) > 0 {
		quoted := make([]string, len(skipped))
		for i, name := range skipped {
			quoted[i] = strconv.Quote(name)
		}
		return checks, fmt.Errorf("could not check deployment(s) %s: gone, or their cluster has no Prometheus yet", strings.Join(quoted, ", "))
	}
	return checks, nil
}

// loadStart is when k6 started loading deployment, for a window from
// windowStart to end, or zero if it had not started by end.
//
// A Burn target starts at its own burningSince: a target added to the
// TestCase later starts later than the rest. A Quick target is loaded by
// TestRuns, one per deployment; it starts at the earliest run that overlaps
// the window, since the k6 numbers over the window include every such run.
// That is the run the UI anchors the window to, or one before it close
// enough to fall in the same window.
func loadStart(tc *benchv1alpha1.TestCase, runs []benchv1alpha1.TestRun, deployment string, windowStart, end time.Time) time.Time {
	if tc.Spec.Profile == benchv1alpha1.ProfileBurn {
		for _, st := range tc.Status.Deployments {
			if st.Name == deployment && st.BurningSince != nil && st.BurningSince.Time.Before(end) {
				return st.BurningSince.Time
			}
		}
		return time.Time{}
	}
	var first time.Time
	for _, run := range runs {
		// Runs from before multi-deployment TestCases have no deploymentRef
		// and ran against the first deployment (resolvedRunDeploymentRef).
		runDeployment := ""
		if run.Spec.DeploymentRef != nil && run.Spec.DeploymentRef.Name != "" {
			runDeployment = run.Spec.DeploymentRef.Name
		} else if refs := tc.Deployments(); len(refs) > 0 {
			runDeployment = refs[0].Name
		}
		if runDeployment != deployment || run.Status.StartTime == nil || !run.Status.StartTime.Time.Before(end) {
			continue
		}
		if done := run.Status.CompletionTime; done != nil && !done.After(windowStart) {
			continue
		}
		if first.IsZero() || run.Status.StartTime.Time.Before(first) {
			first = run.Status.StartTime.Time
		}
	}
	return first
}

// containerCounts runs a per-container instant query and returns its whole,
// non-zero counts by container name.
func (s *Server) containerCounts(r *http.Request, promURL, query, at string) (map[string]int, error) {
	samples, err := s.promInstantMulti(r.Context(), promURL, query, at)
	if err != nil {
		return nil, err
	}
	counts := map[string]int{}
	for _, sample := range samples {
		// increase() extrapolates to the window's edges, so a single
		// restart reads as slightly more or less than 1.
		if n := int(math.Round(sample.Value)); n > 0 {
			counts[sample.Labels["container"]] = n
		}
	}
	return counts, nil
}
