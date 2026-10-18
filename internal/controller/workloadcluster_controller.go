package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	helmv2 "github.com/fluxcd/helm-controller/api/v2"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	controlplanev1 "sigs.k8s.io/cluster-api/api/controlplane/kubeadm/v1beta2"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	capikubeconfig "sigs.k8s.io/cluster-api/util/kubeconfig"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
	"github.com/authentik-labs/akbench/internal/render"
)

// capiDeleteRequeueInterval is how often reconcileDelete polls while waiting
// for CAPI to finish tearing down a Cluster's machines.
const capiDeleteRequeueInterval = 5 * time.Second

// workloadClusterFinalizer guards deletion so the operator can wait for
// CAPI's own Cluster deletion (which tears down machines one at a time) to
// finish before removing the WorkloadCluster - it does not need to reach any
// object on a different cluster, since everything a WorkloadCluster renders
// (the CAPI Cluster and its infrastructure/bootstrap objects) lives on the
// management cluster.
const workloadClusterFinalizer = "bench.goauthentik.io/workload-cluster-cleanup"

// WorkloadClusterReconciler reconciles a WorkloadCluster object: it renders
// the CAPI Cluster/LXCCluster/KubeadmControlPlane/MachineDeployment set
// described in internal/render/workloadcluster.go via the Incus
// infrastructure provider already installed on the management cluster, and
// mirrors the resulting CAPI Cluster's readiness back onto its own status.
type WorkloadClusterReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=bench.goauthentik.io,resources=workloadclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=bench.goauthentik.io,resources=workloadclusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=bench.goauthentik.io,resources=workloadclusters/finalizers,verbs=update
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters;machinedeployments;machinehealthchecks,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=controlplane.cluster.x-k8s.io,resources=kubeadmcontrolplanes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=bootstrap.cluster.x-k8s.io,resources=kubeadmconfigtemplates,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=lxcclusters;lxcmachinetemplates,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch

func (r *WorkloadClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var wc benchv1alpha1.WorkloadCluster
	if err := r.Get(ctx, req.NamespacedName, &wc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !wc.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &wc)
	}
	if !controllerutil.ContainsFinalizer(&wc, workloadClusterFinalizer) {
		controllerutil.AddFinalizer(&wc, workloadClusterFinalizer)
		if err := r.Update(ctx, &wc); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Incus is the only infrastructure provider actually implemented today -
	// spec.provider/spec.incus exist as a discriminated union (like
	// PostgresSpec's mode) so a second provider is a new case here plus new
	// render functions, not a breaking schema change, but there's nothing to
	// render for it yet.
	if (wc.Spec.Provider != benchv1alpha1.InfrastructureIncus && wc.Spec.Provider != "") || wc.Spec.Incus == nil {
		setCondition(r.Recorder, &wc, &wc.Status.Conditions, wc.Generation, benchv1alpha1.ConditionReady, metav1.ConditionFalse,
			benchv1alpha1.ReasonNotImplemented,
			fmt.Sprintf("provider %q is not implemented; only %q (with spec.incus set) is", wc.Spec.Provider, benchv1alpha1.InfrastructureIncus))
		return ctrl.Result{}, r.Status().Update(ctx, &wc)
	}

	// applyCreateOnly, not applyOwned: CAPI's own webhooks/defaulting mutate
	// these objects' specs after creation (rollout strategy defaults, etc.)
	// in ways this operator's freshly-rendered object never has, so a
	// spec-equality check against them never matches - see applyCreateOnly's
	// doc comment for what that caused when this used applyOwned instead.
	// LXCMachineTemplates/KubeadmConfigTemplates are safe to create-only even
	// for a spec.workerPools edit: workerMachineTemplateName folds the
	// pool's flavor into the object's own name, so an edited flavor always
	// means a brand new object here, never an update to an existing one.
	for _, obj := range []client.Object{
		render.LXCCluster(&wc),
		render.ControlPlaneLXCMachineTemplate(&wc),
	} {
		if err := applyCreateOnly(ctx, r.Client, r.Scheme, &wc, obj); err != nil {
			return ctrl.Result{}, fmt.Errorf("apply %s: %w", obj.GetObjectKind().GroupVersionKind().Kind, err)
		}
	}
	for _, pool := range wc.Spec.WorkerPools {
		for _, obj := range []client.Object{
			render.WorkerLXCMachineTemplate(&wc, pool),
			render.WorkerKubeadmConfigTemplate(&wc, pool),
		} {
			if err := applyCreateOnly(ctx, r.Client, r.Scheme, &wc, obj); err != nil {
				return ctrl.Result{}, fmt.Errorf("apply %s for pool %q: %w", obj.GetObjectKind().GroupVersionKind().Kind, pool.Name, err)
			}
		}
	}

	cluster := render.Cluster(&wc)
	if err := applyCreateOnly(ctx, r.Client, r.Scheme, &wc, cluster); err != nil {
		return ctrl.Result{}, fmt.Errorf("apply Cluster: %w", err)
	}

	var existing clusterv1.Cluster
	if err := r.Get(ctx, types.NamespacedName{Name: cluster.Name, Namespace: cluster.Namespace}, &existing); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("get Cluster: %w", err)
		}
		setCondition(r.Recorder, &wc, &wc.Status.Conditions, wc.Generation, benchv1alpha1.ConditionReady, metav1.ConditionFalse,
			benchv1alpha1.ReasonReconciling, "waiting for Cluster to be created")
		return ctrl.Result{}, r.Status().Update(ctx, &wc)
	}

	if waiting, requeue, err := r.ensureControlPlane(ctx, &wc, &existing); err != nil {
		return ctrl.Result{}, err
	} else if waiting != "" {
		setCondition(r.Recorder, &wc, &wc.Status.Conditions, wc.Generation, benchv1alpha1.ConditionReady, metav1.ConditionFalse,
			benchv1alpha1.ReasonReconciling, waiting)
		return ctrl.Result{RequeueAfter: requeue}, r.Status().Update(ctx, &wc)
	}
	// MachineDeployments are applied only once the Cluster exists - CAPI's
	// own webhook/validation rejects a MachineDeployment naming a Cluster
	// that isn't there yet. Unlike everything above, a pool's
	// MachineDeployment genuinely is updated in place (see
	// applyMachineDeploymentPool) - replicas/flavor are the one day-2 edit
	// this controller supports.
	desiredPools := make(map[string]bool, len(wc.Spec.WorkerPools))
	for _, pool := range wc.Spec.WorkerPools {
		desiredPools[pool.Name] = true
		if err := applyMachineDeploymentPool(ctx, r.Client, r.Scheme, &wc, render.MachineDeployment(&wc, pool)); err != nil {
			return ctrl.Result{}, fmt.Errorf("apply MachineDeployment for pool %q: %w", pool.Name, err)
		}
		// Create-only, like the CAPI objects above: Cluster API defaults
		// fields of its own into the spec.
		if err := applyCreateOnly(ctx, r.Client, r.Scheme, &wc, render.MachineHealthCheck(&wc, pool)); err != nil {
			return ctrl.Result{}, fmt.Errorf("apply MachineHealthCheck for pool %q: %w", pool.Name, err)
		}
	}
	if err := r.deleteRemovedWorkerPools(ctx, &wc, desiredPools); err != nil {
		return ctrl.Result{}, fmt.Errorf("delete removed worker pools: %w", err)
	}

	wc.Status.Phase = existing.Status.Phase
	wc.Status.ControlPlaneReady = existing.Status.Initialization.ControlPlaneInitialized != nil && *existing.Status.Initialization.ControlPlaneInitialized
	wc.Status.InfrastructureReady = existing.Status.Initialization.InfrastructureProvisioned != nil && *existing.Status.Initialization.InfrastructureProvisioned
	wc.Status.ObservedGeneration = wc.Generation

	if wc.Status.ControlPlaneReady {
		wc.Status.KubeconfigSecretRef = &corev1.LocalObjectReference{Name: render.KubeconfigSecretName(&wc)}
		r.ensureAddons(ctx, &wc)
	}

	ready := metav1.ConditionFalse
	message := "waiting for Cluster to become ready"
	if cond := meta.FindStatusCondition(existing.Status.Conditions, clusterv1.ClusterAvailableCondition); cond != nil {
		message = cond.Message
	}
	if wc.Status.ControlPlaneReady && wc.Status.InfrastructureReady {
		ready = metav1.ConditionTrue
		message = "control plane and infrastructure ready"
	}
	setCondition(r.Recorder, &wc, &wc.Status.Conditions, wc.Generation, benchv1alpha1.ConditionReady, ready, benchv1alpha1.ReasonReady, message)

	if err := r.Status().Update(ctx, &wc); err != nil {
		return ctrl.Result{}, err
	}
	log.Info("reconciled WorkloadCluster", "phase", wc.Status.Phase, "ready", ready)
	// Add-ons waiting on another one's CRDs (or failed outright) aren't
	// retried by any watch event, unlike HelmRelease-based ones settling.
	for _, a := range wc.Status.Addons {
		if a.State != benchv1alpha1.AddonReady {
			return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
		}
	}
	return ctrl.Result{}, nil
}

