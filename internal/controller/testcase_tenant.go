package controller

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
	"github.com/authentik-labs/akbench/internal/render"
)

// needsTenant reports whether tmpl (nil for an inline or ConfigMap script)
// needs its target to have a seeded tenant.
func needsTenant(tmpl *benchv1alpha1.TestCaseTemplate) bool {
	return tmpl != nil && tmpl.Spec.HasDependency(benchv1alpha1.DependencySeededTenant)
}

// tenantForTarget returns the tenant k6 is told about when tmpl runs against
// topo, a Ready deployment. Nil with no message when tmpl doesn't need one
// (it doesn't list seeded-tenant, or there is no template at all); nil with a
// message saying why when it does but topo has none to offer. There is no
// default tenant: a test that needs one says so, rather than measuring
// against whatever happens to be in the database.
func tenantForTarget(topo *benchv1alpha1.AuthentikDeployment, tmpl *benchv1alpha1.TestCaseTemplate) (*benchv1alpha1.TenantParams, string) {
	if !needsTenant(tmpl) {
		return nil, ""
	}
	if topo.Spec.Tenant == nil {
		return nil, fmt.Sprintf("template %s needs a seeded tenant; deployment %s has none - choose a tenant preset on it", tmpl.Name, topo.Name)
	}
	if cond := meta.FindStatusCondition(topo.Status.Conditions, benchv1alpha1.ConditionTenantSeeded); cond != nil &&
		cond.Status == metav1.ConditionFalse && cond.Reason == benchv1alpha1.ReasonTenantResetRequired {
		return nil, cond.Message
	}
	// A deployment is only Ready with its tenant seeded, but its Ready
	// condition can predate a spec.tenant set since.
	if topo.Status.Tenant == nil || topo.Status.Tenant.State != benchv1alpha1.TenantSeeded {
		return nil, fmt.Sprintf("waiting for deployment %s to seed its tenant", topo.Name)
	}
	params := topo.Status.Tenant.Params
	return &params, ""
}

// tenantRecord is what a result records about the tenant it ran against:
// topo's seeded tenant, given tenant (from tenantForTarget) is set.
func tenantRecord(topo *benchv1alpha1.AuthentikDeployment, tenant *benchv1alpha1.TenantParams) *benchv1alpha1.TenantRecord {
	if tenant == nil {
		return nil
	}
	return &benchv1alpha1.TenantRecord{Preset: topo.Status.Tenant.Preset, Params: *tenant}
}

// tenantSeedingFailure says why a deployment isn't Ready when that's its
// tenant seeding having failed, which won't fix itself: it takes a reset or
// another preset. "" otherwise.
func tenantSeedingFailure(topo *benchv1alpha1.AuthentikDeployment) string {
	if topo == nil || topo.Status.Tenant == nil || topo.Status.Tenant.State != benchv1alpha1.TenantFailed {
		return ""
	}
	return "tenant seeding failed: " + topo.Status.Tenant.Message
}

// differentTenants refuses a test case whose targets hold different seeded
// tenants: its results would compare deployments under different loads of
// data, not the deployments themselves. A target without a seeded tenant is
// left to tenantForTarget. "" when tmpl needs no tenant or they all match;
// a parent test case's children always match, since every deployment it
// creates gets the same spec.tenant.
func (r *TestCaseReconciler) differentTenants(ctx context.Context, tc *benchv1alpha1.TestCase, tmpl *benchv1alpha1.TestCaseTemplate,
	refs []corev1.LocalObjectReference,
) (string, error) {
	if !needsTenant(tmpl) || len(refs) < 2 {
		return "", nil
	}
	var seeded []string
	var first *benchv1alpha1.TenantParams
	same := true
	for _, ref := range refs {
		var topo benchv1alpha1.AuthentikDeployment
		if err := r.Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: tc.Namespace}, &topo); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return "", fmt.Errorf("get AuthentikDeployment %q: %w", ref.Name, err)
		}
		t := topo.Status.Tenant
		if t == nil || t.State != benchv1alpha1.TenantSeeded {
			continue
		}
		seeded = append(seeded, fmt.Sprintf("%s has %q (%d users)", ref.Name, t.Preset, t.Params.Users))
		if first == nil {
			first = &t.Params
		} else if t.Params != *first {
			same = false
		}
	}
	if same {
		return "", nil
	}
	return "the targets hold different seeded tenants: " + strings.Join(seeded, ", ") +
		" - seed them from the same preset to compare them", nil
}

// ensureTenantPassword mirrors the management cluster's tenant password
// Secret (in namespace) onto rc under the same name, for k6 to read the
// users' password from - the same as ensureFixturesSecret does for the
// fixture users. Nothing to do without a tenant. It refuses, saying why,
// when there is no password to mirror or it isn't the one topo's users were
// given: k6 would fail every login, which reads as authentik failing.
func ensureTenantPassword(ctx context.Context, c client.Client, rc client.Client, namespace string,
	topo *benchv1alpha1.AuthentikDeployment, tenant *benchv1alpha1.TenantParams,
) (string, error) {
	if tenant == nil {
		return "", nil
	}
	key := types.NamespacedName{Name: render.TenantPasswordSecretName, Namespace: namespace}
	var source corev1.Secret
	if err := c.Get(ctx, key, &source); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Sprintf("the tenant password Secret %s/%s is gone - reset %s to seed it again", namespace, key.Name, topo.Name), nil
		}
		return "", fmt.Errorf("get tenant password secret: %w", err)
	}
	if passwordFingerprint(string(source.Data[render.TenantPasswordKey])) != topo.Status.Tenant.PasswordFingerprint {
		return fmt.Sprintf("the tenant password changed since %s was seeded - reset %s", topo.Name, topo.Name), nil
	}
	return "", ensureSecretData(ctx, rc, key, source.Data)
}
