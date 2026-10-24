package controller

import (
	"context"
	"fmt"
	"strconv"
	"time"

	helmv2 "github.com/fluxcd/helm-controller/api/v2"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
	"github.com/authentik-labs/akbench/internal/multicluster"
	"github.com/authentik-labs/akbench/internal/render"
)

// authentikDeploymentFinalizer guards the remote objects (namespace, CNPG
// Cluster, generated Secrets - everything spec.clusterRef puts on a
// different cluster than the AuthentikDeployment object itself) that
// Kubernetes garbage collection cannot reach on its own.
const authentikDeploymentFinalizer = "bench.goauthentik.io/remote-cleanup"

// defaultPyroscopeHost/defaultTracingEndpoint are the central metrics
// stack's Pyroscope/Tempo, used whenever a deployment doesn't set
// spec.authentik.pyroscopeHost/errorReporting.tracingEndpoint itself -
// mirrors defaultRemoteWriteURL's env-overridable-default pattern
// (testcase_controller.go) so every deployment gets profiling/tracing wired up
// for free instead of needing it configured per deployment.
var (
	defaultPyroscopeHost   = envOrDefault("BENCH_PYROSCOPE_HOST", "http://pyroscope.bench-system.svc.cluster.local:4040")
	defaultTracingEndpoint = envOrDefault("BENCH_TRACING_ENDPOINT", "http://tempo.bench-system.svc.cluster.local:4318")
)

// kubeconfigSecretName is the Secret name convention Cluster API itself
// defines for a Cluster's admin kubeconfig - the same one
// render.KubeconfigSecretName (internal/render/workloadcluster.go) produces,
// duplicated here rather than imported to avoid a dependency from
// render (WorkloadCluster) back onto AuthentikDeployment's naming.
func kubeconfigSecretName(clusterName string) string {
	return clusterName + "-kubeconfig"
}

// AuthentikDeploymentReconciler reconciles an AuthentikDeployment object.
//
// spec.clusterRef (a cluster.x-k8s.io Cluster name in the same namespace)
// deploys onto any cluster Tracker can reach - a Cluster API-managed
// WorkloadCluster or a pre-existing one - via the "<cluster>-kubeconfig"
// Secret convention CAPI defines. Empty deploys onto the management cluster
// the operator itself runs on. The HelmRelease/HelmRepository Flux objects
// always live on the management cluster regardless (Flux's own
// spec.kubeConfig.secretRef is what points helm-controller at the remote
// cluster) - only the namespace, postgres backend, and the Secrets this
// controller generates move with clusterRef.
type AuthentikDeploymentReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Tracker  *multicluster.Tracker
	Recorder record.EventRecorder
	// VolumeUsage reads postgres volume usage for auto-growth; nil asks
	// the workload cluster's kubelets. Overridden in tests, which have none.
	VolumeUsage volumeUsageFunc
}

// resolveClient returns the client this reconcile should use for
// everything except the HelmRelease/HelmRepository themselves: r.Client
// when spec.clusterRef is empty, or a live client for the named Cluster via
// Tracker otherwise. A non-nil error here always means "not ready yet,
// requeue" (multicluster.ErrClusterNotConnected) rather than a permanent
// failure.
func (r *AuthentikDeploymentReconciler) resolveClient(ctx context.Context, topo *benchv1alpha1.AuthentikDeployment) (client.Client, error) {
	if topo.Spec.ClusterRef.Name == "" {
		return r.Client, nil
	}
	if r.Tracker == nil {
		return nil, fmt.Errorf("multicluster tracker not configured")
	}
	return r.Tracker.GetClient(ctx, client.ObjectKey{Namespace: topo.Namespace, Name: topo.Spec.ClusterRef.Name})
}

// resolveFixturesBlueprint looks up the fixtures blueprint ConfigMap, which,
// like extraValuesRef, always lives alongside the AuthentikDeployment object
// itself (topo.Namespace on the management cluster) rather than on rc -
// `make fixtures` writes it there, and it's config for the deployment, not
// part of it. Optional: a deployment with none configured just skips
// blueprint application entirely (fixtures are a dev/benchmark convenience,
// not required). When one is configured, mirrors it onto rc as a ConfigMap
// (the same create-or-update pattern ensureFixturesSecret/
// ensureScriptConfigMap already use elsewhere) and applies the PVC a
// BlueprintJob will mount, returning the blueprint's own content so the
// caller can compute BlueprintJobName's content hash.
func (r *AuthentikDeploymentReconciler) resolveFixturesBlueprint(ctx context.Context, rc client.Client, topo *benchv1alpha1.AuthentikDeployment) (bool, string, error) {
	var cm corev1.ConfigMap
	if err := r.Get(ctx, types.NamespacedName{Name: fixturesBlueprintConfigMapName, Namespace: topo.Namespace}, &cm); err != nil {
		if apierrors.IsNotFound(err) {
			return false, "", nil
		}
		return false, "", fmt.Errorf("get fixtures blueprint configmap: %w", err)
	}
	content := cm.Data[fixturesBlueprintKey]

	// Mirrored into topo.Spec.Namespace (the deployment's own target
	// namespace, e.g. "main-2026-08-23") - not topo.Namespace (the CR
	// object's own namespace, always "default") - since that's where the
	// BlueprintPVC/BlueprintJob it's mounted into actually live. Confirmed
	// live: mirroring into topo.Namespace instead left the Job's pod stuck
	// in ContainerCreating forever ("configmap ... not found"), since
	// nothing ever mirrored it into the namespace the Job could see.
	var existing corev1.ConfigMap
	mirrorErr := rc.Get(ctx, types.NamespacedName{Name: fixturesBlueprintConfigMapName, Namespace: topo.Spec.Namespace}, &existing)
	switch {
	case apierrors.IsNotFound(mirrorErr):
		mirrored := &corev1.ConfigMap{
			Name: fixturesBlueprintConfigMapName, Namespace: topo.Spec.Namespace,
			Data: map[string]string{fixturesBlueprintKey: content},
		}
		if err := rc.Create(ctx, mirrored); err != nil {
			return false, "", fmt.Errorf("mirror fixtures blueprint configmap: %w", err)
		}
	case mirrorErr != nil:
		return false, "", fmt.Errorf("get mirrored fixtures blueprint configmap: %w", mirrorErr)
	default:
		existing.Data = map[string]string{fixturesBlueprintKey: content}
		if err := rc.Update(ctx, &existing); err != nil {
			return false, "", fmt.Errorf("update mirrored fixtures blueprint configmap: %w", err)
		}
	}

	if err := applyUnowned(ctx, rc, render.BlueprintPVC(topo)); err != nil {
		return false, "", fmt.Errorf("apply blueprint PVC: %w", err)
	}
	return true, content, nil
}

