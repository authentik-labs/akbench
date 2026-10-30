package render

import (
	"strings"
	"testing"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
)

// TestBlueprintJobTargetsAuthentikPool guards a real live-cluster bug: this
// Job must reach the authentik pool (it needs the same namespace/PVC/Secret
// the server/worker pods there have), but originally had no pool placement
// at all - once a cluster's nodes were all tainted by pool, it couldn't
// schedule anywhere ("0/N nodes are available: N node(s) had untolerated
// taint(s)"), silently blocking every fixtures/OAuth blueprint import.
func TestBlueprintJobTargetsAuthentikPool(t *testing.T) {
	topo := exampleBlueprintDeployment()
	wc := &benchv1alpha1.WorkloadCluster{Spec: benchv1alpha1.WorkloadClusterSpec{
		WorkerPools: []benchv1alpha1.WorkerPoolSpec{{Name: "ak", Role: benchv1alpha1.WorkerPoolRoleAuthentik}},
	}}
	job := BlueprintJob(topo, wc, "blueprint-x", "cm", "key")
	if job.Spec.Template.Spec.NodeSelector[NodePoolLabel] != "authentik" {
		t.Errorf("nodeSelector[%s] = %v, want authentik", NodePoolLabel, job.Spec.Template.Spec.NodeSelector[NodePoolLabel])
	}
	if len(job.Spec.Template.Spec.Tolerations) != 1 {
		t.Errorf("tolerations = %v, want exactly one", job.Spec.Template.Spec.Tolerations)
	}
}

func exampleBlueprintDeployment() *benchv1alpha1.AuthentikDeployment {
	return &benchv1alpha1.AuthentikDeployment{
		Name: "authentik-1",
		Spec: benchv1alpha1.AuthentikDeploymentSpec{
			Namespace: "authentik-1",
			Authentik: benchv1alpha1.AuthentikSpec{Image: "ghcr.io/goauthentik/server", Tag: "2026.8.2"},
		},
	}
}

// TestBlueprintJobNameChangesWithContent guards the two properties that
// make re-applying an unchanged blueprint a no-op and a changed one a fresh
// import (the Job name must be a pure function of deployment/label/content),
// and that it always fits Kubernetes' 63-byte label-value cap regardless of
// how long the deployment/TestCase names are (the Job controller copies a
// Job's own name verbatim onto its pods as the "job-name" label value -
// confirmed live that a deployment-name-and-label-prefixed name blows past
// this for realistic TestCase names, hence the fixed-length hash-only form).
func TestBlueprintJobNameChangesWithContent(t *testing.T) {
	topo := exampleBlueprintDeployment()
	a := BlueprintJobName(topo, "fixtures", "content-a")
	b := BlueprintJobName(topo, "fixtures", "content-b")
	aAgain := BlueprintJobName(topo, "fixtures", "content-a")
	if a == b {
		t.Errorf("BlueprintJobName gave the same name for different content: %q", a)
	}
	if a != aAgain {
		t.Errorf("BlueprintJobName is not deterministic: %q vs %q", a, aAgain)
	}
	if len(a) > 63 {
		t.Errorf("BlueprintJobName = %q (%d bytes), want <= 63 bytes (Kubernetes' own label-value cap)", a, len(a))
	}
	longLabel := "oauth-login-a-very-long-testcase-name-that-a-real-user-might-actually-pick"
	if got := BlueprintJobName(topo, longLabel, "content-a"); len(got) > 63 {
		t.Errorf("BlueprintJobName with a long label = %q (%d bytes), want <= 63 bytes", got, len(got))
	}
	if !strings.HasPrefix(a, "blueprint-") {
		t.Errorf("BlueprintJobName = %q, want prefix %q", a, "blueprint-")
	}
}

// TestBlueprintJobMountsConfigMapAndPVCNotDeployment guards the actual point
// of this design (see BlueprintJob's own doc comment): the blueprint PVC is
// mounted only by this one-shot Job, never by the server/worker Deployment
// (HelmValues no longer references it at all) - a real RWO-volume
// rolling-update deadlock (old and new server pod both wanting the same
// single-attach volume) motivated this move.
func TestBlueprintJobMountsConfigMapAndPVCNotDeployment(t *testing.T) {
	topo := exampleBlueprintDeployment()
	job := BlueprintJob(topo, nil, "authentik-1-blueprint-fixtures-abcd1234", "bench-fixtures-blueprint", "blueprint.yaml")

	if job.Namespace != "authentik-1" {
		t.Errorf("job namespace = %q, want %q", job.Namespace, "authentik-1")
	}
	if job.Spec.Template.Spec.RestartPolicy != "Never" {
		t.Errorf("restartPolicy = %q, want Never", job.Spec.Template.Spec.RestartPolicy)
	}

	sc := job.Spec.Template.Spec.SecurityContext
	if sc == nil || sc.FSGroup == nil || *sc.FSGroup != 1000 {
		t.Errorf("pod securityContext.fsGroup = %v, want 1000 (csi-incus volumes are root-owned by default)", sc)
	}

	container := job.Spec.Template.Spec.Containers[0]
	if len(container.EnvFrom) != 1 || container.EnvFrom[0].SecretRef == nil || container.EnvFrom[0].SecretRef.Name != "authentik-1-authentik" {
		t.Errorf("container.envFrom = %+v, want the chart's own generated config secret authentik-1-authentik", container.EnvFrom)
	}

	var hasConfigMapVolume, hasPVCVolume bool
	for _, v := range job.Spec.Template.Spec.Volumes {
		if v.ConfigMap != nil && v.ConfigMap.Name == "bench-fixtures-blueprint" {
			hasConfigMapVolume = true
		}
		if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == BlueprintPVCName(topo) {
			hasPVCVolume = true
		}
	}
	if !hasConfigMapVolume {
		t.Error("job has no ConfigMap volume for the blueprint source")
	}
	if !hasPVCVolume {
		t.Error("job has no PVC volume for BlueprintMountPath")
	}

	if !strings.Contains(container.Command[len(container.Command)-1], "ak apply_blueprint "+BlueprintMountPath) {
		t.Errorf("container command = %v, want it to run ak apply_blueprint against %s", container.Command, BlueprintMountPath)
	}
}

// TestBlueprintJobImageFallsBackToChartVersion: with no tag set, the Job
// runs the installed chart's version (its appVersion), never :latest, and
// a changed image gets a new Job.
func TestBlueprintJobImageFallsBackToChartVersion(t *testing.T) {
	topo := exampleBlueprintDeployment()
	tagged := BlueprintJobName(topo, "fixtures", "content")
	topo.Spec.Authentik.Tag = ""
	topo.Status.ChartVersion = "2026.8.5"
	job := BlueprintJob(topo, nil, "job", "cm", "key")
	if got := job.Spec.Template.Spec.Containers[0].Image; got != "ghcr.io/goauthentik/server:2026.8.5" {
		t.Errorf("image = %q, want the chart version as tag", got)
	}
	if BlueprintJobName(topo, "fixtures", "content") == tagged {
		t.Error("BlueprintJobName didn't change with the image")
	}
}
