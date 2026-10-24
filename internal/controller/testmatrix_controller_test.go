package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
)

var _ = Describe("TestMatrix Controller", func() {
	const name = "matrix"
	ctx := context.Background()
	key := types.NamespacedName{Name: name, Namespace: "default"}
	var r *TestMatrixReconciler

	BeforeEach(func() {
		r = &TestMatrixReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: record.NewFakeRecorder(100)}
		Expect(k8sClient.Create(ctx, &benchv1alpha1.TestMatrix{
			Name: name, Namespace: "default",
			Spec: benchv1alpha1.TestMatrixSpec{
				Variants: []benchv1alpha1.TestMatrixVariant{
					{Name: "pr", Tag: "gh-abc"},
					{Name: "main", Tag: "gh-main"},
				},
				Templates: []string{"login", "oauth-oidc"},
				Deployment: benchv1alpha1.AuthentikDeploymentSpec{
					ClusterRef: corev1.LocalObjectReference{Name: testClusterName},
					Server:     benchv1alpha1.WorkloadSpec{Replicas: 1},
					Worker:     benchv1alpha1.WorkloadSpec{Replicas: 1},
					Postgres:   benchv1alpha1.PostgresSpec{Mode: benchv1alpha1.PostgresEmbedded},
				},
				TestCase: benchv1alpha1.TestCaseSpec{Profile: benchv1alpha1.ProfileBurn},
			},
		})).To(Succeed())
	})

	AfterEach(func() {
		forceDelete(ctx, &benchv1alpha1.TestMatrix{Name: name, Namespace: "default"})
		for _, tc := range []string{"login", "oauth-oidc"} {
			forceDelete(ctx, &benchv1alpha1.TestCase{Name: name + "-" + tc, Namespace: "default"})
			for _, v := range []string{"pr", "main"} {
				forceDelete(ctx, &benchv1alpha1.AuthentikDeployment{Name: name + "-" + v + "-" + tc, Namespace: "default"})
			}
		}
	})

	reconcileOnce := func() {
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
	}

	It("creates a TestCase per template driving a deployment per variant", func() {
		reconcileOnce()

		var tcs benchv1alpha1.TestCaseList
		Expect(k8sClient.List(ctx, &tcs, client.MatchingLabels{benchv1alpha1.LabelTestMatrix: name})).To(Succeed())
		Expect(tcs.Items).To(HaveLen(2))
		var ads benchv1alpha1.AuthentikDeploymentList
		Expect(k8sClient.List(ctx, &ads, client.MatchingLabels{benchv1alpha1.LabelTestMatrix: name})).To(Succeed())
		Expect(ads.Items).To(HaveLen(4))

		var tc benchv1alpha1.TestCase
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name + "-oauth-oidc", Namespace: "default"}, &tc)).To(Succeed())
		Expect(tc.Spec.Template).To(Equal("oauth-oidc"))
		Expect(tc.Spec.DeploymentRefs).To(Equal([]corev1.LocalObjectReference{{Name: name + "-pr-oauth-oidc"}, {Name: name + "-main-oauth-oidc"}}))
		Expect(tc.Spec.BaselineRef.Name).To(Equal(name + "-pr-oauth-oidc"))
		Expect(tc.OwnerReferences).To(HaveLen(1))

		var m benchv1alpha1.TestMatrix
		Expect(k8sClient.Get(ctx, key, &m)).To(Succeed())
		Expect(m.Status.Rows).To(HaveLen(2))
		Expect(m.Status.Rows[0].Cells).To(HaveLen(2))
		Expect(m.Status.Phase).To(Equal(benchv1alpha1.PhaseReconciling))
	})

	It("leaves unchanged children alone on a repeat reconcile", func() {
		reconcileOnce()
		adKey := types.NamespacedName{Name: name + "-pr-login", Namespace: "default"}
		var before, after benchv1alpha1.AuthentikDeployment
		Expect(k8sClient.Get(ctx, adKey, &before)).To(Succeed())
		reconcileOnce()
		Expect(k8sClient.Get(ctx, adKey, &after)).To(Succeed())
		Expect(after.ResourceVersion).To(Equal(before.ResourceVersion))
	})

	It("deletes the children of a removed template", func() {
		reconcileOnce()
		var m benchv1alpha1.TestMatrix
		Expect(k8sClient.Get(ctx, key, &m)).To(Succeed())
		m.Spec.Templates = []string{"login"}
		Expect(k8sClient.Update(ctx, &m)).To(Succeed())
		reconcileOnce()

		err := k8sClient.Get(ctx, types.NamespacedName{Name: name + "-oauth-oidc", Namespace: "default"}, &benchv1alpha1.TestCase{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
		err = k8sClient.Get(ctx, types.NamespacedName{Name: name + "-main-oauth-oidc", Namespace: "default"}, &benchv1alpha1.AuthentikDeployment{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name + "-main-login", Namespace: "default"}, &benchv1alpha1.AuthentikDeployment{})).To(Succeed())
	})
})