// applyExtraValues merges topo.Spec.Authentik.ExtraValuesRef's ConfigMap
// (which, like the fixtures blueprint ConfigMap, always lives alongside the
// AuthentikDeployment object itself rather than on rc) over values, if one is
// referenced. done=true means the caller should return (result, err)
// immediately - either a real error, or this reconcile already updated
// status itself (a not-yet-existing ConfigMap requeues on a timer; invalid
// YAML doesn't, since the extra-values API always touches the deployment
// object on save, which re-triggers this reconcile as soon as the user
// corrects it).
func (r *AuthentikDeploymentReconciler) applyExtraValues(ctx context.Context, topo *benchv1alpha1.AuthentikDeployment, values *apiextensionsv1.JSON) (*apiextensionsv1.JSON, ctrl.Result, bool, error) {
	ref := topo.Spec.Authentik.ExtraValuesRef
	if ref == nil || ref.Name == "" {
		return values, ctrl.Result{}, false, nil
	}
	var cm corev1.ConfigMap
	if err := r.Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: topo.Namespace}, &cm); err != nil {
		if apierrors.IsNotFound(err) {
			setCondition(r.Recorder, topo, &topo.Status.Conditions, topo.Generation, benchv1alpha1.ConditionReady, metav1.ConditionFalse,
				benchv1alpha1.ReasonDependencyNotReady, fmt.Sprintf("extraValuesRef ConfigMap %q not found", ref.Name))
			topo.Status.Phase = benchv1alpha1.PhaseFor(topo.Status.Conditions)
			if err := r.Status().Update(ctx, topo); err != nil {
				return nil, ctrl.Result{}, true, err
			}
			return nil, ctrl.Result{RequeueAfter: 5 * time.Second}, true, nil
		}
		return nil, ctrl.Result{}, true, fmt.Errorf("get extra values configmap: %w", err)
	}
	merged, err := render.MergeExtraValues(values, cm.Data["values.yaml"])
	if err != nil {
		setCondition(r.Recorder, topo, &topo.Status.Conditions, topo.Generation, benchv1alpha1.ConditionReady, metav1.ConditionFalse,
			benchv1alpha1.ReasonReconcileError, fmt.Sprintf("invalid extraValuesRef YAML: %v", err))
		topo.Status.Phase = benchv1alpha1.PhaseFor(topo.Status.Conditions)
		return nil, ctrl.Result{}, true, r.Status().Update(ctx, topo)
	}
	return merged, ctrl.Result{}, false, nil
}

// helmReleaseReadiness mirrors hr's own Ready condition - but only once
// helm-controller has actually observed the exact spec (generation) this
// reconcile just applied. Without that check, a Ready=True left over from
// the *previous* generation reads as ready immediately after this reconcile
// pushes a new one - fine for the Ready condition alone (self-corrects next
// reconcile), but reconcileBlueprint acts on this signal synchronously via
// pod exec, and raced a rolling update this way: it read a stale Ready=True
// from before a spec change added the fixtures volume mount, and exec'd
// into whichever pod happened to still be running - the old one, without
// the mount ("Directory nonexistent"), not the new one the still-in-flight
// rollout was replacing it with.
func (r *AuthentikDeploymentReconciler) helmReleaseReadiness(ctx context.Context, hr *helmv2.HelmRelease) (metav1.ConditionStatus, string, error) {
	var existing helmv2.HelmRelease
	if err := r.Get(ctx, types.NamespacedName{Name: hr.Name, Namespace: hr.Namespace}, &existing); err != nil {
		if apierrors.IsNotFound(err) {
			return metav1.ConditionFalse, "waiting for helm-controller to reconcile", nil
		}
		return metav1.ConditionFalse, "", fmt.Errorf("get HelmRelease: %w", err)
	}
	cond := meta.FindStatusCondition(existing.Status.Conditions, "Ready")
	if existing.Status.ObservedGeneration != existing.Generation {
		// helm-controller only bumps status.observedGeneration once an
		// action finishes - for the whole first install, that's minutes. Its
		// Ready condition is already stamped with the new generation while
		// the action runs, though, and says what it's doing ("Running
		// 'install' action with timeout of 15m0s"), which is what to show.
		if cond != nil && cond.ObservedGeneration == existing.Generation && cond.Status != metav1.ConditionTrue && cond.Message != "" {
			return metav1.ConditionFalse, cond.Message, nil
		}
		return metav1.ConditionFalse, "waiting for helm-controller to pick up the latest spec", nil
	}
	if cond == nil {
		return metav1.ConditionFalse, "waiting for helm-controller to reconcile", nil
	}
	if cond.Status == metav1.ConditionTrue {
		return metav1.ConditionTrue, "HelmRelease is Ready", nil
	}
	return metav1.ConditionFalse, cond.Message, nil
}

