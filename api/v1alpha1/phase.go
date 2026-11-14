package v1alpha1

import (
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ReconcilePhase summarizes an ongoing, declaratively-reconciled resource's
// status at a glance (AuthentikDeployment, TestCase) - distinct from
// TestRunPhase (a one-shot job's terminal outcome) and WorkloadCluster's
// own Phase field (a verbatim mirror of CAPI's external vocabulary), which
// each already have their own field and aren't affected by this type.
// +kubebuilder:validation:Enum=Pending;Reconciling;Ready;Failed;Archived
type ReconcilePhase string

const (
	// PhasePending means the resource has no Ready condition yet (just
	// created, not reconciled once).
	PhasePending ReconcilePhase = "Pending"
	// PhaseReconciling means Ready=False for a reason other than
	// ReasonReconcileError (waiting on a dependency, an in-progress Helm
	// install, ...).
	PhaseReconciling ReconcilePhase = "Reconciling"
	// PhaseReady means Ready=True.
	PhaseReady ReconcilePhase = "Ready"
	// PhaseFailed means Ready=False with reason ReasonReconcileError, or
	// ReasonPostgresStorageFull: the test case stopped itself and needs
	// someone to act before it runs again.
	PhaseFailed ReconcilePhase = "Failed"
	// PhaseArchived means Ready=False with reason ReasonArchived: the
	// resource is intentionally stopped (TestCase spec.archived).
	PhaseArchived ReconcilePhase = "Archived"
)

// PhaseFor derives a ReconcilePhase from a resource's own Ready condition -
// computed on demand rather than tracked independently, so it can never
// drift from the conditions it summarizes.
func PhaseFor(conditions []metav1.Condition) ReconcilePhase {
	cond := meta.FindStatusCondition(conditions, ConditionReady)
	if cond == nil {
		return PhasePending
	}
	if cond.Status == metav1.ConditionTrue {
		return PhaseReady
	}
	switch cond.Reason {
	case ReasonReconcileError, ReasonPostgresStorageFull:
		return PhaseFailed
	case ReasonArchived:
		return PhaseArchived
	}
	return PhaseReconciling
}
