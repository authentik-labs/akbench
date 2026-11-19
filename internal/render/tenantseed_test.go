package render

import (
	"encoding/json"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
	"github.com/authentik-labs/akbench/seeder"
)

var exampleTenantParams = benchv1alpha1.TenantParams{Seed: 7, Users: 5000, Groups: 100, GroupDepth: 3, GroupsPerUser: 5}

func TestTenantParamsFromPreset(t *testing.T) {
	got := TenantParamsFromPreset(benchv1alpha1.TenantPresetSpec{
		Description: "ignored", Seed: 7, Users: 5000, Groups: 100, GroupDepth: 3, GroupsPerUser: 5,
	})
	if got != exampleTenantParams {
		t.Errorf("TenantParamsFromPreset = %+v, want %+v", got, exampleTenantParams)
	}
}

func TestSeederParams(t *testing.T) {
	want := `{"seed":7,"users":5000,"groups":100,"group_depth":3,"groups_per_user":5}`
	if got := SeederParams(exampleTenantParams); got != want {
		t.Errorf("SeederParams = %s, want %s", got, want)
	}
}

func TestTenantSeederConfigMap(t *testing.T) {
	cm := TenantSeederConfigMap(exampleBlueprintDeployment())
	if cm.Name != TenantSeederConfigMapName || cm.Namespace != "authentik-1" {
		t.Errorf("ConfigMap is %s/%s, want authentik-1/%s", cm.Namespace, cm.Name, TenantSeederConfigMapName)
	}
	if cm.Data[TenantSeederScriptKey] != seeder.Script || !strings.Contains(seeder.Script, "def main()") {
		t.Errorf("ConfigMap does not hold seed.py under %q", TenantSeederScriptKey)
	}
}

// TestTenantSeedJobName guards what makes a seeding Job new: the same
// deployment, params, script and image give the same name (so a reconcile
// finds the Job it created), any of them changing gives another, and the
// name fits the 63-byte label value the Job controller copies it into.
func TestTenantSeedJobName(t *testing.T) {
	topo := exampleBlueprintDeployment()
	name := TenantSeedJobName(topo, exampleTenantParams, "script")
	if !strings.HasPrefix(name, TenantSeedJobNamePrefix) || len(name) > 63 {
		t.Errorf("TenantSeedJobName = %q, want prefix %q and at most 63 bytes", name, TenantSeedJobNamePrefix)
	}
	if again := TenantSeedJobName(topo, exampleTenantParams, "script"); again != name {
		t.Errorf("TenantSeedJobName is not deterministic: %q vs %q", name, again)
	}

	moreUsers := exampleTenantParams
	moreUsers.Users++
	otherImage := topo.DeepCopy()
	otherImage.Spec.Authentik.Tag = "2026.8.3"
	otherDeployment := topo.DeepCopy()
	otherDeployment.Name = "authentik-2"
	for label, other := range map[string]string{
		"params":     TenantSeedJobName(topo, moreUsers, "script"),
		"script":     TenantSeedJobName(topo, exampleTenantParams, "other script"),
		"image":      TenantSeedJobName(otherImage, exampleTenantParams, "script"),
		"deployment": TenantSeedJobName(otherDeployment, exampleTenantParams, "script"),
	} {
		if other == name {
			t.Errorf("TenantSeedJobName ignores the %s", label)
		}
	}
}