// applyHelmRelease applies hr - with the worker drained on the old release
// first, then held at zero while a Dedicated-placement upgrade rolls out (see
// render.WorkerUpgrade) - and returns its readiness, which stays False until
// the worker is back up, plus whether to poll for the drained worker pods to
// go away (their deletion on the workload cluster isn't watched).
func (r *AuthentikDeploymentReconciler) applyHelmRelease(ctx context.Context, topo *benchv1alpha1.AuthentikDeployment, hr *helmv2.HelmRelease) (metav1.ConditionStatus, string, bool, error) {
	step, existing, err := r.workerUpgradeStep(ctx, topo, hr)
	if err != nil {
		return metav1.ConditionFalse, "", false, err
	}
	switch step {
	case render.WorkerUpgradeDrain:
		hr.Spec = *existing.Spec.DeepCopy()
		fallthrough
	case render.WorkerUpgradeHold:
		if hr.Spec.Values, err = render.WithWorkerReplicas(hr.Spec.Values, 0); err != nil {
			return metav1.ConditionFalse, "", false, err
		}
	}
	if err := applyOwned(ctx, r.Client, r.Scheme, topo, hr); err != nil {
		return metav1.ConditionFalse, "", false, fmt.Errorf("apply HelmRelease: %w", err)
	}
	ready, message, err := r.helmReleaseReadiness(ctx, hr)
	switch step {
	case render.WorkerUpgradeDrain:
		return metav1.ConditionFalse, "scaling the worker down before upgrading", true, err
	case render.WorkerUpgradeHold:
		return metav1.ConditionFalse, "upgrading with the worker scaled down until the new server pods are running: " + message, false, err
	case render.WorkerUpgradeRelease:
		return metav1.ConditionFalse, "scaling the worker back up after the upgrade", false, err
	}
	return ready, message, false, err
}

// workerUpgradeStep compares hr against the live HelmRelease (returned too)
// to decide whether an upgrade needs the worker scaled down first (see
// render.WorkerUpgrade).
func (r *AuthentikDeploymentReconciler) workerUpgradeStep(ctx context.Context, topo *benchv1alpha1.AuthentikDeployment, hr *helmv2.HelmRelease) (render.WorkerUpgradeStep, *helmv2.HelmRelease, error) {
	if topo.Spec.Placement.Mode != benchv1alpha1.PlacementDedicated {
		return render.WorkerUpgradeNone, nil, nil
	}
	var existing helmv2.HelmRelease
	if err := r.Get(ctx, types.NamespacedName{Name: hr.Name, Namespace: hr.Namespace}, &existing); err != nil {
		if apierrors.IsNotFound(err) {
			return render.WorkerUpgradeNone, nil, nil
		}
		return render.WorkerUpgradeNone, nil, fmt.Errorf("get HelmRelease: %w", err)
	}
	ready := existing.Status.ObservedGeneration == existing.Generation &&
		meta.IsStatusConditionTrue(existing.Status.Conditions, "Ready")
	workerPods, err := r.countWorkerPods(ctx, topo)
	if err != nil {
		return render.WorkerUpgradeNone, nil, fmt.Errorf("list worker pods: %w", err)
	}
	step, err := render.WorkerUpgrade(topo, &existing, hr, ready, workerPods)
	return step, &existing, err
}

// countWorkerPods counts topo's worker pods, terminating ones included -
// they still block the node for Dedicated's anti-affinity. Listed through a
// clientset rather than rc so this doesn't start a cluster-wide Pod
// informer. ponytail: a deployment without a clusterRef reports none, so its
// upgrade only waits for the drain to be applied, not for the pods to go.
func (r *AuthentikDeploymentReconciler) countWorkerPods(ctx context.Context, topo *benchv1alpha1.AuthentikDeployment) (int, error) {
	if r.Tracker == nil || topo.Spec.ClusterRef.Name == "" {
		return 0, nil
	}
	cfg, err := r.Tracker.GetRESTConfig(ctx, client.ObjectKey{Namespace: topo.Namespace, Name: topo.Spec.ClusterRef.Name})
	if err != nil {
		return 0, err
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return 0, err
	}
	pods, err := cs.CoreV1().Pods(topo.Spec.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labels.SelectorFromSet(render.WorkerPodLabels(topo)).String(),
	})
	if err != nil {
		return 0, err
	}
	return len(pods.Items), nil
}

