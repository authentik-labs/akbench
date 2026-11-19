package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// InfrastructureProvider selects which Cluster API infrastructure provider
// WorkloadClusterReconciler provisions a cluster through. Incus is the only
// one implemented today (github.com/lxc/cluster-api-provider-incus, already
// installed on the management cluster) - this is an enum rather than a bare
// string, and each provider gets its own optional sub-spec below (mirroring
// PostgresSpec's Mode/Embedded/CNPG/External split), so adding e.g. AWS or
// vSphere later is a matter of another case and another sub-spec, not a
// breaking change to existing WorkloadClusters.
// +kubebuilder:validation:Enum=Incus
type InfrastructureProvider string

const (
	// InfrastructureIncus provisions via the Incus/LXC infrastructure
	// provider.
	InfrastructureIncus InfrastructureProvider = "Incus"
)

// MachineSpec sizes one set of machines (the control plane or the worker
// MachineDeployment) in a way every infrastructure provider understands -
// only the replica count. Provider-specific sizing (instance
// type/flavor/image/...) lives in that provider's own sub-spec instead (e.g.
// IncusMachineSpec), since it doesn't mean the same thing across providers.
type MachineSpec struct {
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=1
	Replicas int32 `json:"replicas,omitempty"`
}

// IncusMachineSpec sizes one set of machines on the Incus/LXC infrastructure
// provider beyond the provider-agnostic replica count in MachineSpec.
type IncusMachineSpec struct {
	// flavor is an Incus/LXC instance flavor (e.g. "c2-m4" - 2 vCPU, 4GiB).
	// +kubebuilder:default="c2-m4"
	// +optional
	Flavor string `json:"flavor,omitempty"`
}

// IncusProviderSpec configures provisioning via the Incus/LXC infrastructure
// provider. Required when spec.provider is Incus (the default).
type IncusProviderSpec struct {
	// lxcSecretRef names a Secret (in this object's namespace) holding the
	// Incus/LXC daemon connection: server, project, client-crt, client-key
	// (matching the keys cluster-api-provider-incus itself expects).
	LXCSecretRef corev1.LocalObjectReference `json:"lxcSecretRef"`

	// image is the Incus/LXC image new machines boot from.
	// +kubebuilder:default="ubuntu:24.04"
	// +optional
	Image string `json:"image,omitempty"`

	// profiles are the Incus/LXC profiles applied to every machine and to
	// the control plane's load balancer.
	// +kubebuilder:default={"default"}
	// +optional
	Profiles []string `json:"profiles,omitempty"`

	// +optional
	ControlPlane IncusMachineSpec `json:"controlPlane,omitempty"`
}

// WorkerPoolRole is which of this project's own workloads a WorkerPoolSpec's
// nodes are reserved for - see WorkerPoolSpec.Role's own doc comment for the
// full taint/toleration/nodeSelector mechanics this drives.
// +kubebuilder:validation:Enum=Generic;Authentik;K6;Monitoring
type WorkerPoolRole string

const (
	// WorkerPoolRoleGeneric is the zero value: no taint, no label, no
	// workload targets this pool by role - the scheduler's own default
	// behavior applies, exactly as if this field didn't exist.
	WorkerPoolRoleGeneric WorkerPoolRole = "Generic"
	// WorkerPoolRoleAuthentik is reserved for an AuthentikDeployment's own
	// server/worker/embedded-or-CNPG-postgres pods.
	WorkerPoolRoleAuthentik WorkerPoolRole = "Authentik"
	// WorkerPoolRoleK6 is reserved for a TestCase's k6 runner pods.
	WorkerPoolRoleK6 WorkerPoolRole = "K6"
	// WorkerPoolRoleMonitoring is reserved for the observability/addon stack
	// WorkloadClusterReconciler itself installs (Prometheus, Grafana,
	// Pyroscope, Tempo, metrics-server, csi-incus's controller, descheduler)
	// - the "system" pool, in the sense that this project's own management
	// workloads live here rather than on whatever pool is actually being
	// benchmarked.
	WorkerPoolRoleMonitoring WorkerPoolRole = "Monitoring"
)

