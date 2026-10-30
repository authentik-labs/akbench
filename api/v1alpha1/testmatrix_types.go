package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// LabelTestMatrix names the TestMatrix an AuthentikDeployment, TestCase or
// extra-values ConfigMap was created for.
const LabelTestMatrix = "bench.goauthentik.io/testmatrix"

// TestMatrixVariant is one authentik build under test - one column of the
// matrix.
type TestMatrixVariant struct {
	// name identifies the variant in the deployments' names and in views,
	// e.g. "pr" or "2026-6-0".
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:MaxLength=24
	Name string `json:"name"`

	// image overrides deployment.authentik.image for this variant.
	// +optional
	Image string `json:"image,omitempty"`

	// tag is the image tag this variant runs.
	// +kubebuilder:validation:MinLength=1
	Tag string `json:"tag"`
}

// TestMatrixSpec defines a cross product of authentik builds (variants) and
// test case templates: every template gets its own TestCase driving its own
// AuthentikDeployment per variant, so the templates' provisioning and load
// never share a deployment.
type TestMatrixSpec struct {
	// variants are the builds compared against each other.
	// +kubebuilder:validation:MinItems=1
	// +listType=map
	// +listMapKey=name
	Variants []TestMatrixVariant `json:"variants"`

	// baseline names the variant the others are compared against. Defaults
	// to the first variant.
	// +optional
	Baseline string `json:"baseline,omitempty"`

	// templates names the TestCaseTemplates to run, one TestCase each.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:items:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +listType=set
	Templates []string `json:"templates"`

	// deployment is shared by every AuthentikDeployment the matrix creates.
	// namespace is ignored (each deployment gets its own, named after it),
	// as are authentik.tag, and authentik.image wherever a variant sets one.
	Deployment AuthentikDeploymentSpec `json:"deployment"`

	// extraValues is raw Helm values YAML merged over every deployment's
	// generated values (see AuthentikSpec.extraValuesRef).
	// +optional
	ExtraValues string `json:"extraValues,omitempty"`

	// testCase is shared by every TestCase the matrix creates.
	// deploymentRefs, baselineRef and template are set per TestCase.
	TestCase TestCaseSpec `json:"testCase"`
}

// TestMatrixCell is one variant's AuthentikDeployment within a template's row.
type TestMatrixCell struct {
	Variant    string `json:"variant"`
	Deployment string `json:"deployment"`
	// +optional
	Phase ReconcilePhase `json:"phase,omitempty"`
}

// TestMatrixRow is one template's TestCase and the deployments it drives.
type TestMatrixRow struct {
	Template string `json:"template"`
	TestCase string `json:"testCase"`
	// +optional
	Phase ReconcilePhase `json:"phase,omitempty"`
	// +optional
	Cells []TestMatrixCell `json:"cells,omitempty"`
}

// TestMatrixStatus defines the observed state of TestMatrix.
type TestMatrixStatus struct {
	// conditions report Ready: every TestCase and AuthentikDeployment is
	// Ready.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// phase summarizes the Ready condition at a glance. See ReconcilePhase.
	// +optional
	Phase ReconcilePhase `json:"phase,omitempty"`

	// rows is one entry per template, in spec order.
	// +optional
	Rows []TestMatrixRow `json:"rows,omitempty"`

	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Variants",type=string,JSONPath=`.spec.variants[*].name`
// +kubebuilder:printcolumn:name="Templates",type=string,JSONPath=`.spec.templates`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`

// TestMatrix is the Schema for the testmatrices API
type TestMatrix struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// +required
	Spec TestMatrixSpec `json:"spec"`

	// +optional
	Status TestMatrixStatus `json:"status,omitzero"`
}

// BaselineVariant returns spec.baseline, or the first variant's name when
// it is unset or names no variant.
func (m *TestMatrix) BaselineVariant() string {
	for _, v := range m.Spec.Variants {
		if v.Name == m.Spec.Baseline {
			return v.Name
		}
	}
	if len(m.Spec.Variants) == 0 {
		return ""
	}
	return m.Spec.Variants[0].Name
}

// TestCaseName is the TestCase a template's row runs as.
func (m *TestMatrix) TestCaseName(template string) string {
	return m.Name + "-" + template
}

// DeploymentName is the AuthentikDeployment a variant runs as in a
// template's row.
func (m *TestMatrix) DeploymentName(template, variant string) string {
	return m.Name + "-" + variant + "-" + template
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