// ensureAddons installs every best-effort, idempotent addon a Ready control
// plane needs, once its own ensure* method is implemented the same way
// (plain Create, ignore AlreadyExists). Each addon's failure is logged and
// reported as an Event rather than failing the whole reconcile: a
// transient failure here (e.g. the API server mid-cert-rotation) shouldn't
// block updating the rest of this object's status, and the next reconcile
// just retries it.
//
//   - CNI (ensureCNI): kubeadm doesn't install one itself - without it,
//     kubelet reports "cni plugin not initialized" and every Node stays
//     NotReady forever, so nothing can ever schedule.
//   - csi-incus (ensureCSIIncus): without a default StorageClass, any PVC
//     left without an explicit storageClassName (the authentik chart's
//     bundled postgresql, notably) stays Pending forever with "unbound
//     immediate PersistentVolumeClaims", so any Helm install waiting on it
//     times out. Installs csi-incus (github.com/BeryJu/csi-incus) and makes
//     its StorageClass the cluster default (see CSIIncusStorageClass) - real
//     Incus-backed, resizable, snapshot-capable volumes, the only
//     StorageClass a workload cluster gets now (local-path-provisioner was
//     dropped once this became available). Incus-specific, so a no-op when
//     spec.incus isn't set (guarded inside ensureCSIIncus itself, same as
//     the provider check earlier in Reconcile).
//   - metrics-server (ensureMetricsServer): without it, there's no
//     metrics.k8s.io API on this cluster at all (kubeadm doesn't install it
//     any more than it installs a CNI), so `kubectl top` has nothing to read
//     from.
//   - Prometheus (ensurePrometheus): unlike the addons above, this one also
//     resolves a URL cmd/apiserver (on the management cluster) can reach: a
//     management-cluster pod can reach a workload cluster's node IPs
//     directly (confirmed live - the reverse can't), which is what makes the
//     NodePort ensurePrometheus creates actually useful cross-cluster.
//   - cluster-autoscaler (ensureClusterAutoscaler): a no-op unless at least
//     one worker pool actually sets spec.autoscaling - installing an
//     always-on cluster-autoscaler for every WorkloadCluster regardless of
//     whether anything asked for it would be pure waste, the same reasoning
//     ensureCSIIncus already applies to spec.incus being unset.
//   - Pyroscope/Tempo (ensurePyroscope/ensureTempo): without these,
//     authentik's own AUTHENTIK_PYROSCOPE_HOST/
//     AUTHENTIK_ERROR_REPORTING__OTEL_ENDPOINT env vars
//     (internal/render/helmrelease.go) point at nothing.
//   - Loki/Alloy (ensureLoki/ensureAlloy): authentik only logs to stdout,
//     so Alloy tails every pod's logs into Loki.
//   - Grafana (ensureGrafana): pre-wired with datasources for Prometheus,
//     Pyroscope, Tempo and Loki, so it's useful the moment it comes up.
func (r *WorkloadClusterReconciler) ensureAddons(ctx context.Context, wc *benchv1alpha1.WorkloadCluster) {
	log := logf.FromContext(ctx)
	type addon struct {
		name   string
		ensure func(context.Context, *benchv1alpha1.WorkloadCluster) error
		// helmRelease names the add-on's Flux HelmRelease (in wc's
		// namespace), which its state is read from; empty for add-ons
		// applied directly.
		helmRelease string
		// legacyHelmRelease is the fixed, cluster-unscoped name an older
		// operator gave that HelmRelease - see migrateLegacyAddonHelmRelease.
		legacyHelmRelease string
	}
	helmAddon := func(name string, ensure func(context.Context, *benchv1alpha1.WorkloadCluster) error, release string) addon {
		return addon{name, ensure, render.AddonHelmReleaseName(wc, release), release}
	}
	addons := []addon{
		helmAddon("CNI", r.ensureCNI, render.CNIHelmReleaseName),
		helmAddon("metrics-server", r.ensureMetricsServer, render.MetricsServerHelmReleaseName),
		helmAddon("Prometheus", r.ensurePrometheus, render.KubePrometheusStackHelmReleaseName),
		{name: "cluster-autoscaler", ensure: r.ensureClusterAutoscaler, helmRelease: render.ClusterAutoscalerHelmReleaseName(wc)},
		helmAddon("Pyroscope", r.ensurePyroscope, render.PyroscopeHelmReleaseName),
		helmAddon("Tempo", r.ensureTempo, render.TempoHelmReleaseName),
		helmAddon("Loki", r.ensureLoki, render.LokiHelmReleaseName),
		helmAddon("Alloy", r.ensureAlloy, render.AlloyHelmReleaseName),
		helmAddon("Grafana", r.ensureGrafana, render.GrafanaHelmReleaseName),
		helmAddon("descheduler", r.ensureDescheduler, render.DeschedulerHelmReleaseName),
	}
	// csi-incus is Incus-specific (and a no-op without spec.incus).
	if wc.Spec.Incus != nil {
		addons = slices.Insert(addons, 1, helmAddon("csi-incus", r.ensureCSIIncus, render.CSIIncusHelmReleaseName))
	}
	previous := map[string]benchv1alpha1.AddonState{}
	for _, a := range wc.Status.Addons {
		previous[a.Name] = a.State
	}
	for _, addon := range addons {
		err := r.migrateLegacyAddonHelmRelease(ctx, wc, addon.legacyHelmRelease)
		if err == nil {
			err = addon.ensure(ctx, wc)
		}
		state, message := r.addonState(ctx, wc, addon.helmRelease, err)
		setAddonStatus(wc, addon.name, state, message)
		switch {
		case state == benchv1alpha1.AddonFailed:
			log.Error(err, "failed to ensure addon on workload cluster", "addon", addon.name, "message", message)
			r.Recorder.Eventf(wc, corev1.EventTypeWarning, "AddonFailed", "%s: %s", addon.name, message)
		case state == benchv1alpha1.AddonReady && previous[addon.name] != benchv1alpha1.AddonReady:
			r.Recorder.Eventf(wc, corev1.EventTypeNormal, "AddonReady", "%s is ready", addon.name)
		}
	}
}