// recordChartVersion stores the chart version helm-controller last installed
// in status.chartVersion, which render.ChartVersion keeps an unpinned
// deployment on. Recorded before rendering, so the HelmRelease switches from
// "*" to the concrete version in the same reconcile that first sees it. A
// failed lookup keeps the previously recorded version.
func (r *AuthentikDeploymentReconciler) recordChartVersion(ctx context.Context, topo *benchv1alpha1.AuthentikDeployment) {
	var existing helmv2.HelmRelease
	if err := r.Get(ctx, types.NamespacedName{Name: render.HelmReleaseName(topo), Namespace: topo.Namespace}, &existing); err != nil {
		if !apierrors.IsNotFound(err) {
			logf.FromContext(ctx).Error(err, "Failed to get HelmRelease", "name", render.HelmReleaseName(topo))
		}
		return
	}
	if latest := existing.Status.History.Latest(); latest != nil && latest.ChartVersion != "" {
		topo.Status.ChartVersion = latest.ChartVersion
	}
}

// reconcileBlueprint applies the fixtures blueprint (if any is configured)
// once the HelmRelease itself is Ready (a server pod actually exists to
// exec into), returning the BlueprintApplied condition to set and whether
// the caller should requeue on a timer to retry a transient failure. A
// deployment with no blueprint configured reports Ready with nothing done, so
// it never blocks the overall Ready condition.
func (r *AuthentikDeploymentReconciler) reconcileBlueprint(ctx context.Context, rc client.Client, topo *benchv1alpha1.AuthentikDeployment, wc *benchv1alpha1.WorkloadCluster,
	hasBlueprint bool, helmReleaseReady metav1.ConditionStatus, content string,
) (metav1.ConditionStatus, string, bool) {
	switch {
	case !hasBlueprint:
		return metav1.ConditionTrue, "no fixtures blueprint configured", false
	case helmReleaseReady != metav1.ConditionTrue:
		return metav1.ConditionFalse, "waiting for the HelmRelease to be Ready before applying the fixtures blueprint", false
	}

	name := render.BlueprintJobName(topo, "fixtures", content)
	var job batchv1.Job
	err := rc.Get(ctx, types.NamespacedName{Name: name, Namespace: topo.Spec.Namespace}, &job)
	switch {
	case apierrors.IsNotFound(err):
		newJob := render.BlueprintJob(topo, wc, name, fixturesBlueprintConfigMapName, fixturesBlueprintKey)
		if err := rc.Create(ctx, newJob); err != nil && !apierrors.IsAlreadyExists(err) {
			return metav1.ConditionFalse, fmt.Sprintf("create fixtures blueprint import job: %v", err), true
		}
		return metav1.ConditionFalse, "fixtures blueprint import job started", true
	case err != nil:
		return metav1.ConditionFalse, fmt.Sprintf("get fixtures blueprint import job: %v", err), true
	}

	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobComplete && c.Status == corev1.ConditionTrue {
			return metav1.ConditionTrue, "fixtures blueprint applied", false
		}
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			return metav1.ConditionFalse, fmt.Sprintf("fixtures blueprint import job %q failed: %s", name, c.Message), false
		}
	}
	return metav1.ConditionFalse, "waiting for fixtures blueprint import job to complete", true
}

// reconcilePostgresRoleFix creates render.PostgresRoleFixJob once the
// bundled embedded-postgres StatefulSet itself is ready - see that
// function's own doc comment for the real, permanent gap this works around
// (the authentik chart's own bundled postgresql subchart uses the plain,
// official postgres image, whose entrypoint never creates the "postgres"
// role postgres_exporter is hardcoded to authenticate as).
//
// Deliberately checks the postgres StatefulSet directly rather than the
// overall helmReleaseReady condition this file's other reconcile steps use:
// a HelmRelease's own Ready condition is an aggregate across every resource
// the chart renders (server, worker, and postgres together), so a broken
// server/worker image (crash-looping for reasons that have nothing to do
// with postgres) permanently blocks it from ever becoming True - which, with
// the old helmReleaseReady-gated check, meant this fix-up Job would never
// run at all for such a deployment, leaving pg_up stuck at 0 forever even
// though postgres itself was healthy the whole time. Confirmed live: exactly
// this happened to a deployment whose server/worker pods crash-loop on an
// unrelated experimental Python version mismatch.
//
// Best-effort and non-blocking, unlike reconcileBlueprint: this only affects
// observability (empty postgres.json Grafana panels, a permanently-firing
// AuthentikBenchPostgresDown alert), never the deployment's own actual
// readiness, so a failure here is logged/eventer'd but never holds up the
// overall Ready condition.
func (r *AuthentikDeploymentReconciler) reconcilePostgresRoleFix(ctx context.Context, rc client.Client, topo *benchv1alpha1.AuthentikDeployment, wc *benchv1alpha1.WorkloadCluster) {
	if topo.Spec.Postgres.Mode != benchv1alpha1.PostgresEmbedded {
		return
	}
	var sts appsv1.StatefulSet
	key := types.NamespacedName{Name: render.EmbeddedPostgresStatefulSetName(topo), Namespace: topo.Spec.Namespace}
	if err := rc.Get(ctx, key, &sts); err != nil || sts.Status.ReadyReplicas < 1 {
		return
	}
	job := render.PostgresRoleFixJob(topo, wc)
	if err := rc.Create(ctx, job); err != nil && !apierrors.IsAlreadyExists(err) {
		r.Recorder.Eventf(topo, corev1.EventTypeWarning, "PostgresRoleFixFailed", "failed to create postgres role-fix job: %v", err)
	}
}

