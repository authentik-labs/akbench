package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
	"github.com/authentik-labs/akbench/internal/render"
)

// TestMatrixReconciler reconciles a TestMatrix object: it owns one TestCase
// per template and one AuthentikDeployment per template and variant, all in
// the matrix's own namespace, so deleting the matrix garbage-collects them.
type TestMatrixReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=bench.goauthentik.io,resources=testmatrices,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=bench.goauthentik.io,resources=testmatrices/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=bench.goauthentik.io,resources=testmatrices/finalizers,verbs=update
// +kubebuilder:rbac:groups=bench.goauthentik.io,resources=authentikdeployments;testcases,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete

func (r *TestMatrixReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var m benchv1alpha1.TestMatrix
	if err := r.Get(ctx, req.NamespacedName, &m); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !m.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	desired, err := render.TestMatrix(&m)
	if err == nil && m.Spec.TestCase.Archived {
		err = r.archive(ctx, desired.TestCases)
	} else if err == nil {
		err = r.apply(ctx, &m, desired)
	}
	if err != nil {
		setCondition(r.Recorder, &m, &m.Status.Conditions, m.Generation, benchv1alpha1.ConditionReady, metav1.ConditionFalse,
			benchv1alpha1.ReasonReconcileError, err.Error())
		m.Status.Phase = benchv1alpha1.PhaseFor(m.Status.Conditions)
		return ctrl.Result{}, fmt.Errorf("%w (status update: %w)", err, r.Status().Update(ctx, &m))
	}

	if err := r.updateStatus(ctx, &m); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, r.Status().Update(ctx, &m)
}

// apply creates or updates every desired object and deletes the ones a
// shrunk spec no longer wants (a removed template or variant, an
// extra-values ConfigMap whose content changed).
func (r *TestMatrixReconciler) apply(ctx context.Context, m *benchv1alpha1.TestMatrix, desired render.TestMatrixObjects) error {
	keep := map[string]bool{}
	var objs []client.Object
	if cm := desired.ExtraValues; cm != nil {
		// Named after its content, so it never needs updating.
		if err := applyCreateOnly(ctx, r.Client, r.Scheme, m, cm); err != nil {
			return fmt.Errorf("apply ConfigMap %s: %w", cm.Name, err)
		}
		keep[fmt.Sprintf("%T/%s", cm, cm.Name)] = true
	}
	for _, ad := range desired.Deployments {
		objs = append(objs, ad)
	}
	for _, tc := range desired.TestCases {
		objs = append(objs, tc)
	}
	for _, obj := range objs {
		if err := applyOwned(ctx, r.Client, r.Scheme, m, obj); err != nil {
			return fmt.Errorf("apply %T %s: %w", obj, obj.GetName(), err)
		}
		keep[fmt.Sprintf("%T/%s", obj, obj.GetName())] = true
	}

	selector := client.MatchingLabels{benchv1alpha1.LabelTestMatrix: m.Name}
	var stale []client.Object
	var cms corev1.ConfigMapList
	var ads benchv1alpha1.AuthentikDeploymentList
	var tcs benchv1alpha1.TestCaseList
	for _, list := range []client.ObjectList{&cms, &ads, &tcs} {
		if err := r.List(ctx, list, client.InNamespace(m.Namespace), selector); err != nil {
			return err
		}
	}
	for i := range cms.Items {
		stale = append(stale, &cms.Items[i])
	}
	for i := range ads.Items {
		stale = append(stale, &ads.Items[i])
	}
	for i := range tcs.Items {
		stale = append(stale, &tcs.Items[i])
	}
	for _, obj := range stale {
		if keep[fmt.Sprintf("%T/%s", obj, obj.GetName())] || !metav1.IsControlledBy(obj, m) {
			continue
		}
		if err := r.Delete(ctx, obj); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("delete stale %T %s: %w", obj, obj.GetName(), err)
		}
		logf.FromContext(ctx).Info("Deleted stale TestMatrix object", "kind", fmt.Sprintf("%T", obj), "name", obj.GetName())
	}
	return nil
}

// archive marks the matrix's existing TestCases archived, so they stop
// their k6 load, and otherwise leaves its children alone: nothing is
// recreated, updated or pruned, so deployments can be deleted to free their
// clusters while the TestCases keep their results.
func (r *TestMatrixReconciler) archive(ctx context.Context, desired []*benchv1alpha1.TestCase) error {
	for _, d := range desired {
		var tc benchv1alpha1.TestCase
		if err := r.Get(ctx, client.ObjectKeyFromObject(d), &tc); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return err
		}
		if tc.Spec.Archived {
			continue
		}
		patch := client.MergeFrom(tc.DeepCopy())
		tc.Spec.Archived = true
		if err := r.Patch(ctx, &tc, patch); err != nil {
			return fmt.Errorf("archive TestCase %s: %w", tc.Name, err)
		}
	}
	return nil
}

