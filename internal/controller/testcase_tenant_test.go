package controller

import (
	"context"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
	"github.com/authentik-labs/akbench/internal/render"
)

func TestTenantForTarget(t *testing.T) {
	needsTenant := &benchv1alpha1.TestCaseTemplate{Name: "tenant-login", Spec: benchv1alpha1.TestCaseTemplateSpec{
		Dependencies: []benchv1alpha1.TestCaseDependency{benchv1alpha1.DependencySeededTenant},
	}}
	params := benchv1alpha1.TenantParams{Seed: 1, Users: 50, Groups: 20, GroupDepth: 2, GroupsPerUser: 3}
	deployment := func(change func(*benchv1alpha1.AuthentikDeployment)) *benchv1alpha1.AuthentikDeployment {
		topo := &benchv1alpha1.AuthentikDeployment{Name: "d"}
		topo.Spec.Tenant = &benchv1alpha1.TenantSpec{PresetRef: corev1.LocalObjectReference{Name: "tiny"}}
		topo.Status.Tenant = &benchv1alpha1.TenantStatus{Preset: "tiny", Params: params, State: benchv1alpha1.TenantSeeded}
		topo.Status.Conditions = []metav1.Condition{{Type: benchv1alpha1.ConditionTenantSeeded, Status: metav1.ConditionTrue, Reason: benchv1alpha1.ReasonReady}}
		if change != nil {
			change(topo)
		}
		return topo
	}

	for name, tc := range map[string]struct {
		topo    *benchv1alpha1.AuthentikDeployment
		tmpl    *benchv1alpha1.TestCaseTemplate
		refusal string
		tenant  bool
	}{
		"no template (an inline script)":  {topo: deployment(nil)},
		"a template that needs no tenant": {topo: deployment(nil), tmpl: &benchv1alpha1.TestCaseTemplate{Name: "login"}},
		"no tenant on the deployment": {
			topo:    deployment(func(d *benchv1alpha1.AuthentikDeployment) { d.Spec.Tenant, d.Status.Tenant = nil, nil }),
			tmpl:    needsTenant,
			refusal: "template tenant-login needs a seeded tenant; deployment d has none - choose a tenant preset on it",
		},
		"a tenant needing a reset": {
			topo: deployment(func(d *benchv1alpha1.AuthentikDeployment) {
				d.Status.Conditions[0] = metav1.Condition{Type: benchv1alpha1.ConditionTenantSeeded, Status: metav1.ConditionFalse,
					Reason: benchv1alpha1.ReasonTenantResetRequired, Message: "changed - reset the deployment to seed it again"}
			}),
			tmpl:    needsTenant,
			refusal: "changed - reset the deployment to seed it again",
		},
		"a tenant chosen after the deployment last reconciled": {
			topo:    deployment(func(d *benchv1alpha1.AuthentikDeployment) { d.Status.Tenant, d.Status.Conditions = nil, nil }),
			tmpl:    needsTenant,
			refusal: "waiting for deployment d to seed its tenant",
		},
		"a seeded tenant": {topo: deployment(nil), tmpl: needsTenant, tenant: true},
	} {
		got, refusal := tenantForTarget(tc.topo, tc.tmpl)
		if refusal != tc.refusal {
			t.Errorf("%s: refusal %q, want %q", name, refusal, tc.refusal)
		}
		if tc.tenant != (got != nil) || (got != nil && *got != params) {
			t.Errorf("%s: tenant %+v, want one: %v", name, got, tc.tenant)
		}
	}
}

func TestTenantSeedingFailure(t *testing.T) {
	topo := &benchv1alpha1.AuthentikDeployment{}
	if got := tenantSeedingFailure(topo); got != "" {
		t.Errorf("no tenant: got %q", got)
	}
	topo.Status.Tenant = &benchv1alpha1.TenantStatus{State: benchv1alpha1.TenantFailed, Message: "seeding failed: boom"}
	if got := tenantSeedingFailure(topo); got != "tenant seeding failed: seeding failed: boom" {
		t.Errorf("failed tenant: got %q", got)
	}
}

