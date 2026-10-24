package render

import (
	"testing"

	helmv2 "github.com/fluxcd/helm-controller/api/v2"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
)

func TestWorkerUpgrade(t *testing.T) {
	release := func(version, values string) *helmv2.HelmRelease {
		return &helmv2.HelmRelease{Spec: helmv2.HelmReleaseSpec{
			Chart:  &helmv2.HelmChartTemplate{Spec: helmv2.HelmChartTemplateSpec{Version: version}},
			Values: &apiextensionsv1.JSON{Raw: []byte(values)},
		}}
	}
	const (
		running   = `{"server":{"image":"a"},"worker":{"replicas":2,"image":"a"}}`
		upgraded  = `{"server":{"image":"b"},"worker":{"replicas":2,"image":"b"}}`
		drained   = `{"server":{"image":"a"},"worker":{"replicas":0,"image":"a"}}`
		held      = `{"server":{"image":"b"},"worker":{"replicas":0,"image":"b"}}`
		rescaled  = `{"server":{"image":"a"},"worker":{"replicas":3,"image":"a"}}`
		reordered = `{"worker":{"image":"a","replicas":2},"server":{"image":"a"}}`
	)
	for name, tc := range map[string]struct {
		mode          benchv1alpha1.PlacementMode
		recorded      string
		existing      *helmv2.HelmRelease
		desired       *helmv2.HelmRelease
		existingReady bool
		workerPods    int
		want          WorkerUpgradeStep
	}{
		"first install":                {benchv1alpha1.PlacementDedicated, "", nil, release("1", running), false, 0, WorkerUpgradeNone},
		"unchanged":                    {benchv1alpha1.PlacementDedicated, "", release("1", running), release("1", reordered), true, 0, WorkerUpgradeNone},
		"values change":                {benchv1alpha1.PlacementDedicated, "", release("1", running), release("1", upgraded), true, 2, WorkerUpgradeDrain},
		"values change, Default mode":  {benchv1alpha1.PlacementDefault, "", release("1", running), release("1", upgraded), true, 0, WorkerUpgradeNone},
		"chart version change":         {benchv1alpha1.PlacementDedicated, "", release("1", running), release("2", running), true, 2, WorkerUpgradeDrain},
		"drained, pods terminating":    {benchv1alpha1.PlacementDedicated, "", release("1", drained), release("1", upgraded), true, 1, WorkerUpgradeDrain},
		"drained, pods gone":           {benchv1alpha1.PlacementDedicated, "", release("1", drained), release("1", upgraded), true, 0, WorkerUpgradeHold},
		"wildcard pinned to installed": {benchv1alpha1.PlacementDedicated, "2", release("*", running), release("2", running), true, 0, WorkerUpgradeNone},
		"worker scale only":            {benchv1alpha1.PlacementDedicated, "", release("1", running), release("1", rescaled), true, 0, WorkerUpgradeNone},
		"held, rollout in progress":    {benchv1alpha1.PlacementDedicated, "", release("1", held), release("1", upgraded), false, 0, WorkerUpgradeHold},
		"held, rollout done":           {benchv1alpha1.PlacementDedicated, "", release("1", held), release("1", upgraded), true, 0, WorkerUpgradeRelease},
	} {
		topo := exampleDeployment()
		topo.Spec.Placement.Mode = tc.mode
		topo.Status.ChartVersion = tc.recorded
		got, err := WorkerUpgrade(topo, tc.existing, tc.desired, tc.existingReady, tc.workerPods)
		if err != nil || got != tc.want {
			t.Errorf("%s: got %v %v, want %v", name, got, err, tc.want)
		}
	}
}

func TestWithWorkerReplicas(t *testing.T) {
	out, err := WithWorkerReplicas(&apiextensionsv1.JSON{Raw: []byte(`{"worker":{"replicas":3,"image":"a"}}`)}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(out.Raw); got != `{"worker":{"image":"a","replicas":0}}` {
		t.Errorf("got %s", got)
	}
}