// remoteAddonClient builds a one-off client directly from the
// "<name>-kubeconfig" Secret CAPI writes, rather than
// internal/multicluster.Tracker. Every add-on installs through a Flux
// HelmRelease (spec.kubeConfig points helm-controller at the cluster), so
// this is only for the occasional direct read or cleanup: resolving a node
// IP for the NodePort URLs (reachableNodeIP) and removing objects an older,
// hand-rendering operator left behind (removeLegacyObjects) - not the
// long-lived cached/watching connection Tracker exists for (that's what
// AuthentikDeployment/TestCase/TestRun use once they deploy onto this
// cluster via clusterRef).
func (r *WorkloadClusterReconciler) remoteAddonClient(ctx context.Context, wc *benchv1alpha1.WorkloadCluster) (client.Client, error) {
	kubeconfigBytes, err := capikubeconfig.FromSecret(ctx, r.Client, client.ObjectKey{Name: wc.Name, Namespace: wc.Namespace})
	if err != nil {
		return nil, fmt.Errorf("get kubeconfig: %w", err)
	}
	restConfig, err := clientcmd.RESTConfigFromKubeConfig(kubeconfigBytes)
	if err != nil {
		return nil, fmt.Errorf("build rest config: %w", err)
	}
	remoteClient, err := client.New(restConfig, client.Options{})
	if err != nil {
		return nil, fmt.Errorf("build remote client: %w", err)
	}
	return remoteClient, nil
}

