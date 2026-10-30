package v1alpha1

import (
	"hash/fnv"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// TestProfile decides how k6 runs a test - see the package doc comment for
// the quick/burn distinction ported from the Ansible benchmark harness.
// +kubebuilder:validation:Enum=Quick;Burn
type TestProfile string

const (
	// ProfileQuick runs vus*iterations logins then exits. Bounded, executed
	// by creating a TestRun.
	ProfileQuick TestProfile = "Quick"
	// ProfileBurn runs forever (until the TestCase is deleted or switched
	// back to Quick), self-healed by an owned Deployment. Used to keep a
	// target under sustained load.
	ProfileBurn TestProfile = "Burn"
)

// TestCaseSpec defines one k6 load test: which AuthentikDeployment it drives,
// which script/profile/load shape it uses, and which flow it walks. The
// object's name is the k6 `testid` tag every dashboard splits on.
type TestCaseSpec struct {
	// deploymentRef names the AuthentikDeployment this test drives.
	//
	// Deprecated: use deploymentRefs instead, which supports driving the
	// same test against multiple deployments at once. Still read whenever
	// deploymentRefs is empty (see TestCase.Deployments), so existing
	// objects keep working unchanged.
	// +optional
	DeploymentRef corev1.LocalObjectReference `json:"deploymentRef,omitzero"`

	// deploymentRefs names every AuthentikDeployment this test drives - the
	// test runs independently against each one (same script/VUs/iterations),
	// tagged with its own k6 `target` so dashboards/compare views can tell
	// them apart. At least one of deploymentRef/deploymentRefs must resolve
	// to something (see TestCase.Deployments); the controller reports a
	// clear condition if neither does, rather than the CRD enforcing a
	// oneof at the schema level.
	// +optional
	DeploymentRefs []corev1.LocalObjectReference `json:"deploymentRefs,omitempty"`

	// baselineRef names the deployment, among this test's deployments, that
	// the others are compared against. Only views read it - it doesn't
	// change how the test runs. Unset, or naming a deployment the test no
	// longer drives, means the first of its deployments.
	// +optional
	BaselineRef corev1.LocalObjectReference `json:"baselineRef,omitzero"`

	// runnerClusterRef names the cluster.x-k8s.io/v1beta1 Cluster the k6
	// workload itself runs on. Defaults to the deployment's clusterRef.
	// +optional
	RunnerClusterRef *corev1.LocalObjectReference `json:"runnerClusterRef,omitempty"`

	// template names a TestCaseTemplate object (e.g. "login", "oauth-oidc" -
	// the two the operator seeds by default, see
	// internal/controller/seedtemplates.go) - which k6 script runs, plus any
	// extra provisioning that template's flow needs (e.g. oauth-oidc's own
	// oidc-test-client and OAuth2 provider/application). Ignored when inline
	// or scriptConfigMapRef is set.
	// +kubebuilder:default=login
	// +optional
	Template string `json:"template,omitempty"`

	// inline is a raw k6 script body stored directly on this TestCase,
	// instead of picking one of the built-in templates or pointing at an
	// externally-managed ConfigMap. Overrides template; overridden by
	// scriptConfigMapRef.
	// +optional
	Inline string `json:"inline,omitempty"`

	// scriptConfigMapRef, if set, overrides template/inline with a
	// user-supplied script's contents (key "script.ts") in an
	// externally-managed ConfigMap.
	// +optional
	ScriptConfigMapRef *corev1.LocalObjectReference `json:"scriptConfigMapRef,omitempty"`

	Profile TestProfile `json:"profile"`

	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=8
	VUs int32 `json:"vus,omitempty"`

	// iterations is logins per VU. Quick only.
	// +kubebuilder:default=1000
	// +optional
	Iterations int32 `json:"iterations,omitempty"`

	// +kubebuilder:default=default-authentication-flow
	// +optional
	Flow string `json:"flow,omitempty"`

	// quickDuration caps a Quick run that cannot finish its iterations in time.
	// +kubebuilder:default="150s"
	// +optional
	QuickDuration string `json:"quickDuration,omitempty"`

	// burnDuration caps a Burn run - effectively unlimited by default.
	// +kubebuilder:default="8760h"
	// +optional
	BurnDuration string `json:"burnDuration,omitempty"`

	// traceSampleRate is the share of login flows k6 marks as sampled in the
	// traceparent header it sends.
	// +kubebuilder:default="0.01"
	// +optional
	TraceSampleRate string `json:"traceSampleRate,omitempty"`

	// extraTags are merged into the k6 --tag arguments alongside
	// testid/target/profile.
	// +optional
	ExtraTags map[string]string `json:"extraTags,omitempty"`

	// runnerResources sets the k6 container's resources. Defaults to a
	// 1 CPU / 512Mi reservation with no CPU limit (see
	// render.DefaultRunnerResources) - raise it for high VU counts.
	// +optional
	RunnerResources *corev1.ResourceRequirements `json:"runnerResources,omitempty"`

	// seed fixes the pseudo-random sequence a randomized script (e.g. the
	// built-in "mixed-usage" template) draws from, passed to k6 as SEED.
	// Defaults to a value derived from this TestCase's name (see
	// TestCase.Seed), so every run of the same TestCase - and every target
	// it drives - replays the exact same action mix.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=4294967295
	// +optional
	Seed *int64 `json:"seed,omitempty"`

	// archived stops the test case without deleting it: a Burn test case's
	// k6 Deployments are removed and no new TestRuns are started, while the
	// object (and its past TestRuns) stays around for comparison.
	// +optional
	Archived bool `json:"archived,omitempty"`
}

// TestCaseDeploymentStatus is one targeted deployment's own status within a
// (possibly multi-deployment) TestCase.
type TestCaseDeploymentStatus struct {
	// name is the AuthentikDeployment object name (one of TestCase.Deployments()).
	Name string `json:"name"`

	// ready mirrors whether this specific deployment's own k6 workload
	// (Burn Deployment, or readiness to accept a Quick TestRun) is set up.
	Ready bool `json:"ready"`

	// runnerCluster is the cluster this target's k6 workload was last
	// placed on ("" is the management cluster). Kept even once the
	// AuthentikDeployment itself is gone, so the TestCase can still find
	// and delete a Burn Deployment it left on a remote cluster.
	// +optional
	RunnerCluster string `json:"runnerCluster,omitempty"`

	// cluster, namespace and target are the AuthentikDeployment's
	// clusterRef, namespace and targetName as last seen, kept once it is
	// gone so its resource usage, traces, profiles and logs can still be
	// found.
	// +optional
	Cluster string `json:"cluster,omitempty"`
	// +optional
	Namespace string `json:"namespace,omitempty"`
	// +optional
	Target string `json:"target,omitempty"`

	// burnWorkloadRef names the owned Deployment driving this target when
	// profile is Burn.
	// +optional
	BurnWorkloadRef *corev1.LocalObjectReference `json:"burnWorkloadRef,omitempty"`

	// burningSince is the burn Deployment's creationTimestamp - when this
	// target started burning, unaffected by operator restarts or Ready flaps.
	// Kept once the TestCase is archived, alongside burnStoppedAt, so its
	// metrics can still be shown for exactly the time it burned.
	// +optional
	BurningSince *metav1.Time `json:"burningSince,omitempty"`

	// burnStoppedAt is when archiving the TestCase stopped this target's
	// burn Deployment. Cleared again once it is unarchived.
	// +optional
	BurnStoppedAt *metav1.Time `json:"burnStoppedAt,omitempty"`
}

// TestCaseStatus defines the observed state of TestCase.
type TestCaseStatus struct {
	// conditions report Ready, DeploymentReady and ScriptReady.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// phase summarizes the Ready condition at a glance - computed from
	// conditions, never set independently. See ReconcilePhase.
	// +optional
	Phase ReconcilePhase `json:"phase,omitempty"`

	// deployments is one entry per TestCase.Deployments() target - replaces
	// the single burnWorkloadRef an earlier, single-deployment-only version
	// of this status had (status is always recomputed fresh every
	// reconcile, so there was nothing to preserve across that change).
	// +optional
	Deployments []TestCaseDeploymentStatus `json:"deployments,omitempty"`

	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Deployment",type=string,JSONPath=`.spec.deploymentRefs[*].name`
// +kubebuilder:printcolumn:name="Profile",type=string,JSONPath=`.spec.profile`
// +kubebuilder:printcolumn:name="Archived",type=boolean,JSONPath=`.spec.archived`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`

// TestCase is the Schema for the testcases API
type TestCase struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// +required
	Spec TestCaseSpec `json:"spec"`

	// +optional
	Status TestCaseStatus `json:"status,omitzero"`
}

// Deployments returns every deployment this TestCase drives, preferring
// spec.deploymentRefs when set and falling back to the single, deprecated
// spec.deploymentRef otherwise. This is the only supported way to read
// which deployments a TestCase targets - every controller/API call site
// goes through this rather than either field directly, so a pre-migration
// TestCase (deploymentRef only) keeps working identically forever. Returns
// nil if neither field is set (the controller reports a clear condition
// for that case rather than this method guessing).
func (tc *TestCase) Deployments() []corev1.LocalObjectReference {
	return tc.Spec.Deployments()
}

// Deployments is TestCase.Deployments on a bare spec, for callers that only
// hold one (e.g. a TestRun's status.resolvedSpec snapshot).
func (s *TestCaseSpec) Deployments() []corev1.LocalObjectReference {
	if len(s.DeploymentRefs) > 0 {
		return s.DeploymentRefs
	}
	if s.DeploymentRef.Name != "" {
		return []corev1.LocalObjectReference{s.DeploymentRef}
	}
	return nil
}

// Seed returns spec.seed if set, otherwise a stable 32-bit FNV-1a hash of
// the TestCase's name - fixed per TestCase without anyone having to pick
// one, and reproducible if the TestCase is ever recreated under the same
// name.
func (tc *TestCase) Seed() uint32 {
	if tc.Spec.Seed != nil {
		return uint32(*tc.Spec.Seed)
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(tc.Name))
	return h.Sum32()
}

// MigrateDeploymentRef moves a deprecated singular spec.deploymentRef into
// spec.deploymentRefs, returning whether anything changed. Lossless:
// Deployments() resolves to the same list before and after.
func (s *TestCaseSpec) MigrateDeploymentRef() bool {
	if len(s.DeploymentRefs) > 0 || s.DeploymentRef.Name == "" {
		return false
	}
	s.DeploymentRefs = []corev1.LocalObjectReference{s.DeploymentRef}
	s.DeploymentRef = corev1.LocalObjectReference{}
	return true
}

// +kubebuilder:object:root=true

// TestCaseList contains a list of TestCase
type TestCaseList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []TestCase `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &TestCase{}, &TestCaseList{})
		return nil
	})
}
