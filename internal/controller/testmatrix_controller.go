package controller

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
)

// TestMatrixReconciler migrates every TestMatrix into the parent TestCase
// that replaced it: it creates a TestCase of the same name with the matrix's
// spec, hands it the matrix's children (relabeled, and controlled by the
// TestCase instead), then deletes the matrix. The children keep their names
// and are never recreated, so a running burn carries on undisturbed.
type TestMatrixReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=bench.goauthentik.io,resources=testmatrices,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups=bench.goauthentik.io,resources=testmatrices/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=bench.goauthentik.io,resources=authentikdeployments;testcases,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;update;patch

func (r *TestMatrixReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var m benchv1alpha1.TestMatrix
	if err := r.Get(ctx, req.NamespacedName, &m); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !m.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	parent, err := r.ensureParent(ctx, &m)
	if err != nil {
		setCondition(r.Recorder, &m, &m.Status.Conditions, m.Generation, benchv1alpha1.ConditionReady, metav1.ConditionFalse,
			benchv1alpha1.ReasonReconcileError, "migrating to a TestCase: "+err.Error())
		m.Status.Phase = benchv1alpha1.PhaseFor(m.Status.Conditions)
		return ctrl.Result{}, fmt.Errorf("%w (status update: %w)", err, r.Status().Update(ctx, &m))
	}
	if err := r.adoptChildren(ctx, &m, parent); err != nil {
		return ctrl.Result{}, err
	}
	// Nothing is controlled by the matrix anymore, so the default
	// background propagation leaves the children alone.
	if err := r.Delete(ctx, &m); client.IgnoreNotFound(err) != nil {
		return ctrl.Result{}, err
	}
	logf.FromContext(ctx).Info("Migrated TestMatrix to TestCase", "name", m.Name)
	return ctrl.Result{}, nil
}

// ensureParent returns the parent TestCase m migrates into, creating it if
// needed. A TestCase of that name that isn't a parent predates the
// migration and is never touched.
func (r *TestMatrixReconciler) ensureParent(ctx context.Context, m *benchv1alpha1.TestMatrix) (*benchv1alpha1.TestCase, error) {
	var tc benchv1alpha1.TestCase
	err := r.Get(ctx, client.ObjectKeyFromObject(m), &tc)
	if apierrors.IsNotFound(err) {
		parent := m.TestCase()
		if err := r.Create(ctx, parent); err != nil {
			return nil, err
		}
		return parent, nil
	}
	if err != nil {
		return nil, err
	}
	if !tc.IsParent() {
		return nil, fmt.Errorf("TestCase %s already exists and has no templates", tc.Name)
	}
	return &tc, nil
}

// adoptChildren moves every object m controls from LabelTestMatrix to
// LabelParent and from m's controller reference to parent's.
func (r *TestMatrixReconciler) adoptChildren(ctx context.Context, m *benchv1alpha1.TestMatrix, parent *benchv1alpha1.TestCase) error {
	children, err := listChildren(ctx, r.Client, m.Namespace, client.MatchingLabels{benchv1alpha1.LabelTestMatrix: m.Name})
	if err != nil {
		return err
	}
	for _, obj := range children {
		if !metav1.IsControlledBy(obj, m) {
			continue
		}
		patch := client.MergeFrom(obj.DeepCopyObject().(client.Object))
		labels := obj.GetLabels()
		delete(labels, benchv1alpha1.LabelTestMatrix)
		labels[benchv1alpha1.LabelParent] = parent.Name
		obj.SetLabels(labels)
		refs := obj.GetOwnerReferences()
		for i := range refs {
			if refs[i].UID == m.UID {
				refs = append(refs[:i], refs[i+1:]...)
				break
			}
		}
		obj.SetOwnerReferences(refs)
		if err := controllerutil.SetControllerReference(parent, obj, r.Scheme); err != nil {
			return err
		}
		if err := r.Patch(ctx, obj, patch); err != nil {
			return fmt.Errorf("adopt %T %s: %w", obj, obj.GetName(), err)
		}
	}
	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *TestMatrixReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&benchv1alpha1.TestMatrix{}).
		Named("testmatrix").
		Complete(r)
}