// ensureCNI installs Flannel onto the workload cluster via its own official
// Helm chart - a Flux HelmRepository + HelmRelease applied here on the
// management cluster with spec.kubeConfig pointing helm-controller at the
// workload cluster, the same mechanism ensureCSIIncus uses.
func (r *WorkloadClusterReconciler) ensureCNI(ctx context.Context, wc *benchv1alpha1.WorkloadCluster) error {
	if err := applyOwned(ctx, r.Client, r.Scheme, wc, render.CNIHelmRepository(wc.Namespace)); err != nil {
		return fmt.Errorf("apply CNI HelmRepository: %w", err)
	}
	hr := render.CNIHelmRelease(wc)
	if err := applyOwned(ctx, r.Client, r.Scheme, wc, hr); err != nil {
		return fmt.Errorf("apply CNI HelmRelease: %w", err)
	}
	return nil
}

// ensureCSIIncus installs csi-incus (github.com/BeryJu/csi-incus) onto the
// workload cluster via its own published Helm chart - a Flux
// HelmRepository + HelmRelease applied here on the management cluster with
// spec.kubeConfig pointing helm-controller at the workload cluster,
// mirroring exactly how AuthentikDeploymentReconciler installs the authentik
// chart remotely (internal/render/helmrelease.go). Chart values are built
// by remapping this WorkloadCluster's own Incus/LXC connection Secret
// (spec.incus.lxcSecretRef) into the shape the chart expects (see
// render.CSIIncusHelmValues) - reusing the existing CAPI provisioning
// credentials rather than requiring a second certificate. This is the only
// StorageClass a workload cluster gets - no more local-path-provisioner
// fallback. A no-op when spec.incus isn't set: csi-incus is Incus-specific,
// unlike ensureCNI/ensureMetricsServer.
func (r *WorkloadClusterReconciler) ensureCSIIncus(ctx context.Context, wc *benchv1alpha1.WorkloadCluster) error {
	if wc.Spec.Incus == nil {
		return nil
	}
	var source corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Name: wc.Spec.Incus.LXCSecretRef.Name, Namespace: wc.Namespace}, &source); err != nil {
		return fmt.Errorf("get incus credentials secret: %w", err)
	}
	values, err := render.CSIIncusHelmValues(&source, wc)
	if err != nil {
		return err
	}
	if err := applyOwned(ctx, r.Client, r.Scheme, wc, render.CSIIncusHelmRepository(wc.Namespace)); err != nil {
		return fmt.Errorf("apply csi-incus HelmRepository: %w", err)
	}
	hr := render.CSIIncusHelmRelease(wc, values)
	if err := applyOwned(ctx, r.Client, r.Scheme, wc, hr); err != nil {
		return fmt.Errorf("apply csi-incus HelmRelease: %w", err)
	}
	return nil
}

// ensureDescheduler installs kubernetes-sigs/descheduler onto the workload
// cluster via its own official Helm chart - same Flux HelmRepository +
// HelmRelease mechanism as ensureCNI/ensureCSIIncus/ensureMetricsServer. See
// render.DeschedulerHelmValues' own doc comment for why it exists and what
// (little) is overridden from the chart's own defaults.
func (r *WorkloadClusterReconciler) ensureDescheduler(ctx context.Context, wc *benchv1alpha1.WorkloadCluster) error {
	values, err := render.DeschedulerHelmValues(wc)
	if err != nil {
		return err
	}
	if err := applyOwned(ctx, r.Client, r.Scheme, wc, render.DeschedulerHelmRepository(wc.Namespace)); err != nil {
		return fmt.Errorf("apply descheduler HelmRepository: %w", err)
	}
	hr := render.DeschedulerHelmRelease(wc, values)
	if err := applyOwned(ctx, r.Client, r.Scheme, wc, hr); err != nil {
		return fmt.Errorf("apply descheduler HelmRelease: %w", err)
	}
	return nil
}

// ensureMetricsServer installs kubernetes-sigs/metrics-server onto the
// workload cluster via its own official Helm chart - same Flux
// HelmRepository + HelmRelease mechanism as ensureCNI/ensureCSIIncus.
func (r *WorkloadClusterReconciler) ensureMetricsServer(ctx context.Context, wc *benchv1alpha1.WorkloadCluster) error {
	values, err := render.MetricsServerHelmValues(wc)
	if err != nil {
		return err
	}
	if err := applyOwned(ctx, r.Client, r.Scheme, wc, render.MetricsServerHelmRepository(wc.Namespace)); err != nil {
		return fmt.Errorf("apply metrics-server HelmRepository: %w", err)
	}
	hr := render.MetricsServerHelmRelease(wc, values)
	if err := applyOwned(ctx, r.Client, r.Scheme, wc, hr); err != nil {
		return fmt.Errorf("apply metrics-server HelmRelease: %w", err)
	}
	return nil
}

