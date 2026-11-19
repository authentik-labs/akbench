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
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
	"github.com/authentik-labs/akbench/internal/render"
)

// reconcileParent reconciles a TestCase with spec.templates: it owns one
// child TestCase per template and one AuthentikDeployment per template and
// variant, all in its own namespace, so deleting it garbage-collects them.
// It drives no k6 workload itself.
func (r *TestCaseReconciler) reconcileParent(ctx context.Context, tc *benchv1alpha1.TestCase) (ctrl.Result, error) {
	if len(tc.Status.Deployments) > 0 {
		// A TestCase turned into a parent stops the k6 load it drove itself.
		pending, err := r.stopAllBurnDeployments(ctx, tc, false)
		if err != nil {
			return ctrl.Result{}, err
		}
		if pending {
			return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
		}
		tc.Status.Deployments = nil
		meta.RemoveStatusCondition(&tc.Status.Conditions, benchv1alpha1.ConditionBurnStaged)
	}

	desired, err := render.TestCaseChildren(tc)
	if err == nil && tc.Spec.Archived {
		err = r.archiveChildren(ctx, desired.TestCases)
	} else if err == nil {
		err = r.applyChildren(ctx, tc, desired)
	}
	if err != nil {
		setCondition(r.Recorder, tc, &tc.Status.Conditions, tc.Generation, benchv1alpha1.ConditionReady, metav1.ConditionFalse,
			benchv1alpha1.ReasonReconcileError, err.Error())
		tc.Status.Phase = benchv1alpha1.PhaseFor(tc.Status.Conditions)
		return ctrl.Result{}, fmt.Errorf("%w (status update: %w)", err, r.Status().Update(ctx, tc))
	}

	if err := r.updateParentStatus(ctx, tc); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, r.Status().Update(ctx, tc)
}

// applyChildren creates or updates every desired object and deletes the
// ones a shrunk spec no longer wants (a removed template or variant, an
// extra-values ConfigMap whose content changed).
func (r *TestCaseReconciler) applyChildren(ctx context.Context, tc *benchv1alpha1.TestCase, desired render.TestCaseChildObjects) error {
	keep := map[string]bool{}
	var objs []client.Object
	if cm := desired.ExtraValues; cm != nil {
		// Named after its content, so it never needs updating.
		if err := applyCreateOnly(ctx, r.Client, r.Scheme, tc, cm); err != nil {
			return fmt.Errorf("apply ConfigMap %s: %w", cm.Name, err)
		}
		keep[fmt.Sprintf("%T/%s", cm, cm.Name)] = true
	}
	for _, ad := range desired.Deployments {
		objs = append(objs, ad)
	}
	for _, child := range desired.TestCases {
		objs = append(objs, child)
	}
	for _, obj := range objs {
		if err := applyOwned(ctx, r.Client, r.Scheme, tc, obj); err != nil {
			return fmt.Errorf("apply %T %s: %w", obj, obj.GetName(), err)
		}
		keep[fmt.Sprintf("%T/%s", obj, obj.GetName())] = true
	}

	stale, err := listChildren(ctx, r.Client, tc.Namespace, client.MatchingLabels{benchv1alpha1.LabelParent: tc.Name})
	if err != nil {
		return err
	}
	for _, obj := range stale {
		if keep[fmt.Sprintf("%T/%s", obj, obj.GetName())] || !metav1.IsControlledBy(obj, tc) {
			continue
		}
		if err := r.Delete(ctx, obj); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("delete stale %T %s: %w", obj, obj.GetName(), err)
		}
		logf.FromContext(ctx).Info("Deleted stale TestCase child", "kind", fmt.Sprintf("%T", obj), "name", obj.GetName())
	}
	return nil
}

// listChildren lists every kind of object a parent TestCase creates
// matching selector.
func listChildren(ctx context.Context, c client.Client, namespace string, selector client.MatchingLabels) ([]client.Object, error) {
	var cms corev1.ConfigMapList
	var ads benchv1alpha1.AuthentikDeploymentList
	var tcs benchv1alpha1.TestCaseList
	for _, list := range []client.ObjectList{&cms, &ads, &tcs} {
		if err := c.List(ctx, list, client.InNamespace(namespace), selector); err != nil {
			return nil, err
		}
	}
	var out []client.Object
	for i := range cms.Items {
		out = append(out, &cms.Items[i])
	}
	for i := range ads.Items {
		out = append(out, &ads.Items[i])
	}
	for i := range tcs.Items {
		out = append(out, &tcs.Items[i])
	}
	return out, nil
}

