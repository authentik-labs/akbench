package api

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
)

func TestBreakdownEndOfArchivedTestCase(t *testing.T) {
	stopped := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	archivedAt := stopped.Add(time.Hour)
	tc := sampleTestCase("login", "topo", benchv1alpha1.ProfileBurn)
	tc.Status.Conditions = []metav1.Condition{{
		Type: benchv1alpha1.ConditionReady, Reason: benchv1alpha1.ReasonArchived, LastTransitionTime: metav1.NewTime(archivedAt),
	}}
	if end := breakdownEnd(tc); time.Since(end) > time.Minute {
		t.Errorf("unarchived end = %v, want now", end)
	}

	tc.Spec.Archived = true
	if end := breakdownEnd(tc); !end.Equal(archivedAt.Add(time.Minute)) {
		t.Errorf("end without burnStoppedAt = %v, want a minute past archiving", end)
	}

	tc.Status.Deployments = []benchv1alpha1.TestCaseDeploymentStatus{{Name: "topo", BurnStoppedAt: &metav1.Time{Time: stopped}}}
	if end := breakdownEnd(tc); !end.Equal(stopped.Add(time.Minute)) {
		t.Errorf("end = %v, want a minute past the burn stopping", end)
	}
}

func TestBreakdownEndOfStorageFullTestCase(t *testing.T) {
	stopped := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	tc := sampleTestCase("login", "topo", benchv1alpha1.ProfileBurn)
	tc.Status.Deployments = []benchv1alpha1.TestCaseDeploymentStatus{{Name: "topo", BurnStoppedAt: &metav1.Time{Time: stopped}}}
	tc.Status.Conditions = []metav1.Condition{{
		Type: benchv1alpha1.ConditionReady, Status: metav1.ConditionFalse, Reason: benchv1alpha1.ReasonPostgresStorageFull,
	}}
	if end := breakdownEnd(tc); !end.Equal(stopped.Add(time.Minute)) {
		t.Errorf("end while stopped = %v, want a minute past the burn stopping", end)
	}

	// Restarting: the stop is still recorded until the burn starts again,
	// but the test case is live again.
	tc.Status.Conditions[0].Reason = benchv1alpha1.ReasonDependencyNotReady
	if end := breakdownEnd(tc); time.Since(end) > time.Minute {
		t.Errorf("end while restarting = %v, want now", end)
	}
}