// controlPlaneNodeRoleLabel is kubeadm's own standard label, set on every
// control-plane node regardless of whether its NoSchedule taint is still
// present - a more stable selector for "find a node that will always
// exist" than trying to key off any particular worker pool.
const controlPlaneNodeRoleLabel = "node-role.kubernetes.io/control-plane"

// reachableNodeIP returns an InternalIP a management-cluster pod can use to
// reach this workload cluster directly - confirmed live this session that
// this direction works even though the reverse (workload cluster pod to
// management cluster pod) doesn't. Prefers a control-plane node (long-lived
// by construction; worker pools can be resized/replaced), falling back to
// whatever the first Node in the list reports.
func reachableNodeIP(ctx context.Context, remoteClient client.Client) (string, error) {
	var nodes corev1.NodeList
	if err := remoteClient.List(ctx, &nodes, client.MatchingLabels{controlPlaneNodeRoleLabel: ""}); err != nil {
		return "", fmt.Errorf("list control-plane nodes: %w", err)
	}
	if len(nodes.Items) == 0 {
		if err := remoteClient.List(ctx, &nodes); err != nil {
			return "", fmt.Errorf("list nodes: %w", err)
		}
	}
	for _, node := range nodes.Items {
		for _, addr := range node.Status.Addresses {
			if addr.Type == corev1.NodeInternalIP {
				return addr.Address, nil
			}
		}
	}
	return "", fmt.Errorf("no node with an InternalIP found")
}

// ensurePrometheus installs kube-prometheus-stack (prometheus-operator +
// Prometheus with the remote-write receiver enabled + node-exporter +
// kube-state-metrics + Alertmanager, pre-wired with this project's own
// default alert rules - see render.KubePrometheusStackHelmValues) onto the
// workload cluster via its own official Helm chart - the same Flux
// HelmRepository + HelmRelease mechanism ensureCNI/ensureCSIIncus/
// ensureMetricsServer already use, applied here on the management cluster
// with spec.kubeConfig pointing at the workload cluster, rather than the
// hand-rolled single-Deployment Prometheus this replaces. node-exporter is
// the actual point of the switch: the prior install had no per-node
// CPU/memory/disk/network metrics at all. Alertmanager is bundled into this
// same ensure-step (not a separate one) because prometheus-operator wires
// the Alertmanager CR it creates straight to the Prometheus CR it also
// creates - splitting them would only add a second HelmRelease for no
// benefit. Records management-cluster-reachable URLs for both in this
// object's own status so cmd/apiserver/the web UI can query them directly
// (see internal/api/metrics.go's per-testid Prometheus resolution) -
// Prometheus's own URL is unchanged from before, since the chart-managed
// Prometheus is still reachable at the exact same compat Service
// name/NodePort (see render.PrometheusServiceName's own doc comment for why
// no other consumer needed to change).
func (r *WorkloadClusterReconciler) ensurePrometheus(ctx context.Context, wc *benchv1alpha1.WorkloadCluster) error {
	values, err := render.KubePrometheusStackHelmValues(wc)
	if err != nil {
		return err
	}
	if err := applyOwned(ctx, r.Client, r.Scheme, wc, render.KubePrometheusStackHelmRepository(wc.Namespace)); err != nil {
		return fmt.Errorf("apply kube-prometheus-stack HelmRepository: %w", err)
	}
	hr := render.KubePrometheusStackHelmRelease(wc, values)
	if err := applyOwned(ctx, r.Client, r.Scheme, wc, hr); err != nil {
		return fmt.Errorf("apply kube-prometheus-stack HelmRelease: %w", err)
	}

	remoteClient, err := r.remoteAddonClient(ctx, wc)
	if err != nil {
		return err
	}
	// The default alert rules ship through the chart now
	// (additionalPrometheusRulesMap); an older operator applied them as
	// their own PrometheusRule, which would define every alert twice. A
	// single lookup per reconcile, unlike the other add-ons' cleanup, which
	// stops once their release is Ready: this release already is on every
	// cluster that has the old rule.
	if err := removeLegacyObjects(ctx, remoteClient, render.KubePrometheusStackHelmReleaseName,
		[]client.Object{legacyMonitoringObject("PrometheusRule", render.LegacyDefaultAlertRulesName)}); err != nil {
		return err
	}
	nodeIP, err := reachableNodeIP(ctx, remoteClient)
	if err != nil {
		return fmt.Errorf("resolve reachable node IP: %w", err)
	}
	wc.Status.PrometheusURL = fmt.Sprintf("http://%s:%d", nodeIP, render.PrometheusNodePort)
	wc.Status.AlertmanagerURL = fmt.Sprintf("http://%s:%d", nodeIP, render.AlertmanagerNodePort)
	return nil
}

