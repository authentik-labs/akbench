package controller

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
	"github.com/authentik-labs/akbench/internal/multicluster"
	"github.com/authentik-labs/akbench/internal/render"
	k6scripts "github.com/authentik-labs/akbench/k6/scripts"
)

const testCaseFinalizer = "bench.goauthentik.io/remote-cleanup"

// defaultRemoteWriteURL is the in-cluster Prometheus every k6 workload
// remote-writes to before M4 introduces the central mTLS receiver. It is
// intentionally a package var, not a const, so tests and later milestones
// can override it without touching call sites.
var defaultRemoteWriteURL = envOrDefault("BENCH_PROMETHEUS_RW_URL", "http://prometheus-server.bench-system.svc.cluster.local/api/v1/write")

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// TestCaseReconciler reconciles a TestCase object.
//
// The k6 workload runs on spec.runnerClusterRef if set, else the deployment's
// own spec.clusterRef, else the management cluster - resolved fresh every
// reconcile via Tracker, since a workload cluster's reachability can change
// over time.
type TestCaseReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Tracker  *multicluster.Tracker
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=bench.goauthentik.io,resources=testcases,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=bench.goauthentik.io,resources=testcases/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=bench.goauthentik.io,resources=testcases/finalizers,verbs=update
// +kubebuilder:rbac:groups=bench.goauthentik.io,resources=authentikdeployments,verbs=get;list;watch
// +kubebuilder:rbac:groups=bench.goauthentik.io,resources=testcasetemplates,verbs=get;list;watch
// +kubebuilder:rbac:groups=bench.goauthentik.io,resources=workloadclusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch;create;update
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create

