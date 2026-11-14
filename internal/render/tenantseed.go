package render

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
	"github.com/authentik-labs/akbench/seeder"
)

// TenantSeederConfigMapName holds the seeder script (seeder/seed.py) under
// TenantSeederScriptKey, in a deployment's own namespace, for
// TenantSeedJob to mount.
const (
	TenantSeederConfigMapName = "tenant-seeder"
	TenantSeederScriptKey     = "seed.py"
)

// TenantPasswordSecretName is the Secret holding the password every seeded
// user gets, under TenantPasswordKey. The operator generates it once per
// namespace on the management cluster, and mirrors it under the same name
// into a deployment's own namespace for the seeding Job and into a test
// case's namespace on its runner cluster for k6. One name for all three, so
// nothing can render a reference to a Secret by another name.
const (
	TenantPasswordSecretName = "bench-tenant-password"
	TenantPasswordKey        = "password"
)

// TenantSeedJobNamePrefix starts every TenantSeedJobName, which is how a
// deployment reset finds the seeding Jobs to delete.
const TenantSeedJobNamePrefix = "seed-"

// clusterAutoscalerSafeToEvict, set to "false" on a pod, keeps
// cluster-autoscaler from scaling the pod's node down while it runs.
const clusterAutoscalerSafeToEvict = "cluster-autoscaler.kubernetes.io/safe-to-evict"

// tenantSeederMountPath is where TenantSeedJob mounts the seeder ConfigMap.
const tenantSeederMountPath = "/seed"

// tenantSeederResources are the seeding container's requests. The seeder is
// one Python process running authentik's own code (`ak shell`), which takes
// a few hundred MiB before it does anything; on top of that it holds one
// batch of rows at a time (seed.py's BATCH_SIZE, 5,000 users or
// memberships), tens of MiB, whatever the preset's size. It mostly waits on
// Postgres, so half a CPU is plenty. Requests only, no limits: they keep it
// from being the first pod a busy node gives up, and a limit would only
// turn a slow or large run into an OOM kill.
func tenantSeederResources() corev1.ResourceRequirements {
	return corev1.ResourceRequirements{Requests: corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("500m"),
		corev1.ResourceMemory: resource.MustParse("512Mi"),
	}}
}

// tenantSeedDeadline caps a seeding run. 100,000 users are extrapolated to
// take a minute or two, never measured, and the huge preset has five times
// that; two hours leaves plenty of room for a slow database without letting
// a stuck run hold the deployment back forever.
const tenantSeedDeadline = 2 * 60 * 60

// TenantParamsFromPreset copies the numbers out of a preset, leaving out its
// description.
func TenantParamsFromPreset(spec benchv1alpha1.TenantPresetSpec) benchv1alpha1.TenantParams {
	return benchv1alpha1.TenantParams{
		Seed:          spec.Seed,
		Users:         spec.Users,
		Groups:        spec.Groups,
		GroupDepth:    spec.GroupDepth,
		GroupsPerUser: spec.GroupsPerUser,
	}
}

// seederParams are a tenant's numbers under the seeder's own names (seed.py's
// PARAMS).
type seederParams struct {
	Seed          int32 `json:"seed"`
	Users         int32 `json:"users"`
	Groups        int32 `json:"groups"`
	GroupDepth    int32 `json:"group_depth"`
	GroupsPerUser int32 `json:"groups_per_user"`
}

// SeederParams is p as the seeder's TENANT_PARAMS: a JSON object under the
// seeder's snake_case names, the one place the API's camelCase names meet
// them. test/contract checks the keys against seed.py. A struct of ints
// always marshals, in field order, so the same params always give the same
// text.
func SeederParams(p benchv1alpha1.TenantParams) string {
	b, _ := json.Marshal(seederParams(p))
	return string(b)
}

// TenantSeederConfigMap holds the seeder script in t's namespace on its
// cluster. It is the same for every deployment and every preset; what
// differs goes into the Job's environment.
func TenantSeederConfigMap(t *benchv1alpha1.AuthentikDeployment) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		Name: TenantSeederConfigMapName, Namespace: t.Spec.Namespace,
		Data: map[string]string{TenantSeederScriptKey: seeder.Script},
	}
}