// updateStatus fills status.rows from the children's own phases and rolls
// them up into the matrix's Ready condition.
func (r *TestMatrixReconciler) updateStatus(ctx context.Context, m *benchv1alpha1.TestMatrix) error {
	var notReady, failed []string
	phaseOf := func(obj client.Object, conditions func() []metav1.Condition) (benchv1alpha1.ReconcilePhase, error) {
		err := r.Get(ctx, types.NamespacedName{Name: obj.GetName(), Namespace: m.Namespace}, obj)
		if apierrors.IsNotFound(err) {
			notReady = append(notReady, obj.GetName())
			return benchv1alpha1.PhasePending, nil
		}
		if err != nil {
			return "", err
		}
		phase := benchv1alpha1.PhaseFor(conditions())
		switch phase {
		case benchv1alpha1.PhaseReady, benchv1alpha1.PhaseArchived:
		case benchv1alpha1.PhaseFailed:
			failed = append(failed, obj.GetName())
		default:
			notReady = append(notReady, obj.GetName())
		}
		return phase, nil
	}

	m.Status.Rows = nil
	staged := true
	for _, template := range m.Spec.Templates {
		var tc benchv1alpha1.TestCase
		tc.Name = m.TestCaseName(template)
		phase, err := phaseOf(&tc, func() []metav1.Condition { return tc.Status.Conditions })
		if err != nil {
			return err
		}
		staged = staged && meta.IsStatusConditionTrue(tc.Status.Conditions, benchv1alpha1.ConditionBurnStaged)
		row := benchv1alpha1.TestMatrixRow{Template: template, TestCase: tc.Name, Phase: phase}
		for _, v := range m.Spec.Variants {
			var ad benchv1alpha1.AuthentikDeployment
			ad.Name = m.DeploymentName(template, v.Name)
			phase, err := phaseOf(&ad, func() []metav1.Condition { return ad.Status.Conditions })
			if err != nil {
				return err
			}
			row.Cells = append(row.Cells, benchv1alpha1.TestMatrixCell{Variant: v.Name, Deployment: ad.Name, Phase: phase})
		}
		m.Status.Rows = append(m.Status.Rows, row)
	}

	// Like a TestCase does for its targets (openBurnStartGate), but across
	// the templates' TestCases: they all start burnStartLead after the last
	// of them is staged. One added later starts right away.
	switch {
	case m.Spec.TestCase.Archived || m.Spec.TestCase.Profile != benchv1alpha1.ProfileBurn:
		m.Status.BurnStartAt = nil
	case m.Status.BurnStartAt == nil && staged:
		m.Status.BurnStartAt = &metav1.Time{Time: time.Now().Add(burnStartLead).Truncate(time.Second)}
		logf.FromContext(ctx).Info("Scheduled burn start", "startAt", m.Status.BurnStartAt.Time)
	}

	switch {
	case m.Spec.TestCase.Archived:
		setCondition(r.Recorder, m, &m.Status.Conditions, m.Generation, benchv1alpha1.ConditionReady, metav1.ConditionFalse,
			benchv1alpha1.ReasonArchived, "test cases are archived")
	case len(failed) > 0:
		setCondition(r.Recorder, m, &m.Status.Conditions, m.Generation, benchv1alpha1.ConditionReady, metav1.ConditionFalse,
			benchv1alpha1.ReasonReconcileError, "failed: "+strings.Join(failed, ", "))
	case len(notReady) > 0:
		setCondition(r.Recorder, m, &m.Status.Conditions, m.Generation, benchv1alpha1.ConditionReady, metav1.ConditionFalse,
			benchv1alpha1.ReasonDependencyNotReady, "waiting for: "+strings.Join(notReady, ", "))
	default:
		setCondition(r.Recorder, m, &m.Status.Conditions, m.Generation, benchv1alpha1.ConditionReady, metav1.ConditionTrue,
			benchv1alpha1.ReasonReady, "every test case and deployment is ready")
	}
	m.Status.ObservedGeneration = m.Generation
	m.Status.Phase = benchv1alpha1.PhaseFor(m.Status.Conditions)
	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *TestMatrixReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&benchv1alpha1.TestMatrix{}).
		Owns(&benchv1alpha1.AuthentikDeployment{}).
		Owns(&benchv1alpha1.TestCase{}).
		Owns(&corev1.ConfigMap{}).
		Named("testmatrix").
		Complete(r)
}