// WorkerPoolSpec is one named, independently-sized group of worker machines,
// letting a WorkloadCluster run more than one worker shape at once - e.g. a
// small pool for the k6 runner alongside a larger one sized for load
// generation. Pool names must be unique within a WorkloadCluster.
//
// Editing an existing pool's replicas or flavor is applied in place by
// WorkloadClusterReconciler: replicas patches the MachineDeployment
// directly, and a flavor change rolls that pool's machines through Cluster
// API's normal MachineDeployment rollout (a new, immutably-named
// LXCMachineTemplate is created and the MachineDeployment is repointed at
// it - the old template is left behind rather than garbage collected, the
// same way an unused ReplicaSet lingers after a Deployment rollout).
// Removing a pool from the list deletes its MachineDeployment; adding a new
// name creates one. The control plane itself is not resizable/reconfigurable
// this way yet.
//
// replicas stops being reconciled once autoscaling is set (see its own doc
// comment) - cluster-autoscaler owns .spec.replicas from then on, and
// WorkloadClusterReconciler would otherwise fight it back to replicas on
// every reconcile.
type WorkerPoolSpec struct {
	// name identifies this pool; it becomes part of the MachineDeployment's
	// own name, so it follows the same naming rules as a Kubernetes object.
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Name string `json:"name"`

	// role declares which of this project's own workloads this pool's nodes
	// are meant for - WorkloadClusterReconciler taints every node registered
	// into a non-Generic pool (see render.NodePoolLabel) with
	// bench.goauthentik.io/pool=<role>:NoSchedule and the matching node
	// label, and every workload this project renders for that role
	// (AuthentikDeployment's server/worker/postgres pods for Authentik, a
	// TestCase's k6 runner for K6, the observability/addon stack for
	// Monitoring) is given a matching nodeSelector plus a toleration for
	// that one taint - a toleration alone only ever *permits* scheduling
	// onto a tainted node, it never *attracts* a pod there, so both are
	// needed together. Generic (the zero value) carries no taint and no
	// label at all, so every pool created before this field existed, or any
	// pool a user adds without a role, keeps behaving exactly as it always
	// did - nothing here is retroactive.
	// +kubebuilder:validation:Enum=Generic;Authentik;K6;Monitoring
	// +kubebuilder:default=Generic
	// +optional
	Role WorkerPoolRole `json:"role,omitempty"`

	// replicas is this pool's initial/fixed size. Only used at creation time
	// once autoscaling is set - see AutoscalingSpec.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=1
	// +optional
	Replicas int32 `json:"replicas,omitempty"`

	// incus is this pool's Incus-specific sizing. Required when
	// spec.provider is Incus.
	// +optional
	Incus IncusMachineSpec `json:"incus,omitempty"`

	// autoscaling enables cluster-autoscaler for this pool (see
	// WorkloadClusterReconciler's ensureClusterAutoscaler). When set, this
	// pool's MachineDeployment carries cluster-autoscaler's own min/max-size
	// annotations and replicas above is only used to size it at creation -
	// cluster-autoscaler scales it from then on.
	// +optional
	Autoscaling *AutoscalingSpec `json:"autoscaling,omitempty"`
}

// AutoscalingSpec bounds cluster-autoscaler's node-group scaling for one
// WorkerPoolSpec via cluster-autoscaler's own well-known Cluster API
// annotations (cluster.x-k8s.io/cluster-api-autoscaler-node-group-{min,max}-size
// - see https://github.com/kubernetes/autoscaler/blob/master/cluster-autoscaler/cloudprovider/clusterapi/README.md),
// applied to the pool's MachineDeployment by render.MachineDeployment.
// +kubebuilder:validation:XValidation:rule="!has(self.minReplicas) || self.minReplicas <= self.maxReplicas",message="minReplicas must not exceed maxReplicas"
type AutoscalingSpec struct {
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=0
	// +optional
	MinReplicas int32 `json:"minReplicas,omitempty"`

	// +kubebuilder:validation:Minimum=1
	MaxReplicas int32 `json:"maxReplicas"`
}