func (r *TestCaseReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var tc benchv1alpha1.TestCase
	if err := r.Get(ctx, req.NamespacedName, &tc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !tc.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &tc)
	}
	if !controllerutil.ContainsFinalizer(&tc, testCaseFinalizer) {
		controllerutil.AddFinalizer(&tc, testCaseFinalizer)
		if err := r.Update(ctx, &tc); err != nil {
			return ctrl.Result{}, err
		}
	}

	if err := r.migrateDeploymentRef(ctx, &tc); err != nil {
		return ctrl.Result{}, err
	}

	if tc.Spec.Archived {
		return r.reconcileArchived(ctx, &tc)
	}

	refs := tc.Deployments()
	if len(refs) == 0 {
		setCondition(r.Recorder, &tc, &tc.Status.Conditions, tc.Generation, benchv1alpha1.ConditionDeploymentReady, metav1.ConditionFalse,
			benchv1alpha1.ReasonDependencyNotReady, "neither deploymentRef nor deploymentRefs is set")
		setCondition(r.Recorder, &tc, &tc.Status.Conditions, tc.Generation, benchv1alpha1.ConditionReady, metav1.ConditionFalse,
			benchv1alpha1.ReasonDependencyNotReady, "waiting for a deployment to be named")
		tc.Status.Phase = benchv1alpha1.PhaseFor(tc.Status.Conditions)
		return ctrl.Result{}, r.Status().Update(ctx, &tc)
	}

	var tmpl *benchv1alpha1.TestCaseTemplate
	usingTemplate := tc.Spec.Inline == "" && tc.Spec.ScriptConfigMapRef == nil
	if usingTemplate {
		var err error
		tmpl, err = resolveTemplate(ctx, r.Client, tc.Namespace, tc.Spec.Template)
		if err != nil {
			if apierrors.IsNotFound(err) {
				setCondition(r.Recorder, &tc, &tc.Status.Conditions, tc.Generation, benchv1alpha1.ConditionScriptReady, metav1.ConditionFalse,
					benchv1alpha1.ReasonDependencyNotReady, fmt.Sprintf("TestCaseTemplate %q not found", tc.Spec.Template))
				setCondition(r.Recorder, &tc, &tc.Status.Conditions, tc.Generation, benchv1alpha1.ConditionReady, metav1.ConditionFalse,
					benchv1alpha1.ReasonDependencyNotReady, "waiting for template")
				tc.Status.Phase = benchv1alpha1.PhaseFor(tc.Status.Conditions)
				return ctrl.Result{}, r.Status().Update(ctx, &tc)
			}
			return ctrl.Result{}, fmt.Errorf("resolve template: %w", err)
		}
	}

	// One pass per target deployment - everything in reconcileTarget
	// (readiness, runner-cluster resolution, script/fixtures mirroring,
	// OAuth provisioning, the k6 workload itself) is independently resolved
	// per target, since two targets can legitimately be on different
	// clusters (resolveRunnerClient only defaults to a *shared* cluster when
	// spec.runnerClusterRef pins one explicitly for the whole TestCase).
	var statuses []benchv1alpha1.TestCaseDeploymentStatus
	var notReadyMessages []string
	allReady := true
	needsRequeue := false
	keepBurnNames := desiredBurnNames(&tc, refs)
	// Runner clusters visited this pass, keyed by cluster name ("" is the
	// local cluster), so stale Burn Deployments are cleaned up once per
	// cluster after every target has been reconciled.
	runnerClients := map[string]client.Client{}
	previous := map[string]benchv1alpha1.TestCaseDeploymentStatus{}
	for _, st := range tc.Status.Deployments {
		previous[st.Name] = st
	}
	startAt := burnStartAt(&tc)
	burnStaged := true

	for _, ref := range refs {
		result, err := r.reconcileTarget(ctx, &tc, tmpl, ref, startAt)
		if err != nil {
			return requeueWhileWarmingUp(err)
		}
		keepRecordedStatus(&tc, &result, previous[ref.Name])
		burnStaged = burnStaged && result.burnStaged
		statuses = append(statuses, result.status)
		if result.runnerClient != nil {
			runnerClients[result.runnerCluster] = result.runnerClient
		}
		if !result.ready {
			allReady = false
			needsRequeue = needsRequeue || result.requeue
			notReadyMessages = append(notReadyMessages, result.notReadyMsg)
		}
	}
	if tc.Spec.Profile != benchv1alpha1.ProfileBurn && tc.Spec.Profile != benchv1alpha1.ProfileQuick {
		setCondition(r.Recorder, &tc, &tc.Status.Conditions, tc.Generation, benchv1alpha1.ConditionReady, metav1.ConditionFalse,
			benchv1alpha1.ReasonReconcileError, fmt.Sprintf("unknown profile %q", tc.Spec.Profile))
		tc.Status.Phase = benchv1alpha1.PhaseFor(tc.Status.Conditions)
		return ctrl.Result{}, r.Status().Update(ctx, &tc)
	}

	// Cleans up a target that's been removed from DeploymentRefs (or every
	// target, switching Burn->Quick) on each runner cluster visited above,
	// plus every runner cluster a removed target last ran on (from the
	// previous status). This must run after the loop, with the complete keep
	// set: running it per target would delete every later target's Burn
	// Deployment on a shared runner cluster, only for the next iteration to
	// recreate it.
	for _, prev := range previous {
		cluster := prev.RunnerCluster
		if _, ok := runnerClients[cluster]; ok {
			continue
		}
		rc, _, err := resolveRunnerClient(ctx, r.Tracker, r.Client, tc.Namespace, nil, corev1.LocalObjectReference{Name: cluster})
		if err != nil {
			log.Info("Skipped stale burn cleanup on unreachable runner cluster", "cluster", cluster, "error", err.Error())
			continue
		}
		runnerClients[cluster] = rc
	}
	for cluster, rc := range runnerClients {
		if err := r.cleanupStaleBurnDeployments(ctx, rc, &tc, keepBurnNames); err != nil {
			return ctrl.Result{}, fmt.Errorf("clean up stale burn deployments on runner cluster %q: %w", cluster, err)
		}
	}

	tc.Status.Deployments = statuses
	if !allReady {
		reason := benchv1alpha1.ReasonDependencyNotReady
		setCondition(r.Recorder, &tc, &tc.Status.Conditions, tc.Generation, benchv1alpha1.ConditionDeploymentReady, metav1.ConditionFalse,
			reason, strings.Join(notReadyMessages, "; "))
		setCondition(r.Recorder, &tc, &tc.Status.Conditions, tc.Generation, benchv1alpha1.ConditionReady, metav1.ConditionFalse,
			reason, strings.Join(notReadyMessages, "; "))
		tc.Status.Phase = benchv1alpha1.PhaseFor(tc.Status.Conditions)
		if err := r.Status().Update(ctx, &tc); err != nil {
			return ctrl.Result{}, err
		}
		if needsRequeue {
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
		return ctrl.Result{}, nil
	}
	result := openBurnStartGate(ctx, &tc, startAt, burnStaged)
	setCondition(r.Recorder, &tc, &tc.Status.Conditions, tc.Generation, benchv1alpha1.ConditionDeploymentReady, metav1.ConditionTrue,
		benchv1alpha1.ReasonReady, "every deployment is Ready")
	setCondition(r.Recorder, &tc, &tc.Status.Conditions, tc.Generation, benchv1alpha1.ConditionScriptReady, metav1.ConditionTrue,
		benchv1alpha1.ReasonReady, "script resolved")

	tc.Status.ObservedGeneration = tc.Generation
	setCondition(r.Recorder, &tc, &tc.Status.Conditions, tc.Generation, benchv1alpha1.ConditionReady, metav1.ConditionTrue,
		benchv1alpha1.ReasonReady, "test case reconciled")
	tc.Status.Phase = benchv1alpha1.PhaseFor(tc.Status.Conditions)
	if err := r.Status().Update(ctx, &tc); err != nil {
		return ctrl.Result{}, err
	}
	log.Info("reconciled TestCase", "profile", tc.Spec.Profile, "deployments", len(refs))
	return result, nil
}

