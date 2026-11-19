package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// TestRunPhase tracks a single Quick execution of a TestCase.
// +kubebuilder:validation:Enum=Pending;Running;Succeeded;Failed
type TestRunPhase string

const (
	TestRunPending   TestRunPhase = "Pending"
	TestRunRunning   TestRunPhase = "Running"
	TestRunSucceeded TestRunPhase = "Succeeded"
	TestRunFailed    TestRunPhase = "Failed"
)

// TestRunOverrides lets a single run adjust load shape without mutating the
// TestCase it comes from.
type TestRunOverrides struct {
	// +optional
	VUs *int32 `json:"vus,omitempty"`
	// +optional
	Iterations *int32 `json:"iterations,omitempty"`
	// +optional
	Flow *string `json:"flow,omitempty"`
}

// TestRunSpec requests one bounded (Quick) execution of a TestCase.
type TestRunSpec struct {
	TestCaseRef corev1.LocalObjectReference `json:"testCaseRef"`

	// deploymentRef pins this run to one specific entry of the TestCase's
	// own TestCase.Deployments() - needed once a TestCase can target more
	// than one AuthentikDeployment, since one TestRun is still exactly one
	// Job against exactly one deployment ("Run now" fans out into one
	// TestRun per deployment rather than teaching TestRun to run several
	// Jobs itself). Unset on every TestRun created before multi-deployment
	// TestCases existed; TestRunReconciler falls back to the resolved
	// TestCase's own Deployments()[0] in that case, which is always correct
	// for those older, inherently single-deployment runs.
	// +optional
	DeploymentRef *corev1.LocalObjectReference `json:"deploymentRef,omitempty"`

	// +optional
	Overrides *TestRunOverrides `json:"overrides,omitempty"`
}

// TestRunStatus defines the observed state of TestRun.
type TestRunStatus struct {
	// +optional
	Phase TestRunPhase `json:"phase,omitempty"`

	// resolvedSpec is an audit snapshot of the TestCaseSpec this run actually
	// used, with overrides merged in, independent of later edits/deletes of
	// the TestCase itself.
	// +optional
	ResolvedSpec *TestCaseSpec `json:"resolvedSpec,omitempty"`

	// tenant is the seeded tenant the run's k6 Job was given, for a
	// template with the seeded-tenant dependency.
	// +optional
	Tenant *TenantRecord `json:"tenant,omitempty"`

	// +optional
	JobRef *corev1.LocalObjectReference `json:"jobRef,omitempty"`
	// +optional
	StartTime *metav1.Time `json:"startTime,omitempty"`
	// +optional
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`
	// +optional
	GrafanaAnnotationID *int64 `json:"grafanaAnnotationID,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="TestCase",type=string,JSONPath=`.spec.testCaseRef.name`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Started",type=date,JSONPath=`.status.startTime`

// TestRun is the Schema for the testruns API
type TestRun struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// +required
	Spec TestRunSpec `json:"spec"`

	// +optional
	Status TestRunStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// TestRunList contains a list of TestRun
type TestRunList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []TestRun `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &TestRun{}, &TestRunList{})
		return nil
	})
}
