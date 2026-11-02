package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
)

var _ = Describe("TestMatrix migration", func() {
	const name = "legacy"
	ctx := context.Background()
	key := types.NamespacedName{Name: name, Namespace: "default"}
	childKey := types.NamespacedName{Name: name + "-login", Namespace: "default"}
	adKey := types.NamespacedName{Name: name + "-pr-login", Namespace: "default"}
	var r *TestMatrixReconciler
	var m *benchv1alpha1.TestMatrix

	BeforeEach(func() {
		r = &TestMatrixReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: record.NewFakeRecorder(100)}
		m = &benchv1alpha1.TestMatrix{
			Name: name, Namespace: "default",
			Labels: map[string]string{"bench.goauthentik.io/pull-request": "1"},
			Spec: benchv1alpha1.TestMatrixSpec{
				Variants:  []benchv1alpha1.TestCaseVariant{{Name: "pr", Tag: "gh-abc"}},
				Baseline:  "pr",
				Templates: []string{"login"},
				Deployment: benchv1alpha1.AuthentikDeploymentSpec{
					ClusterRef: corev1.LocalObjectReference{Name: testClusterName},
					Server:     benchv1alpha1.WorkloadSpec{Replicas: 1},
					Worker:     benchv1alpha1.WorkloadSpec{Replicas: 1},
					Postgres:   benchv1alpha1.PostgresSpec{Mode: benchv1alpha1.PostgresEmbedded},
				},
				TestCase: benchv1alpha1.TestCaseSpec{Profile: benchv1alpha1.ProfileBurn, VUs: 3},
			},
		}
		Expect(k8sClient.Create(ctx, m)).To(Succeed())

		// What the matrix controller created before the migration.
		labels := map[string]string{benchv1alpha1.LabelTestMatrix: name}
		ad := &benchv1alpha1.AuthentikDeployment{Name: adKey.Name, Namespace: "default", Labels: labels, Spec: m.Spec.Deployment}
		ad.Spec.Namespace = adKey.Name
		child := &benchv1alpha1.TestCase{Name: childKey.Name, Namespace: "default", Labels: labels, Spec: benchv1alpha1.TestCaseSpec{
			Profile: benchv1alpha1.ProfileBurn, Template: "login", DeploymentRefs: []corev1.LocalObjectReference{{Name: adKey.Name}},
		}}
		Expect(controllerutil.SetControllerReference(m, ad, k8sClient.Scheme())).To(Succeed())
		Expect(controllerutil.SetControllerReference(m, child, k8sClient.Scheme())).To(Succeed())
		Expect(k8sClient.Create(ctx, ad)).To(Succeed())
		Expect(k8sClient.Create(ctx, child)).To(Succeed())
	})

	AfterEach(func() {
		forceDelete(ctx, &benchv1alpha1.TestMatrix{Name: name, Namespace: "default"})
		forceDelete(ctx, &benchv1alpha1.TestCase{Name: name, Namespace: "default"})
		forceDelete(ctx, &benchv1alpha1.TestCase{Name: childKey.Name, Namespace: "default"})
		forceDelete(ctx, &benchv1alpha1.AuthentikDeployment{Name: adKey.Name, Namespace: "default"})
	})

	It("moves the matrix and its children to a parent TestCase", func() {
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		var parent benchv1alpha1.TestCase
		Expect(k8sClient.Get(ctx, key, &parent)).To(Succeed())
		Expect(parent.Spec.Templates).To(Equal([]string{"login"}))
		Expect(parent.Spec.Variants).To(Equal(m.Spec.Variants))
		Expect(parent.Spec.BaselineVariant).To(Equal("pr"))
		Expect(parent.Spec.VUs).To(Equal(int32(3)))
		Expect(parent.Spec.Deployment).NotTo(BeNil())
		Expect(parent.Labels).To(HaveKeyWithValue("bench.goauthentik.io/pull-request", "1"))

		var child benchv1alpha1.TestCase
		Expect(k8sClient.Get(ctx, childKey, &child)).To(Succeed())
		var ad benchv1alpha1.AuthentikDeployment
		Expect(k8sClient.Get(ctx, adKey, &ad)).To(Succeed())
		for _, obj := range []metav1.Object{&child, &ad} {
			Expect(obj.GetLabels()).To(Equal(map[string]string{benchv1alpha1.LabelParent: name}))
			Expect(obj.GetOwnerReferences()).To(HaveLen(1))
			Expect(metav1.IsControlledBy(obj, &parent)).To(BeTrue())
		}

		err = k8sClient.Get(ctx, key, &benchv1alpha1.TestMatrix{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
	})

	It("keeps an archived matrix archived without respawning what was deleted", func() {
		// Archived, and its deployment deleted to free the cluster.
		Expect(k8sClient.Get(ctx, key, m)).To(Succeed())
		m.Spec.TestCase.Archived = true
		Expect(k8sClient.Update(ctx, m)).To(Succeed())
		var child benchv1alpha1.TestCase
		Expect(k8sClient.Get(ctx, childKey, &child)).To(Succeed())
		child.Spec.Archived = true
		Expect(k8sClient.Update(ctx, &child)).To(Succeed())
		forceDelete(ctx, &benchv1alpha1.AuthentikDeployment{Name: adKey.Name, Namespace: "default"})

		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		var parent benchv1alpha1.TestCase
		Expect(k8sClient.Get(ctx, key, &parent)).To(Succeed())
		Expect(parent.Spec.Archived).To(BeTrue())

		tcr := &TestCaseReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: record.NewFakeRecorder(100), Tracker: testTracker}
		for range 2 {
			_, err = tcr.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
		}
		err = k8sClient.Get(ctx, adKey, &benchv1alpha1.AuthentikDeployment{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
		Expect(k8sClient.Get(ctx, childKey, &child)).To(Succeed())
		Expect(child.Spec.Archived).To(BeTrue())
		Expect(k8sClient.Get(ctx, key, &parent)).To(Succeed())
		Expect(parent.Status.Phase).To(Equal(benchv1alpha1.PhaseArchived))
	})

	It("leaves a TestCase of the same name that isn't a parent alone", func() {
		Expect(k8sClient.Create(ctx, &benchv1alpha1.TestCase{Name: name, Namespace: "default", Spec: benchv1alpha1.TestCaseSpec{
			Profile: benchv1alpha1.ProfileQuick, DeploymentRefs: []corev1.LocalObjectReference{{Name: "other"}},
		}})).To(Succeed())
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).To(HaveOccurred())

		var child benchv1alpha1.TestCase
		Expect(k8sClient.Get(ctx, childKey, &child)).To(Succeed())
		Expect(child.Labels).To(HaveKey(benchv1alpha1.LabelTestMatrix))
		var matrix benchv1alpha1.TestMatrix
		Expect(k8sClient.Get(ctx, key, &matrix)).To(Succeed())
		Expect(matrix.Status.Phase).To(Equal(benchv1alpha1.PhaseFailed))
	})
})