// burnStartLead is how far ahead of now a burn's start gate opens. A
// ConfigMap change reaches a mounted volume on the kubelet's next pod
// resync, up to about 90s later.
const burnStartLead = 2 * time.Minute

// burnStartAt is when a Burn TestCase's start gate opened (its earliest
// running target's burningSince), or nil while it is still closed.
func burnStartAt(tc *benchv1alpha1.TestCase) *metav1.Time {
	if tc.Spec.Profile != benchv1alpha1.ProfileBurn {
		return nil
	}
	var startAt *metav1.Time
	for _, st := range tc.Status.Deployments {
		if st.BurningSince != nil && st.BurnStoppedAt == nil && (startAt == nil || st.BurningSince.Before(startAt)) {
			startAt = st.BurningSince
		}
	}
	return startAt
}

// openBurnStartGate records when a Burn TestCase whose AuthentikDeployments
// are all Ready starts burning. The first time every k6 pod is up too
// (staged), that's burnStartLead from now, so the start time reaches all of
// them before it passes. The returned result polls until then.
func openBurnStartGate(ctx context.Context, tc *benchv1alpha1.TestCase, startAt *metav1.Time, staged bool) ctrl.Result {
	var result ctrl.Result
	if tc.Spec.Profile != benchv1alpha1.ProfileBurn {
		return result
	}
	if startAt == nil {
		if !staged {
			return ctrl.Result{RequeueAfter: 5 * time.Second}
		}
		startAt = &metav1.Time{Time: time.Now().Add(burnStartLead).Truncate(time.Second)}
		logf.FromContext(ctx).Info("Scheduled burn start", "startAt", startAt.Time)
		// The next reconcile writes it into the script ConfigMaps.
		result.RequeueAfter = time.Second
	}
	now := metav1.Now()
	for i := range tc.Status.Deployments {
		st := &tc.Status.Deployments[i]
		if st.BurningSince == nil && st.BurnWorkloadRef != nil {
			// A target added once the burn already started starts now.
			st.BurningSince = startAt.DeepCopy()
			if startAt.Before(&now) {
				st.BurningSince = &now
			}
		}
	}
	return result
}

// burnDeploymentStaged reports whether dep's k6 pod is fully rolled out
// and running, i.e. waiting at the start gate.
func burnDeploymentStaged(dep *appsv1.Deployment) bool {
	want := ptr.Deref(dep.Spec.Replicas, 1)
	return dep.Status.ObservedGeneration == dep.Generation &&
		dep.Status.Replicas == want && dep.Status.UpdatedReplicas == want && dep.Status.ReadyReplicas == want
}

// keepRecordedStatus carries over what a target's freshly computed status
// can't know by itself from prev: when its burn started, and, if the target
// couldn't be resolved this pass (its deployment is gone or not Ready, or
// its cluster is unreachable), where it and its k6 workload last ran, so a
// later delete still finds the workload and its telemetry can still be
// shown.
func keepRecordedStatus(tc *benchv1alpha1.TestCase, result *targetResult, prev benchv1alpha1.TestCaseDeploymentStatus) {
	if result.runnerClient == nil {
		keepRecordedPlacement(&result.status, prev)
	}
	if tc.Spec.Profile == benchv1alpha1.ProfileBurn && prev.BurnStoppedAt == nil {
		result.status.BurningSince = prev.BurningSince
	}
}

// keepRecordedPlacement carries prev's runner cluster over to st, and where
// the deployment itself ran unless st resolved that afresh.
func keepRecordedPlacement(st *benchv1alpha1.TestCaseDeploymentStatus, prev benchv1alpha1.TestCaseDeploymentStatus) {
	st.RunnerCluster = prev.RunnerCluster
	if st.Namespace == "" {
		st.Cluster, st.Namespace, st.Target = prev.Cluster, prev.Namespace, prev.Target
	}
}

