package render

import (
	"fmt"
	"slices"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
)

// K6Image is the k6 image every rendered workload runs. Pinned rather than
// left to "latest" so a benchmark run is reproducible (deps/values.yaml). Must be >=0.57.0:
// login.ts uses TypeScript syntax (type-only imports, interfaces, typed
// consts), and k6 only transpiles .ts files natively without extra flags
// from that version on - earlier images fail every run with a script parse
// error before a single request is ever sent.
var K6Image = pinnedImage("k6")

// K6BrowserImage is grafana/k6's browser-capable build (bundled Chromium,
// launched with --no-sandbox per k6's own docs) - not wired to any TestCase
// field yet. Adopting it later means: use this image instead of K6Image,
// set K6_BROWSER_HEADLESS=true, and the script itself must declare
// options.scenarios.<x>.options.browser.type = "chromium" (k6/browser is a
// script-level concern, not something this renderer configures). The exact
// tag is a placeholder - k6 only documents "master-with-browser"; a real
// pinned version-tagged browser build needs confirming against the actual
// image registry before this is ever used.
const K6BrowserImage = "grafana/k6:master-with-browser" // placeholder, unverified tag

// ScriptVolumeMountPath, FixturesMountPath and UsersFilePath match the paths
// the ported login.ts (and any future script) expects, mirroring
// roles/runner/templates/compose.yml.j2's volume layout.
const (
	ScriptVolumeMountPath = "/data/tests"
	FixturesMountPath     = "/data/fixtures"
	UsersFilePath         = FixturesMountPath + "/users.json"
)

// K6WorkloadParams carries everything render needs to build a k6 pod for one
// TestCase execution (a Quick Job or a Burn Deployment), so the render
// functions below stay call-site readable instead of chains of positional
// strings.
type K6WorkloadParams struct {
	TestCase *benchv1alpha1.TestCase
	// DeploymentName is the real AuthentikDeployment object name this
	// specific workload drives - one of TestCase.Deployments(), resolved by
	// the caller per-target. Distinct from TargetName (the k6 `target`
	// metric/tag label, which can be overridden to something else): the
	// deployment's own pods are labeled by object name
	// (render.LabelDeployment), so PodTemplate's anti-affinity has to match
	// on this value, not TargetName, to find them.
	DeploymentName string
	TargetName     string
	// BenchHost/BenchPort are the AuthentikDeployment's in-cluster service
	// endpoint this run drives.
	BenchHost string
	BenchPort int32
	// RemoteWriteURL is the runner cluster's Prometheus remote-write
	// endpoint. Local in M1-M3; the mTLS central receiver from M4 on.
	RemoteWriteURL string
	// ScriptConfigMapName holds the shared library under LibFileName and,
	// unless UserScriptConfigMapName is set, the k6 script under the fixed
	// key "script.ts" (see scriptFileName).
	ScriptConfigMapName string
	// UserScriptConfigMapName is a TestCase's spec.scriptConfigMapRef: its
	// keys are mounted next to the library, so a BYO script can import it too.
	UserScriptConfigMapName string
	// FixturesSecretName holds the generated users.json fixture.
	FixturesSecretName string
	// Dependencies locates the resolved template's
	// TestCaseTemplateSpec.Dependencies (see
	// internal/controller/testdeps.go). Only the ones set render env vars,
	// so a script without dependencies never sees any.
	Dependencies DependencyEndpoints
	// NodeSelector/Tolerations target this TestCase's own k6-pool node (see
	// render.K6PoolPlacement), resolved by the caller from the runner's own
	// WorkloadCluster - empty when that cluster has no K6-role pool (or when
	// the runner is the management cluster itself).
	NodeSelector map[string]string
	Tolerations  []corev1.Toleration
}