func TestTenantSeedJob(t *testing.T) {
	topo := exampleBlueprintDeployment()
	wc := &benchv1alpha1.WorkloadCluster{Spec: benchv1alpha1.WorkloadClusterSpec{
		WorkerPools: []benchv1alpha1.WorkerPoolSpec{{Name: "ak", Role: benchv1alpha1.WorkerPoolRoleAuthentik}},
	}}
	job := TenantSeedJob(topo, wc, "seed-x", exampleTenantParams)

	if job.Name != "seed-x" || job.Namespace != "authentik-1" {
		t.Errorf("Job is %s/%s, want authentik-1/seed-x", job.Namespace, job.Name)
	}
	if *job.Spec.BackoffLimit != 0 || *job.Spec.ActiveDeadlineSeconds != 7200 || *job.Spec.TTLSecondsAfterFinished != 86400 {
		t.Errorf("backoffLimit/activeDeadlineSeconds/ttl = %d/%d/%d, want 0/7200/86400",
			*job.Spec.BackoffLimit, *job.Spec.ActiveDeadlineSeconds, *job.Spec.TTLSecondsAfterFinished)
	}
	pod := job.Spec.Template.Spec
	if pod.NodeSelector[NodePoolLabel] != "authentik" || len(pod.Tolerations) != 1 {
		t.Errorf("placement = %v %v, want the authentik pool", pod.NodeSelector, pod.Tolerations)
	}
	if job.Spec.Template.Labels[benchv1alpha1.LabelDeployment] != topo.Name {
		t.Errorf("pod labels = %v, want the deployment label", job.Spec.Template.Labels)
	}

	c := pod.Containers[0]
	if c.Image != "ghcr.io/goauthentik/server:2026.8.2" {
		t.Errorf("image = %q, want the deployment's own", c.Image)
	}
	if got := strings.Join(c.Command, " "); got != `sh -c ak shell -c "$(cat /seed/seed.py)"` {
		t.Errorf("command = %q, want the script passed to ak shell -c", got)
	}
	if c.TerminationMessagePolicy != corev1.TerminationMessageFallbackToLogsOnError {
		t.Errorf("terminationMessagePolicy = %q, want FallbackToLogsOnError", c.TerminationMessagePolicy)
	}
	if len(c.EnvFrom) != 1 || c.EnvFrom[0].SecretRef.Name != authentikConfigSecretName(topo) {
		t.Errorf("envFrom = %+v, want the chart's config Secret", c.EnvFrom)
	}

	env := map[string]corev1.EnvVar{}
	for _, e := range c.Env {
		env[e.Name] = e
	}
	var params map[string]int
	if err := json.Unmarshal([]byte(env["TENANT_PARAMS"].Value), &params); err != nil {
		t.Fatalf("TENANT_PARAMS = %q: %v", env["TENANT_PARAMS"].Value, err)
	}
	if params["users"] != 5000 || params["group_depth"] != 3 || len(params) != 5 {
		t.Errorf("TENANT_PARAMS = %v", params)
	}
	ref := env["TENANT_PASSWORD"].ValueFrom
	if ref == nil || ref.SecretKeyRef == nil || ref.SecretKeyRef.Name != "bench-tenant-password" || ref.SecretKeyRef.Key != TenantPasswordKey {
		t.Errorf("TENANT_PASSWORD = %+v, want a reference to %s/%s", env["TENANT_PASSWORD"], TenantPasswordSecretName, TenantPasswordKey)
	}

	if len(c.VolumeMounts) != 1 || c.VolumeMounts[0].MountPath != "/seed" || !c.VolumeMounts[0].ReadOnly {
		t.Errorf("volumeMounts = %+v, want the seeder read-only at /seed", c.VolumeMounts)
	}
	if len(pod.Volumes) != 1 || pod.Volumes[0].ConfigMap == nil || pod.Volumes[0].ConfigMap.Name != TenantSeederConfigMapName {
		t.Errorf("volumes = %+v, want the %s ConfigMap", pod.Volumes, TenantSeederConfigMapName)
	}
}

// TestTenantSeedJobSurvivesDisruptions guards that an evicted, preempted or
// drained seeding pod is retried rather than failing the Job - with
// BackoffLimit 0 it would otherwise leave the deployment Failed until a
// reset, for something that wasn't the seeder's fault - and that the pod
// gives the cluster reasons not to disrupt it in the first place.
func TestTenantSeedJobSurvivesDisruptions(t *testing.T) {
	job := TenantSeedJob(exampleBlueprintDeployment(), nil, "seed-x", exampleTenantParams)
	pod := job.Spec.Template
	if pod.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("restartPolicy = %q, want Never (podFailurePolicy needs it)", pod.Spec.RestartPolicy)
	}
	if got := pod.Annotations["cluster-autoscaler.kubernetes.io/safe-to-evict"]; got != "false" {
		t.Errorf("safe-to-evict annotation = %q, want \"false\"", got)
	}
	if res := pod.Spec.Containers[0].Resources; res.Requests.Cpu().IsZero() || res.Requests.Memory().IsZero() || len(res.Limits) != 0 {
		t.Errorf("resources = %+v, want cpu and memory requests and no limits", res)
	}
	policy := job.Spec.PodFailurePolicy
	if policy == nil || len(policy.Rules) != 1 {
		t.Fatalf("podFailurePolicy = %+v, want one rule", policy)
	}
	rule := policy.Rules[0]
	if rule.Action != batchv1.PodFailurePolicyActionIgnore || rule.OnExitCodes != nil || len(rule.OnPodConditions) != 1 {
		t.Fatalf("podFailurePolicy rule = %+v, want Ignore on a pod condition", rule)
	}
	if cond := rule.OnPodConditions[0]; cond.Type != corev1.DisruptionTarget || cond.Status != corev1.ConditionTrue {
		t.Errorf("podFailurePolicy ignores %s=%s, want DisruptionTarget=True", cond.Type, cond.Status)
	}
}
