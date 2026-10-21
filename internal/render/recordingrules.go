package render

// recordingRulesKey is the k6 recording rules' key in
// kube-prometheus-stack's additionalPrometheusRulesMap, next to
// defaultAlertRulesKey.
const recordingRulesKey = "k6-recording-rules"

// Recorded k6 series, pre-aggregated to one series per (testid, target)
// every minute. The compare endpoint's summary over a long Burn
// reads these instead of the raw k6 series: k6 tags every sample with its
// full request context (url, method, status, ...), so rate(raw[3d]) scans
// thousands of series' worth of samples on every poll, where
// avg_over_time(recorded[3d]) scans one sample per minute per target.
const (
	RecordedHTTPReqs          = "testid_target:k6_http_reqs:rate1m"
	RecordedHTTPReqsFailed    = "testid_target:k6_http_reqs_failed:rate1m"
	RecordedIterations        = "testid_target:k6_iterations:rate1m"
	RecordedHTTPReqDuration   = "testid_target:k6_http_req_duration_seconds:rate1m"
	RecordedIterationDuration = "testid_target:k6_iteration_duration_seconds:rate1m"
)

// K6RecordingRuleGroups builds the recording rules behind the Recorded*
// series. The two durations are native histograms (K6_FEATURES=
// native-histograms), which recording rules store as native histograms too,
// so histogram_quantile over sum_over_time of them still works.
func K6RecordingRuleGroups() []any {
	record := func(name, expr string) map[string]any {
		return map[string]any{"record": name, "expr": expr}
	}
	by := "sum by (testid, target) "
	return []any{
		map[string]any{
			"name":     "authentik-bench.k6.rules",
			"interval": "1m",
			"rules": []any{
				record(RecordedHTTPReqs, by+`(rate(k6_http_reqs_total[1m]))`),
				record(RecordedHTTPReqsFailed, by+`(rate(k6_http_reqs_total{expected_response="false"}[1m]))`),
				record(RecordedIterations, by+`(rate(k6_iterations_total[1m]))`),
				record(RecordedHTTPReqDuration, by+`(rate(k6_http_req_duration_seconds[1m]))`),
				record(RecordedIterationDuration, by+`(rate(k6_iteration_duration_seconds[1m]))`),
			},
		},
	}
}
