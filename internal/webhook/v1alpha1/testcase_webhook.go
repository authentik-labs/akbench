package v1alpha1

import (
	"context"
	"slices"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
	"github.com/authentik-labs/akbench/internal/capacity"
	"github.com/authentik-labs/akbench/internal/multicluster"
)

// SetupTestCaseWebhookWithManager registers the webhook for TestCase in the manager.
// tracker may be nil, which skips the capacity check.
func SetupTestCaseWebhookWithManager(mgr ctrl.Manager, tracker *multicluster.Tracker) error {
	return ctrl.NewWebhookManagedBy(mgr, &benchv1alpha1.TestCase{}).
		WithValidator(&TestCaseValidator{Capacity: newCapacityChecker(mgr, tracker)}).
		Complete()
}

// +kubebuilder:webhook:path=/validate-bench-goauthentik-io-v1alpha1-testcase,mutating=false,failurePolicy=fail,sideEffects=None,groups=bench.goauthentik.io,resources=testcases,verbs=create;update,versions=v1alpha1,name=vtestcase-v1alpha1.kb.io,admissionReviewVersions=v1

// TestCaseValidator rejects a TestCase whose k6 runners can't fit on their
// runner cluster's k6 pool, even once cluster-autoscaler has grown it - a
// Burn runner would otherwise sit Pending, and every Quick run's Job with it.
//
// +kubebuilder:object:generate=false
type TestCaseValidator struct {
	// Capacity is optional: nil skips the capacity check.
	Capacity *CapacityChecker
}

// ValidateCreate implements admission.Validator so a webhook will be registered for the type TestCase.
func (v *TestCaseValidator) ValidateCreate(ctx context.Context, obj *benchv1alpha1.TestCase) (admission.Warnings, error) {
	if errs := nameErrors(obj.Name, maxTestCaseNameLength); len(errs) > 0 {
		return nil, apierrors.NewInvalid(schema.GroupKind{Group: benchv1alpha1.GroupVersion.Group, Kind: "TestCase"}, obj.Name, errs)
	}
	return v.validate(ctx, nil, obj)
}

// ValidateUpdate implements admission.Validator so a webhook will be registered for the type TestCase.
func (v *TestCaseValidator) ValidateUpdate(ctx context.Context, oldObj, newObj *benchv1alpha1.TestCase) (admission.Warnings, error) {
	if !newObj.DeletionTimestamp.IsZero() {
		return nil, nil
	}
	return v.validate(ctx, oldObj, newObj)
}

// ValidateDelete implements admission.Validator so a webhook will be registered for the type TestCase.
func (v *TestCaseValidator) ValidateDelete(_ context.Context, _ *benchv1alpha1.TestCase) (admission.Warnings, error) {
	return nil, nil
}

func (v *TestCaseValidator) validate(ctx context.Context, oldObj, obj *benchv1alpha1.TestCase) (admission.Warnings, error) {
	if v.Capacity == nil {
		return nil, nil
	}
	if oldObj != nil && equality.Semantic.DeepEqual(runnerSpec(oldObj), runnerSpec(obj)) {
		return nil, nil
	}

	// One k6 runner per target, on the target's own cluster unless
	// runnerClusterRef pins one. A target that doesn't exist (yet) is
	// skipped: the controller reports it, and there's no cluster to check.
	byCluster := map[string][]capacity.Group{}
	var clusters []string
	for _, ref := range obj.Deployments() {
		var d benchv1alpha1.AuthentikDeployment
		if err := v.Capacity.Mgmt.Get(ctx, types.NamespacedName{Namespace: obj.Namespace, Name: ref.Name}, &d); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return nil, apierrors.NewInternalError(err)
		}
		cluster := d.Spec.ClusterRef.Name
		if obj.Spec.RunnerClusterRef != nil && obj.Spec.RunnerClusterRef.Name != "" {
			cluster = obj.Spec.RunnerClusterRef.Name
		}
		if cluster == "" {
			continue
		}
		if _, ok := byCluster[cluster]; !ok {
			clusters = append(clusters, cluster)
		}
		byCluster[cluster] = append(byCluster[cluster], capacity.Group{Pods: []capacity.Pod{runnerPod(obj, ref.Name)}})
	}
	slices.Sort(clusters)

	// The TestCase's own running k6 pods are replaced, not added to.
	ownRunner := func(p *corev1.Pod) bool { return p.Labels[benchv1alpha1.LabelTestCase] == obj.Name }
	var warnings admission.Warnings
	var errs field.ErrorList
	for _, cluster := range clusters {
		warning, err := v.Capacity.fit(ctx, obj.Namespace, cluster,
			capacity.Placement{Role: benchv1alpha1.WorkerPoolRoleK6}, ownRunner, byCluster[cluster])
		if err != nil {
			errs = append(errs, field.Forbidden(field.NewPath("spec"), err.Error()))
		}
		if warning != "" {
			warnings = append(warnings, warning)
		}
	}
	if len(errs) == 0 {
		return warnings, nil
	}
	return warnings, apierrors.NewInvalid(schema.GroupKind{Group: benchv1alpha1.GroupVersion.Group, Kind: "TestCase"}, obj.Name, errs)
}

// runnerSpec is the part of a TestCase's spec that decides what k6 pods it
// schedules, and where.
func runnerSpec(tc *benchv1alpha1.TestCase) []any {
	return []any{tc.Deployments(), tc.Spec.RunnerClusterRef, tc.Spec.RunnerResources}
}