// archiveChildren marks the parent's existing children archived, so they
// stop their k6 load, and otherwise leaves them alone: nothing is
// recreated, updated or pruned, so deployments can be deleted to free their
// clusters while the children keep their results.
func (r *TestCaseReconciler) archiveChildren(ctx context.Context, desired []*benchv1alpha1.TestCase) error {
	for _, d := range desired {
		var child benchv1alpha1.TestCase
		if err := r.Get(ctx, client.ObjectKeyFromObject(d), &child); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return err
		}
		if child.Spec.Archived {
			continue
		}
		patch := client.MergeFrom(child.DeepCopy())
		child.Spec.Archived = true
		if err := r.Patch(ctx, &child, patch); err != nil {
			return fmt.Errorf("archive TestCase %s: %w", child.Name, err)
		}
	}
	return nil
}

// updateParentStatus fills status.children from the children's own phases
// and rolls them up into the parent's Ready condition.
func (r *TestCaseReconciler) updateParentStatus(ctx context.Context, tc *benchv1alpha1.TestCase) error {
	// storageFull is every failed child that stopped its burn because a
	// target ran out of postgres storage, with why: unlike other failures
	// it says what to do about it.
	var notReady, failed, storageFull []string
	phaseOf := func(obj client.Object, conditions func() []metav1.Condition) (benchv1alpha1.ReconcilePhase, error) {
		err := r.Get(ctx, types.NamespacedName{Name: obj.GetName(), Namespace: tc.Namespace}, obj)
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
			if ready := meta.FindStatusCondition(conditions(), benchv1alpha1.ConditionReady); ready.Reason == benchv1alpha1.ReasonPostgresStorageFull {
				storageFull = append(storageFull, obj.GetName()+": "+ready.Message)
			}
		default:
			notReady = append(notReady, obj.GetName())
		}
		return phase, nil
	}

	tc.Status.Children = nil
	staged := true
	for _, template := range tc.Spec.Templates {
		var child benchv1alpha1.TestCase
		child.Name = tc.ChildName(template)
		phase, err := phaseOf(&child, func() []metav1.Condition { return child.Status.Conditions })
		if err != nil {
			return err
		}
		staged = staged && meta.IsStatusConditionTrue(child.Status.Conditions, benchv1alpha1.ConditionBurnStaged)
		row := benchv1alpha1.TestCaseChildStatus{Template: template, TestCase: child.Name, Phase: phase}
		for _, v := range tc.Spec.Variants {
			var ad benchv1alpha1.AuthentikDeployment
			ad.Name = tc.ChildDeploymentName(template, v.Name)
			phase, err := phaseOf(&ad, func() []metav1.Condition { return ad.Status.Conditions })
			if err != nil {
				return err
			}
			row.Cells = append(row.Cells, benchv1alpha1.TestCaseChildCell{Variant: v.Name, Deployment: ad.Name, Phase: phase})
		}
		tc.Status.Children = append(tc.Status.Children, row)
	}

	// Like a TestCase does for its targets (openBurnStartGate), but across
	// the children: they all start burnStartLead after the last of them is
	// staged. One added later starts right away.
	switch {
	case tc.Spec.Archived || tc.Spec.Profile != benchv1alpha1.ProfileBurn:
		tc.Status.BurnStartAt = nil
	case tc.Status.BurnStartAt == nil && staged:
		tc.Status.BurnStartAt = &metav1.Time{Time: time.Now().Add(burnStartLead).Truncate(time.Second)}
		logf.FromContext(ctx).Info("Scheduled burn start", "startAt", tc.Status.BurnStartAt.Time)
	}

	switch {
	case tc.Spec.Archived:
		setCondition(r.Recorder, tc, &tc.Status.Conditions, tc.Generation, benchv1alpha1.ConditionReady, metav1.ConditionFalse,
			benchv1alpha1.ReasonArchived, "test cases are archived")
	case len(failed) > 0 && len(failed) == len(storageFull):
		setCondition(r.Recorder, tc, &tc.Status.Conditions, tc.Generation, benchv1alpha1.ConditionReady, metav1.ConditionFalse,
			benchv1alpha1.ReasonPostgresStorageFull, strings.Join(storageFull, "; "))
	case len(failed) > 0:
		setCondition(r.Recorder, tc, &tc.Status.Conditions, tc.Generation, benchv1alpha1.ConditionReady, metav1.ConditionFalse,
			benchv1alpha1.ReasonReconcileError, strings.Join(append([]string{"failed: " + strings.Join(failed, ", ")}, storageFull...), "; "))
	case len(notReady) > 0:
		setCondition(r.Recorder, tc, &tc.Status.Conditions, tc.Generation, benchv1alpha1.ConditionReady, metav1.ConditionFalse,
			benchv1alpha1.ReasonDependencyNotReady, "waiting for: "+strings.Join(notReady, ", "))
	default:
		setCondition(r.Recorder, tc, &tc.Status.Conditions, tc.Generation, benchv1alpha1.ConditionReady, metav1.ConditionTrue,
			benchv1alpha1.ReasonReady, "every test case and deployment is ready")
	}
	tc.Status.ObservedGeneration = tc.Generation
	tc.Status.Phase = benchv1alpha1.PhaseFor(tc.Status.Conditions)
	return nil
}
