package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// PlacementMode decides how an AuthentikDeployment's pods are scheduled, which
// is the "alone on nodes vs shared nodes vs packed together" knob the
// benchmark compares.
// +kubebuilder:validation:Enum=Default;Dedicated;Packed
type PlacementMode string

const (
	// PlacementDefault leaves scheduling to the default scheduler, so pods
	// may land on nodes shared with other workloads (the noisy-neighbor case).
	PlacementDefault PlacementMode = "Default"
	// PlacementDedicated requires one pod per node (via pod anti-affinity) and
	// applies the given tolerations/nodeSelector, so pods land alone on
	// pre-tainted/pre-labeled nodes. The operator does not create node pools
	// itself - the target cluster must already have suitable nodes.
	PlacementDedicated PlacementMode = "Dedicated"
	// PlacementPacked requires server and worker pods to land on the same
	// node as every other pod of this release (via pod affinity keyed on
	// the release's own app.kubernetes.io/instance label, which the bundled
	// postgres subchart's pod carries too) - the opposite of Dedicated, for
	// benchmarking every component sharing one node's resources.
	PlacementPacked PlacementMode = "Packed"
)

// PlacementSpec controls pod scheduling for an AuthentikDeployment's server and
// worker pods.
type PlacementSpec struct {
	// mode selects Default (shared nodes) or Dedicated (one pod per node).
	// +kubebuilder:default=Default
	// +optional
	Mode PlacementMode `json:"mode,omitempty"`

	// nodeSelector is applied to server/worker pods in Dedicated mode.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// tolerations are applied to server/worker pods in Dedicated mode, so
	// they can land on tainted, dedicated nodes.
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`

	// topologyKey is the anti-affinity deployment key used in Dedicated mode.
	// +kubebuilder:default=kubernetes.io/hostname
	// +optional
	TopologyKey string `json:"topologyKey,omitempty"`
}

// PostgresMode selects which of the three benchmarked Postgres backends an
// AuthentikDeployment uses.
// +kubebuilder:validation:Enum=Embedded;CNPG;External
type PostgresMode string

const (
	// PostgresEmbedded uses the authentik chart's own bundled postgresql
	// subchart (single instance).
	PostgresEmbedded PostgresMode = "Embedded"
	// PostgresCNPG provisions a CloudNativePG Cluster (multi-instance, HA)
	// and points the authentik chart at its connection secret.
	PostgresCNPG PostgresMode = "CNPG"
	// PostgresExternal points the authentik chart at a user-supplied,
	// operator-unmanaged external Postgres via a connection secret.
	PostgresExternal PostgresMode = "External"
)

// EmbeddedPostgresSpec configures the authentik chart's bundled postgresql
// subchart.
type EmbeddedPostgresSpec struct {
	// +optional
	StorageSize string `json:"storageSize,omitempty"`
	// +optional
	StorageClassName string `json:"storageClassName,omitempty"`
	// maxStorageSize caps volume auto-growth: once the postgres volume is
	// more than 80% full, the operator grows it by half its size, up to this
	// limit (12Gi when unset). Needs a StorageClass with
	// allowVolumeExpansion.
	// +optional
	MaxStorageSize string `json:"maxStorageSize,omitempty"`
	// tls serves the bundled postgresql over TLS with a self-signed,
	// chart-generated certificate, and has authentik connect with
	// sslmode=require.
	// +optional
	TLS bool `json:"tls,omitempty"`
}

// CNPGPostgresSpec configures a CloudNativePG Cluster provisioned for this
// deployment.
type CNPGPostgresSpec struct {
	// instances is the number of CNPG cluster instances (>1 for HA).
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=3
	Instances int32 `json:"instances,omitempty"`
	// +optional
	StorageSize string `json:"storageSize,omitempty"`
	// +optional
	StorageClassName string `json:"storageClassName,omitempty"`
	// maxStorageSize caps volume auto-growth: once the postgres volume is
	// more than 80% full, the operator grows it by half its size, up to this
	// limit (12Gi when unset). Needs a StorageClass with
	// allowVolumeExpansion.
	// +optional
	MaxStorageSize string `json:"maxStorageSize,omitempty"`
}

// ExternalPostgresSpec points at an operator-unmanaged Postgres instance.
type ExternalPostgresSpec struct {
	// secretRef names a Secret on the target cluster with host/port/database/
	// username/password keys.
	SecretRef corev1.LocalObjectReference `json:"secretRef"`
	// +kubebuilder:default=require
	// +optional
	SSLMode string `json:"sslMode,omitempty"`
}

// PostgresSpec selects and configures one of the three benchmarked Postgres
// backends.
type PostgresSpec struct {
	// +kubebuilder:default=Embedded
	Mode PostgresMode `json:"mode,omitempty"`
	// +optional
	Embedded *EmbeddedPostgresSpec `json:"embedded,omitempty"`
	// +optional
	CNPG *CNPGPostgresSpec `json:"cnpg,omitempty"`
	// +optional
	External *ExternalPostgresSpec `json:"external,omitempty"`
}

// WorkloadSpec sizes one of the authentik chart's workloads (server or
// worker).
type WorkloadSpec struct {
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=1
	Replicas int32 `json:"replicas,omitempty"`
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
}

// ErrorReportingSpec mirrors authentik's error_reporting config section, used
// both for Sentry (released images) and for OTLP export (the
// goauthentik/authentik#25412 branch). On by default.
type ErrorReportingSpec struct {
	// enabled is a pointer so an explicit false survives omitempty; nil
	// (including a spec with no authentik/errorReporting at all, where the
	// CRD default never applies) means enabled.
	// +kubebuilder:default=true
	// +optional
	Enabled *bool `json:"enabled,omitempty"`
	// tracingEndpoint is the OTLP base endpoint spans are exported to.
	// +optional
	TracingEndpoint string `json:"tracingEndpoint,omitempty"`
	// +kubebuilder:default="0.1"
	// +optional
	SampleRate string `json:"sampleRate,omitempty"`
}

// AuthentikSpec configures the authentik chart install itself.
type AuthentikSpec struct {
	// +kubebuilder:default="ghcr.io/goauthentik/server"
	// +optional
	Image string `json:"image,omitempty"`
	// +optional
	Tag string `json:"tag,omitempty"`
	// +optional
	LogLevel string `json:"logLevel,omitempty"`
	// +optional
	ErrorReporting ErrorReportingSpec `json:"errorReporting,omitempty"`
	// pyroscopeHost is where authentik sends continuous profiles.
	// +optional
	PyroscopeHost string `json:"pyroscopeHost,omitempty"`
	// extraValuesRef names a ConfigMap (a "values.yaml" key) merged over the
	// generated Helm values last, as an escape hatch.
	// +optional
	ExtraValuesRef *corev1.LocalObjectReference `json:"extraValuesRef,omitempty"`
}

// ChartSpec pins the authentik Helm chart source used to install this
// deployment.
type ChartSpec struct {
	// +kubebuilder:default="https://charts.goauthentik.io"
	// +optional
	Repo string `json:"repo,omitempty"`
	// +optional
	Version string `json:"version,omitempty"`
}

// AuthentikDeploymentSpec defines one benchmarkable authentik deployment shape:
// which cluster it runs on, how many server/worker replicas, how its pods are
// scheduled, and which of the three Postgres backends it uses.
type AuthentikDeploymentSpec struct {
	// clusterRef names a cluster.x-k8s.io/v1beta1 Cluster this deployment is
	// deployed onto. Deploying onto the management cluster itself is not
	// allowed.
	// +kubebuilder:validation:XValidation:rule="has(self.name) && self.name != ''",message="clusterRef.name must name a workload cluster; deploying onto the management cluster is not allowed"
	ClusterRef corev1.LocalObjectReference `json:"clusterRef"`

	// targetName is the identity this deployment is known by everywhere else:
	// the `target` label Prometheus attaches to its metrics, and the `target`
	// a TestCase names. Defaults to metadata.name.
	// +optional
	TargetName string `json:"targetName,omitempty"`

	// namespace is the namespace this deployment is installed into on the
	// target cluster.
	Namespace string `json:"namespace"`

	// +optional
	Chart ChartSpec `json:"chart,omitempty"`

	Server WorkloadSpec `json:"server"`
	Worker WorkloadSpec `json:"worker"`

	// +optional
	Placement PlacementSpec `json:"placement,omitempty"`

	Postgres PostgresSpec `json:"postgres"`

	// +optional
	Authentik AuthentikSpec `json:"authentik,omitempty"`
}

// PostgresStorageStatus reports postgres volume usage as last checked for
// auto-growth.
type PostgresStorageStatus struct {
	// size is what the operator has grown the postgres volumes to, if it
	// has grown them at all. It never shrinks back to spec's storageSize:
	// a PVC can't be shrunk.
	// +optional
	Size string `json:"size,omitempty"`
	// usedBytes and capacityBytes are the fullest postgres volume's
	// filesystem usage, as its node's kubelet reported it.
	// +optional
	UsedBytes int64 `json:"usedBytes,omitempty"`
	// +optional
	CapacityBytes int64 `json:"capacityBytes,omitempty"`
	// lastChecked is when usage was last read.
	// +optional
	LastChecked *metav1.Time `json:"lastChecked,omitempty"`
}

// AuthentikDeploymentStatus defines the observed state of AuthentikDeployment.
type AuthentikDeploymentStatus struct {
	// conditions report Ready, PostgresReady and HelmReleaseReady.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// phase summarizes the Ready condition at a glance - computed from
	// conditions, never set independently. See ReconcilePhase.
	// +optional
	Phase ReconcilePhase `json:"phase,omitempty"`

	// +optional
	HelmReleaseRef *corev1.LocalObjectReference `json:"helmReleaseRef,omitempty"`
	// +optional
	CNPGClusterRef *corev1.LocalObjectReference `json:"cnpgClusterRef,omitempty"`
	// postgresStorage tracks the postgres volumes while maxStorageSize
	// auto-growth is on.
	// +optional
	PostgresStorage *PostgresStorageStatus `json:"postgresStorage,omitempty"`
	// serviceEndpoint is the in-cluster host:port a TestCase drives, e.g.
	// <release>-server.<namespace>.svc.cluster.local:80.
	// +optional
	ServiceEndpoint string `json:"serviceEndpoint,omitempty"`
	// chartVersion is the authentik chart version helm-controller installed.
	// With spec.chart.version unset, the HelmRelease stays on this version
	// rather than following new upstream chart releases (which would restart
	// authentik mid-benchmark).
	// +optional
	ChartVersion string `json:"chartVersion,omitempty"`
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Target",type=string,JSONPath=`.spec.targetName`
// +kubebuilder:printcolumn:name="Postgres",type=string,JSONPath=`.spec.postgres.mode`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`

// AuthentikDeployment is the Schema for the authentikdeployments API
type AuthentikDeployment struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// +required
	Spec AuthentikDeploymentSpec `json:"spec"`

	// +optional
	Status AuthentikDeploymentStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// AuthentikDeploymentList contains a list of AuthentikDeployment
type AuthentikDeploymentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []AuthentikDeployment `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &AuthentikDeployment{}, &AuthentikDeploymentList{})
		return nil
	})
}
