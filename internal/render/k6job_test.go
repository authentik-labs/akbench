package render

import (
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
)

// TestPodTemplateAppliesK6PoolPlacement confirms PodTemplate passes
// NodeSelector/Tolerations straight through onto the pod spec - the
// controller resolves these via K6PoolPlacement before building
// K6WorkloadParams, so this just guards the rendering side of that wiring.
func TestPodTemplateAppliesK6PoolPlacement(t *testing.T) {
	p := K6WorkloadParams{
		TestCase:     &benchv1alpha1.TestCase{Name: "login-a"},
		NodeSelector: map[string]string{NodePoolLabel: "k6"},
		Tolerations:  []corev1.Toleration{{Key: NodePoolLabel, Value: "k6", Effect: corev1.TaintEffectNoSchedule}},
	}
	tmpl := p.PodTemplate()
	if tmpl.Spec.NodeSelector[NodePoolLabel] != "k6" {
		t.Errorf("NodeSelector = %v, want %s=k6", tmpl.Spec.NodeSelector, NodePoolLabel)
	}
	if len(tmpl.Spec.Tolerations) != 1 || tmpl.Spec.Tolerations[0].Value != "k6" {
		t.Errorf("Tolerations = %v, want one matching k6", tmpl.Spec.Tolerations)
	}
}

// TestPodTemplateMountsLibNextToBYOScript guards that a BYO script
// ConfigMap still gets the shared library mounted next to it.
func TestPodTemplateMountsLibNextToBYOScript(t *testing.T) {
	p := K6WorkloadParams{
		TestCase:                &benchv1alpha1.TestCase{Name: "login-a"},
		ScriptConfigMapName:     "k6-script-login-a",
		UserScriptConfigMapName: "my-script",
	}
	var sources []corev1.VolumeProjection
	for _, v := range p.PodTemplate().Spec.Volumes {
		if v.Name == "scripts" && v.Projected != nil {
			sources = v.Projected.Sources
		}
	}
	if len(sources) != 2 ||
		sources[0].ConfigMap.Name != "k6-script-login-a" || len(sources[0].ConfigMap.Items) != 2 ||
		sources[0].ConfigMap.Items[0].Key != LibFileName || sources[0].ConfigMap.Items[1].Key != StartAtFileName ||
		sources[1].ConfigMap.Name != "my-script" || sources[1].ConfigMap.Items != nil {
		t.Errorf("scripts volume sources = %+v, want only %s and %s from the generated ConfigMap, then all of my-script", sources, LibFileName, StartAtFileName)
	}
}

// TestPodTemplateNeverSharesNodeWithItsDeployment guards the k6 runner's own
// required anti-affinity: it must never land on the same node as the
// deployment it's driving (DeploymentName, not TargetName, which is only a
// metric label and can differ), so the load generator never competes with
// the system under test for the same node's CPU.
func TestPodTemplateNeverSharesNodeWithItsDeployment(t *testing.T) {
	p := K6WorkloadParams{
		TestCase: &benchv1alpha1.TestCase{
			Name: "login-a",
			Spec: benchv1alpha1.TestCaseSpec{
				DeploymentRefs: []corev1.LocalObjectReference{{Name: "topo-a"}},
				Profile:        benchv1alpha1.ProfileBurn,
			},
		},
		DeploymentName: "topo-a",
		TargetName:     "some-other-display-name",
	}
	tmpl := p.PodTemplate()

	if got := tmpl.Labels[benchv1alpha1.LabelDeployment]; got != "topo-a" {
		t.Errorf("pod template deployment label = %q, want topo-a", got)
	}
	if tmpl.Spec.Affinity == nil || tmpl.Spec.Affinity.PodAntiAffinity == nil {
		t.Fatal("pod template has no PodAntiAffinity")
	}
	terms := tmpl.Spec.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution
	if len(terms) != 1 {
		t.Fatalf("got %d required anti-affinity terms, want 1", len(terms))
	}
	if got := terms[0].LabelSelector.MatchLabels[benchv1alpha1.LabelDeployment]; got != "topo-a" {
		t.Errorf("required term matches deployment=%q, want topo-a", got)
	}
	if terms[0].TopologyKey != "kubernetes.io/hostname" {
		t.Errorf("required term topologyKey = %q, want kubernetes.io/hostname", terms[0].TopologyKey)
	}
}

// TestBurnDeploymentSelectorIsUniquePerDeployment guards a real bug: a
// TestCase driving more than one deployment renders one BurnDeployment per
// target, all sharing the same TestCase name - a selector keyed only on
// LabelTestCase would make every one of those Deployments identical and
// have them fight over each other's pods, which Kubernetes forbids anyway
// (two Deployments can't have equal selectors). LabelDeployment must be in
// the selector too, disambiguating one target from another.
func TestBurnDeploymentSelectorIsUniquePerDeployment(t *testing.T) {
	tc := &benchv1alpha1.TestCase{
		Name: "login-multi",
		Spec: benchv1alpha1.TestCaseSpec{
			DeploymentRefs: []corev1.LocalObjectReference{{Name: "topo-a"}, {Name: "topo-b"}},
			Profile:        benchv1alpha1.ProfileBurn,
		},
	}
	depA := BurnDeployment("k6-login-multi-a", "default", K6WorkloadParams{TestCase: tc, DeploymentName: "topo-a", TargetName: "a"})
	depB := BurnDeployment("k6-login-multi-b", "default", K6WorkloadParams{TestCase: tc, DeploymentName: "topo-b", TargetName: "b"})

	if depA.Spec.Selector.MatchLabels[benchv1alpha1.LabelDeployment] != "topo-a" {
		t.Errorf("deployment A selector = %v, missing LabelDeployment=topo-a", depA.Spec.Selector.MatchLabels)
	}
	if depB.Spec.Selector.MatchLabels[benchv1alpha1.LabelDeployment] != "topo-b" {
		t.Errorf("deployment B selector = %v, missing LabelDeployment=topo-b", depB.Spec.Selector.MatchLabels)
	}
	selA, _ := metav1.LabelSelectorAsMap(depA.Spec.Selector)
	selB, _ := metav1.LabelSelectorAsMap(depB.Spec.Selector)
	if labelsEqual(selA, selB) {
		t.Fatalf("deployment A and B have equal selectors, they would fight over each other's pods: %v vs %v", selA, selB)
	}
}