// migrateDeploymentRef is a one-time migration of the deprecated singular
// spec.deploymentRef into spec.deploymentRefs, so every object converges on
// the one field the UI, printcolumns and samples use. Lossless:
// Deployments() already treated the two identically, this only makes that
// explicit in the stored spec.
func (r *TestCaseReconciler) migrateDeploymentRef(ctx context.Context, tc *benchv1alpha1.TestCase) error {
	if !tc.Spec.MigrateDeploymentRef() {
		return nil
	}
	if err := r.Update(ctx, tc); err != nil {
		return err
	}
	logf.FromContext(ctx).Info("Migrated TestCase deploymentRef to deploymentRefs", "deployment", tc.Spec.DeploymentRefs[0].Name)
	return nil
}

// errClusterWarmingUp aborts a reconcile without touching status because a
// cluster it needs is unreachable only since the operator just started (see
// multicluster.Tracker.WarmingUp): keeping the last known status beats
// flapping every TestCase to not-Ready on each restart.
var errClusterWarmingUp = errors.New("cluster connection is still warming up")

// requeueWhileWarmingUp turns errClusterWarmingUp into a short requeue and
// passes any other error through.
func requeueWhileWarmingUp(err error) (ctrl.Result, error) {
	if errors.Is(err, errClusterWarmingUp) {
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}
	return ctrl.Result{}, err
}

// targetResult is one target deployment's own outcome from reconcileTarget.
// runnerClient is non-nil whenever resolveRunnerClient itself succeeded,
// even if a later step for this same target (e.g. OAuth provisioning) still
// failed - that runner cluster was genuinely visited and still needs its
// stale Burn Deployments cleaned up by the caller.
type targetResult struct {
	status        benchv1alpha1.TestCaseDeploymentStatus
	runnerCluster string
	runnerClient  client.Client
	ready         bool
	requeue       bool
	notReadyMsg   string
	// burnStaged is true unless this target's burn Deployment exists and
	// its pod isn't up yet.
	burnStaged bool
}

