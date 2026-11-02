package v1alpha1

import (
	"maps"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// LabelTestMatrix names the TestMatrix an object was created for, before
// TestMatrix was folded into parent TestCases. The migration swaps it for
// LabelParent.
const LabelTestMatrix = "bench.goauthentik.io/testmatrix"

// TestMatrixSpec defines a cross product of authentik builds (variants) and
// test case templates. Superseded by a parent TestCase (spec.templates): the
// operator migrates every TestMatrix into one, keeping the objects it
// created, and deletes it - this type only remains for that migration.
type TestMatrixSpec struct {
	// +kubebuilder:validation:MinItems=1
	// +listType=map
	// +listMapKey=name
	Variants []TestCaseVariant `json:"variants"`

	// +optional
	Baseline string `json:"baseline,omitempty"`

	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:items:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +listType=set
	Templates []string `json:"templates"`

	Deployment AuthentikDeploymentSpec `json:"deployment"`

	// +optional
	ExtraValues string `json:"extraValues,omitempty"`

	TestCase TestCaseSpec `json:"testCase"`
}

// TestMatrixStatus defines the observed state of TestMatrix.
type TestMatrixStatus struct {
	// conditions report Ready=False while the migration into a TestCase
	// fails.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// +optional
	Phase ReconcilePhase `json:"phase,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Variants",type=string,JSONPath=`.spec.variants[*].name`
// +kubebuilder:printcolumn:name="Templates",type=string,JSONPath=`.spec.templates`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`

// TestMatrix is the Schema for the testmatrices API, kept only to migrate
// existing objects (see TestMatrixSpec).
type TestMatrix struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// +required
	Spec TestMatrixSpec `json:"spec"`

	// +optional
	Status TestMatrixStatus `json:"status,omitzero"`
}

// TestCase is the parent TestCase m migrates into: named like m, so its
// children and deployments keep their names.
func (m *TestMatrix) TestCase() *TestCase {
	spec := m.Spec.TestCase.DeepCopy()
	spec.DeploymentRefs, spec.BaselineRef, spec.Template = nil, corev1.LocalObjectReference{}, ""
	spec.Templates = m.Spec.Templates
	spec.Variants = m.Spec.Variants
	spec.BaselineVariant = m.Spec.Baseline
	spec.Deployment = m.Spec.Deployment.DeepCopy()
	spec.ExtraValues = m.Spec.ExtraValues
	return &TestCase{
		Name: m.Name, Namespace: m.Namespace, Labels: maps.Clone(m.Labels),
		Spec: *spec,
	}
}

// +kubebuilder:object:root=true

// TestMatrixList contains a list of TestMatrix
type TestMatrixList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []TestMatrix `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &TestMatrix{}, &TestMatrixList{})
		return nil
	})
}