// +kubebuilder:rbac:groups=bench.goauthentik.io,resources=authentikdeployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=bench.goauthentik.io,resources=authentikdeployments/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=bench.goauthentik.io,resources=authentikdeployments/finalizers,verbs=update
// +kubebuilder:rbac:groups=helm.toolkit.fluxcd.io,resources=helmreleases,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=source.toolkit.fluxcd.io,resources=helmrepositories,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=postgresql.cnpg.io,resources=clusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=namespaces,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch;create;update
// +kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;watch;create;update
// +kubebuilder:rbac:groups=core,resources=persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;delete

func (r *AuthentikDeploymentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var topo benchv1alpha1.AuthentikDeployment
	if err := r.Get(ctx, req.NamespacedName, &topo); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !topo.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &topo)
	}
	if !controllerutil.ContainsFinalizer(&topo, authentikDeploymentFinalizer) {
		controllerutil.AddFinalizer(&topo, authentikDeploymentFinalizer)
		if err := r.Update(ctx, &topo); err != nil {
			return ctrl.Result{}, err
		}
	}

	rc, err := r.resolveClient(ctx, &topo)
	if err != nil && r.Tracker.WarmingUp(ctx, client.ObjectKey{Namespace: topo.Namespace, Name: topo.Spec.ClusterRef.Name}, err) &&
		meta.FindStatusCondition(topo.Status.Conditions, benchv1alpha1.ConditionReady) != nil {
		// Right after an operator restart: keep the last known status
		// instead of flapping every deployment to not-Ready until the
		// ClusterCache has connected.
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}
	if err != nil {
		setCondition(r.Recorder, &topo, &topo.Status.Conditions, topo.Generation, benchv1alpha1.ConditionReady, metav1.ConditionFalse,
			benchv1alpha1.ReasonDependencyNotReady, fmt.Sprintf("waiting for cluster %q to be reachable: %v", topo.Spec.ClusterRef.Name, err))
		topo.Status.Phase = benchv1alpha1.PhaseFor(topo.Status.Conditions)
		if err := r.Status().Update(ctx, &topo); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	if _, ok := topo.Annotations[benchv1alpha1.AnnotationResetRequested]; ok {
		return r.reconcileReset(ctx, rc, &topo)
	}

	if err := ensureNamespace(ctx, rc, topo.Spec.Namespace); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensure namespace: %w", err)
	}

	// wc is this deployment's own WorkloadCluster object (nil for the
	// management cluster, or if it can't be resolved) - used only for
	// node-pool placement (AuthentikPoolPlacement), never anything load-
	// bearing, so a lookup failure here falls through silently rather than
	// blocking the reconcile the way resolveClient's own failure above does.
	wc := resolveWorkloadCluster(ctx, r.Client, topo.Namespace, topo.Spec.ClusterRef.Name)

	// Before resolvePostgres: in CNPG mode, a size grown here is applied
	// by the Cluster resolvePostgres renders.
	storageRecheck := r.reconcilePostgresStorage(ctx, rc, &topo)

	pgConn, pgReady, err := r.resolvePostgres(ctx, rc, &topo, wc)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("resolve postgres: %w", err)
	}
	if !pgReady {
		setCondition(r.Recorder, &topo, &topo.Status.Conditions, topo.Generation, benchv1alpha1.ConditionReady, metav1.ConditionFalse,
			benchv1alpha1.ReasonReconciling, "waiting on postgres backend")
		topo.Status.Phase = benchv1alpha1.PhaseFor(topo.Status.Conditions)
		if err := r.Status().Update(ctx, &topo); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	repo := render.HelmRepository(topo.Namespace, topo.Spec.Chart)
	if err := applyOwned(ctx, r.Client, r.Scheme, &topo, repo); err != nil {
		return ctrl.Result{}, fmt.Errorf("apply HelmRepository: %w", err)
	}

	// The chart does not generate AUTHENTIK_SECRET_KEY itself - authentik
	// refuses to start without one - so the operator generates and caches it
	// once, the same way the Ansible harness's credentials/ directory does.
	// Passed as a literal value (authentik.secret_key), not via
	// authentik.existingSecret: existingSecret tells the chart to skip
	// rendering its own authentik.env-derived config Secret entirely, which
	// is also where postgresql.*/log_level/error_reporting get baked into
	// AUTHENTIK_* env vars - existingSecret would silently drop all of those.
	secretKey, err := ensureRandomSecret(ctx, rc,
		types.NamespacedName{Name: topo.Name + "-authentik-config", Namespace: topo.Spec.Namespace},
		"AUTHENTIK_SECRET_KEY", 32)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("ensure authentik secret key: %w", err)
	}

	pyroscopeHost := topo.Spec.Authentik.PyroscopeHost
	if pyroscopeHost == "" {
		pyroscopeHost = defaultPyroscopeHost
	}
	tracingEndpoint := topo.Spec.Authentik.ErrorReporting.TracingEndpoint
	if tracingEndpoint == "" {
		tracingEndpoint = defaultTracingEndpoint
	}

	hasBlueprint, blueprintContent, err := r.resolveFixturesBlueprint(ctx, rc, &topo)
	if err != nil {
		return ctrl.Result{}, err
	}

	values, err := render.HelmValues(&topo, wc, secretKey, pgConn, pyroscopeHost, tracingEndpoint)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("build helm values: %w", err)
	}
	merged, result, done, err := r.applyExtraValues(ctx, &topo, values)
	if done {
		return result, err
	}
	values = merged
	kubeConfigSecretName := ""
	if topo.Spec.ClusterRef.Name != "" {
		kubeConfigSecretName = kubeconfigSecretName(topo.Spec.ClusterRef.Name)
	}
	r.recordChartVersion(ctx, &topo)
	hr := render.HelmRelease(&topo, kubeConfigSecretName, values)
	helmReleaseReady, helmReleaseMessage, requeueForWorker, err := r.applyHelmRelease(ctx, &topo, hr)
	if err != nil {
		return ctrl.Result{}, err
	}
	setCondition(r.Recorder, &topo, &topo.Status.Conditions, topo.Generation, benchv1alpha1.ConditionHelmReleaseReady, helmReleaseReady,
		benchv1alpha1.ReasonReady, helmReleaseMessage)

	topo.Status.HelmReleaseRef = &corev1.LocalObjectReference{Name: hr.Name}
	topo.Status.ServiceEndpoint = render.ServiceEndpoint(&topo)
	topo.Status.ObservedGeneration = topo.Generation

	blueprintReady, blueprintMessage, requeueForBlueprint := r.reconcileBlueprint(ctx, rc, &topo, wc, hasBlueprint, helmReleaseReady, blueprintContent)
	setCondition(r.Recorder, &topo, &topo.Status.Conditions, topo.Generation, benchv1alpha1.ConditionBlueprintApplied, blueprintReady,
		benchv1alpha1.ReasonReady, blueprintMessage)

	r.reconcilePostgresRoleFix(ctx, rc, &topo, wc)

	ready := helmReleaseReady == metav1.ConditionTrue && blueprintReady == metav1.ConditionTrue
	if ready {
		setCondition(r.Recorder, &topo, &topo.Status.Conditions, topo.Generation, benchv1alpha1.ConditionReady, metav1.ConditionTrue,
			benchv1alpha1.ReasonReady, "authentik chart installed and ready")
	} else {
		msg := helmReleaseMessage
		if helmReleaseReady == metav1.ConditionTrue {
			msg = blueprintMessage
		}
		setCondition(r.Recorder, &topo, &topo.Status.Conditions, topo.Generation, benchv1alpha1.ConditionReady, metav1.ConditionFalse,
			benchv1alpha1.ReasonReconciling, msg)
	}

	topo.Status.Phase = benchv1alpha1.PhaseFor(topo.Status.Conditions)

	if err := r.Status().Update(ctx, &topo); err != nil {
		return ctrl.Result{}, err
	}
	log.Info("reconciled AuthentikDeployment", "ready", ready)
	if requeueForBlueprint || requeueForWorker {
		return ctrl.Result{RequeueAfter: soonest(10*time.Second, storageRecheck)}, nil
	}
	return ctrl.Result{RequeueAfter: storageRecheck}, nil
}

