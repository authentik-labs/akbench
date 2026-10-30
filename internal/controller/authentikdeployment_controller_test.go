package controller

import (
	"context"
	"encoding/json"

	helmv2 "github.com/fluxcd/helm-controller/api/v2"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
	"github.com/authentik-labs/akbench/internal/render"
)

var _ = Describe("AuthentikDeployment Controller", func() {
	Context("Embedded postgres, Default placement (M1 scope)", func() {
		const resourceName = "deployment-embedded"
		var typeNamespacedName types.NamespacedName

		BeforeEach(func() {
			typeNamespacedName = types.NamespacedName{Name: resourceName, Namespace: "default"}
			topo := &benchv1alpha1.AuthentikDeployment{
				Name: resourceName, Namespace: "default",
				Spec: benchv1alpha1.AuthentikDeploymentSpec{
					ClusterRef: corev1.LocalObjectReference{Name: testClusterName},
					Namespace:  "authentik-" + resourceName,
					Server:     benchv1alpha1.WorkloadSpec{Replicas: 2},
					Worker:     benchv1alpha1.WorkloadSpec{Replicas: 1},
					Postgres:   benchv1alpha1.PostgresSpec{Mode: benchv1alpha1.PostgresEmbedded},
				},
			}
			Expect(k8sClient.Create(ctx, topo)).To(Succeed())
		})

		AfterEach(func() {
			forceDelete(ctx, &benchv1alpha1.AuthentikDeployment{Name: resourceName, Namespace: "default"})
		})

		It("renders a HelmRepository and a HelmRelease with the deployment's values", func() {
			r := &AuthentikDeploymentReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: record.NewFakeRecorder(100), Tracker: testTracker}
			_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			var hr helmv2.HelmRelease
			Expect(k8sClient.Get(context.Background(), typeNamespacedName, &hr)).To(Succeed())
			Expect(hr.Spec.TargetNamespace).To(Equal("authentik-" + resourceName))
			Expect(hr.Spec.Chart.Spec.Chart).To(Equal("authentik"))
			Expect(hr.Spec.KubeConfig).NotTo(BeNil())
			Expect(hr.Spec.KubeConfig.SecretRef.Name).To(Equal(kubeconfigSecretName(testClusterName)))

			var values render.AuthentikDeploymentHelmValues
			Expect(hr.Spec.Values).NotTo(BeNil())
			Expect(json.Unmarshal(hr.Spec.Values.Raw, &values)).To(Succeed())
			Expect(values.Server["replicas"]).To(BeNumerically("==", 2))
			Expect(values.Postgres["enabled"]).To(BeTrue())

			var topo benchv1alpha1.AuthentikDeployment
			Expect(k8sClient.Get(context.Background(), typeNamespacedName, &topo)).To(Succeed())
			postgresReady := meta.FindStatusCondition(topo.Status.Conditions, benchv1alpha1.ConditionPostgresReady)
			Expect(postgresReady).NotTo(BeNil())
			Expect(postgresReady.Status).To(Equal(metav1.ConditionTrue))
			// No real helm-controller runs in envtest, so HelmReleaseReady/Ready
			// stay False until it reconciles the HelmRelease we created above -
			// that's the meaningful thing to assert here, not a fake Ready=True.
			helmReleaseReady := meta.FindStatusCondition(topo.Status.Conditions, benchv1alpha1.ConditionHelmReleaseReady)
			Expect(helmReleaseReady).NotTo(BeNil())
			Expect(helmReleaseReady.Status).To(Equal(metav1.ConditionFalse))
		})

		// Regression test for a real bug: the postgres role-fix Job used to
		// be gated on the *whole* HelmRelease's Ready condition, which - as
		// the test above documents - never becomes True under envtest (no
		// real helm-controller runs here) and, live, never becomes True for
		// a deployment whose server/worker pods are crash-looping for
		// reasons that have nothing to do with postgres (confirmed: exactly
		// this happened to a real deployment, leaving pg_up stuck at 0
		// forever even though postgres itself was healthy the whole time).
		// The Job must appear once the postgres StatefulSet itself is
		// ready, independent of the rest of the release.
		It("creates the postgres role-fix Job once the postgres StatefulSet is ready, even if the HelmRelease overall isn't", func() {
			r := &AuthentikDeploymentReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: record.NewFakeRecorder(100), Tracker: testTracker}
			_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			var topo benchv1alpha1.AuthentikDeployment
			Expect(k8sClient.Get(context.Background(), typeNamespacedName, &topo)).To(Succeed())

			sts := &appsv1.StatefulSet{
				Name:      render.EmbeddedPostgresStatefulSetName(&topo),
				Namespace: topo.Spec.Namespace,
				Spec: appsv1.StatefulSetSpec{
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "postgresql"}},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "postgresql"}},
						Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "postgresql", Image: "postgres:17"}}},
					},
					ServiceName: render.EmbeddedPostgresStatefulSetName(&topo),
				},
			}
			Expect(k8sClient.Create(ctx, sts)).To(Succeed())
			sts.Status.Replicas = 1
			sts.Status.ReadyReplicas = 1
			Expect(k8sClient.Status().Update(ctx, sts)).To(Succeed())
			DeferCleanup(func() { forceDelete(ctx, sts) })

			_, err = r.Reconcile(context.Background(), reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			// HelmReleaseReady still False (no real helm-controller here) -
			// the Job must exist anyway.
			Expect(k8sClient.Get(context.Background(), typeNamespacedName, &topo)).To(Succeed())
			helmReleaseReady := meta.FindStatusCondition(topo.Status.Conditions, benchv1alpha1.ConditionHelmReleaseReady)
			Expect(helmReleaseReady.Status).To(Equal(metav1.ConditionFalse))

			var job batchv1.Job
			jobKey := types.NamespacedName{Name: render.PostgresRoleFixJobName(&topo), Namespace: topo.Spec.Namespace}
			Expect(k8sClient.Get(context.Background(), jobKey, &job)).To(Succeed())
		})
	})

	Context("Dedicated placement", func() {
		const resourceName = "deployment-dedicated"
		var typeNamespacedName types.NamespacedName

		BeforeEach(func() {
			typeNamespacedName = types.NamespacedName{Name: resourceName, Namespace: "default"}
			topo := &benchv1alpha1.AuthentikDeployment{
				Name: resourceName, Namespace: "default",
				Spec: benchv1alpha1.AuthentikDeploymentSpec{
					ClusterRef: corev1.LocalObjectReference{Name: testClusterName},
					Namespace:  "authentik-" + resourceName,
					Server:     benchv1alpha1.WorkloadSpec{Replicas: 1},
					Worker:     benchv1alpha1.WorkloadSpec{Replicas: 1},
					Postgres:   benchv1alpha1.PostgresSpec{Mode: benchv1alpha1.PostgresEmbedded},
					Placement:  benchv1alpha1.PlacementSpec{Mode: benchv1alpha1.PlacementDedicated},
				},
			}
			Expect(k8sClient.Create(ctx, topo)).To(Succeed())
		})

		AfterEach(func() {
			forceDelete(ctx, &benchv1alpha1.AuthentikDeployment{Name: resourceName, Namespace: "default"})
		})

		It("renders a required pod anti-affinity directly on server/worker", func() {
			r := &AuthentikDeploymentReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: record.NewFakeRecorder(100), Tracker: testTracker}
			_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			var hr helmv2.HelmRelease
			Expect(k8sClient.Get(context.Background(), typeNamespacedName, &hr)).To(Succeed())
			var values render.AuthentikDeploymentHelmValues
			Expect(json.Unmarshal(hr.Spec.Values.Raw, &values)).To(Succeed())
			// server.affinity/worker.affinity, not global.affinity.podAntiAffinity:
			// the chart's own preset mechanism is no longer used at all -
			// ServerWorkerAffinity renders the equivalent required
			// anti-affinity directly, alongside the always-present
			// cross-deployment term (see its own doc comment for why the two
			// can't be mixed: server.affinity/worker.affinity being set to
			// anything makes the chart skip its preset entirely).
			raw, err := json.Marshal(values.Server["affinity"])
			Expect(err).NotTo(HaveOccurred())
			var affinity map[string]any
			Expect(json.Unmarshal(raw, &affinity)).To(Succeed())
			podAntiAffinity, _ := affinity["podAntiAffinity"].(map[string]any)
			terms, _ := podAntiAffinity["requiredDuringSchedulingIgnoredDuringExecution"].([]any)
			Expect(terms).To(HaveLen(2), "want cross-deployment + own-release required terms")
		})
	})

	Context("External postgres", func() {
		const resourceName = "deployment-external"
		var typeNamespacedName types.NamespacedName

		BeforeEach(func() {
			typeNamespacedName = types.NamespacedName{Name: resourceName, Namespace: "default"}
			targetNs := "authentik-" + resourceName
			Expect(k8sClient.Create(ctx, &corev1.Namespace{Name: targetNs})).To(Succeed())
			Expect(k8sClient.Create(ctx, &corev1.Secret{
				Name: "external-pg", Namespace: targetNs,
				StringData: map[string]string{
					"host": "pg.example.com", "port": "5432", "database": "authentik",
					"username": "authentik", "password": "s3cret",
				},
			})).To(Succeed())
			topo := &benchv1alpha1.AuthentikDeployment{
				Name: resourceName, Namespace: "default",
				Spec: benchv1alpha1.AuthentikDeploymentSpec{
					ClusterRef: corev1.LocalObjectReference{Name: testClusterName},
					Namespace:  targetNs,
					Server:     benchv1alpha1.WorkloadSpec{Replicas: 1},
					Worker:     benchv1alpha1.WorkloadSpec{Replicas: 1},
					Postgres: benchv1alpha1.PostgresSpec{
						Mode:     benchv1alpha1.PostgresExternal,
						External: &benchv1alpha1.ExternalPostgresSpec{SecretRef: corev1.LocalObjectReference{Name: "external-pg"}},
					},
				},
			}
			Expect(k8sClient.Create(ctx, topo)).To(Succeed())
		})

		AfterEach(func() {
			forceDelete(ctx, &benchv1alpha1.AuthentikDeployment{Name: resourceName, Namespace: "default"})
			forceDelete(ctx, &corev1.Namespace{Name: "authentik-" + resourceName})
		})

		It("wires the referenced secret's connection into the chart values without touching CNPG", func() {
			r := &AuthentikDeploymentReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: record.NewFakeRecorder(100), Tracker: testTracker}
			_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			var hr helmv2.HelmRelease
			Expect(k8sClient.Get(context.Background(), typeNamespacedName, &hr)).To(Succeed())
			var values render.AuthentikDeploymentHelmValues
			Expect(json.Unmarshal(hr.Spec.Values.Raw, &values)).To(Succeed())
			Expect(values.Postgres["enabled"]).To(BeFalse())
			pg, _ := values.Authentik["postgresql"].(map[string]any)
			Expect(pg["host"]).To(Equal("pg.example.com"))
			Expect(pg["password"]).To(Equal("s3cret"))

			var topo benchv1alpha1.AuthentikDeployment
			Expect(k8sClient.Get(context.Background(), typeNamespacedName, &topo)).To(Succeed())
			postgresReady := meta.FindStatusCondition(topo.Status.Conditions, benchv1alpha1.ConditionPostgresReady)
			Expect(postgresReady).NotTo(BeNil())
			Expect(postgresReady.Status).To(Equal(metav1.ConditionTrue))
		})
	})

	It("rejects a deployment without a clusterRef (the management cluster)", func() {
		topo := &benchv1alpha1.AuthentikDeployment{
			Name: "deployment-management", Namespace: "default",
			Spec: benchv1alpha1.AuthentikDeploymentSpec{
				Namespace: "authentik-deployment-management",
				Server:    benchv1alpha1.WorkloadSpec{Replicas: 1},
				Worker:    benchv1alpha1.WorkloadSpec{Replicas: 1},
				Postgres:  benchv1alpha1.PostgresSpec{Mode: benchv1alpha1.PostgresEmbedded},
			},
		}
		Expect(k8sClient.Create(ctx, topo)).To(MatchError(ContainSubstring("management cluster is not allowed")))
	})

	Context("clusterRef set without a multicluster tracker wired up", func() {
		It("reports DependencyNotReady and requeues instead of touching a remote cluster", func() {
			topo := &benchv1alpha1.AuthentikDeployment{
				Name: "deployment-remote", Namespace: "default",
				Spec: benchv1alpha1.AuthentikDeploymentSpec{
					ClusterRef: corev1.LocalObjectReference{Name: "some-workload-cluster"},
					Namespace:  "authentik-deployment-remote",
					Server:     benchv1alpha1.WorkloadSpec{Replicas: 1},
					Worker:     benchv1alpha1.WorkloadSpec{Replicas: 1},
					Postgres:   benchv1alpha1.PostgresSpec{Mode: benchv1alpha1.PostgresEmbedded},
				},
			}
			Expect(k8sClient.Create(context.Background(), topo)).To(Succeed())
			defer forceDelete(context.Background(), topo)

			// No Tracker configured (this suite only unit-tests reconcile
			// logic directly, without a running manager/ClusterCache) -
			// resolveClient must report this as "not ready yet", not panic.
			r := &AuthentikDeploymentReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: record.NewFakeRecorder(100)}
			key := types.NamespacedName{Name: topo.Name, Namespace: topo.Namespace}
			result, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeNumerically(">", 0))

			Expect(k8sClient.Get(context.Background(), key, topo)).To(Succeed())
			ready := meta.FindStatusCondition(topo.Status.Conditions, benchv1alpha1.ConditionReady)
			Expect(ready).NotTo(BeNil())
			Expect(ready.Status).To(Equal(metav1.ConditionFalse))
			Expect(ready.Reason).To(Equal(benchv1alpha1.ReasonDependencyNotReady))
		})
	})
})
