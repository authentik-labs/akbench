package v1alpha1

import (
	"slices"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// TestCaseTemplateSpec is one reusable k6 test preset a TestCase can select
// via spec.template (by this object's name) - the editable, cluster-native
// replacement for what used to be a fixed, code-compiled catalog
// (internal/testtemplate's own Registry, now removed). Editing a template
// here - through the web UI, or plain kubectl - takes effect the next time a
// TestCase referencing it reconciles; nothing needs rebuilding or
// redeploying.
type TestCaseTemplateSpec struct {
	// description documents what this template exercises, shown in the
	// web UI's template picker.
	// +optional
	Description string `json:"description,omitempty"`

	// script is the k6 test script (TypeScript) this template runs -
	// mirrored verbatim into the k6-script-<testcase> ConfigMap by
	// TestCaseReconciler, the same way spec.inline already is on a TestCase
	// itself.
	Script string `json:"script"`

	// dependencies lists the extra services TestCaseReconciler provisions
	// for any TestCase selecting this template, beyond the target
	// AuthentikDeployment itself, plus the authentik objects each needs:
	// oidc-client (oidc-test-client + OAuth2 provider), saml-client
	// (saml-test-sp + SAML provider), oidc-idp (dex + OAuth source) and
	// saml-idp (saml-test-idp + SAML source) and oauth-token (two OAuth2
	// providers driven directly at the token endpoint, no service) - see
	// internal/controller/testdeps.go and internal/render/testdeps.go. Scoped to the TestCase, not the
	// AuthentikDeployment it targets, so "what is being tested" stays fully
	// decoupled from "what it's being tested against" the same way the k6
	// workload itself already is. Templates can combine several (e.g. an
	// oidc-idp federating into authentik while an oidc-client logs in).
	// +listType=set
	// +optional
	Dependencies []TestCaseDependency `json:"dependencies,omitempty"`
}

// TestCaseDependency names one extra service a TestCaseTemplate needs
// provisioned next to its target AuthentikDeployment.
// +kubebuilder:validation:Enum=oidc-client;saml-client;oidc-idp;saml-idp;oauth-token
type TestCaseDependency string

const (
	// DependencyOIDCClient is an OIDC relying party (oidc-test-client)
	// authenticating against authentik as the OpenID provider.
	DependencyOIDCClient TestCaseDependency = "oidc-client"
	// DependencySAMLClient is a SAML service provider authenticating
	// against authentik as the SAML IdP.
	DependencySAMLClient TestCaseDependency = "saml-client"
	// DependencyOIDCIdP is an upstream OpenID provider authentik federates
	// logins to (an OAuth source).
	DependencyOIDCIdP TestCaseDependency = "oidc-idp"
	// DependencySAMLIdP is an upstream SAML IdP authentik federates logins
	// to (a SAML source).
	DependencySAMLIdP TestCaseDependency = "saml-idp"
	// DependencyOAuthToken is a pair of confidential OAuth2 clients k6
	// drives at authentik's token endpoint: one issuing client_credentials
	// tokens, one exchanging those for its own (RFC 8693 token exchange).
	DependencyOAuthToken TestCaseDependency = "oauth-token"
)

// HasDependency reports whether this template lists dep.
func (s *TestCaseTemplateSpec) HasDependency(dep TestCaseDependency) bool {
	return slices.Contains(s.Dependencies, dep)
}

// TestCaseTemplateStatus defines the observed state of TestCaseTemplate.
type TestCaseTemplateStatus struct {
	// conditions report Ready - True once spec.script is non-empty. A
	// template has no remote/owned resources of its own to reconcile; this
	// only exists so a broken template (e.g. accidentally emptied via
	// kubectl edit) is visible the same way every other CRD's status already is.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// phase summarizes the Ready condition at a glance - computed from
	// conditions, never set independently. See ReconcilePhase.
	// +optional
	Phase ReconcilePhase `json:"phase,omitempty"`

	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Dependencies",type=string,JSONPath=`.spec.dependencies`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`

// TestCaseTemplate is the Schema for the testcasetemplates API
type TestCaseTemplate struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// +required
	Spec TestCaseTemplateSpec `json:"spec"`

	// +optional
	Status TestCaseTemplateStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// TestCaseTemplateList contains a list of TestCaseTemplate
type TestCaseTemplateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []TestCaseTemplate `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &TestCaseTemplate{}, &TestCaseTemplateList{})
		return nil
	})
}