func (r *AuthentikDeploymentReconciler) reconcileDelete(ctx context.Context, topo *benchv1alpha1.AuthentikDeployment) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	if !controllerutil.ContainsFinalizer(topo, authentikDeploymentFinalizer) {
		return ctrl.Result{}, nil
	}

	// Delete the HelmRelease first and wait for helm-controller's own
	// finalizer to finish running `helm uninstall`, instead of deleting
	// spec.namespace immediately. `helm uninstall` is what correctly removes
	// resources the chart renders as cluster-scoped (e.g. authentik's own
	// ClusterRole/ClusterRoleBinding) - those live in neither the
	// HelmRelease's namespace nor spec.namespace, so a namespace delete can
	// never reach them, and an orphaned one then blocks every future install
	// under the same release name with an ownership-conflict error.
	var hr helmv2.HelmRelease
	err := r.Get(ctx, types.NamespacedName{Name: render.HelmReleaseName(topo), Namespace: topo.Namespace}, &hr)
	switch {
	case apierrors.IsNotFound(err):
		// Already gone - fall through to the namespace sweep below.
	case err != nil:
		return ctrl.Result{}, fmt.Errorf("get HelmRelease: %w", err)
	default:
		if hr.DeletionTimestamp.IsZero() {
			if err := r.Delete(ctx, &hr); err != nil && !apierrors.IsNotFound(err) {
				return ctrl.Result{}, fmt.Errorf("delete HelmRelease: %w", err)
			}
		}
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}

	// Everything else - the AUTHENTIK_SECRET_KEY/postgres-password Secrets
	// ensureRandomSecret created, and (CNPG mode) the Cluster this controller
	// applies - lives in spec.namespace, a *different* namespace an
	// ownerReference can't reach (and, with clusterRef set, a different
	// cluster entirely), so deleting that namespace on whichever cluster it's
	// on is what cleans those up.
	if topo.Spec.Namespace != "" {
		rc, err := r.resolveClient(ctx, topo)
		if err != nil {
			// The remote cluster is unreachable - most likely its
			// WorkloadCluster was already torn down, taking spec.namespace
			// with it. Don't block finalizer removal forever on a cluster
			// that may never come back.
			log.Info("cluster unreachable during delete, skipping remote namespace cleanup", "clusterRef", topo.Spec.ClusterRef.Name, "error", err.Error())
		} else {
			ns := &corev1.Namespace{Name: topo.Spec.Namespace}
			if err := rc.Delete(ctx, ns); err != nil && !apierrors.IsNotFound(err) {
				return ctrl.Result{}, fmt.Errorf("delete target namespace %q: %w", topo.Spec.Namespace, err)
			}
		}
	}
	controllerutil.RemoveFinalizer(topo, authentikDeploymentFinalizer)
	return ctrl.Result{}, r.Update(ctx, topo)
}