// EnvVars builds the exact environment contract k6/scripts/login.ts reads,
// ported unchanged from roles/runner/templates/compose.yml.j2 so the
// resulting metrics carry the same meaning as the Ansible-based harness.
func (p K6WorkloadParams) EnvVars() []corev1.EnvVar {
	tc := p.TestCase
	return append([]corev1.EnvVar{
		{Name: "TEST_ID", Value: tc.Name},
		{Name: "TARGET", Value: p.TargetName},
		{Name: "PROFILE", Value: k6ProfileValue(tc.Spec.Profile)},
		{Name: "BENCH_HOST", Value: p.BenchHost},
		{Name: "BENCH_PORT", Value: fmt.Sprintf("%d", p.BenchPort)},
		{Name: "VUS", Value: fmt.Sprintf("%d", tc.Spec.VUs)},
		{Name: "ITERATIONS", Value: fmt.Sprintf("%d", tc.Spec.Iterations)},
		{Name: "FLOW", Value: tc.Spec.Flow},
		{Name: "QUICK_DURATION", Value: tc.Spec.QuickDuration},
		{Name: "BURN_DURATION", Value: tc.Spec.BurnDuration},
		{Name: "TRACE_SAMPLE_RATE", Value: tc.Spec.TraceSampleRate},
		{Name: "SEED", Value: fmt.Sprintf("%d", tc.Seed())},
		{Name: "USERS_FILE", Value: UsersFilePath},
		{Name: "K6_PROMETHEUS_RW_SERVER_URL", Value: p.RemoteWriteURL},
		{Name: "K6_FEATURES", Value: "native-histograms"},
		{Name: "K6_PROMETHEUS_RW_PUSH_INTERVAL", Value: "1s"},
	}, p.Dependencies.EnvVars()...)
}

// DependencyEndpoints is where a k6 script finds each test dependency its
// template declares. Zero values mean that dependency isn't in use.
type DependencyEndpoints struct {
	// OIDCClientHost/Port: oidc-test-client (oidc-client).
	OIDCClientHost string
	OIDCClientPort int32
	// SAMLClientHost/Port: saml-test-sp (saml-client).
	SAMLClientHost string
	SAMLClientPort int32
	// OIDCSourceSlug: authentik's OAuth source federating to dex (oidc-idp);
	// a login starts at /source/oauth/login/<slug>/.
	OIDCSourceSlug string
	// SAMLSourceSlug: authentik's SAML source federating to saml-test-idp
	// (saml-idp); a login starts at /source/saml/<slug>/.
	SAMLSourceSlug string
}

// EnvVars renders the env vars for the dependencies in use.
func (d DependencyEndpoints) EnvVars() []corev1.EnvVar {
	var vars []corev1.EnvVar
	if d.OIDCClientHost != "" {
		vars = append(vars,
			corev1.EnvVar{Name: "OAUTH_TEST_CLIENT_HOST", Value: d.OIDCClientHost},
			corev1.EnvVar{Name: "OAUTH_TEST_CLIENT_PORT", Value: fmt.Sprintf("%d", d.OIDCClientPort)},
		)
	}
	if d.SAMLClientHost != "" {
		vars = append(vars,
			corev1.EnvVar{Name: "SAML_TEST_SP_HOST", Value: d.SAMLClientHost},
			corev1.EnvVar{Name: "SAML_TEST_SP_PORT", Value: fmt.Sprintf("%d", d.SAMLClientPort)},
		)
	}
	if d.OIDCSourceSlug != "" {
		vars = append(vars,
			corev1.EnvVar{Name: "OIDC_IDP_SOURCE_SLUG", Value: d.OIDCSourceSlug},
			corev1.EnvVar{Name: "OIDC_IDP_USERNAME", Value: DexEmail},
			corev1.EnvVar{Name: "OIDC_IDP_PASSWORD", Value: DexPassword},
		)
	}
	if d.SAMLSourceSlug != "" {
		vars = append(vars,
			corev1.EnvVar{Name: "SAML_IDP_SOURCE_SLUG", Value: d.SAMLSourceSlug},
			corev1.EnvVar{Name: "SAML_IDP_USERNAME", Value: SAMLTestIdPUsername},
			corev1.EnvVar{Name: "SAML_IDP_PASSWORD", Value: SAMLTestIdPPassword},
		)
	}
	return vars
}

func k6ProfileValue(p benchv1alpha1.TestProfile) string {
	if p == benchv1alpha1.ProfileBurn {
		return "burn"
	}
	return "quick"
}