// reconcileTarget resolves and reconciles everything one target deployment
// of a TestCase needs: deployment readiness, the runner cluster, mirrored
// script/fixtures, OAuth client provisioning, and the k6 workload itself
// (Burn only - Quick execution is a TestRun's job). A returned error aborts
// the whole TestCase reconcile; a not-ready target instead reports itself
// via the returned result so its siblings still get their own chance to
// reconcile.
func (r *TestCaseReconciler) reconcileTarget(ctx context.Context, tc *benchv1alpha1.TestCase, tmpl *benchv1alpha1.TestCaseTemplate, ref corev1.LocalObjectReference, startAt *metav1.Time) (targetResult, error) {
	result := targetResult{status: benchv1alpha1.TestCaseDeploymentStatus{Name: ref.Name}, burnStaged: true}

	topo, targetName, host, port, err := resolveReadyDeployment(ctx, r.Client, tc.Namespace, ref.Name)
	if err != nil {
		switch {
		case apierrors.IsNotFound(err):
			result.notReadyMsg = fmt.Sprintf("%s: AuthentikDeployment not found", ref.Name)
		case errors.Is(err, errDeploymentNotReady):
			result.notReadyMsg = fmt.Sprintf("%s: not Ready yet", ref.Name)
		default:
			return targetResult{}, fmt.Errorf("resolve deployment %q: %w", ref.Name, err)
		}
		return result, nil
	}
	result.status.Cluster, result.status.Namespace, result.status.Target = topo.Spec.ClusterRef.Name, topo.Spec.Namespace, targetName

	rc, remote, err := resolveRunnerClient(ctx, r.Tracker, r.Client, tc.Namespace, tc.Spec.RunnerClusterRef, topo.Spec.ClusterRef)
	if err != nil {
		if r.Tracker.WarmingUp(ctx, client.ObjectKey{Namespace: tc.Namespace, Name: runnerClusterName(tc, topo)}, err) &&
			meta.FindStatusCondition(tc.Status.Conditions, benchv1alpha1.ConditionReady) != nil {
			return targetResult{}, errClusterWarmingUp
		}
		result.requeue = true
		result.notReadyMsg = fmt.Sprintf("%s: runner cluster unreachable: %v", ref.Name, err)
		return result, nil
	}
	result.runnerCluster = runnerClusterName(tc, topo)
	result.runnerClient = rc
	result.status.RunnerCluster = result.runnerCluster
	if remote {
		if err := ensureNamespace(ctx, rc, tc.Namespace); err != nil {
			return targetResult{}, fmt.Errorf("ensure runner namespace for %q: %w", ref.Name, err)
		}
	}

	scriptCM, err := r.ensureScriptConfigMap(ctx, rc, remote, tc, tmpl, startAt)
	if err != nil {
		return targetResult{}, fmt.Errorf("ensure script configmap for %q: %w", ref.Name, err)
	}
	if err := ensureFixturesSecret(ctx, r.Client, rc, tc.Namespace); err != nil {
		// No synthetic users loaded yet (SeedFixtures runs at operator startup;
		// it skips a half-loaded pair, e.g. a hand-made Secret): a setup step
		// still to do, not a failure - report it on the TestCase rather
		// than only in the operator log, and pick it up once it's there.
		if apierrors.IsNotFound(err) {
			result.requeue = true
			result.notReadyMsg = fmt.Sprintf("%s: test users not loaded - Secret %s/%s not found (the operator seeds them at startup - see its log)",
				ref.Name, tc.Namespace, fixturesSecretName)
			return result, nil
		}
		return targetResult{}, fmt.Errorf("ensure fixtures secret for %q: %w", ref.Name, err)
	}

	var deps render.DependencyEndpoints
	if tmpl != nil {
		deps, err = r.ensureDependencies(ctx, rc, remote, tc, ref.Name, topo, &tmpl.Spec, host, port)
		if err != nil {
			result.notReadyMsg = fmt.Sprintf("%s: %v", ref.Name, err)
			return result, nil
		}
	}

	runnerWC := resolveWorkloadCluster(ctx, r.Client, tc.Namespace, result.runnerCluster)
	nodeSelector, tolerations := render.K6PoolPlacement(runnerWC)
	params := render.K6WorkloadParams{
		TestCase:                tc,
		DeploymentName:          ref.Name,
		TargetName:              targetName,
		BenchHost:               host,
		BenchPort:               port,
		RemoteWriteURL:          defaultRemoteWriteURL,
		ScriptConfigMapName:     scriptCM,
		UserScriptConfigMapName: userScriptConfigMapName(tc),
		FixturesSecretName:      fixturesSecretName,
		Dependencies:            deps,
		NodeSelector:            nodeSelector,
		Tolerations:             tolerations,
	}

	if tc.Spec.Profile == benchv1alpha1.ProfileBurn {
		dep, err := r.reconcileBurn(ctx, rc, remote, tc, ref.Name, params)
		if err != nil {
			return targetResult{}, fmt.Errorf("reconcile burn deployment for %q: %w", ref.Name, err)
		}
		result.status.BurnWorkloadRef = &corev1.LocalObjectReference{Name: dep.Name}
		result.burnStaged = burnDeploymentStaged(dep)
	}
	// Quick: execution is a TestRun's job, nothing to reconcile continuously
	// here. An unrecognized profile is likewise a no-op - Reconcile checks
	// Spec.Profile's validity itself once, rather than re-deriving the same
	// TestCase-level verdict for every target.

	result.status.Ready = true
	result.ready = true
	return result, nil
}

func (r *TestCaseReconciler) reconcileDelete(ctx context.Context, tc *benchv1alpha1.TestCase) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(tc, testCaseFinalizer) {
		return ctrl.Result{}, nil
	}

	pending, err := r.stopAllBurnDeployments(ctx, tc, time.Since(tc.DeletionTimestamp.Time) >= deleteCleanupGracePeriod)
	if err != nil {
		return ctrl.Result{}, err
	}
	if pending {
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}
	controllerutil.RemoveFinalizer(tc, testCaseFinalizer)
	return ctrl.Result{}, r.Update(ctx, tc)
}

// reconcileArchived stops every k6 Burn Deployment of an archived TestCase
// and reports it as Archived. An archived TestCase never gives up on a
// cluster it is known to have run on, since nothing else would stop that load.
func (r *TestCaseReconciler) reconcileArchived(ctx context.Context, tc *benchv1alpha1.TestCase) (ctrl.Result, error) {
	pending, err := r.stopAllBurnDeployments(ctx, tc, false)
	if err != nil {
		return ctrl.Result{}, err
	}
	for i := range tc.Status.Deployments {
		st := &tc.Status.Deployments[i]
		st.Ready = false
		if !pending {
			st.BurnWorkloadRef = nil
			if st.BurningSince != nil && st.BurnStoppedAt == nil {
				now := metav1.Now()
				st.BurnStoppedAt = &now
			}
		}
	}
	setCondition(r.Recorder, tc, &tc.Status.Conditions, tc.Generation, benchv1alpha1.ConditionReady, metav1.ConditionFalse,
		benchv1alpha1.ReasonArchived, "test case is archived")
	tc.Status.Phase = benchv1alpha1.PhaseFor(tc.Status.Conditions)
	tc.Status.ObservedGeneration = tc.Generation
	if err := r.Status().Update(ctx, tc); err != nil {
		return ctrl.Result{}, err
	}
	if pending {
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}
	return ctrl.Result{}, nil
}

