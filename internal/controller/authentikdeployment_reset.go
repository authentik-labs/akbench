package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	helmv2 "github.com/fluxcd/helm-controller/api/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
	"github.com/authentik-labs/akbench/internal/render"
)

// resetPollInterval is how often a reset in progress re-checks that what it
// deleted is actually gone.
const resetPollInterval = 2 * time.Second

// reconcileReset runs one step of a reset requested via
// benchv1alpha1.AnnotationResetRequested, in order:
//
//  1. delete the HelmRelease and wait for helm-controller's `helm uninstall`,
//     so nothing holds the database volumes any more;
//  2. (CNPG mode) delete the CNPG Cluster, which owns its own PVCs;
//  3. delete every PVC in spec.namespace except the BlueprintPVC (a staging
//     area for blueprint files, not state) and wait until they're gone;
//  4. delete every blueprint import Job and the postgres role-fix Job, so
//     reconcileBlueprint, TestCaseReconciler.ensureDependencies and
//     reconcilePostgresRoleFix all run them again against the new database.
//
// Each step waits until the remote cluster client's (cached) reads no longer
// see what it deleted: a stale cached Job reading as Complete after the
// annotation is gone would otherwise skip re-applying that blueprint.
// Generated Secrets (AUTHENTIK_SECRET_KEY, the embedded postgres password)
// are kept - a fresh volume is initialised with the same password.
//
// Once everything is gone the annotation is removed, and the Update that
// does so triggers the normal reconcile, which reinstalls from scratch.
func (r *AuthentikDeploymentReconciler) reconcileReset(ctx context.Context, rc client.Client, topo *benchv1alpha1.AuthentikDeployment) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if topo.Spec.Postgres.Mode == benchv1alpha1.PostgresExternal {
		r.Recorder.Eventf(topo, corev1.EventTypeWarning, "ResetRejected", "cannot reset a deployment using an external postgres the operator doesn't manage")
		return ctrl.Result{}, r.clearResetAnnotation(ctx, topo)
	}

	step, err := r.resetStep(ctx, rc, topo)
	if err != nil {
		return ctrl.Result{}, err
	}
	if step != "" {
		setCondition(r.Recorder, topo, &topo.Status.Conditions, topo.Generation, benchv1alpha1.ConditionReady, metav1.ConditionFalse,
			benchv1alpha1.ReasonResetting, step)
		topo.Status.Phase = benchv1alpha1.PhaseFor(topo.Status.Conditions)
		if err := r.Status().Update(ctx, topo); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: resetPollInterval}, nil
	}

	// The volumes auto-growth enlarged are gone; the new ones start at
	// spec's storageSize again.
	if topo.Status.PostgresStorage != nil {
		topo.Status.PostgresStorage = nil
		if err := r.Status().Update(ctx, topo); err != nil {
			return ctrl.Result{}, err
		}
	}

	r.Recorder.Eventf(topo, corev1.EventTypeNormal, "Reset", "deleted the database and blueprint import jobs, reinstalling")
	log.Info("Reset AuthentikDeployment", "name", topo.Name)
	return ctrl.Result{}, r.clearResetAnnotation(ctx, topo)
}

// resetStep performs (or waits on) the first unfinished reset step,
// returning a human-readable description of it, or "" once all are done.
func (r *AuthentikDeploymentReconciler) resetStep(ctx context.Context, rc client.Client, topo *benchv1alpha1.AuthentikDeployment) (string, error) {
	var hr helmv2.HelmRelease
	switch err := r.Get(ctx, types.NamespacedName{Name: render.HelmReleaseName(topo), Namespace: topo.Namespace}, &hr); {
	case err == nil:
		if hr.DeletionTimestamp.IsZero() {
			if err := r.Delete(ctx, &hr); err != nil && !apierrors.IsNotFound(err) {
				return "", fmt.Errorf("delete HelmRelease: %w", err)
			}
		}
		return "resetting: uninstalling the Helm release", nil
	case !apierrors.IsNotFound(err):
		return "", fmt.Errorf("get HelmRelease: %w", err)
	}

	if topo.Spec.Postgres.Mode == benchv1alpha1.PostgresCNPG {
		cluster := &unstructured.Unstructured{}
		cluster.SetGroupVersionKind(render.CNPGCluster(topo, nil).GroupVersionKind())
		switch err := rc.Get(ctx, types.NamespacedName{Name: render.CNPGClusterName(topo), Namespace: topo.Spec.Namespace}, cluster); {
		case err == nil:
			if cluster.GetDeletionTimestamp().IsZero() {
				if err := rc.Delete(ctx, cluster); err != nil && !apierrors.IsNotFound(err) {
					return "", fmt.Errorf("delete CNPG Cluster: %w", err)
				}
			}
			return "resetting: deleting the CNPG Cluster", nil
		case !apierrors.IsNotFound(err):
			return "", fmt.Errorf("get CNPG Cluster: %w", err)
		}
	}

	var pvcs corev1.PersistentVolumeClaimList
	if err := rc.List(ctx, &pvcs, client.InNamespace(topo.Spec.Namespace)); err != nil {
		return "", fmt.Errorf("list PVCs: %w", err)
	}
	remainingPVCs := 0
	for i := range pvcs.Items {
		pvc := &pvcs.Items[i]
		if pvc.Name == render.BlueprintPVCName(topo) {
			continue
		}
		remainingPVCs++
		if pvc.DeletionTimestamp.IsZero() {
			if err := rc.Delete(ctx, pvc); err != nil && !apierrors.IsNotFound(err) {
				return "", fmt.Errorf("delete PVC %q: %w", pvc.Name, err)
			}
		}
	}
	if remainingPVCs > 0 {
		return fmt.Sprintf("resetting: waiting for %d database PVC(s) to be deleted", remainingPVCs), nil
	}

	var jobs batchv1.JobList
	if err := rc.List(ctx, &jobs, client.InNamespace(topo.Spec.Namespace)); err != nil {
		return "", fmt.Errorf("list Jobs: %w", err)
	}
	remainingJobs := 0
	for i := range jobs.Items {
		job := &jobs.Items[i]
		if !strings.HasPrefix(job.Name, render.BlueprintJobNamePrefix) && job.Name != render.PostgresRoleFixJobName(topo) {
			continue
		}
		remainingJobs++
		if job.DeletionTimestamp.IsZero() {
			if err := rc.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && !apierrors.IsNotFound(err) {
				return "", fmt.Errorf("delete Job %q: %w", job.Name, err)
			}
		}
	}
	if remainingJobs > 0 {
		return fmt.Sprintf("resetting: waiting for %d blueprint job(s) to be deleted", remainingJobs), nil
	}
	return "", nil
}

func (r *AuthentikDeploymentReconciler) clearResetAnnotation(ctx context.Context, topo *benchv1alpha1.AuthentikDeployment) error {
	patch := client.MergeFrom(topo.DeepCopy())
	delete(topo.Annotations, benchv1alpha1.AnnotationResetRequested)
	return r.Patch(ctx, topo, patch)
}