// scriptFileName is the file under ScriptVolumeMountPath k6 is told to run -
// always "script.ts": ensureScriptConfigMap (testcase_controller.go) mirrors
// whichever source wins (scriptConfigMapRef > inline > the resolved
// TestCaseTemplate's own script) under this one fixed key regardless of
// where the content actually came from.
const scriptFileName = "script.ts"

// LibFileName is the shared k6 library (k6/scripts/bench.ts) mounted next to
// every script, which imports it as "./bench.ts".
const LibFileName = "bench.ts"

// Args builds the k6 command line, matching the --tag scheme every Grafana
// dashboard splits on: testid, target, profile, plus any extraTags.
func (p K6WorkloadParams) Args() []string {
	tc := p.TestCase
	keys := make([]string, 0, len(tc.Spec.ExtraTags))
	for k := range tc.Spec.ExtraTags {
		keys = append(keys, k)
	}
	slices.Sort(keys)

	const tagFlag = "--tag"

	args := make([]string, 0, 9+2*len(keys)+1)
	args = append(args,
		"run",
		"--out", "experimental-prometheus-rw",
		tagFlag, "testid="+tc.Name,
		tagFlag, "target="+p.TargetName,
		tagFlag, "profile="+k6ProfileValue(tc.Spec.Profile),
	)
	for _, k := range keys {
		args = append(args, tagFlag, fmt.Sprintf("%s=%s", k, tc.Spec.ExtraTags[k]))
	}
	args = append(args, ScriptVolumeMountPath+"/"+scriptFileName)
	return args
}

func (p K6WorkloadParams) volumes() ([]corev1.Volume, []corev1.VolumeMount) {
	sources := []corev1.VolumeProjection{{
		ConfigMap: &corev1.ConfigMapProjection{Name: p.ScriptConfigMapName},
	}}
	if p.UserScriptConfigMapName != "" {
		sources[0].ConfigMap.Items = []corev1.KeyToPath{{Key: LibFileName, Path: LibFileName}}
		sources = append(sources, corev1.VolumeProjection{
			ConfigMap: &corev1.ConfigMapProjection{Name: p.UserScriptConfigMapName},
		})
	}
	volumes := []corev1.Volume{
		{
			Name:      "scripts",
			Projected: &corev1.ProjectedVolumeSource{Sources: sources},
		},
		{
			Name:   "fixtures",
			Secret: &corev1.SecretVolumeSource{SecretName: p.FixturesSecretName},
		},
	}
	mounts := []corev1.VolumeMount{
		{Name: "scripts", MountPath: ScriptVolumeMountPath, ReadOnly: true},
		{Name: "fixtures", MountPath: FixturesMountPath, ReadOnly: true},
	}
	return volumes, mounts
}

// DefaultRunnerResources is what a k6 container gets when its TestCase sets
// no runnerResources. The CPU request keeps the load generator from being
// starved by a co-scheduled pod (a starved k6 under-reports throughput and
// over-reports latency); there is deliberately no CPU limit, since CFS
// throttling would skew the numbers the same way. Memory is capped at the
// request so an oversized VU count OOMs loudly instead of swapping.
func DefaultRunnerResources() corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("1"),
			corev1.ResourceMemory: resource.MustParse("512Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceMemory: resource.MustParse("512Mi"),
		},
	}
}

