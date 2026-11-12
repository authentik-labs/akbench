package v1alpha1

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestPhaseForNoConditions(t *testing.T) {
	if got := PhaseFor(nil); got != PhasePending {
		t.Errorf("PhaseFor(nil) = %q, want %q", got, PhasePending)
	}
}

func TestPhaseForReady(t *testing.T) {
	conditions := []metav1.Condition{
		{Type: ConditionReady, Status: metav1.ConditionTrue, Reason: ReasonReady},
	}
	if got := PhaseFor(conditions); got != PhaseReady {
		t.Errorf("PhaseFor(Ready=True) = %q, want %q", got, PhaseReady)
	}
}

func TestPhaseForFailed(t *testing.T) {
	conditions := []metav1.Condition{
		{Type: ConditionReady, Status: metav1.ConditionFalse, Reason: ReasonReconcileError},
	}
	if got := PhaseFor(conditions); got != PhaseFailed {
		t.Errorf("PhaseFor(Ready=False, ReconcileError) = %q, want %q", got, PhaseFailed)
	}
}

func TestPhaseForReconciling(t *testing.T) {
	conditions := []metav1.Condition{
		{Type: ConditionReady, Status: metav1.ConditionFalse, Reason: ReasonDependencyNotReady},
	}
	if got := PhaseFor(conditions); got != PhaseReconciling {
		t.Errorf("PhaseFor(Ready=False, DependencyNotReady) = %q, want %q", got, PhaseReconciling)
	}
}

func TestPhaseForArchived(t *testing.T) {
	conditions := []metav1.Condition{
		{Type: ConditionReady, Status: metav1.ConditionFalse, Reason: ReasonArchived},
	}
	if got := PhaseFor(conditions); got != PhaseArchived {
		t.Errorf("PhaseFor(Ready=False, Archived) = %q, want %q", got, PhaseArchived)
	}
}

func TestPhaseForPostgresStorageFull(t *testing.T) {
	conditions := []metav1.Condition{
		{Type: ConditionReady, Status: metav1.ConditionFalse, Reason: ReasonPostgresStorageFull},
	}
	if got := PhaseFor(conditions); got != PhaseFailed {
		t.Errorf("PhaseFor(Ready=False, PostgresStorageFull) = %q, want %q", got, PhaseFailed)
	}
}