// TenantSeedJobName is a hash of everything that makes one seeding run
// differ from another - the deployment, the params, the script and the
// image - for the same reasons as BlueprintJobName: a Job can't be changed
// once created, and its name has to fit in a 63-byte label value.
func TenantSeedJobName(t *benchv1alpha1.AuthentikDeployment, params benchv1alpha1.TenantParams, script string) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "%s/%s/%s/%s", t.Name, SeederParams(params), script, authentikImage(t)))
	return TenantSeedJobNamePrefix + hex.EncodeToString(sum[:])[:16]
}

// TenantSeedJob runs seeder/seed.py inside the deployment's own authentik
// image, with the same database settings as its server (the chart's config
// Secret, as BlueprintJob uses), so the tenant is written through that
// build's own models.
//
// The script goes to `ak shell -c`, not stdin: the shell command only reads
// stdin if a script is already waiting there when it looks, and otherwise
// opens an interactive prompt.
//
// A failure is not retried (BackoffLimit 0): the seeder fails the same way
// every time for the same database, and a failure leaves nothing behind. Its
// last line says why ("seeding failed: ..."), which FallbackToLogsOnError
// turns into the container's termination message for the controller to
// report. A pod that was disrupted rather than failed - evicted, preempted,
// or its node drained or scaled down - is different: the podFailurePolicy
// doesn't count it, so the Job starts another pod, which seeds from scratch
// (the killed run's transaction was rolled back) or finds the tenant already
// there (it had just committed). The pod also asks cluster-autoscaler not to
// scale its node down while it runs. The descheduler the workload clusters
// run (DeschedulerHelmValues) offers no such opt-out under the chart's
// default policy: it leaves noEvictionPolicy at Preferred, under which the
// descheduler.alpha.kubernetes.io/prefer-no-eviction annotation stops
// nothing, and turns off the protection of pods with local storage. But it
// evicts through the Eviction API, which marks the pod as disrupted, and of
// pods of the same priority it evicts those without requests first.
//
// The Job is TTL-deleted after a day; what was seeded is recorded in the
// deployment's status, not here.
func TenantSeedJob(t *benchv1alpha1.AuthentikDeployment, wc *benchv1alpha1.WorkloadCluster, name string, params benchv1alpha1.TenantParams) *batchv1.Job {
	nodeSelector, tolerations := AuthentikPoolPlacement(wc)
	scriptPath := tenantSeederMountPath + "/" + TenantSeederScriptKey
	return &batchv1.Job{
		Name: name, Namespace: t.Spec.Namespace,
		Spec: batchv1.JobSpec{
			BackoffLimit: new(int32(0)),
			PodFailurePolicy: &batchv1.PodFailurePolicy{Rules: []batchv1.PodFailurePolicyRule{{
				Action: batchv1.PodFailurePolicyActionIgnore,
				OnPodConditions: []batchv1.PodFailurePolicyOnPodConditionsPattern{{
					Type: corev1.DisruptionTarget, Status: corev1.ConditionTrue,
				}},
			}}},
			ActiveDeadlineSeconds:   new(int64(tenantSeedDeadline)),
			TTLSecondsAfterFinished: new(int32(24 * 60 * 60)),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      DeploymentLabels(t.Name),
					Annotations: map[string]string{clusterAutoscalerSafeToEvict: "false"},
				},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					NodeSelector:  nodeSelector,
					Tolerations:   tolerations,
					Containers: []corev1.Container{{
						Name:    "seed-tenant",
						Image:   authentikImage(t),
						Command: []string{"sh", "-c", fmt.Sprintf(`ak shell -c "$(cat %s)"`, scriptPath)},
						EnvFrom: []corev1.EnvFromSource{
							{SecretRef: &corev1.SecretEnvSource{Name: authentikConfigSecretName(t)}},
						},
						Env: []corev1.EnvVar{
							{Name: "TENANT_PARAMS", Value: SeederParams(params)},
							{Name: "TENANT_PASSWORD", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
								Name: TenantPasswordSecretName, Key: TenantPasswordKey,
							}}},
						},
						VolumeMounts: []corev1.VolumeMount{
							{Name: "seeder", MountPath: tenantSeederMountPath, ReadOnly: true},
						},
						Resources:                tenantSeederResources(),
						TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
					}},
					Volumes: []corev1.Volume{{
						Name:      "seeder",
						ConfigMap: &corev1.ConfigMapVolumeSource{Name: TenantSeederConfigMapName},
					}},
				},
			},
		},
	}
}