// TestBurnDeploymentRecreates guards against a rolling update, which would
// run two k6 pods against the target during every rollout.
func TestBurnDeploymentRecreates(t *testing.T) {
	tc := &benchv1alpha1.TestCase{Name: "login", Spec: benchv1alpha1.TestCaseSpec{Profile: benchv1alpha1.ProfileBurn}}
	dep := BurnDeployment("k6-login", "default", K6WorkloadParams{TestCase: tc, DeploymentName: "topo"})
	if dep.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType || dep.Spec.Strategy.RollingUpdate != nil {
		t.Fatalf("strategy = %+v, want Recreate", dep.Spec.Strategy)
	}
}

func labelsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// TestEnvVarsSeedIsFixedPerTestCase guards the mixed-usage template's
// reproducibility: SEED must be the same for every target a TestCase drives
// (so their action sequences are comparable), differ between TestCases, and
// honor an explicit spec.seed.
func TestEnvVarsSeedIsFixedPerTestCase(t *testing.T) {
	seedOf := func(p K6WorkloadParams) string {
		for _, v := range p.EnvVars() {
			if v.Name == "SEED" {
				return v.Value
			}
		}
		t.Fatal("no SEED env var rendered")
		return ""
	}
	tc := &benchv1alpha1.TestCase{Name: "mixed-a"}
	other := &benchv1alpha1.TestCase{Name: "mixed-b"}

	a := seedOf(K6WorkloadParams{TestCase: tc, TargetName: "topo-a"})
	if b := seedOf(K6WorkloadParams{TestCase: tc, TargetName: "topo-b"}); a != b {
		t.Errorf("SEED differs between targets of one TestCase: %s vs %s", a, b)
	}
	if b := seedOf(K6WorkloadParams{TestCase: other}); a == b {
		t.Errorf("SEED %s is the same for two different TestCases", a)
	}

	explicit := int64(42)
	tc.Spec.Seed = &explicit
	if got := seedOf(K6WorkloadParams{TestCase: tc}); got != "42" {
		t.Errorf("SEED = %s, want explicit spec.seed 42", got)
	}
}

// TestPodTemplateRunnerResources guards the k6 container's CPU reservation
// (an unreserved load generator gets starved and skews every number it
// reports) and that a TestCase's runnerResources override replaces it.
func TestPodTemplateRunnerResources(t *testing.T) {
	p := K6WorkloadParams{TestCase: &benchv1alpha1.TestCase{Name: "login-a"}}
	got := p.PodTemplate().Spec.Containers[0].Resources
	if got.Requests.Cpu().String() != "1" {
		t.Errorf("default cpu request = %s, want 1", got.Requests.Cpu())
	}
	if _, ok := got.Limits[corev1.ResourceCPU]; ok {
		t.Error("default has a cpu limit, want none (CFS throttling skews results)")
	}

	override := DefaultRunnerResources()
	override.Requests[corev1.ResourceCPU] = resource.MustParse("4")
	p.TestCase.Spec.RunnerResources = &override
	if got := p.PodTemplate().Spec.Containers[0].Resources.Requests.Cpu().String(); got != "4" {
		t.Errorf("overridden cpu request = %s, want 4", got)
	}
}

// TestIsolationTermsSpanNamespaces guards the bug that let deployments (and
// k6 runners) share nodes in a live cluster: a PodAffinityTerm without a
// namespaceSelector only matches pods in its own namespace, and every
// deployment and the k6 runners each live in a namespace of their own.
func TestIsolationTermsSpanNamespaces(t *testing.T) {
	for name, term := range map[string]corev1.PodAffinityTerm{
		"cross": crossDeploymentAntiAffinityTerm("topo-a"),
		"same":  sameDeploymentAntiAffinityTerm("topo-a"),
	} {
		sel := term.NamespaceSelector
		if sel == nil || len(sel.MatchLabels) != 0 || len(sel.MatchExpressions) != 0 {
			t.Errorf("%s term namespaceSelector = %v, want empty (all namespaces)", name, sel)
		}
	}
}

func TestBurnDeploymentWaitsForStartGate(t *testing.T) {
	p := K6WorkloadParams{TestCase: &benchv1alpha1.TestCase{}}
	c := BurnDeployment("k6", "ns", p).Spec.Template.Spec.Containers[0]
	if len(c.Command) != 4 || c.Command[0] != "sh" || !strings.Contains(c.Command[2], StartAtFileName) || c.Command[3] != "k6" {
		t.Fatalf("burn container isn't wrapped in the start gate: %q", c.Command)
	}
	if !slices.Equal(c.Args, p.Args()) {
		t.Fatalf("args = %q, want k6's own %q", c.Args, p.Args())
	}
	if QuickJob("j", "ns", p).Spec.Template.Spec.Containers[0].Command != nil {
		t.Fatal("quick jobs must not wait for a start gate")
	}
}