// PodTemplate builds the k6 pod template shared by both the Quick Job and the
// Burn Deployment.
func (p K6WorkloadParams) PodTemplate() corev1.PodTemplateSpec {
	resources := DefaultRunnerResources()
	if p.TestCase.Spec.RunnerResources != nil {
		resources = *p.TestCase.Spec.RunnerResources
	}
	volumes, mounts := p.volumes()
	labels := map[string]string{
		benchv1alpha1.LabelTestCase:   p.TestCase.Name,
		benchv1alpha1.LabelProfile:    k6ProfileValue(p.TestCase.Spec.Profile),
		benchv1alpha1.LabelDeployment: p.DeploymentName,
	}
	return corev1.PodTemplateSpec{
		Labels: labels,
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{
				{
					Name:         "k6",
					Image:        K6Image,
					Args:         p.Args(),
					Env:          p.EnvVars(),
					VolumeMounts: mounts,
					Resources:    resources,
				},
			},
			Volumes:      volumes,
			NodeSelector: p.NodeSelector,
			Tolerations:  p.Tolerations,
			// The load generator must never land on the same node as the
			// deployment it's driving - otherwise it competes with the very
			// system it's measuring for that node's CPU, skewing the
			// numbers it produces. Required, not preferred: a soft
			// preference could still be overridden by the scheduler under
			// pressure.
			Affinity: &corev1.Affinity{
				PodAntiAffinity: &corev1.PodAntiAffinity{
					RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{
						sameDeploymentAntiAffinityTerm(p.DeploymentName),
					},
				},
			},
		},
	}
}

// BurnDeploymentName is the one place this name is computed, shared by the
// controller (to create/find/clean up) and anything else that needs to
// reproduce it. deploymentName is the AuthentikDeployment *object* name
// (TestCase.Deployments() entry), not the k6 TargetName metric label -
// object names are guaranteed unique/DNS-safe within the namespace, the
// metric label isn't. Always included, even when a TestCase drives only one
// deployment: a fixed, never-suffixed alternative would need permanent
// special-casing throughout the controller for no real benefit - Burn
// Deployments are stateless traffic generators, so the one visible side
// effect of this naming (an already-running single-deployment Burn
// TestCase's load generator gets recreated under its new name the first
// time this ships) costs nothing but a brief restart.
func BurnDeploymentName(testCaseName, deploymentName string) string {
	return "k6-" + testCaseName + "-" + deploymentName
}

// QuickJob builds the immutable, individually-inspectable Job a TestRun owns
// for a Quick profile execution. backoffLimit is 0: a failed quick run should
// surface as a failed TestRun, not retry silently.
func QuickJob(name, namespace string, p K6WorkloadParams) *batchv1.Job {
	backoffLimit := int32(0)
	return &batchv1.Job{
		Name:      name,
		Namespace: namespace,
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoffLimit,
			Template:     p.PodTemplate(),
		},
	}
}

// BurnDeployment builds the self-healing, single-replica Deployment a Burn
// TestCase owns directly, mirroring docker-compose's
// `restart: unless-stopped`. Recreate strategy guarantees the old pod is
// fully gone before a config change starts a new one, so a target is never
// hit by two overlapping burn pods.
//
// Selector includes LabelDeployment alongside LabelTestCase: a TestCase
// driving more than one deployment renders one BurnDeployment per target,
// all sharing the same LabelTestCase value - without LabelDeployment too,
// every one of those Deployments would have an identical selector and fight
// over each other's pods (Kubernetes requires disjoint selectors across
// Deployments). Both labels are already on the pod template (PodTemplate);
// this just mirrors them onto the selector that has to agree with it.
func BurnDeployment(name, namespace string, p K6WorkloadParams) *appsv1.Deployment {
	replicas := int32(1)
	tmpl := p.PodTemplate()
	tmpl.Spec.RestartPolicy = corev1.RestartPolicyAlways
	selector := &metav1.LabelSelector{MatchLabels: map[string]string{
		benchv1alpha1.LabelTestCase:   p.TestCase.Name,
		benchv1alpha1.LabelDeployment: p.DeploymentName,
	}}
	return &appsv1.Deployment{
		Name:      name,
		Namespace: namespace,
		// Object-level labels (distinct from the pod template's own,
		// above) so the controller can List every Burn Deployment
		// belonging to a TestCase - needed to clean up a target that's
		// been removed from DeploymentRefs, since there's no single
		// fixed name to Delete by once a TestCase can have more than one.
		Labels: selector.MatchLabels,
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: selector,
			// Recreate, not RollingUpdate: a rollout would briefly run old
			// and new k6 pods side by side, doubling the load on the target
			// (and skewing its metrics) for the overlap.
			Strategy: appsv1.DeploymentStrategy{
				Type: appsv1.RecreateDeploymentStrategyType,
			},
			Template: tmpl,
		},
	}
}
