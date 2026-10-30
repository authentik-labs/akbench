package render

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
)

// blueprintSourceMountPath is where a blueprint-source ConfigMap (see
// BlueprintJob) is mounted, read-only, in the Job's pod - separate from
// BlueprintMountPath (the PVC): ak apply_blueprint requires its argument to
// live under the chart's own configured blueprints_dir, which a ConfigMap
// volume can't itself be mounted at without also being writable there (see
// BlueprintMountPath's own doc comment) - the Job's own command copies from
// here to there first.
const blueprintSourceMountPath = "/blueprints/source"

// authentikConfigSecretName is the authentik Helm chart's own generated
// Secret (templates/secret.yaml, named via its `authentik.fullname` helper)
// holding every derived AUTHENTIK_* env var (secret_key, postgresql.*,
// log_level, error_reporting...) - confirmed directly against a live
// install (kubectl get secrets), not assumed. BlueprintJob's container
// envFrom's this exact Secret so ak apply_blueprint (a Django management
// command, not a self-contained tool) gets a real database connection,
// identical to what the server/worker pods themselves run with.
func authentikConfigSecretName(t *benchv1alpha1.AuthentikDeployment) string {
	return AuthentikFullname(HelmReleaseName(t))
}

// BlueprintJobName is deterministic from the deployment name, a caller-given
// label distinguishing which blueprint this is (e.g. "fixtures", or an
// oauth-oidc TestCase's own name), and the blueprint content - so a content
// change creates a new Job (Jobs are immutable once created, there's no
// "update and re-run"), while re-applying identical content is a no-op: the
// caller's create call just hits AlreadyExists for the same name, the same
// idempotent-apply convention every other addon in this codebase already
// follows.
//
// The whole name is a single hash, not <deployment>-blueprint-<label>-<hash>
// as the obvious/readable version would be: Kubernetes' Job controller
// copies a Job's own name verbatim onto its pods as the "job-name" label
// value, and label values are capped at 63 bytes - confirmed live, a
// deployment name + label + hash comfortably exceeds that for realistic
// names (e.g. a longer TestCase name), and the create call fails outright
// ("must be no more than 63 bytes") rather than silently truncating.
//
// The image is hashed in too, so a Job stuck on an image that can't be
// pulled is replaced once the image changes, instead of blocking forever.
func BlueprintJobName(t *benchv1alpha1.AuthentikDeployment, label, content string) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "%s/%s/%s/%s", t.Name, label, content, blueprintImage(t)))
	return BlueprintJobNamePrefix + hex.EncodeToString(sum[:])[:16]
}

// blueprintImage is the authentik image the deployment runs. With no tag
// set, the chart uses its own appVersion, which matches the chart version
// (there is no :latest tag), so this falls back to the installed chart
// version. Blueprint Jobs are only created once the HelmRelease is Ready,
// by which point status.chartVersion is recorded.
func blueprintImage(t *benchv1alpha1.AuthentikDeployment) string {
	image := t.Spec.Authentik.Image
	if image == "" {
		image = "ghcr.io/goauthentik/server"
	}
	tag := t.Spec.Authentik.Tag
	if tag == "" {
		tag = ChartVersion(t)
	}
	return image + ":" + tag
}

// BlueprintJobNamePrefix starts every BlueprintJobName, which is how a
// deployment reset finds the import Jobs to delete so they run again.
const BlueprintJobNamePrefix = "blueprint-"

// BlueprintJob renders a one-shot Job that imports a blueprint: it mounts
// sourceConfigMapName (read-only, the blueprint content under
// sourceConfigMapKey) and BlueprintPVC (writable, under the chart's own
// blueprints_dir), copies the former onto the latter, and runs
// `ak apply_blueprint`.
//
// Deliberately not mounted into the server/worker Deployment at all (see
// HelmValues, which no longer references BlueprintPVC in any way): a Job's
// pod releases the PVC as soon as it completes, rather than holding it for
// the Deployment's entire lifetime, which is what caused a real RWO-volume
// rolling-update deadlock - old and new server pods both wanting to attach
// the same single-attach volume at once - every time the deployment's pod
// template changed for any unrelated reason at all (confirmed live: a
// label-only change was enough to trigger it).
//
// TTLSecondsAfterFinished lets Kubernetes' own TTL controller garbage
// collect a completed import Job on its own; content-hashed names mean
// these otherwise accumulate one object per distinct blueprint version
// forever.
func BlueprintJob(t *benchv1alpha1.AuthentikDeployment, wc *benchv1alpha1.WorkloadCluster, name, sourceConfigMapName, sourceConfigMapKey string) *batchv1.Job {
	targetPath := BlueprintMountPath + "/" + BlueprintFileName
	sourcePath := blueprintSourceMountPath + "/" + sourceConfigMapKey

	nodeSelector, tolerations := AuthentikPoolPlacement(wc)
	return &batchv1.Job{
		Kind: "Job", APIVersion: "batch/v1",
		Name: name, Namespace: t.Spec.Namespace,
		Spec: batchv1.JobSpec{
			BackoffLimit:            new(int32(3)),
			TTLSecondsAfterFinished: new(int32(3600)),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: DeploymentLabels(t.Name)},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					NodeSelector:  nodeSelector,
					Tolerations:   tolerations,
					// A freshly provisioned csi-incus block volume is
					// root:root-owned, unwritable by the authentik image's
					// non-root user without this - the same fix HelmValues
					// used to need on the server Deployment before this PVC
					// moved here (see fsGroupPolicy: File on csi-incus's own
					// CSIDriver).
					SecurityContext: &corev1.PodSecurityContext{FSGroup: new(int64(1000))},
					Containers: []corev1.Container{{
						Name:    "apply-blueprint",
						Image:   blueprintImage(t),
						Command: []string{"sh", "-c", fmt.Sprintf("cp %s %s && ak apply_blueprint %s", sourcePath, targetPath, targetPath)},
						EnvFrom: []corev1.EnvFromSource{
							{SecretRef: &corev1.SecretEnvSource{Name: authentikConfigSecretName(t)}},
						},
						VolumeMounts: []corev1.VolumeMount{
							{Name: "blueprint-source", MountPath: blueprintSourceMountPath, ReadOnly: true},
							{Name: "blueprint-target", MountPath: BlueprintMountPath},
						},
					}},
					Volumes: []corev1.Volume{
						{
							Name:      "blueprint-source",
							ConfigMap: &corev1.ConfigMapVolumeSource{Name: sourceConfigMapName},
						},
						{
							Name:                  "blueprint-target",
							PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: BlueprintPVCName(t)},
						},
					},
				},
			},
		},
	}
}