// ensureClusterAutoscaler installs cluster-autoscaler (kubernetes.github.io/
// autoscaler, --cloud-provider=clusterapi) for this WorkloadCluster if at
// least one of its worker pools sets spec.autoscaling - a no-op otherwise.
// Unlike every other addon in this file, this applies locally (r.Client, no
// spec.kubeConfig on the HelmRelease) rather than remotely onto the
// workload cluster: cluster-autoscaler must run wherever the Cluster API
// MachineDeployment/Cluster objects it watches actually live, which is the
// management cluster this operator itself runs on (see
// render.ClusterAutoscalerHelmRelease's own doc comment for the verified
// clusterAPIMode reasoning).
func (r *WorkloadClusterReconciler) ensureClusterAutoscaler(ctx context.Context, wc *benchv1alpha1.WorkloadCluster) error {
	if !render.HasAutoscalingPool(wc) {
		// Every pool's autoscaling was removed (or never set) - tear down a
		// previously-installed instance rather than leave it running
		// forever against node groups that no longer carry the
		// min/max-size annotations it was managing. Plain Delete, not a
		// wait for helm-controller's own uninstall to finish (unlike
		// AuthentikDeploymentReconciler's own deletion path) - this has no
		// target-namespace sweep afterward that would race an in-progress
		// uninstall, so there's nothing to block on.
		hr := render.ClusterAutoscalerHelmRelease(wc, nil)
		if err := r.Delete(ctx, hr); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete cluster-autoscaler HelmRelease: %w", err)
		}
		return nil
	}
	values, err := render.ClusterAutoscalerHelmValues(wc)
	if err != nil {
		return err
	}
	if err := applyOwned(ctx, r.Client, r.Scheme, wc, render.ClusterAutoscalerHelmRepository(wc.Namespace)); err != nil {
		return fmt.Errorf("apply cluster-autoscaler HelmRepository: %w", err)
	}
	hr := render.ClusterAutoscalerHelmRelease(wc, values)
	if err := applyOwned(ctx, r.Client, r.Scheme, wc, hr); err != nil {
		return fmt.Errorf("apply cluster-autoscaler HelmRelease: %w", err)
	}
	return nil
}

// ensureControlPlane creates the KubeadmControlPlane once it is safe to.
// A non-empty waiting message means it isn't yet: the caller reports it as
// the Ready condition and requeues after requeue.
//
// KubeadmControlPlane/MachineDeployment must not be created until the
// Cluster's controlPlaneEndpoint is populated (copied up from
// LXCCluster.status once Incus provisions the load balancer): the
// kubeadm bootstrap provider bakes controlPlaneEndpoint into the very
// first control-plane machine's ClusterConfiguration, which is also what
// tells kubeadm to add it as an apiserver certificate SAN. Created
// before that, the machine boots with a cert that's only valid for its
// own IP, and every client going through the load balancer's IP/DNS -
// including this operator's own multicluster tracker - fails TLS
// verification permanently for that machine's lifetime. For the same
// reason, an IPv6 endpoint is swapped for IPv4 here, before the control
// plane exists - see preferIPv4Endpoint.
func (r *WorkloadClusterReconciler) ensureControlPlane(ctx context.Context, wc *benchv1alpha1.WorkloadCluster, cluster *clusterv1.Cluster) (waiting string, requeue time.Duration, err error) {
	if cluster.Spec.ControlPlaneEndpoint.Host == "" {
		return "waiting for the load balancer's controlPlaneEndpoint before creating the control plane", 2 * time.Second, nil
	}
	kcp := render.KubeadmControlPlane(wc)
	var existing controlplanev1.KubeadmControlPlane
	switch err := r.Get(ctx, client.ObjectKeyFromObject(kcp), &existing); {
	case apierrors.IsNotFound(err):
		wait, err := r.preferIPv4Endpoint(ctx, wc, cluster)
		if err != nil {
			return "", 0, err
		}
		if wait {
			return "waiting for the load balancer's IPv4 address before creating the control plane", 5 * time.Second, nil
		}
	case err != nil:
		return "", 0, fmt.Errorf("get KubeadmControlPlane: %w", err)
	}
	if err := applyCreateOnly(ctx, r.Client, r.Scheme, wc, kcp); err != nil {
		return "", 0, fmt.Errorf("apply KubeadmControlPlane: %w", err)
	}
	return "", 0, nil
}