var _ = Describe("Test cases on a seeded tenant", func() {
	// Not "tenant-login": the suite seeds the built-in template of that name.
	const templateName = "seeded-tenant-test"
	ctx := context.Background()
	params := benchv1alpha1.TenantParams{Seed: 4, Users: 50, Groups: 20, GroupDepth: 2, GroupsPerUser: 3}
	var topo *benchv1alpha1.AuthentikDeployment
	var password string

	// seedWith fakes what AuthentikDeploymentReconciler records once the
	// tenant is in the database.
	seedWith := func(topo *benchv1alpha1.AuthentikDeployment, preset string, params benchv1alpha1.TenantParams) {
		GinkgoHelper()
		topo.Spec.Tenant = &benchv1alpha1.TenantSpec{PresetRef: corev1.LocalObjectReference{Name: preset}}
		Expect(k8sClient.Update(ctx, topo)).To(Succeed())
		topo.Status.Tenant = &benchv1alpha1.TenantStatus{
			Preset: preset, Params: params, PasswordFingerprint: passwordFingerprint(password), State: benchv1alpha1.TenantSeeded,
		}
		meta.SetStatusCondition(&topo.Status.Conditions, metav1.Condition{
			Type: benchv1alpha1.ConditionTenantSeeded, Status: metav1.ConditionTrue, Reason: benchv1alpha1.ReasonReady, Message: "seeded",
		})
		Expect(k8sClient.Status().Update(ctx, topo)).To(Succeed())
	}
	seed := func(topo *benchv1alpha1.AuthentikDeployment) {
		GinkgoHelper()
		seedWith(topo, "tiny", params)
	}
	// needsReset fakes the deployment controller finding that topo's tenant
	// no longer matches what it asks for.
	needsReset := func(topo *benchv1alpha1.AuthentikDeployment) {
		GinkgoHelper()
		meta.SetStatusCondition(&topo.Status.Conditions, metav1.Condition{
			Type: benchv1alpha1.ConditionTenantSeeded, Status: metav1.ConditionFalse,
			Reason: benchv1alpha1.ReasonTenantResetRequired, Message: `TenantPreset "tiny" has changed since the tenant was seeded - reset the deployment to seed it again`,
		})
		Expect(k8sClient.Status().Update(ctx, topo)).To(Succeed())
	}
	replacePassword := func() {
		GinkgoHelper()
		var secret corev1.Secret
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: render.TenantPasswordSecretName, Namespace: "default"}, &secret)).To(Succeed())
		secret.Data[render.TenantPasswordKey] = []byte("replaced")
		Expect(k8sClient.Update(ctx, &secret)).To(Succeed())
	}
	testCaseOf := func(name string, profile benchv1alpha1.TestProfile, template string, targets ...string) *benchv1alpha1.TestCase {
		GinkgoHelper()
		refs := make([]corev1.LocalObjectReference, 0, len(targets))
		for _, target := range targets {
			refs = append(refs, corev1.LocalObjectReference{Name: target})
		}
		tc := &benchv1alpha1.TestCase{
			Name: name, Namespace: "default",
			Spec: benchv1alpha1.TestCaseSpec{
				DeploymentRefs: refs,
				Template:       template,
				Profile:        profile,
				VUs:            1,
				Iterations:     1,
			},
		}
		Expect(k8sClient.Create(ctx, tc)).To(Succeed())
		DeferCleanup(func() { forceDelete(ctx, tc) })
		return tc
	}
	testCase := func(name string, profile benchv1alpha1.TestProfile) *benchv1alpha1.TestCase {
		GinkgoHelper()
		tc := &benchv1alpha1.TestCase{
			Name: name, Namespace: "default",
			Spec: benchv1alpha1.TestCaseSpec{
				DeploymentRefs: []corev1.LocalObjectReference{{Name: topo.Name}},
				Template:       templateName,
				Profile:        profile,
				VUs:            1,
				Iterations:     1,
			},
		}
		Expect(k8sClient.Create(ctx, tc)).To(Succeed())
		DeferCleanup(func() { forceDelete(ctx, tc) })
		return tc
	}
	reconcileTestCase := func(tc *benchv1alpha1.TestCase) *metav1.Condition {
		GinkgoHelper()
		r := &TestCaseReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: &record.FakeRecorder{}, Tracker: testTracker}
		_, err := r.Reconcile(ctx, reconcile.Request{Name: tc.Name, Namespace: tc.Namespace})
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: tc.Name, Namespace: tc.Namespace}, tc)).To(Succeed())
		ready := meta.FindStatusCondition(tc.Status.Conditions, benchv1alpha1.ConditionReady)
		Expect(ready).NotTo(BeNil())
		return ready
	}
	// startQuickRun reconciles a new run of tc once, returning it and the
	// result.
	startQuickRun := func(tc *benchv1alpha1.TestCase) (*benchv1alpha1.TestRun, reconcile.Result) {
		GinkgoHelper()
		run := &benchv1alpha1.TestRun{
			Name: tc.Name + "-1", Namespace: "default",
			Spec: benchv1alpha1.TestRunSpec{TestCaseRef: corev1.LocalObjectReference{Name: tc.Name}},
		}
		Expect(k8sClient.Create(ctx, run)).To(Succeed())
		DeferCleanup(func() { forceDelete(ctx, run) })
		r := &TestRunReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: &record.FakeRecorder{}, Tracker: testTracker}
		result, err := r.Reconcile(ctx, reconcile.Request{Name: run.Name, Namespace: "default"})
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: run.Name, Namespace: "default"}, run)).To(Succeed())
		return run, result
	}
	expectRunHeld := func(run *benchv1alpha1.TestRun, result reconcile.Result, message string) {
		GinkgoHelper()
		Expect(result.RequeueAfter).To(BeNumerically(">", 0))
		Expect(run.Status.JobRef).To(BeNil())
		ready := meta.FindStatusCondition(run.Status.Conditions, benchv1alpha1.ConditionReady)
		Expect(ready).NotTo(BeNil())
		Expect(ready.Message).To(ContainSubstring(message))
	}
	expectTenantEnv := func(env []corev1.EnvVar) {
		GinkgoHelper()
		values := envMap(env)
		Expect(values).To(HaveKeyWithValue("TENANT_USERS", "50"))
		Expect(values).To(HaveKeyWithValue("TENANT_SEED", "4"))
		var password *corev1.EnvVar
		for i := range env {
			if env[i].Name == "TENANT_PASSWORD" {
				password = &env[i]
			}
		}
		Expect(password).NotTo(BeNil())
		Expect(password.ValueFrom.SecretKeyRef.Name).To(Equal(render.TenantPasswordSecretName))
		Expect(password.ValueFrom.SecretKeyRef.Key).To(Equal(render.TenantPasswordKey))
	}

	BeforeEach(func() {
		topo = createReadyDeployment(ctx, "tenant-target", "tenant-target", "tenant-target-server.ns.svc.cluster.local")
		DeferCleanup(func() { forceDelete(ctx, topo) })
		tmpl := &benchv1alpha1.TestCaseTemplate{
			Name: templateName, Namespace: "default",
			Spec: benchv1alpha1.TestCaseTemplateSpec{
				Script:       "export default function () {}",
				Dependencies: []benchv1alpha1.TestCaseDependency{benchv1alpha1.DependencySeededTenant},
			},
		}
		Expect(k8sClient.Create(ctx, tmpl)).To(Succeed())
		DeferCleanup(func() { forceDelete(ctx, tmpl) })
		// The deployment controller generates this once it seeds anything.
		var err error
		password, err = ensureRandomSecret(ctx, k8sClient, types.NamespacedName{Name: render.TenantPasswordSecretName, Namespace: "default"}, render.TenantPasswordKey, 32)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			forceDelete(ctx, &corev1.Secret{Name: render.TenantPasswordSecretName, Namespace: "default"})
		})
	})

	It("refuses a deployment without a tenant", func() {
		tc := testCase("tenant-none", benchv1alpha1.ProfileBurn)
		ready := reconcileTestCase(tc)
		Expect(ready.Status).To(Equal(metav1.ConditionFalse))
		Expect(ready.Message).To(ContainSubstring("template seeded-tenant-test needs a seeded tenant; deployment tenant-target has none"))
		err := k8sClient.Get(ctx, types.NamespacedName{Name: render.BurnDeploymentName(tc.Name, topo.Name), Namespace: "default"}, &appsv1.Deployment{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
	})

	It("refuses a tenant that needs a reset", func() {
		seed(topo)
		needsReset(topo)
		ready := reconcileTestCase(testCase("tenant-reset", benchv1alpha1.ProfileBurn))
		Expect(ready.Status).To(Equal(metav1.ConditionFalse))
		Expect(ready.Message).To(ContainSubstring("reset the deployment to seed it again"))
	})

	It("says when the deployment's seeding failed", func() {
		topo.Status.Tenant = &benchv1alpha1.TenantStatus{Preset: "tiny", Params: params, State: benchv1alpha1.TenantFailed, Message: "seeding failed: boom"}
		meta.SetStatusCondition(&topo.Status.Conditions, metav1.Condition{
			Type: benchv1alpha1.ConditionReady, Status: metav1.ConditionFalse, Reason: benchv1alpha1.ReasonSeedingFailed, Message: "seeding failed: boom",
		})
		Expect(k8sClient.Status().Update(ctx, topo)).To(Succeed())
		ready := reconcileTestCase(testCase("tenant-failed", benchv1alpha1.ProfileBurn))
		Expect(ready.Message).To(Equal("tenant-target: tenant seeding failed: seeding failed: boom"))
	})

	It("gives a burn the tenant's numbers and password, provisioning nothing else", func() {
		seed(topo)
		tc := testCase("tenant-burn", benchv1alpha1.ProfileBurn)
		ready := reconcileTestCase(tc)
		Expect(ready.Status).To(Equal(metav1.ConditionTrue), ready.Message)

		var dep appsv1.Deployment
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: render.BurnDeploymentName(tc.Name, topo.Name), Namespace: "default"}, &dep)).To(Succeed())
		expectTenantEnv(dep.Spec.Template.Spec.Containers[0].Env)
		Expect(tc.Status.Deployments).To(HaveLen(1))
		Expect(tc.Status.Deployments[0].Tenant).To(Equal(&benchv1alpha1.TenantRecord{Preset: "tiny", Params: params}))
		// seeded-tenant alone has nothing to import into authentik.
		err := k8sClient.Get(ctx, types.NamespacedName{Name: "deps-blueprint-" + tc.Name + "-" + topo.Name, Namespace: topo.Spec.Namespace}, &corev1.ConfigMap{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "a dependency blueprint was rendered: %v", err)
	})

	It("gives a quick run the tenant's numbers and password", func() {
		seed(topo)
		run, _ := startQuickRun(testCase("tenant-quick", benchv1alpha1.ProfileQuick))
		Expect(run.Status.JobRef).NotTo(BeNil())
		var job batchv1.Job
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: run.Status.JobRef.Name, Namespace: "default"}, &job)).To(Succeed())
		expectTenantEnv(job.Spec.Template.Spec.Containers[0].Env)
		Expect(run.Status.Tenant).To(Equal(&benchv1alpha1.TenantRecord{Preset: "tiny", Params: params}))
	})

	It("holds a quick run on a deployment without a tenant", func() {
		run, result := startQuickRun(testCase("tenant-quick-none", benchv1alpha1.ProfileQuick))
		expectRunHeld(run, result, "template seeded-tenant-test needs a seeded tenant")
		Expect(run.Status.Tenant).To(BeNil())
	})

	It("holds a quick run on a tenant that needs a reset", func() {
		seed(topo)
		needsReset(topo)
		run, result := startQuickRun(testCase("tenant-quick-reset", benchv1alpha1.ProfileQuick))
		expectRunHeld(run, result, "reset the deployment to seed it again")
	})

	It("refuses a password other than the one the users were given", func() {
		seed(topo)
		replacePassword()
		const refusal = "the tenant password changed since tenant-target was seeded - reset tenant-target"

		tc := testCase("tenant-password", benchv1alpha1.ProfileBurn)
		ready := reconcileTestCase(tc)
		Expect(ready.Status).To(Equal(metav1.ConditionFalse))
		Expect(ready.Message).To(Equal("tenant-target: " + refusal))
		err := k8sClient.Get(ctx, types.NamespacedName{Name: render.BurnDeploymentName(tc.Name, topo.Name), Namespace: "default"}, &appsv1.Deployment{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "a burn was started with a password the users don't have")

		run, result := startQuickRun(testCase("tenant-password-quick", benchv1alpha1.ProfileQuick))
		expectRunHeld(run, result, refusal)
	})

	It("waits, rather than failing, when the password Secret is gone", func() {
		seed(topo)
		forceDelete(ctx, &corev1.Secret{Name: render.TenantPasswordSecretName, Namespace: "default"})
		r := &TestCaseReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: &record.FakeRecorder{}, Tracker: testTracker}
		tc := testCase("tenant-no-password", benchv1alpha1.ProfileBurn)
		result, err := r.Reconcile(ctx, reconcile.Request{Name: tc.Name, Namespace: tc.Namespace})
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(BeNumerically(">", 0))
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: tc.Name, Namespace: tc.Namespace}, tc)).To(Succeed())
		ready := meta.FindStatusCondition(tc.Status.Conditions, benchv1alpha1.ConditionReady)
		Expect(ready.Status).To(Equal(metav1.ConditionFalse))
		Expect(ready.Message).To(ContainSubstring("the tenant password Secret default/bench-tenant-password is gone"))

		run, result := startQuickRun(testCase("tenant-no-password-quick", benchv1alpha1.ProfileQuick))
		expectRunHeld(run, result, "is gone")
	})

	It("keeps a running burn going once its tenant needs a reset", func() {
		seed(topo)
		tc := testCase("tenant-burn-reset", benchv1alpha1.ProfileBurn)
		Expect(reconcileTestCase(tc).Status).To(Equal(metav1.ConditionTrue))
		needsReset(topo)
		Expect(reconcileTestCase(tc).Status).To(Equal(metav1.ConditionFalse))
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: render.BurnDeploymentName(tc.Name, topo.Name), Namespace: "default"}, &appsv1.Deployment{})).
			To(Succeed())
		Expect(tc.Status.Deployments[0].Tenant).NotTo(BeNil(), "the record of the tenant it runs on is kept")
	})

	It("refuses to compare targets seeded with different tenants", func() {
		other := createReadyDeployment(ctx, "tenant-other", "tenant-other", "tenant-other-server.ns.svc.cluster.local")
		DeferCleanup(func() { forceDelete(ctx, other) })
		seed(topo)
		bigger := params
		bigger.Users = 5000
		seedWith(other, "small", bigger)

		tc := testCaseOf("tenant-compare", benchv1alpha1.ProfileBurn, templateName, topo.Name, other.Name)
		ready := reconcileTestCase(tc)
		Expect(ready.Status).To(Equal(metav1.ConditionFalse))
		Expect(ready.Message).To(Equal(`the targets hold different seeded tenants: tenant-target has "tiny" (50 users), ` +
			`tenant-other has "small" (5000 users) - seed them from the same preset to compare them`))
		for _, target := range []string{topo.Name, other.Name} {
			err := k8sClient.Get(ctx, types.NamespacedName{Name: render.BurnDeploymentName(tc.Name, target), Namespace: "default"}, &appsv1.Deployment{})
			Expect(apierrors.IsNotFound(err)).To(BeTrue(), "a burn was set up on %s", target)
		}

		// The same numbers, however the preset is named, compare fine:
		// what a parent test case's children always get.
		seedWith(other, "tiny-copy", params)
		Expect(reconcileTestCase(tc).Status).To(Equal(metav1.ConditionTrue))
	})

	It("provisions oidc-client as usual next to a seeded tenant", func() {
		seed(topo)
		tmpl := &benchv1alpha1.TestCaseTemplate{
			Name: "tenant-oidc", Namespace: "default",
			Spec: benchv1alpha1.TestCaseTemplateSpec{
				Script: "export default function () {}",
				Dependencies: []benchv1alpha1.TestCaseDependency{
					benchv1alpha1.DependencyOIDCClient, benchv1alpha1.DependencySeededTenant,
				},
			},
		}
		Expect(k8sClient.Create(ctx, tmpl)).To(Succeed())
		DeferCleanup(func() { forceDelete(ctx, tmpl) })
		// The deployment controller creates it; none runs here.
		Expect(ensureNamespace(ctx, k8sClient, topo.Spec.Namespace)).To(Succeed())
		tc := testCaseOf("tenant-oidc", benchv1alpha1.ProfileBurn, tmpl.Name, topo.Name)
		first := reconcileTestCase(tc)
		Expect(first.Message).To(ContainSubstring("waiting for dependency blueprint import job"))

		// The blueprint is exactly oidc-client's: the tenant adds nothing.
		var secret corev1.Secret
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: render.OIDCTestClientName(tc, topo.Name) + "-secret", Namespace: "default"}, &secret)).
			To(Succeed())
		want, err := dependencyBlueprint{OIDCClient: &oidcClientBlueprint{
			idsFor(tc, topo.Name).oidcClient, string(secret.Data["client-secret"]),
			render.OIDCTestClientRootURL(tc, topo.Name, "default") + "/auth/callback",
		}}.render()
		Expect(err).NotTo(HaveOccurred())
		var cm corev1.ConfigMap
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "deps-blueprint-" + tc.Name + "-" + topo.Name, Namespace: topo.Spec.Namespace}, &cm)).
			To(Succeed())
		Expect(cm.Data[dependencyBlueprintKey]).To(Equal(want))

		var deployment benchv1alpha1.AuthentikDeployment
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(topo), &deployment)).To(Succeed())
		completeJob(ctx, types.NamespacedName{
			Name:      render.BlueprintJobName(&deployment, "deps-"+tc.Name+"-"+topo.Name, want),
			Namespace: topo.Spec.Namespace,
		})
		ready := reconcileTestCase(tc)
		Expect(ready.Status).To(Equal(metav1.ConditionTrue), ready.Message)
		var dep appsv1.Deployment
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: render.BurnDeploymentName(tc.Name, topo.Name), Namespace: "default"}, &dep)).To(Succeed())
		env := dep.Spec.Template.Spec.Containers[0].Env
		expectTenantEnv(env)
		Expect(envMap(env)).To(HaveKey("OAUTH_TEST_CLIENT_HOST"))
	})
})

// completeJob sets the conditions the Job controller, which envtest doesn't
// run, would on a Job that succeeded.
func completeJob(ctx context.Context, key types.NamespacedName) {
	GinkgoHelper()
	var job batchv1.Job
	Expect(k8sClient.Get(ctx, key, &job)).To(Succeed())
	now := metav1.Now()
	job.Status.StartTime, job.Status.CompletionTime, job.Status.Succeeded = &now, &now, 1
	job.Status.Conditions = []batchv1.JobCondition{
		{Type: batchv1.JobSuccessCriteriaMet, Status: corev1.ConditionTrue, LastTransitionTime: now},
		{Type: batchv1.JobComplete, Status: corev1.ConditionTrue, LastTransitionTime: now},
	}
	Expect(k8sClient.Status().Update(ctx, &job)).To(Succeed())
}
