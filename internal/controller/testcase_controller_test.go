package controller

import (
	"context"
	"strconv"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
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

var _ = Describe("TestCase Controller", func() {
	var topo *benchv1alpha1.AuthentikDeployment

	BeforeEach(func() {
		topo = createReadyDeployment(context.Background(), "testcase-deployment", "authentik-1", "authentik-1-server.ns.svc.cluster.local")
	})

	AfterEach(func() {
		forceDelete(context.Background(), topo)
	})

	Context("Burn profile", func() {
		const resourceName = "burn-login"
		typeNamespacedName := types.NamespacedName{Name: resourceName, Namespace: "default"}

		BeforeEach(func() {
			tc := &benchv1alpha1.TestCase{
				Name: resourceName, Namespace: "default",
				Spec: benchv1alpha1.TestCaseSpec{
					DeploymentRefs: []corev1.LocalObjectReference{{Name: topo.Name}},
					Profile:        benchv1alpha1.ProfileBurn,
					VUs:            8,
					Flow:           "default-authentication-flow",
				},
			}
			Expect(k8sClient.Create(context.Background(), tc)).To(Succeed())
		})

		AfterEach(func() {
			forceDelete(context.Background(), &benchv1alpha1.TestCase{Name: typeNamespacedName.Name, Namespace: typeNamespacedName.Namespace})
		})

		It("owns a Deployment tagged with the testid/target/profile contract", func() {
			r := &TestCaseReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: record.NewFakeRecorder(100), Tracker: testTracker}
			_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			var dep appsv1.Deployment
			depName := "k6-" + resourceName + "-" + topo.Name
			depKey := types.NamespacedName{Name: depName, Namespace: "default"}
			Expect(k8sClient.Get(context.Background(), depKey, &dep)).To(Succeed())

			args := dep.Spec.Template.Spec.Containers[0].Args
			Expect(args).To(ContainElement("testid=" + resourceName))
			Expect(args).To(ContainElement("target=authentik-1"))
			Expect(args).To(ContainElement("profile=burn"))

			env := envMap(dep.Spec.Template.Spec.Containers[0].Env)
			Expect(env["TARGET"]).To(Equal("authentik-1"))
			Expect(env["BENCH_HOST"]).To(Equal("authentik-1-server.ns.svc.cluster.local"))
			Expect(env["BENCH_PORT"]).To(Equal("80"))
			Expect(env["PROFILE"]).To(Equal("burn"))

			var tc benchv1alpha1.TestCase
			Expect(k8sClient.Get(context.Background(), typeNamespacedName, &tc)).To(Succeed())
			depStatus := findDeploymentStatus(tc.Status.Deployments, topo.Name)
			Expect(depStatus).NotTo(BeNil())
			Expect(depStatus.Ready).To(BeTrue())
			Expect(depStatus.BurnWorkloadRef).NotTo(BeNil())
			Expect(depStatus.BurnWorkloadRef.Name).To(Equal(depName))
			ready := meta.FindStatusCondition(tc.Status.Conditions, benchv1alpha1.ConditionReady)
			Expect(ready).NotTo(BeNil())
			Expect(ready.Status).To(Equal(metav1.ConditionTrue))
		})

		It("starts k6 only once its pod is up, and records that as burningSince", func() {
			r := &TestCaseReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: record.NewFakeRecorder(100), Tracker: testTracker}
			res, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())
			Expect(res.RequeueAfter).NotTo(BeZero())

			var tc benchv1alpha1.TestCase
			Expect(k8sClient.Get(context.Background(), typeNamespacedName, &tc)).To(Succeed())
			Expect(findDeploymentStatus(tc.Status.Deployments, topo.Name).BurningSince).To(BeNil())

			markBurnDeploymentStaged(render.BurnDeploymentName(resourceName, topo.Name))
			_, err = r.Reconcile(context.Background(), reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(context.Background(), typeNamespacedName, &tc)).To(Succeed())
			burningSince := findDeploymentStatus(tc.Status.Deployments, topo.Name).BurningSince
			Expect(burningSince).NotTo(BeNil())
			Expect(burningSince.After(time.Now())).To(BeTrue())

			// The next pass releases the waiting pod.
			_, err = r.Reconcile(context.Background(), reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())
			var cm corev1.ConfigMap
			Expect(k8sClient.Get(context.Background(), types.NamespacedName{Name: "k6-script-" + resourceName, Namespace: "default"}, &cm)).To(Succeed())
			Expect(cm.Data[render.StartAtFileName]).To(Equal(strconv.FormatInt(burningSince.Unix(), 10)))
		})

		It("stops its burn Deployment when archived but keeps the TestCase", func() {
			r := &TestCaseReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: record.NewFakeRecorder(100), Tracker: testTracker}
			_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())
			markBurnDeploymentStaged(render.BurnDeploymentName(resourceName, topo.Name))
			_, err = r.Reconcile(context.Background(), reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			var tc benchv1alpha1.TestCase
			Expect(k8sClient.Get(context.Background(), typeNamespacedName, &tc)).To(Succeed())
			tc.Spec.Archived = true
			Expect(k8sClient.Update(context.Background(), &tc)).To(Succeed())
			_, err = r.Reconcile(context.Background(), reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			var deps appsv1.DeploymentList
			Expect(k8sClient.List(context.Background(), &deps, client.InNamespace("default"),
				client.MatchingLabels{benchv1alpha1.LabelTestCase: resourceName})).To(Succeed())
			Expect(deps.Items).To(BeEmpty())
			Expect(k8sClient.Get(context.Background(), typeNamespacedName, &tc)).To(Succeed())
			Expect(tc.Status.Phase).To(Equal(benchv1alpha1.PhaseArchived))
			archived := findDeploymentStatus(tc.Status.Deployments, topo.Name)
			Expect(archived.BurnWorkloadRef).To(BeNil())
			// The burn's window stays recorded, so its metrics can still be
			// shown for the time it ran.
			Expect(archived.BurningSince).NotTo(BeNil())
			Expect(archived.BurnStoppedAt).NotTo(BeNil())
			// So does where the deployment ran, to find its telemetry once
			// it's deleted.
			Expect(archived.Namespace).To(Equal(topo.Spec.Namespace))
			Expect(archived.Target).To(Equal("authentik-1"))

			// A later reconcile keeps the original stop time.
			stoppedAt := *archived.BurnStoppedAt
			_, err = r.Reconcile(context.Background(), reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(context.Background(), typeNamespacedName, &tc)).To(Succeed())
			Expect(findDeploymentStatus(tc.Status.Deployments, topo.Name).BurnStoppedAt.Equal(&stoppedAt)).To(BeTrue())
		})
	})

	Context("Quick profile", func() {
		const resourceName = "quick-login"
		typeNamespacedName := types.NamespacedName{Name: resourceName, Namespace: "default"}

		BeforeEach(func() {
			tc := &benchv1alpha1.TestCase{
				Name: resourceName, Namespace: "default",
				Spec: benchv1alpha1.TestCaseSpec{
					DeploymentRefs: []corev1.LocalObjectReference{{Name: topo.Name}},
					Profile:        benchv1alpha1.ProfileQuick,
					VUs:            4,
					Iterations:     100,
					Flow:           "default-authentication-flow",
				},
			}
			Expect(k8sClient.Create(context.Background(), tc)).To(Succeed())
		})

		AfterEach(func() {
			forceDelete(context.Background(), &benchv1alpha1.TestCase{Name: typeNamespacedName.Name, Namespace: typeNamespacedName.Namespace})
		})

		It("does not create a burn Deployment - execution belongs to a TestRun", func() {
			r := &TestCaseReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: record.NewFakeRecorder(100), Tracker: testTracker}
			_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			var dep appsv1.Deployment
			depKey := types.NamespacedName{Name: "k6-" + resourceName, Namespace: "default"}
			err = k8sClient.Get(context.Background(), depKey, &dep)
			Expect(apierrors.IsNotFound(err)).To(BeTrue())

			var tc benchv1alpha1.TestCase
			Expect(k8sClient.Get(context.Background(), typeNamespacedName, &tc)).To(Succeed())
			scriptReady := meta.FindStatusCondition(tc.Status.Conditions, benchv1alpha1.ConditionScriptReady)
			Expect(scriptReady).NotTo(BeNil())
			Expect(scriptReady.Status).To(Equal(metav1.ConditionTrue))
		})
	})

	Context("inline script", func() {
		const resourceName = "inline-script"
		typeNamespacedName := types.NamespacedName{Name: resourceName, Namespace: "default"}
		const inlineBody = "export default function () { /* custom */ }"

		BeforeEach(func() {
			tc := &benchv1alpha1.TestCase{
				Name: resourceName, Namespace: "default",
				Spec: benchv1alpha1.TestCaseSpec{
					DeploymentRefs: []corev1.LocalObjectReference{{Name: topo.Name}},
					Inline:         inlineBody,
					Profile:        benchv1alpha1.ProfileQuick,
					VUs:            1,
					Iterations:     1,
				},
			}
			Expect(k8sClient.Create(context.Background(), tc)).To(Succeed())
		})

		AfterEach(func() {
			forceDelete(context.Background(), &benchv1alpha1.TestCase{Name: typeNamespacedName.Name, Namespace: typeNamespacedName.Namespace})
		})

		It("mirrors the inline body verbatim into the script ConfigMap under script.ts, overriding template", func() {
			r := &TestCaseReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: record.NewFakeRecorder(100), Tracker: testTracker}
			_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			var cm corev1.ConfigMap
			cmKey := types.NamespacedName{Name: "k6-script-" + resourceName, Namespace: "default"}
			Expect(k8sClient.Get(context.Background(), cmKey, &cm)).To(Succeed())
			Expect(cm.Data).To(HaveKeyWithValue("script.ts", inlineBody))
			Expect(cm.Data).To(HaveKey(render.LibFileName), "the shared k6 library is mounted next to every script")
		})
	})

	Context("unknown template", func() {
		const resourceName = "bad-template"
		typeNamespacedName := types.NamespacedName{Name: resourceName, Namespace: "default"}

		BeforeEach(func() {
			tc := &benchv1alpha1.TestCase{
				Name: resourceName, Namespace: "default",
				Spec: benchv1alpha1.TestCaseSpec{
					DeploymentRefs: []corev1.LocalObjectReference{{Name: topo.Name}},
					Template:       "does-not-exist",
					Profile:        benchv1alpha1.ProfileQuick,
					VUs:            1,
					Iterations:     1,
				},
			}
			Expect(k8sClient.Create(context.Background(), tc)).To(Succeed())
		})

		AfterEach(func() {
			forceDelete(context.Background(), &benchv1alpha1.TestCase{Name: typeNamespacedName.Name, Namespace: typeNamespacedName.Namespace})
		})

		It("reports DependencyNotReady with a clear message instead of silently falling back", func() {
			r := &TestCaseReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: record.NewFakeRecorder(100), Tracker: testTracker}
			_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			var tc benchv1alpha1.TestCase
			Expect(k8sClient.Get(context.Background(), typeNamespacedName, &tc)).To(Succeed())
			ready := meta.FindStatusCondition(tc.Status.Conditions, benchv1alpha1.ConditionReady)
			Expect(ready).NotTo(BeNil())
			Expect(ready.Status).To(Equal(metav1.ConditionFalse))
			Expect(ready.Reason).To(Equal(benchv1alpha1.ReasonDependencyNotReady))

			script := meta.FindStatusCondition(tc.Status.Conditions, benchv1alpha1.ConditionScriptReady)
			Expect(script).NotTo(BeNil())
			Expect(script.Status).To(Equal(metav1.ConditionFalse))
			Expect(script.Message).To(ContainSubstring("does-not-exist"))
		})
	})

	Context("Burn profile, multiple deployments", func() {
		const resourceName = "burn-login-multi"
		typeNamespacedName := types.NamespacedName{Name: resourceName, Namespace: "default"}
		var topoB *benchv1alpha1.AuthentikDeployment

		BeforeEach(func() {
			topoB = createReadyDeployment(context.Background(), "testcase-deployment-b", "authentik-2", "authentik-2-server.ns.svc.cluster.local")
			tc := &benchv1alpha1.TestCase{
				Name: resourceName, Namespace: "default",
				Spec: benchv1alpha1.TestCaseSpec{
					DeploymentRefs: []corev1.LocalObjectReference{{Name: topo.Name}, {Name: topoB.Name}},
					Profile:        benchv1alpha1.ProfileBurn,
					VUs:            8,
					Flow:           "default-authentication-flow",
				},
			}
			Expect(k8sClient.Create(context.Background(), tc)).To(Succeed())
		})

		AfterEach(func() {
			forceDelete(context.Background(), &benchv1alpha1.TestCase{Name: typeNamespacedName.Name, Namespace: typeNamespacedName.Namespace})
			forceDelete(context.Background(), topoB)
		})

		It("owns one independently-selectable burn Deployment per target and reports both in status", func() {
			r := &TestCaseReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: record.NewFakeRecorder(100), Tracker: testTracker}
			_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			depAName := "k6-" + resourceName + "-" + topo.Name
			depBName := "k6-" + resourceName + "-" + topoB.Name

			var depA, depB appsv1.Deployment
			Expect(k8sClient.Get(context.Background(), types.NamespacedName{Name: depAName, Namespace: "default"}, &depA)).To(Succeed())
			Expect(k8sClient.Get(context.Background(), types.NamespacedName{Name: depBName, Namespace: "default"}, &depB)).To(Succeed())

			argsA := depA.Spec.Template.Spec.Containers[0].Args
			Expect(argsA).To(ContainElement("target=authentik-1"))
			argsB := depB.Spec.Template.Spec.Containers[0].Args
			Expect(argsB).To(ContainElement("target=authentik-2"))

			selA, _ := metav1.LabelSelectorAsMap(depA.Spec.Selector)
			selB, _ := metav1.LabelSelectorAsMap(depB.Spec.Selector)
			Expect(selA).NotTo(Equal(selB), "the two burn Deployments must not share a selector")

			var tc benchv1alpha1.TestCase
			Expect(k8sClient.Get(context.Background(), typeNamespacedName, &tc)).To(Succeed())
			Expect(tc.Status.Deployments).To(HaveLen(2))
			statusA := findDeploymentStatus(tc.Status.Deployments, topo.Name)
			statusB := findDeploymentStatus(tc.Status.Deployments, topoB.Name)
			Expect(statusA).NotTo(BeNil())
			Expect(statusA.Ready).To(BeTrue())
			Expect(statusA.BurnWorkloadRef.Name).To(Equal(depAName))
			Expect(statusB).NotTo(BeNil())
			Expect(statusB.Ready).To(BeTrue())
			Expect(statusB.BurnWorkloadRef.Name).To(Equal(depBName))

			ready := meta.FindStatusCondition(tc.Status.Conditions, benchv1alpha1.ConditionReady)
			Expect(ready).NotTo(BeNil())
			Expect(ready.Status).To(Equal(metav1.ConditionTrue))
		})

		It("keeps every target's burn Deployment across reconciles instead of recreating it", func() {
			r := &TestCaseReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: record.NewFakeRecorder(100), Tracker: testTracker}
			_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			depBKey := types.NamespacedName{Name: "k6-" + resourceName + "-" + topoB.Name, Namespace: "default"}
			var before appsv1.Deployment
			Expect(k8sClient.Get(context.Background(), depBKey, &before)).To(Succeed())

			_, err = r.Reconcile(context.Background(), reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			var after appsv1.Deployment
			Expect(k8sClient.Get(context.Background(), depBKey, &after)).To(Succeed())
			Expect(after.UID).To(Equal(before.UID), "a later target's burn Deployment must not be deleted by an earlier target's cleanup")
		})

		It("deletes every burn Deployment even when its AuthentikDeployments were deleted first", func() {
			r := &TestCaseReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: record.NewFakeRecorder(100), Tracker: testTracker}
			_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			var tc benchv1alpha1.TestCase
			Expect(k8sClient.Get(context.Background(), typeNamespacedName, &tc)).To(Succeed())
			Expect(findDeploymentStatus(tc.Status.Deployments, topoB.Name).RunnerCluster).To(Equal(testClusterName))

			// The UI deletes the deployments and the TestCase together; the
			// deployments' removal is usually observed first.
			forceDelete(context.Background(), topo)
			forceDelete(context.Background(), topoB)
			_, err = r.Reconcile(context.Background(), reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(context.Background(), typeNamespacedName, &tc)).To(Succeed())
			Expect(findDeploymentStatus(tc.Status.Deployments, topoB.Name).RunnerCluster).To(Equal(testClusterName),
				"an unresolvable target must keep the runner cluster it last ran on")

			Expect(k8sClient.Delete(context.Background(), &tc)).To(Succeed())
			_, err = r.Reconcile(context.Background(), reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			var deps appsv1.DeploymentList
			Expect(k8sClient.List(context.Background(), &deps, client.InNamespace("default"),
				client.MatchingLabels{benchv1alpha1.LabelTestCase: resourceName})).To(Succeed())
			Expect(deps.Items).To(BeEmpty())
		})
	})

	Context("Burn profile, pre-multi-deployment object", func() {
		const resourceName = "burn-login-legacy"
		typeNamespacedName := types.NamespacedName{Name: resourceName, Namespace: "default"}
		legacyKey := types.NamespacedName{Name: "k6-" + resourceName, Namespace: "default"}

		BeforeEach(func() {
			tc := &benchv1alpha1.TestCase{
				Name: resourceName, Namespace: "default",
				Spec: benchv1alpha1.TestCaseSpec{
					DeploymentRef: corev1.LocalObjectReference{Name: topo.Name}, //nolint:staticcheck // exercising the deprecated field's migration
					Profile:       benchv1alpha1.ProfileBurn,
					VUs:           8,
					Flow:          "default-authentication-flow",
				},
			}
			Expect(k8sClient.Create(context.Background(), tc)).To(Succeed())

			// The Burn Deployment as written before multi-deployment support:
			// named k6-<testcase>, no object labels, selector on LabelTestCase only.
			podLabels := map[string]string{benchv1alpha1.LabelTestCase: resourceName}
			legacy := &appsv1.Deployment{
				Name: legacyKey.Name, Namespace: legacyKey.Namespace,
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{MatchLabels: podLabels},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{Labels: podLabels},
						Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "k6", Image: "grafana/k6"}}},
					},
				},
			}
			Expect(k8sClient.Create(context.Background(), legacy)).To(Succeed())
		})

		AfterEach(func() {
			forceDelete(context.Background(), &benchv1alpha1.TestCase{Name: typeNamespacedName.Name, Namespace: typeNamespacedName.Namespace})
			_ = k8sClient.Delete(context.Background(), &appsv1.Deployment{Name: legacyKey.Name, Namespace: legacyKey.Namespace})
		})

		It("migrates deploymentRef into deploymentRefs and replaces the legacy burn Deployment", func() {
			r := &TestCaseReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: record.NewFakeRecorder(100), Tracker: testTracker}
			_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			var tc benchv1alpha1.TestCase
			Expect(k8sClient.Get(context.Background(), typeNamespacedName, &tc)).To(Succeed())
			Expect(tc.Spec.DeploymentRefs).To(Equal([]corev1.LocalObjectReference{{Name: topo.Name}}))
			Expect(tc.Deployments()).To(Equal([]corev1.LocalObjectReference{{Name: topo.Name}}))

			var legacy appsv1.Deployment
			err = k8sClient.Get(context.Background(), legacyKey, &legacy)
			Expect(apierrors.IsNotFound(err)).To(BeTrue(), "legacy burn Deployment should be deleted, got %v", err)

			var current appsv1.Deployment
			Expect(k8sClient.Get(context.Background(), types.NamespacedName{Name: "k6-" + resourceName + "-" + topo.Name, Namespace: "default"}, &current)).To(Succeed())
		})
	})
})

func envMap(vars []corev1.EnvVar) map[string]string {
	m := make(map[string]string, len(vars))
	for _, v := range vars {
		m[v.Name] = v.Value
	}
	return m
}

func findDeploymentStatus(deployments []benchv1alpha1.TestCaseDeploymentStatus, name string) *benchv1alpha1.TestCaseDeploymentStatus {
	for i := range deployments {
		if deployments[i].Name == name {
			return &deployments[i]
		}
	}
	return nil
}

// markBurnDeploymentStaged fakes the Deployment controller envtest lacks:
// the named burn Deployment's k6 pod is up, waiting at the start gate.
func markBurnDeploymentStaged(name string) {
	var dep appsv1.Deployment
	Expect(k8sClient.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "default"}, &dep)).To(Succeed())
	dep.Status.ObservedGeneration = dep.Generation
	dep.Status.Replicas, dep.Status.UpdatedReplicas, dep.Status.ReadyReplicas = 1, 1, 1
	Expect(k8sClient.Status().Update(context.Background(), &dep)).To(Succeed())
}
