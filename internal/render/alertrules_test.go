package render

import (
	"strings"
	"testing"

	"github.com/prometheus/common/model"
)

func defaultAlertRules(t *testing.T) []map[string]any {
	t.Helper()
	groups := DefaultAlertRuleGroups()
	if len(groups) != 1 {
		t.Fatalf("got %d rule groups, want 1", len(groups))
	}
	raw := groups[0].(map[string]any)["rules"].([]any)
	rules := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		rules = append(rules, r.(map[string]any))
	}
	return rules
}

// TestDefaultAlertRulesWellFormed checks what Prometheus would reject or
// silently mangle: duplicate names, empty fields, a bad `for` duration and
// unbalanced brackets in an expression.
func TestDefaultAlertRulesWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for _, r := range defaultAlertRules(t) {
		name, _ := r["alert"].(string)
		if name == "" || seen[name] {
			t.Errorf("rule name %q is empty or duplicated", name)
		}
		seen[name] = true
		expr, _ := r["expr"].(string)
		if expr == "" {
			t.Errorf("%s: empty expr", name)
		}
		for _, pair := range []string{"()", "{}", "[]"} {
			if strings.Count(expr, pair[:1]) != strings.Count(expr, pair[1:]) {
				t.Errorf("%s: unbalanced %s in %s", name, pair, expr)
			}
		}
		if _, err := model.ParseDuration(r["for"].(string)); err != nil {
			t.Errorf("%s: for: %v", name, err)
		}
		annotations := r["annotations"].(map[string]any)
		if annotations["summary"] == "" || annotations["description"] == "" {
			t.Errorf("%s: missing summary or description", name)
		}
		if sev := r["labels"].(map[string]any)["severity"]; sev != severityWarning && sev != severityCritical {
			t.Errorf("%s: severity %v", name, sev)
		}
	}
}

// TestDefaultAlertRulesCoverReportedFailureModes pins the rules for the
// failure modes of the production incident report to the metric and label
// values authentik and the postgres exporters really emit.
func TestDefaultAlertRulesCoverReportedFailureModes(t *testing.T) {
	exprs := map[string]string{}
	for _, r := range defaultAlertRules(t) {
		exprs[r["alert"].(string)] = r["expr"].(string)
	}
	want := map[string][]string{
		"AuthentikBenchExpiredModelCleanupStalled": {`actor_name="authentik.core.tasks.clean_expired_models"`, "[30m]"},
		"AuthentikBenchExpiredModelCleanupFailing": {"authentik_tasks_errors_total", "authentik_tasks_total"},
		"AuthentikBenchExpiredModelCleanupSlow":    {"authentik_tasks_duration_milliseconds_sum", "authentik_tasks_duration_milliseconds_count"},
		"AuthentikBenchChannelMessageBacklog":      {`relname="django_channels_postgres_message"`, `(cnpg_)?pg_stat_user_tables_n_live_tup`},
		"AuthentikBenchGroupChannelBacklog":        {`relname="django_channels_postgres_groupchannel"`},
		"AuthentikBenchCacheEntryBacklog":          {`relname="django_postgres_cache_cacheentry"`},
		"AuthentikBenchPolicyEngineLabelCardinality": {
			"authentik_policies_engine_time_total_seconds_count", `obj_type="authentik.policies.models.PolicyBindingModel"`,
		},
		"AuthentikBenchPolicyCacheKeys":   {"authentik_policies_cached"},
		"AuthentikBenchMetricsScrapeSlow": {`scrape_duration_seconds{job="authentik"}`},
	}
	for name, parts := range want {
		expr, ok := exprs[name]
		if !ok {
			t.Errorf("missing alert rule %s", name)
			continue
		}
		for _, part := range parts {
			if !strings.Contains(expr, part) {
				t.Errorf("%s: expr %s does not contain %s", name, expr, part)
			}
		}
	}
}
