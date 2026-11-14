package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// TenantPresetSpec is a "size" for the test data an authentik deployment is
// to be seeded with: how many users and groups it gets, how the groups nest
// and how many groups each user is in. Kept under a name, the same numbers
// can be used again for another deployment, and a result can say what it was
// measured against. Nothing seeds a deployment from a preset yet. Like a
// TestCaseTemplate it is editable, through the web UI or plain kubectl, and
// the operator ships a few built-in ones (see
// internal/controller/seedpresets.go).
//
// A dimension added later is added here as an optional field that defaults
// to 0, meaning none of it - so a preset saved before the dimension existed
// stays valid and keeps describing the same tenant.
// +kubebuilder:validation:XValidation:rule="self.groupsPerUser <= self.groups",message="groupsPerUser must not exceed groups"
// +kubebuilder:validation:XValidation:rule="self.groupDepth <= (self.groups > 0 ? self.groups : 1)",message="groupDepth must not exceed groups: each level needs a group"
type TenantPresetSpec struct {
	// description says what this preset stands for, in a sentence.
	// +optional
	Description string `json:"description,omitempty"`

	// seed makes the tenant reproducible: the same seed and numbers always
	// produce the same users, groups and memberships, which is what keeps
	// the deployments of one comparison identical.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=2147483647
	// +kubebuilder:default=1
	// +optional
	Seed int32 `json:"seed,omitempty"`

	// users is how many users the tenant has, named u000000 onwards. Capped
	// so those names keep their six digits, which scripts and blueprints
	// rely on.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=1000000
	// +required
	Users int32 `json:"users"`

	// groups is how many groups the tenant has, named g000000 onwards.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1000000
	// +required
	Groups int32 `json:"groups"`

	// groupDepth is how many levels the group tree has: 1 is a flat list of
	// groups, 2 is one root group with every other group below it, and so
	// on. The tree has a single root, g000000, which every user belongs to
	// through the tree. At most groups, since each level needs a group.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=10
	// +required
	GroupDepth int32 `json:"groupDepth"`

	// groupsPerUser is how many groups each user is a direct member of. A
	// few groups end up large and most small, the way real directories are.
	// +kubebuilder:validation:Minimum=0
	// +required
	GroupsPerUser int32 `json:"groupsPerUser"`
}

// +kubebuilder:object:root=true
// +kubebuilder:printcolumn:name="Users",type=integer,JSONPath=`.spec.users`
// +kubebuilder:printcolumn:name="Groups",type=integer,JSONPath=`.spec.groups`
// +kubebuilder:printcolumn:name="Depth",type=integer,JSONPath=`.spec.groupDepth`
// +kubebuilder:printcolumn:name="Groups per user",type=integer,JSONPath=`.spec.groupsPerUser`

// TenantPreset is the Schema for the tenantpresets API. It has no status:
// nothing reconciles a preset, and the schema itself rejects one that could
// not be seeded.
type TenantPreset struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// +required
	Spec TenantPresetSpec `json:"spec"`
}

// +kubebuilder:object:root=true

// TenantPresetList contains a list of TenantPreset
type TenantPresetList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []TenantPreset `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &TenantPreset{}, &TenantPresetList{})
		return nil
	})
}