// deleteRemovedWorkerPools deletes the MachineDeployment (and, via CAPI's own
// garbage collection of a deleted MachineDeployment's owned objects, its
// Machines) for any pool that used to be in spec.workerPools but no longer
// is. Listed by the cluster-name label render.MachineDeployment sets on
// every pool it renders, rather than an owner-reference index, since every
// pool's MachineDeployment already carries that label for CAPI's own
// benefit.
func (r *WorkloadClusterReconciler) deleteRemovedWorkerPools(ctx context.Context, wc *benchv1alpha1.WorkloadCluster, desiredPools map[string]bool) error {
	var list clusterv1.MachineDeploymentList
	if err := r.List(ctx, &list, client.InNamespace(wc.Namespace),
		client.MatchingLabels{"cluster.x-k8s.io/cluster-name": wc.Name}); err != nil {
		return fmt.Errorf("list MachineDeployments: %w", err)
	}
	for i := range list.Items {
		md := &list.Items[i]
		poolName, ok := strings.CutPrefix(md.Name, wc.Name+"-")
		if !ok || desiredPools[poolName] {
			continue
		}
		if err := r.Delete(ctx, md); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete MachineDeployment %s: %w", md.Name, err)
		}
		mhc := &clusterv1.MachineHealthCheck{Name: md.Name, Namespace: md.Namespace}
		if err := r.Delete(ctx, mhc); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete MachineHealthCheck %s: %w", md.Name, err)
		}
	}
	return nil
}

func (r *WorkloadClusterReconciler) reconcileDelete(ctx context.Context, wc *benchv1alpha1.WorkloadCluster) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(wc, workloadClusterFinalizer) {
		return ctrl.Result{}, nil
	}

	// Deleting the owned Cluster (rather than relying on GC once the
	// WorkloadCluster itself disappears) lets CAPI actually tear down its
	// machines before this finalizer is removed - the LXCMachineTemplates/
	// KubeadmControlPlane/MachineDeployment all get garbage-collected as the
	// Cluster's owned objects once CAPI is done with them.
	var cluster clusterv1.Cluster
	err := r.Get(ctx, types.NamespacedName{Name: wc.Name, Namespace: wc.Namespace}, &cluster)
	switch {
	case apierrors.IsNotFound(err):
		controllerutil.RemoveFinalizer(wc, workloadClusterFinalizer)
		return ctrl.Result{}, r.Update(ctx, wc)
	case err != nil:
		return ctrl.Result{}, fmt.Errorf("get Cluster: %w", err)
	default:
		if cluster.DeletionTimestamp.IsZero() {
			if err := r.Delete(ctx, &cluster); err != nil && !apierrors.IsNotFound(err) {
				return ctrl.Result{}, fmt.Errorf("delete Cluster: %w", err)
			}
		}
		return ctrl.Result{RequeueAfter: capiDeleteRequeueInterval}, nil
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *WorkloadClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&benchv1alpha1.WorkloadCluster{}).
		Owns(&clusterv1.Cluster{}).
		// The add-on HelmReleases it applies: their Ready condition is what
		// status.addons reports, so it has to follow them settling.
		Owns(&helmv2.HelmRelease{}).
		Named("workloadcluster").
		Complete(r)
}