// resolvePostgres implements all three Postgres modes, returning the
// connection info HelmValues needs (nil for Embedded once its password
// secret exists - the chart wires itself to its own bundled subchart) and
// whether the backend is ready for the authentik chart to be installed
// against. A false ready with a nil error means "still reconciling, check
// back later" (e.g. CNPG cluster not healthy yet), not a failure.
func (r *AuthentikDeploymentReconciler) resolvePostgres(ctx context.Context, rc client.Client, topo *benchv1alpha1.AuthentikDeployment, wc *benchv1alpha1.WorkloadCluster) (*render.PostgresConnection, bool, error) {
	switch topo.Spec.Postgres.Mode {
	case benchv1alpha1.PostgresCNPG:
		return r.resolveCNPGPostgres(ctx, rc, topo, wc)
	case benchv1alpha1.PostgresExternal:
		return r.resolveExternalPostgres(ctx, rc, topo)
	default: // PostgresEmbedded
		return r.resolveEmbeddedPostgres(ctx, rc, topo)
	}
}

func (r *AuthentikDeploymentReconciler) resolveEmbeddedPostgres(ctx context.Context, rc client.Client, topo *benchv1alpha1.AuthentikDeployment) (*render.PostgresConnection, bool, error) {
	// Pin the bundled postgresql subchart's own auth.password to a value we
	// generate, so it matches what we also hand authentik as
	// AUTHENTIK_POSTGRESQL__PASSWORD - the two are otherwise generated
	// independently and would never agree.
	pw, err := ensureRandomSecret(ctx, rc,
		types.NamespacedName{Name: topo.Name + "-authentik-postgres", Namespace: topo.Spec.Namespace},
		"password", 24)
	if err != nil {
		return nil, false, fmt.Errorf("ensure postgres password: %w", err)
	}
	setCondition(r.Recorder, topo, &topo.Status.Conditions, topo.Generation, benchv1alpha1.ConditionPostgresReady, metav1.ConditionTrue,
		benchv1alpha1.ReasonReady, "using the authentik chart's bundled postgresql subchart")
	return &render.PostgresConnection{Password: pw}, true, nil
}