// stopAllBurnDeployments deletes this TestCase's Burn Deployments on every
// cluster it may have left one on, returning pending when a cluster it is
// known to have used couldn't be reached and giveUp is false.
func (r *TestCaseReconciler) stopAllBurnDeployments(ctx context.Context, tc *benchv1alpha1.TestCase, giveUp bool) (bool, error) {
	log := logf.FromContext(ctx)
	clusters, err := r.deleteCandidateClusters(ctx, tc)
	if err != nil {
		return false, err
	}
	pending := false
	for _, cluster := range clusters {
		rc, _, err := resolveRunnerClient(ctx, r.Tracker, r.Client, tc.Namespace, nil, corev1.LocalObjectReference{Name: cluster.name})
		if err == nil {
			err = r.cleanupStaleBurnDeployments(ctx, rc, tc, nil)
		}
		if err == nil {
			continue
		}
		// A cluster this TestCase is known to have run k6 on gets a grace
		// period to come back, so a brief outage doesn't leave a load
		// generator running forever. Any other cluster is only swept on a
		// best-effort basis.
		if cluster.recorded && !giveUp {
			log.Info("Failed to clean up burn Deployments, retrying", "cluster", cluster.name, "error", err.Error())
			pending = true
			continue
		}
		log.Info("Skipped burn Deployment cleanup on unreachable cluster", "cluster", cluster.name, "error", err.Error())
		if cluster.recorded && r.Recorder != nil {
			r.Recorder.Eventf(tc, corev1.EventTypeWarning, "CleanupSkipped",
				"Gave up deleting k6 burn Deployments on cluster %q: %v", cluster.name, err)
		}
	}
	return pending, nil
}

// deleteCleanupGracePeriod bounds how long a deleting TestCase waits for an
// unreachable runner cluster it is known to have used.
const deleteCleanupGracePeriod = 5 * time.Minute

type cleanupCluster struct {
	name string
	// recorded is true when the TestCase is known to have placed a k6
	// workload on this cluster (its status, spec.runnerClusterRef, or a
	// still-existing target deployment's clusterRef).
	recorded bool
}

// deleteCandidateClusters lists every cluster a deleting TestCase may have
// left a Burn Deployment on. Its targets' AuthentikDeployments are often
// deleted alongside it, so their clusterRefs can't be relied on alone: the
// status' recorded runner clusters and every WorkloadCluster are swept too.
func (r *TestCaseReconciler) deleteCandidateClusters(ctx context.Context, tc *benchv1alpha1.TestCase) ([]cleanupCluster, error) {
	recorded := map[string]bool{"": true}
	for _, st := range tc.Status.Deployments {
		recorded[st.RunnerCluster] = true
	}
	if tc.Spec.RunnerClusterRef != nil {
		recorded[tc.Spec.RunnerClusterRef.Name] = true
	}
	for _, ref := range tc.Deployments() {
		var topo benchv1alpha1.AuthentikDeployment
		if err := r.Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: tc.Namespace}, &topo); err == nil {
			recorded[runnerClusterName(tc, &topo)] = true
		}
	}
	all := map[string]bool{}
	var wcs benchv1alpha1.WorkloadClusterList
	if err := r.List(ctx, &wcs, client.InNamespace(tc.Namespace)); err != nil {
		return nil, err
	}
	for _, wc := range wcs.Items {
		all[wc.Name] = true
	}
	for name := range recorded {
		all[name] = true
	}
	out := make([]cleanupCluster, 0, len(all))
	for name := range all {
		out = append(out, cleanupCluster{name: name, recorded: recorded[name]})
	}
	slices.SortFunc(out, func(a, b cleanupCluster) int { return strings.Compare(a.name, b.name) })
	return out, nil
}

