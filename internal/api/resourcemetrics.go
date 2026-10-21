package api

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/types"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
)

// resourceMetric is one resource-usage query this endpoint can run, scoped
// to a deployment's own namespace rather than a testid label the way
// compareMetrics is - cAdvisor never tags container metrics with a k6
// testid, only namespace/pod/container. Summed across every pod in that
// namespace (not broken out by pod) - one line per *test*, matching every
// other chart on the Compare page. An earlier version broke this out
// "by (pod)" instead; confirmed live that was unreadable, since a
// deployment mid-rollout leaves its old ReplicaSet generation's pods in the
// same time window as the new one, turning a single test into 6+ tangled
// legend entries.
type resourceMetric struct {
	Label string
	Unit  string
	// Query takes %[1]q the namespace, %[2]s the rate() window (memory's
	// plain gauge read ignores it) and %[3]s extra cAdvisor matchers (the
	// component's pod filter, or nothing). Always formatted with all three:
	// explicit argument indexes keep fmt from flagging an unused one.
	Query string
}

// resourceMetrics is keyed by the ?resource= query param. container!="",
// container!="POD" excludes cAdvisor's own per-pod aggregate/pause-container
// series, the same filter internal/render/manifests/grafana-dashboards/authentik.json's own "CPU by
// pod"/"Memory by pod" panels use. "and on (pod) kube_pod_owner{owner_kind!=
// \"Job\"}" excludes one-shot Job pods (the fixtures blueprint import, the
// postgres role-fix job) - confirmed live these otherwise skew the sum
// alongside the deployment's own ongoing server/worker/postgres pods, which
// is what this chart is actually meant to show. %[1]q is reused for both
// namespace filters (the cAdvisor metric and the kube_pod_owner join both
// need it).
var resourceMetrics = map[string]resourceMetric{
	metricCPU: {
		Label: "CPU",
		Unit:  unitCores,
		Query: `sum(rate(container_cpu_usage_seconds_total{namespace=%[1]q, container!="", container!="POD"%[3]s}[%[2]s])` +
			` and on (pod) kube_pod_owner{namespace=%[1]q, owner_kind!="Job"})`,
	},
	metricMemory: {
		Label: "Memory",
		Unit:  unitBytes,
		Query: `sum(container_memory_working_set_bytes{namespace=%[1]q, container!="", container!="POD"%[3]s}` +
			` and on (pod) kube_pod_owner{namespace=%[1]q, owner_kind!="Job"})`,
	},
}

// deploymentTarget is one AuthentikDeployment a testid's TestCase drives,
// resolved for the cAdvisor-based resource-usage query: where its
// server/worker/postgres pods actually run (namespace + the Prometheus that
// scrapes that cluster) and its own display Target (the k6 `target` tag),
// so a multi-deployment TestCase's resource charts can tell its targets
// apart the same way its k6-metric charts already do.
type deploymentTarget struct {
	PrometheusURL string
	Namespace     string
	Target        string
	// Deployment is the AuthentikDeployment's name, which prefixes its
	// authentik pods' names (<deployment>-authentik-<component>-...).
	Deployment string
}

// deploymentTargetsForTestID resolves every AuthentikDeployment a testid's
// own TestCase drives (one per TestCase.Deployments() entry) - deliberately
// NOT the same resolution prometheusURLsForTestID uses: that one prefers the
// k6 *runner's* cluster (TestCase.Spec.RunnerClusterRef), which can
// legitimately differ from a deployment's own cluster, but the deployment's
// server/worker/postgres pods (what this endpoint reports on) always live on
// that deployment's own clusterRef/namespace regardless of where the runner
// executes. A target that can't be resolved (deleted AuthentikDeployment
// its TestCase never recorded, or
// a WorkloadCluster that hasn't reconciled a PrometheusURL yet) is skipped
// rather than failing the whole testid, the same "one bad entry shouldn't
// blank the rest" reasoning listAlerts already uses.
func (s *Server) deploymentTargetsForTestID(ctx context.Context, testid string) []deploymentTarget {
	var tc benchv1alpha1.TestCase
	if err := s.client.Get(ctx, types.NamespacedName{Name: testid, Namespace: s.cfg.Namespace}, &tc); err != nil {
		return nil
	}
	var targets []deploymentTarget
	for _, ref := range tc.Deployments() {
		cluster, namespace, targetName, ok := s.targetDeployment(ctx, &tc, ref.Name)
		if !ok {
			continue
		}
		promURL := s.prometheusURLForCluster(ctx, cluster)
		if cluster != "" && promURL == "" {
			continue
		}
		targets = append(targets, deploymentTarget{PrometheusURL: promURL, Namespace: namespace, Target: targetName, Deployment: ref.Name})
	}
	return targets
}

// listResourceMetrics answers GET
// /metrics/resources?testids=a,b&resource=cpu|memory&start=&end=[&component=server|worker][&window=][&step=] -
// one summed line per testid's own deployment. component narrows the sum to
// that authentik process's pods (same names profiles.go selects by);
// without it every non-Job pod in the namespace counts, postgres included. Reuses metricsRange's own
// response shape (rangeResponse/RangeSeries), just populated from a
// namespace-scoped cAdvisor query instead of a testid-labeled k6 one, so the
// frontend's existing per-testid chart renderer needs no new type, only a
// different fetch function.
func (s *Server) listResourceMetrics(w http.ResponseWriter, r *http.Request) {
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

	metric, ok := resourceMetrics[q.Get("resource")]
	if !ok {
		writeError(w, r, http.StatusBadRequest, fmt.Sprintf("unknown resource %q", q.Get("resource")))
		return
	}

	component := q.Get("component")
	if component != "" && !slices.Contains(profileComponents, component) {
		writeError(w, r, http.StatusBadRequest, fmt.Sprintf("unknown component %q", component))
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

	resp := rangeResponse{Metric: CompareMetric{Key: q.Get("resource"), Label: metric.Label, Unit: metric.Unit}}
	for _, id := range testids {
		for _, target := range s.deploymentTargetsForTestID(r.Context(), id) {
			podFilter := ""
			if component != "" {
				podFilter = fmt.Sprintf(", pod=~%q", regexp.QuoteMeta(target.Deployment)+"-authentik-"+component+"-.+")
			}
			query := fmt.Sprintf(metric.Query, target.Namespace, window, podFilter)
			points, err := s.promRange(r.Context(), target.PrometheusURL, query, start, end, step)
			if err != nil {
				writeError(w, r, http.StatusBadGateway, err.Error())
				return
			}
			resp.Series = append(resp.Series, RangeSeries{TestID: id, Target: target.Target, Points: points})
		}
	}
	writeJSON(w, r, http.StatusOK, resp)
}