// WorkloadClusterSpec describes a Kubernetes cluster this operator asks
// Cluster API to provision through the selected infrastructure provider, so
// AuthentikDeployment/TestCase can target it via clusterRef instead of the
// management cluster.
type WorkloadClusterSpec struct {
	// kubernetesVersion is the version kubeadm installs, e.g. "v1.36.1".
	// +kubebuilder:validation:Pattern=`^v[0-9]+\.[0-9]+\.[0-9]+$`
	KubernetesVersion string `json:"kubernetesVersion"`

	// provider selects the Cluster API infrastructure provider to use.
	// +kubebuilder:default=Incus
	// +optional
	Provider InfrastructureProvider `json:"provider,omitempty"`

	// +optional
	ControlPlane MachineSpec `json:"controlPlane,omitempty"`

	// workerPools are the WorkloadCluster's worker machine groups - at least
	// one is required. See WorkerPoolSpec for how editing this list after
	// creation behaves. Omitting it entirely (rather than submitting an
	// empty list, which MinItems below still rejects) defaults to three
	// pools - one each for the three roles this project's own workloads
	// actually use (Authentik/K6/Monitoring) - applied by the API server's
	// own structural-schema defaulting before MinItems is even checked, not
	// by this operator's own code, so it works the same whether the object
	// is created via kubectl, the web UI, or any other client.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:default={{name: "authentik", role: "Authentik", replicas: 2, incus: {flavor: "c2-m4"}}, {name: "k6", role: "K6", replicas: 2, incus: {flavor: "c2-m4"}}, {name: "monitoring", role: "Monitoring", replicas: 1, incus: {flavor: "c2-m4"}}}
	WorkerPools []WorkerPoolSpec `json:"workerPools"`

	// incus configures provisioning when provider is Incus.
	// +optional
	Incus *IncusProviderSpec `json:"incus,omitempty"`

	// prometheus sizes this cluster's own Prometheus.
	// +optional
	Prometheus *PrometheusSpec `json:"prometheus,omitempty"`
}

// PrometheusSpec configures how much metrics history a WorkloadCluster's
// Prometheus keeps.
type PrometheusSpec struct {
	// retention is how long metrics are kept, as a Prometheus duration
	// (e.g. "60d", "1y"). Defaults to 60d.
	// +kubebuilder:validation:Pattern=`^(([0-9]+)y)?(([0-9]+)w)?(([0-9]+)d)?(([0-9]+)h)?(([0-9]+)m)?(([0-9]+)s)?$`
	// +optional
	Retention string `json:"retention,omitempty"`

	// storageSize is the Prometheus volume's size (default 100Gi). Older
	// metrics are dropped before retention once the volume is 85% full.
	// Raising it later doesn't resize an existing volume - expand its PVC
	// by hand (needs a StorageClass with allowVolumeExpansion).
	// +optional
	StorageSize string `json:"storageSize,omitempty"`
}