// cleanupStaleBurnDeployments deletes every Burn Deployment this TestCase
// owns on rc whose name isn't in keep - a target removed from
// DeploymentRefs, or every target when switching Burn->Quick (keep == nil).
// Relies on BurnDeployment's own object-level labels (LabelTestCase, set by
// render.BurnDeployment) to find them all without needing to already know
// their exact names, plus deleteLegacyBurnDeployment for the one Deployment
// that predates those labels.
func (r *TestCaseReconciler) cleanupStaleBurnDeployments(ctx context.Context, rc client.Client, tc *benchv1alpha1.TestCase, keep map[string]bool) error {
	if err := deleteLegacyBurnDeployment(ctx, rc, tc); err != nil {
		return err
	}
	var list appsv1.DeploymentList
	if err := rc.List(ctx, &list, client.InNamespace(tc.Namespace), client.MatchingLabels{benchv1alpha1.LabelTestCase: tc.Name}); err != nil {
		return err
	}
	for i := range list.Items {
		dep := &list.Items[i]
		if keep[dep.Name] {
			continue
		}
		if err := rc.Delete(ctx, dep); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// desiredBurnNames is every target's Burn Deployment name, including
// targets that aren't Ready this pass - a transiently unready target
// shouldn't lose its load generator just because another target shares its
// runner cluster. Empty for Quick, so a Burn->Quick switch cleans up
// everything.
func desiredBurnNames(tc *benchv1alpha1.TestCase, refs []corev1.LocalObjectReference) map[string]bool {
	keep := map[string]bool{}
	if tc.Spec.Profile != benchv1alpha1.ProfileBurn {
		return keep
	}
	for _, ref := range refs {
		keep[render.BurnDeploymentName(tc.Name, ref.Name)] = true
	}
	return keep
}

// runnerClusterName is the cluster resolveRunnerClient picks for one
// target, by name ("" is the local cluster).
func runnerClusterName(tc *benchv1alpha1.TestCase, topo *benchv1alpha1.AuthentikDeployment) string {
	if tc.Spec.RunnerClusterRef != nil && tc.Spec.RunnerClusterRef.Name != "" {
		return tc.Spec.RunnerClusterRef.Name
	}
	return topo.Spec.ClusterRef.Name
}

// deleteLegacyBurnDeployment removes the "k6-<testcase>" Burn Deployment
// written before a TestCase could drive more than one deployment. It had no
// object-level labels, so cleanupStaleBurnDeployments' label List never
// finds it, and left alone it would keep generating load against the
// original target alongside its renamed render.BurnDeploymentName
// successor. Ownership is checked via its pod template label and its
// selector lacking LabelDeployment, since the bare name alone can collide
// with a current-scheme name ("k6-<a>-<b>" is both TestCase "a-b"'s legacy
// name and TestCase "a"'s name for deployment "b").
func deleteLegacyBurnDeployment(ctx context.Context, rc client.Client, tc *benchv1alpha1.TestCase) error {
	var dep appsv1.Deployment
	if err := rc.Get(ctx, types.NamespacedName{Name: "k6-" + tc.Name, Namespace: tc.Namespace}, &dep); err != nil {
		return client.IgnoreNotFound(err)
	}
	if dep.Spec.Template.Labels[benchv1alpha1.LabelTestCase] != tc.Name {
		return nil
	}
	if dep.Spec.Selector != nil {
		if _, ok := dep.Spec.Selector.MatchLabels[benchv1alpha1.LabelDeployment]; ok {
			return nil
		}
	}
	logf.FromContext(ctx).Info("Deleting legacy Burn Deployment", "name", dep.Name)
	if err := rc.Delete(ctx, &dep); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// reconcileBurn applies one target's k6 burn Deployment via rc, returning
// it as stored on the cluster. An ownerReference is only set when the Deployment lives on the
// local cluster (remote==false): a cross-cluster ownerReference would carry
// a UID the remote cluster's garbage collector can never recognize, so
// remote cleanup instead goes through cleanupStaleBurnDeployments above.
func (r *TestCaseReconciler) reconcileBurn(ctx context.Context, rc client.Client, remote bool, tc *benchv1alpha1.TestCase, deploymentName string, params render.K6WorkloadParams) (*appsv1.Deployment, error) {
	name := render.BurnDeploymentName(tc.Name, deploymentName)
	dep := render.BurnDeployment(name, tc.Namespace, params)
	if !remote {
		if err := controllerutil.SetControllerReference(tc, dep, r.Scheme); err != nil {
			return nil, err
		}
	}
	existing := &appsv1.Deployment{}
	err := rc.Get(ctx, types.NamespacedName{Name: name, Namespace: tc.Namespace}, existing)
	switch {
	case apierrors.IsNotFound(err):
		if err := rc.Create(ctx, dep); err != nil {
			return nil, err
		}
		return dep, nil
	case err != nil:
		return nil, err
	default:
		existing.Spec = dep.Spec
		if err := rc.Update(ctx, existing); err != nil {
			return nil, err
		}
	}
	return existing, nil
}

// ensureScriptConfigMap mirrors the shared k6 library and the resolved
// script - inline content stored on the TestCase itself, else the resolved
// TestCaseTemplate's own script - into the ConfigMap the k6 pod mounts on
// rc, returning its name. A user-supplied scriptConfigMapRef (BYO) is
// mounted as is next to it, so only the library goes here then. tmpl is nil
// when scriptConfigMapRef or inline already won upstream (see Reconcile's
// usingTemplate). startAt, once a burn's start gate opened, releases its
// waiting k6 pods (render.StartAtFileName).
func (r *TestCaseReconciler) ensureScriptConfigMap(ctx context.Context, rc client.Client, remote bool, tc *benchv1alpha1.TestCase, tmpl *benchv1alpha1.TestCaseTemplate, startAt *metav1.Time) (string, error) {
	lib, ok := k6scripts.Get(render.LibFileName)
	if !ok {
		return "", fmt.Errorf("embedded k6 library %q is missing", render.LibFileName)
	}
	data := map[string]string{render.LibFileName: lib}
	switch {
	case tc.Spec.ScriptConfigMapRef != nil:
	case tc.Spec.Inline != "":
		data["script.ts"] = tc.Spec.Inline
	case tmpl.Spec.Script == "":
		return "", fmt.Errorf("TestCaseTemplate %q has an empty script", tmpl.Name)
	default:
		data["script.ts"] = tmpl.Spec.Script
	}
	if startAt != nil {
		data[render.StartAtFileName] = strconv.FormatInt(startAt.Unix(), 10)
	}

	name := "k6-script-" + tc.Name
	cm := &corev1.ConfigMap{
		Name: name, Namespace: tc.Namespace,
		Data: data,
	}
	if !remote {
		if err := controllerutil.SetControllerReference(tc, cm, r.Scheme); err != nil {
			return "", err
		}
	}
	existing := &corev1.ConfigMap{}
	err := rc.Get(ctx, types.NamespacedName{Name: name, Namespace: tc.Namespace}, existing)
	switch {
	case apierrors.IsNotFound(err):
		if err := rc.Create(ctx, cm); err != nil {
			return "", err
		}
	case err != nil:
		return "", err
	default:
		existing.Data = cm.Data
		if err := rc.Update(ctx, existing); err != nil {
			return "", err
		}
	}
	return name, nil
}

// userScriptConfigMapName is the TestCase's BYO script ConfigMap, if any.
func userScriptConfigMapName(tc *benchv1alpha1.TestCase) string {
	if tc.Spec.ScriptConfigMapRef == nil {
		return ""
	}
	return tc.Spec.ScriptConfigMapRef.Name
}

func findCondition(conditions []metav1.Condition, condType string) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == condType {
			return &conditions[i]
		}
	}
	return nil
}

// splitServiceEndpoint parses an AuthentikDeployment's "host:port" status
// field.
func splitServiceEndpoint(endpoint string) (string, int32, error) {
	host, portStr, found := strings.Cut(endpoint, ":")
	if !found {
		return "", 0, fmt.Errorf("expected host:port, got %q", endpoint)
	}
	var port int32
	if _, err := fmt.Sscanf(portStr, "%d", &port); err != nil {
		return "", 0, fmt.Errorf("invalid port in %q: %w", endpoint, err)
	}
	return host, port, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *TestCaseReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&benchv1alpha1.TestCase{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.ConfigMap{}).
		// Without this, a TestCase left in DependencyNotReady only
		// re-checks its deployment on its own next unrelated reconcile (a
		// spec edit, an owned object changing, or the informer's resync
		// period) - the deployment later becoming Ready doesn't trigger
		// anything on its own, so the stale condition can sit for a long
		// time even though the real dependency is satisfied.
		Watches(&benchv1alpha1.AuthentikDeployment{}, handler.EnqueueRequestsFromMapFunc(r.testCasesForDeployment)).
		Named("testcase").
		Complete(r)
}

// testCasesForDeployment maps an AuthentikDeployment change to every TestCase in
// the same namespace referencing it, so its controller re-checks readiness
// as soon as the deployment's own status changes instead of waiting for an
// unrelated trigger.
func (r *TestCaseReconciler) testCasesForDeployment(ctx context.Context, topo client.Object) []reconcile.Request {
	var list benchv1alpha1.TestCaseList
	if err := r.List(ctx, &list, client.InNamespace(topo.GetNamespace())); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for _, tc := range list.Items {
		if slices.ContainsFunc(tc.Deployments(), func(ref corev1.LocalObjectReference) bool { return ref.Name == topo.GetName() }) {
			reqs = append(reqs, reconcile.Request{Name: tc.Name, Namespace: tc.Namespace})
		}
	}
	return reqs
}
