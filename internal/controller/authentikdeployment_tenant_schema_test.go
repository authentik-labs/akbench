package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
)

// The tenant fields only work if the CRDs keep them: an API server prunes
// any field its schema doesn't list, without an error.
var _ = Describe("AuthentikDeployment tenant schema", func() {
	ctx := context.Background()
	deployment := func(name string) *benchv1alpha1.AuthentikDeployment {
		return &benchv1alpha1.AuthentikDeployment{
			Name: name, Namespace: "default",
			Spec: benchv1alpha1.AuthentikDeploymentSpec{
				ClusterRef: corev1.LocalObjectReference{Name: testClusterName},
				Namespace:  "authentik-" + name,
				Server:     benchv1alpha1.WorkloadSpec{Replicas: 1},
				Worker:     benchv1alpha1.WorkloadSpec{Replicas: 1},
				Postgres:   benchv1alpha1.PostgresSpec{Mode: benchv1alpha1.PostgresEmbedded},
				Tenant:     &benchv1alpha1.TenantSpec{PresetRef: corev1.LocalObjectReference{Name: "small"}},
			},
		}
	}

	It("keeps spec.tenant and status.tenant", func() {
		topo := deployment("tenant-schema")
		Expect(k8sClient.Create(ctx, topo)).To(Succeed())
		DeferCleanup(func() { forceDelete(ctx, topo) })

		now := metav1.Now()
		topo.Status.Tenant = &benchv1alpha1.TenantStatus{
			Preset:              "small",
			Params:              benchv1alpha1.TenantParams{Seed: 1, Users: 5000, Groups: 100, GroupDepth: 3, GroupsPerUser: 5},
			PasswordFingerprint: "0123456789ab",
			State:               benchv1alpha1.TenantSeeded,
			SeededAt:            &now,
		}
		Expect(k8sClient.Status().Update(ctx, topo)).To(Succeed())

		var stored benchv1alpha1.AuthentikDeployment
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(topo), &stored)).To(Succeed())
		Expect(stored.Spec.Tenant).NotTo(BeNil())
		Expect(stored.Spec.Tenant.PresetRef.Name).To(Equal("small"))
		Expect(stored.Status.Tenant).NotTo(BeNil())
		Expect(stored.Status.Tenant.Params.GroupsPerUser).To(BeEquivalentTo(5))
		Expect(stored.Status.Tenant.State).To(Equal(benchv1alpha1.TenantSeeded))
	})

	It("rejects an unknown tenant state", func() {
		topo := deployment("tenant-schema-state")
		Expect(k8sClient.Create(ctx, topo)).To(Succeed())
		DeferCleanup(func() { forceDelete(ctx, topo) })

		topo.Status.Tenant = &benchv1alpha1.TenantStatus{Preset: "small", State: "Half-seeded"}
		Expect(k8sClient.Status().Update(ctx, topo)).NotTo(Succeed())
	})

	It("accepts seeded-tenant as a template dependency", func() {
		tmpl := &benchv1alpha1.TestCaseTemplate{
			Name: "tenant-schema", Namespace: "default",
			Spec: benchv1alpha1.TestCaseTemplateSpec{
				Script:       "export default function () {}",
				Dependencies: []benchv1alpha1.TestCaseDependency{benchv1alpha1.DependencySeededTenant},
			},
		}
		Expect(k8sClient.Create(ctx, tmpl)).To(Succeed())
		DeferCleanup(func() { forceDelete(ctx, tmpl) })
	})
})