// WorkloadClusterStatus mirrors the underlying CAPI Cluster's readiness, plus
// where to find the kubeconfig once it exists.
type WorkloadClusterStatus struct {
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// phase mirrors the CAPI Cluster's own status.phase (Provisioning,
	// Provisioned, Deleting...).
	// +optional
	Phase string `json:"phase,omitempty"`
	// +optional
	ControlPlaneReady bool `json:"controlPlaneReady,omitempty"`
	// +optional
	InfrastructureReady bool `json:"infrastructureReady,omitempty"`

	// KubeconfigSecretRef names the "<cluster>-kubeconfig" Secret CAPI
	// writes once the control plane is up - what
	// internal/multicluster.Tracker resolves a clusterRef against.
	// +optional
	KubeconfigSecretRef *corev1.LocalObjectReference `json:"kubeconfigSecretRef,omitempty"`

	// PrometheusURL is where this cluster's own Prometheus (installed by
	// WorkloadClusterReconciler's ensurePrometheus) is reachable *from the
	// management cluster* - a node's InternalIP plus the fixed NodePort
	// render.PrometheusNodePort exposes, not the cluster-relative DNS name
	// k6 workloads push metrics to (that only resolves from inside this
	// same cluster). cmd/apiserver reads this to query per-testid metrics
	// without a central receiver.
	// +optional
	PrometheusURL string `json:"prometheusURL,omitempty"`

	// GrafanaURL is where this cluster's own Grafana (installed by
	// WorkloadClusterReconciler's ensureGrafana) is reachable *from the
	// management cluster* - a node's InternalIP plus the fixed NodePort
	// render.GrafanaNodePort exposes. cmd/apiserver's own reverse proxy
	// (internal/api/grafana_proxy.go) connects to this to embed a
	// dashboard in the web UI's Grafana page.
	// +optional
	GrafanaURL string `json:"grafanaURL,omitempty"`

	// AlertmanagerURL is where this cluster's own Alertmanager (bundled in
	// the same kube-prometheus-stack install ensurePrometheus applies) is
	// reachable *from the management cluster* - a node's InternalIP plus
	// the fixed NodePort render.AlertmanagerNodePort exposes. Lets a client
	// (e.g. the web UI) query/list firing alerts (GET /api/v2/alerts)
	// without a central receiver, the same reachability model
	// PrometheusURL/GrafanaURL already use.
	// +optional
	AlertmanagerURL string `json:"alertmanagerURL,omitempty"`

	// addons reports each add-on WorkloadClusterReconciler installs onto
	// the cluster once its control plane is up (CNI, storage, monitoring,
	// ...), in install order. Empty until then.
	// +listType=map
	// +listMapKey=name
	// +optional
	Addons []AddonStatus `json:"addons,omitempty"`

	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// AddonState is where one add-on stands.
// +kubebuilder:validation:Enum=Ready;Progressing;Waiting;Failed
type AddonState string

const (
	// AddonReady: installed - for a Helm-based add-on, its HelmRelease is
	// Ready.
	AddonReady AddonState = "Ready"
	// AddonProgressing: applied, and its HelmRelease is still installing or
	// upgrading.
	AddonProgressing AddonState = "Progressing"
	// AddonWaiting: blocked on something that resolves by itself, typically
	// a CRD another add-on (kube-prometheus-stack) hasn't installed yet.
	AddonWaiting AddonState = "Waiting"
	// AddonFailed: applying it failed, or its HelmRelease did.
	AddonFailed AddonState = "Failed"
)

// AddonStatus is one add-on's state on the workload cluster.
type AddonStatus struct {
	// name is the add-on's display name, e.g. "CNI" or "Prometheus".
	Name  string     `json:"name"`
	State AddonState `json:"state"`
	// message explains anything but Ready.
	// +optional
	Message string `json:"message,omitempty"`
	// lastTransitionTime is when state last changed.
	// +optional
	LastTransitionTime metav1.Time `json:"lastTransitionTime,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// The move-hierarchy label makes `clusterctl move` carry every
// WorkloadCluster - and everything it owns (the CAPI Cluster tree, its
// HelmReleases/HelmRepositories, generated Secrets) - to the target
// management cluster with ownerReferences rebuilt against the new UIDs.
// Without it, clusterctl discovers WorkloadCluster as the Cluster's owner but
// never creates it on the target, and the move fails on an ownerReference
// with an empty UID. See hack/migrate-to-appliance.sh.
// +kubebuilder:metadata:labels="clusterctl.cluster.x-k8s.io/move-hierarchy=true"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="ControlPlane",type=boolean,JSONPath=`.status.controlPlaneReady`
// +kubebuilder:printcolumn:name="Infrastructure",type=boolean,JSONPath=`.status.infrastructureReady`

// WorkloadCluster is the Schema for the workloadclusters API
type WorkloadCluster struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// +required
	Spec WorkloadClusterSpec `json:"spec"`

	// +optional
	Status WorkloadClusterStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// WorkloadClusterList contains a list of WorkloadCluster
type WorkloadClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []WorkloadCluster `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &WorkloadCluster{}, &WorkloadClusterList{})
		return nil
	})
}

// HasPoolRole reports whether this WorkloadCluster has at least one worker
// pool reserved for role - what render.AuthentikPoolPlacement/
// render.K6PoolPlacement/render.MonitoringPoolPlacement check before
// targeting a workload at that role's taint/label at all, so a
// WorkloadCluster that never got a pool for some role (every one created
// before this field existed, or one whose pools were deliberately left
// Generic) never ends up with a workload's nodeSelector pointing at a label
// no node actually carries.
func (wc *WorkloadCluster) HasPoolRole(role WorkerPoolRole) bool {
	for _, pool := range wc.Spec.WorkerPools {
		if pool.Role == role {
			return true
		}
	}
	return false
}