// resolveCNPGPostgres applies the CNPG Cluster via rc (the management
// cluster when clusterRef is empty, otherwise a remote client) rather than
// applyOwned: a cross-cluster ownerReference is meaningless (the owning
// AuthentikDeployment's UID doesn't exist in the remote cluster's etcd for its
// garbage collector to recognize), and cleanup already goes through
// reconcileDelete's namespace sweep on whichever cluster this is, so no
// finalizer-tracked per-object cleanup is needed either.
func (r *AuthentikDeploymentReconciler) resolveCNPGPostgres(ctx context.Context, rc client.Client, topo *benchv1alpha1.AuthentikDeployment, wc *benchv1alpha1.WorkloadCluster) (*render.PostgresConnection, bool, error) {
	cluster := render.CNPGCluster(topo, wc)
	if err := applyUnowned(ctx, rc, cluster); err != nil {
		return nil, false, fmt.Errorf("apply CNPG Cluster: %w", err)
	}
	topo.Status.CNPGClusterRef = &corev1.LocalObjectReference{Name: cluster.GetName()}

	existing := &unstructured.Unstructured{}
	existing.SetAPIVersion(cluster.GetAPIVersion())
	existing.SetKind(cluster.GetKind())
	if err := rc.Get(ctx, types.NamespacedName{Name: cluster.GetName(), Namespace: cluster.GetNamespace()}, existing); err != nil {
		return nil, false, fmt.Errorf("get CNPG Cluster: %w", err)
	}
	if !render.CNPGReady(existing) {
		setCondition(r.Recorder, topo, &topo.Status.Conditions, topo.Generation, benchv1alpha1.ConditionPostgresReady, metav1.ConditionFalse,
			benchv1alpha1.ReasonReconciling, "waiting for CNPG Cluster to become healthy")
		return nil, false, nil
	}

	secretName := render.CNPGAppSecretName(topo)
	var sec corev1.Secret
	if err := rc.Get(ctx, types.NamespacedName{Name: secretName, Namespace: topo.Spec.Namespace}, &sec); err != nil {
		if apierrors.IsNotFound(err) {
			setCondition(r.Recorder, topo, &topo.Status.Conditions, topo.Generation, benchv1alpha1.ConditionPostgresReady, metav1.ConditionFalse,
				benchv1alpha1.ReasonReconciling, "waiting for CNPG's generated app secret")
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("get CNPG app secret: %w", err)
	}
	setCondition(r.Recorder, topo, &topo.Status.Conditions, topo.Generation, benchv1alpha1.ConditionPostgresReady, metav1.ConditionTrue,
		benchv1alpha1.ReasonReady, "CNPG Cluster is healthy")
	return connectionFromSecret(sec, "host", "port", "dbname", "username", "password"), true, nil
}

func (r *AuthentikDeploymentReconciler) resolveExternalPostgres(ctx context.Context, rc client.Client, topo *benchv1alpha1.AuthentikDeployment) (*render.PostgresConnection, bool, error) {
	ext := topo.Spec.Postgres.External
	if ext == nil {
		return nil, false, fmt.Errorf("postgres mode External requires spec.postgres.external")
	}
	var sec corev1.Secret
	if err := rc.Get(ctx, types.NamespacedName{Name: ext.SecretRef.Name, Namespace: topo.Spec.Namespace}, &sec); err != nil {
		if apierrors.IsNotFound(err) {
			setCondition(r.Recorder, topo, &topo.Status.Conditions, topo.Generation, benchv1alpha1.ConditionPostgresReady, metav1.ConditionFalse,
				benchv1alpha1.ReasonDependencyNotReady, fmt.Sprintf("external postgres secret %q not found", ext.SecretRef.Name))
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("get external postgres secret: %w", err)
	}
	setCondition(r.Recorder, topo, &topo.Status.Conditions, topo.Generation, benchv1alpha1.ConditionPostgresReady, metav1.ConditionTrue,
		benchv1alpha1.ReasonReady, "using the referenced external postgres secret")
	return connectionFromSecret(sec, "host", "port", "database", "username", "password"), true, nil
}

// connectionFromSecret reads a PostgresConnection out of a Secret's data
// fields, keyed by name so callers can match CNPG's ("dbname") vs. the
// External contract's ("database") differing key for the database name.
func connectionFromSecret(sec corev1.Secret, hostKey, portKey, dbKey, userKey, passwordKey string) *render.PostgresConnection {
	port, _ := strconv.Atoi(string(sec.Data[portKey]))
	if port == 0 {
		port = 5432
	}
	return &render.PostgresConnection{
		Host:     string(sec.Data[hostKey]),
		Port:     int32(port),
		Database: string(sec.Data[dbKey]),
		User:     string(sec.Data[userKey]),
		Password: string(sec.Data[passwordKey]),
	}
}

// soonest is the shorter of two requeue delays, where 0 means none.
func soonest(a, b time.Duration) time.Duration {
	if a == 0 || (b != 0 && b < a) {
		return b
	}
	return a
}

func ensureNamespace(ctx context.Context, rc client.Client, name string) error {
	ns := &corev1.Namespace{Name: name}
	err := rc.Get(ctx, types.NamespacedName{Name: name}, ns)
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	if err := rc.Create(ctx, ns); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *AuthentikDeploymentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&benchv1alpha1.AuthentikDeployment{}).
		Owns(&helmv2.HelmRelease{}).
		// Without this, a deployment whose extraValuesRef/fixtures blueprint
		// ConfigMap didn't exist yet only notices it was created on its own
		// next unrelated reconcile - the same staleness TestCaseReconciler's
		// AuthentikDeployment watch fixes, just one hop earlier: fixturesCM's
		// creation (`make fixtures`, run any time after the deployment
		// already exists) doesn't touch the deployment at all on its own.
		Watches(&corev1.ConfigMap{}, handler.EnqueueRequestsFromMapFunc(r.deploymentsForConfigMap)).
		// Defense in depth alongside blueprintApplyTimeout (blueprint.go):
		// the default of 1 means a single slow reconcile - blueprint
		// application under load is the known slow one - blocks every
		// *other* deployment's reconciles too, not just its own, since they
		// all share the one worker.
		WithOptions(controller.Options{MaxConcurrentReconciles: 5}).
		Named("authentikdeployment").
		Complete(r)
}

// deploymentsForConfigMap maps a changed ConfigMap to every AuthentikDeployment
// in the same namespace that references it - by name, for the fixtures
// blueprint (a fixed name every deployment in a namespace shares), or via
// spec.authentik.extraValuesRef for a deployment-specific one.
func (r *AuthentikDeploymentReconciler) deploymentsForConfigMap(ctx context.Context, cm client.Object) []reconcile.Request {
	var list benchv1alpha1.AuthentikDeploymentList
	if err := r.List(ctx, &list, client.InNamespace(cm.GetNamespace())); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for _, topo := range list.Items {
		ref := topo.Spec.Authentik.ExtraValuesRef
		if cm.GetName() == fixturesBlueprintConfigMapName || (ref != nil && ref.Name == cm.GetName()) {
			reqs = append(reqs, reconcile.Request{Name: topo.Name, Namespace: topo.Namespace})
		}
	}
	return reqs
}
